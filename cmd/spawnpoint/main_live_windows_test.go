//go:build windows

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"spawnpoint/internal/opslog"
)

// These tests run the real executable and stop it the way an operator or the
// operating system would. They are the measurement behind the completion test
// for this stage — `stopping reason=` and `stopped` present in the operations
// log — and the start of the exit-path measurement in 0008-L 6.4.
//
// They are skipped unless SPAWNPOINT_LIVE_SHUTDOWN=1, matching the convention
// the command-line spike established: the default test run starts no processes.
//
//	SPAWNPOINT_LIVE_SHUTDOWN=1 go test ./cmd/spawnpoint/
//
// Every run uses its own port, database path and log directory. 0004-NR 1.5.1
// warns that touching the production instance takes down everything it manages.

const liveEnv = "SPAWNPOINT_LIVE_SHUTDOWN"

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to run the live shutdown checks", liveEnv)
	}
}

// The executable is built once for the whole test binary and removed at the
// end, so it outlives any individual test's temporary directory.
var builtPath string

func TestMain(m *testing.M) {
	// Helper mode, used by the console-close measurement in 0008-L 6.4. This
	// binary is re-run with the probe's pid so that the attaching to its
	// console — which makes the attacher a recipient of the very event being
	// delivered — happens in a process that can be spent (see
	// exitpath_live_windows_test.go).
	if pid := os.Getenv(envCloseConsoleOf); pid != "" {
		n, err := strconv.Atoi(pid)
		if err != nil {
			os.Exit(10)
		}
		os.Exit(closeConsoleOf(n))
	}

	code := m.Run()
	if builtPath != "" {
		os.RemoveAll(filepath.Dir(builtPath))
	}
	if probePath != "" {
		os.RemoveAll(filepath.Dir(probePath))
	}
	os.Exit(code)
}

func build(t *testing.T) string {
	t.Helper()
	if builtPath != "" {
		return builtPath
	}
	dir, err := os.MkdirTemp("", "spawnpoint-build")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "spawnpoint.exe")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		t.Fatalf("go build: %v\n%s", err, out)
	}
	builtPath = exe
	return exe
}

// freePort returns a port nothing is listening on. There is an unavoidable race
// between closing this listener and the server binding it; the alternative,
// a fixed port, races with whatever else is on the machine and with a parallel
// run of these same tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// instance is one running server under test.
type instance struct {
	cmd     *exec.Cmd
	logDir  string
	logPath string
	port    int
	stderr  *strings.Builder
}

// start launches the executable in its own process group, which is what makes
// a console control event addressable to it alone.
func start(t *testing.T, port int) *instance {
	t.Helper()
	exe := build(t)
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"SPAWNPOINT_HOST=127.0.0.1",
		"SPAWNPOINT_PORT="+strconv.Itoa(port),
		"SPAWNPOINT_DB_PATH="+filepath.Join(dir, "spawnpoint.db"),
		"SPAWNPOINT_LOG_DIR="+logDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", exe, err)
	}
	inst := &instance{
		cmd:     cmd,
		logDir:  logDir,
		logPath: filepath.Join(logDir, opslog.FileName),
		port:    port,
		stderr:  &stderr,
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return inst
}

func (i *instance) log() string {
	b, err := os.ReadFile(i.logPath)
	if err != nil {
		return ""
	}
	return string(b)
}

// waitForLog blocks until the operations log contains want.
func (i *instance) waitForLog(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	i.waitForCount(t, want, 1, timeout)
}

// waitForCount blocks until want appears at least n times. The count matters
// when a log file is shared across runs: a record left by an earlier run would
// otherwise be read as the current one being ready.
func (i *instance) waitForCount(t *testing.T, want string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Count(i.log(), want) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operations log never contained %d× %q within %v\nlog:\n%s\nstderr:\n%s",
		n, want, timeout, i.log(), i.stderr)
}

// waitExit returns the process exit code.
func (i *instance) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- i.cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err == nil {
			return 0
		}
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		t.Fatalf("wait: %v", err)
	case <-time.After(timeout):
		t.Fatalf("process did not exit within %v\nlog:\n%s", timeout, i.log())
	}
	return -1
}

var procGenerateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").
	NewProc("GenerateConsoleCtrlEvent")

// sendConsoleCtrl delivers a console control event to a process group.
func sendConsoleCtrl(event uint32, groupID int) error {
	r, _, err := procGenerateConsoleCtrlEvent.Call(uintptr(event), uintptr(groupID))
	if r == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent(%d, %d): %w", event, groupID, err)
	}
	return nil
}

const ctrlBreak = 1

// The completion test for this stage (0009-CH T2), measured on the real
// executable rather than on the sequence in isolation.
func TestLiveConsoleCtrlLeavesTrace(t *testing.T) {
	requireLive(t)
	inst := start(t, freePort(t))
	inst.waitForLog(t, "INFO listening", 20*time.Second)

	if err := sendConsoleCtrl(ctrlBreak, inst.cmd.Process.Pid); err != nil {
		t.Fatalf("cannot deliver the console event: %v", err)
	}
	if code := inst.waitExit(t, 30*time.Second); code != 0 {
		t.Errorf("exit code = %d, want 0\nlog:\n%s\nstderr:\n%s", code, inst.log(), inst.stderr)
	}

	got := inst.log()
	// Printed under -v: this is a measurement, and the record it produced is
	// the evidence worth keeping.
	t.Logf("operations log after a console control event:\n%s", got)
	if !strings.Contains(got, "INFO stopping reason=console_ctrl") {
		t.Errorf("no `stopping reason=console_ctrl` record:\n%s", got)
	}
	if !strings.Contains(got, "INFO stopped exit_code=0") {
		t.Errorf("no `stopped` record:\n%s", got)
	}
	// Ordering is the point of the whole design: the reason is on disk before
	// the cleanup runs, so a process killed part way through still explains
	// itself (0008-L 2.4.1 ①).
	//
	// Whole records, not bare words. Since the runner landed, startup writes
	// `runner restored ... status=stopped`, which contains "stopped" and comes
	// long before any shutdown.
	if strings.Index(got, "INFO stopping reason=") > strings.Index(got, "INFO stopped exit_code=") {
		t.Errorf("stopping was recorded after stopped:\n%s", got)
	}
}

// 0007-P [서비스 기동]: the startup records, produced by the real binary.
func TestLiveStartupRecords(t *testing.T) {
	requireLive(t)
	port := freePort(t)
	inst := start(t, port)
	inst.waitForLog(t, "INFO listening", 20*time.Second)

	got := inst.log()
	wantStart := fmt.Sprintf("INFO start host=127.0.0.1 port=%d", port)
	if !strings.Contains(got, wantStart) {
		t.Errorf("missing %q:\n%s", wantStart, got)
	}
	if !strings.Contains(got, "auth=disabled") {
		t.Errorf("auth mode not recorded:\n%s", got)
	}
	if !strings.Contains(got, fmt.Sprintf("INFO listening http://127.0.0.1:%d", port)) {
		t.Errorf("listening address not recorded:\n%s", got)
	}

	// The port really is held, not merely reported.
	if _, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		t.Error("the port is still free; the server did not bind it")
	}
	sendConsoleCtrl(ctrlBreak, inst.cmd.Process.Pid)
	inst.waitExit(t, 30*time.Second)
}

// 0004-NR F3 / 0008-L E-26: an occupied port fails loudly with exit code 1 so
// the service restart policy retries it.
func TestLiveOccupiedPortFails(t *testing.T) {
	requireLive(t)
	port := freePort(t)
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("cannot occupy the port: %v", err)
	}
	defer blocker.Close()

	inst := start(t, port)
	if code := inst.waitExit(t, 30*time.Second); code != 1 {
		t.Errorf("exit code = %d, want 1\nlog:\n%s\nstderr:\n%s", code, inst.log(), inst.stderr)
	}
	got := inst.log()
	if !strings.Contains(got, "ERROR bind_failed") {
		t.Errorf("no bind_failed record:\n%s", got)
	}
	if !strings.Contains(got, "ERROR exiting exit_code=1") {
		t.Errorf("no exiting record:\n%s", got)
	}
	// The failing detail has to name the port, or a reader cannot tell which
	// address was contested.
	if !strings.Contains(got, fmt.Sprintf("port=%d", port)) {
		t.Errorf("bind_failed does not carry the port:\n%s", got)
	}
}

// 0008-L 3.2: a configuration error stops the process before the log exists, so
// stderr is the only channel and exit code 2 keeps the service from retrying.
func TestLiveInvalidConfigurationExitsUnrecoverable(t *testing.T) {
	requireLive(t)
	exe := build(t)
	dir := t.TempDir()

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"SPAWNPOINT_PORT=not-a-port",
		"SPAWNPOINT_DB_PATH="+filepath.Join(dir, "spawnpoint.db"),
		"SPAWNPOINT_LOG_DIR="+filepath.Join(dir, "logs"),
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("exit = %v, want code 2\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "SPAWNPOINT_PORT") {
		t.Errorf("stderr does not name the offending variable:\n%s", out)
	}
}

// 0008-L E-27: if the operations log cannot be opened the process must not stay
// resident — a server that cannot say why it died is the defect being removed.
func TestLiveUnopenableLogDirectoryExitsUnrecoverable(t *testing.T) {
	requireLive(t)
	exe := build(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "logs")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"SPAWNPOINT_PORT="+strconv.Itoa(freePort(t)),
		"SPAWNPOINT_DB_PATH="+filepath.Join(dir, "spawnpoint.db"),
		"SPAWNPOINT_LOG_DIR="+blocker,
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("exit = %v, want code 2\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "operations log") && !strings.Contains(string(out), "log directory") {
		t.Errorf("stderr does not explain the failure:\n%s", out)
	}
}

// The operations log has to survive a restart, since the reason a server went
// down is read after the next one is already up.
func TestLiveRestartAppendsToTheSameLog(t *testing.T) {
	requireLive(t)
	port := freePort(t)

	first := start(t, port)
	first.waitForLog(t, "INFO listening", 20*time.Second)
	sendConsoleCtrl(ctrlBreak, first.cmd.Process.Pid)
	first.waitExit(t, 30*time.Second)
	firstLog := first.log()

	// Reuse the first run's directories so the second run opens the same file.
	exe := build(t)
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"SPAWNPOINT_HOST=127.0.0.1",
		"SPAWNPOINT_PORT="+strconv.Itoa(port),
		"SPAWNPOINT_DB_PATH="+filepath.Join(first.logDir, "..", "spawnpoint.db"),
		"SPAWNPOINT_LOG_DIR="+first.logDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		t.Fatalf("second start: %v", err)
	}
	second := &instance{cmd: cmd, logDir: first.logDir, logPath: first.logPath,
		port: port, stderr: &strings.Builder{}}
	// The first run's `listening` record is still in the file, so wait for the
	// second one rather than for the first match.
	second.waitForCount(t, "INFO listening", 2, 20*time.Second)
	sendConsoleCtrl(ctrlBreak, cmd.Process.Pid)
	second.waitExit(t, 30*time.Second)

	got := second.log()
	if !strings.HasPrefix(got, firstLog) {
		t.Fatalf("the second run did not append to the first run's log:\n%s", got)
	}
	if n := strings.Count(got, "INFO start "); n != 2 {
		t.Errorf("start recorded %d times, want 2", n)
	}
	if n := strings.Count(got, "INFO stopped "); n != 2 {
		t.Errorf("stopped recorded %d times, want 2", n)
	}
}
