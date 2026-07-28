//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"spawnpoint/internal/config"
	"spawnpoint/internal/lifecycle"
	"spawnpoint/internal/opslog"
	"spawnpoint/internal/runner"
)

// The exit-path measurement of 0008-L 6.4: a server started in console mode
// with two children running, then killed five different ways.
//
//	SPAWNPOINT_LIVE_SHUTDOWN=1 go test ./cmd/spawnpoint/ -run TestExitPath -v
//
// Every run uses its own log directory and its own children. 0004-NR 1.5.1
// warns that doing this to the production instance takes every managed child
// down with it, so nothing here touches a registered command: the children are
// two throwaway shell lines.
//
// What is measured is tools/exitprobe rather than the executable itself.
// Registering a command is an HTTP request and the request front end is T6, so
// today there is no way to get a child into the real binary. The probe wires
// the same config, opslog, lifecycle, host and runner together and differs only
// in where its commands come from — that is, in the one component that does
// nothing yet. See the probe's own documentation.

// A child that stays alive and produces a little output, so the log collector
// has something to lose if it is torn down early.
const probeChild = "echo child-%d-up & ping -n 120 127.0.0.1 >nul"

// exitProbe is one running probe.
type exitProbe struct {
	cmd     *exec.Cmd
	logDir  string
	logPath string
	// children are the pids the probe reported. They are what "no child left
	// behind" is checked against; the probe's own record of them is exactly
	// what a leak would not contradict.
	children []int
}

func buildProbe(t *testing.T) string {
	t.Helper()
	if probePath != "" {
		return probePath
	}
	dir, err := os.MkdirTemp("", "spawnpoint-exitprobe")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "exitprobe.exe")
	out, err := exec.Command("go", "build", "-o", exe, "../../tools/exitprobe").CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("go build exitprobe: %v\n%s", err, out)
	}
	probePath = exe
	return exe
}

var probePath string

// startProbe runs the probe with two children and waits until both are up.
//
// ownConsole gives the probe a console of its own, which the close measurement
// needs: the event is delivered to every process attached to a console, and
// sharing the test runner's console would take the test runner down with it.
func startProbe(t *testing.T, ownConsole bool) *exitProbe {
	t.Helper()
	exe := buildProbe(t)
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"SPAWNPOINT_HOST=127.0.0.1",
		"SPAWNPOINT_PORT="+strconv.Itoa(freePort(t)),
		"SPAWNPOINT_LOG_DIR="+logDir,
		"SPAWNPOINT_DB_PATH="+filepath.Join(dir, "spawnpoint.db"),
		"SPAWNPOINT_PROBE_COMMANDS="+
			fmt.Sprintf(probeChild, 1)+"\n"+fmt.Sprintf(probeChild, 2),
	)
	flags := uint32(syscall.CREATE_NEW_PROCESS_GROUP)
	if ownConsole {
		flags = createNewConsole
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the probe: %v", err)
	}

	p := &exitProbe{
		cmd:     cmd,
		logDir:  logDir,
		logPath: filepath.Join(logDir, opslog.FileName),
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		// Whatever the measurement did or failed to do, nothing is left
		// running on the machine afterwards.
		for _, pid := range p.children {
			if alive(pid) {
				exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
			}
		}
	})
	p.children = p.waitReady(t, 2)
	return p
}

// waitReady reads the probe's report of what it started.
func (p *exitProbe) waitReady(t *testing.T, want int) []int {
	t.Helper()
	path := filepath.Join(p.logDir, "probe-ready.txt")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			var pids []int
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				fields := strings.Fields(line)
				if len(fields) != 3 {
					continue
				}
				if fields[1] != "running" {
					t.Fatalf("child %s did not start (%s)", fields[0], fields[1])
				}
				pid, err := strconv.Atoi(fields[2])
				if err != nil {
					t.Fatalf("unreadable pid in %q", line)
				}
				pids = append(pids, pid)
			}
			if len(pids) == want {
				for _, pid := range pids {
					if !alive(pid) {
						t.Fatalf("child %d was reported running but is not", pid)
					}
				}
				return pids
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the probe never reported %d running children\nops log:\n%s", want, p.log())
	return nil
}

func (p *exitProbe) log() string {
	b, err := os.ReadFile(p.logPath)
	if err != nil {
		return ""
	}
	return string(b)
}

func (p *exitProbe) waitExit(t *testing.T, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("the probe did not exit within %v\nops log:\n%s", timeout, p.log())
	}
}

// requireCleanTrace is judgement 3 of 0008-L 6.4: `stopping reason=...` and
// `stopped` are both in the operations log, and no child is left running.
//
// wantTerminated is how many children the sequence is expected to have stopped
// itself. It is not always two, and the difference is the point: a child that
// shares the server's console receives the console event directly and is
// already gone by the time step ③ looks at it. Skipping such a child is correct
// — there is nothing to terminate — so the count is stated per path rather than
// assumed.
func (p *exitProbe) requireCleanTrace(t *testing.T, reason string, wantTerminated int) {
	t.Helper()
	got := p.log()
	// Printed under -v: this is a measurement, and the record is the evidence.
	t.Logf("operations log after %s:\n%s", reason, got)

	if want := "INFO stopping reason=" + reason; !strings.Contains(got, want) {
		t.Errorf("no %q record:\n%s", want, got)
	}
	if !strings.Contains(got, "INFO stopped exit_code=") {
		t.Errorf("no `stopped` record:\n%s", got)
	}
	// Whole records, not bare words: `runner restored ... status=stopped`
	// contains "stopped" and is written long before the shutdown starts.
	if strings.Index(got, "INFO stopping reason=") > strings.Index(got, "INFO stopped exit_code=") {
		t.Errorf("stopping was recorded after stopped:\n%s", got)
	}
	if n := strings.Count(got, "INFO child terminated"); n < wantTerminated {
		t.Errorf("`child terminated` recorded %d times, want at least %d:\n%s",
			n, wantTerminated, got)
	}
	p.requireNoChildLeft(t)
}

// requireNoChildLeft is the part of the judgement that is common to all five
// paths, including the forced one where no trace is expected.
func (p *exitProbe) requireNoChildLeft(t *testing.T) {
	t.Helper()
	for _, pid := range p.children {
		if !waitGone(pid, 10*time.Second) {
			t.Errorf("child %d is still running after the server exited", pid)
		}
	}
}

// (a) — interrupt. 0008-L 2.4.2 gives it the full budget.
func TestExitPathInterrupt(t *testing.T) {
	requireLive(t)
	p := startProbe(t, false)

	if err := sendConsoleCtrl(ctrlBreak, p.cmd.Process.Pid); err != nil {
		t.Fatalf("cannot deliver the console event: %v", err)
	}
	p.waitExit(t, 30*time.Second)
	// Both children are stopped by the sequence here. Each child is started in
	// a process group of its own, so a control event addressed to the server's
	// group does not reach them (0008-L 2.1 rule 3).
	p.requireCleanTrace(t, "console_ctrl", 2)
}

// (c) — the console window being closed.
//
// 0004-NR 1.5 judged that the current implementation almost certainly does not
// run its cleanup on this path, and could not measure it. This measures it.
//
// The event cannot be raised with an API — GenerateConsoleCtrlEvent refuses
// CTRL_CLOSE_EVENT — so the console is closed the way a person closes it, by
// posting WM_CLOSE to the console window. Doing that requires attaching to the
// probe's console, and a process attached to a console receives the event too,
// so the attaching is done by a separate helper process: this test binary,
// re-run with SPAWNPOINT_TEST_CLOSE_CONSOLE_OF set.
func TestExitPathConsoleClose(t *testing.T) {
	requireLive(t)
	p := startProbe(t, true)

	report := filepath.Join(t.TempDir(), "close-console.txt")
	helper := exec.Command(os.Args[0])
	helper.Env = append(os.Environ(),
		envCloseConsoleOf+"="+strconv.Itoa(p.cmd.Process.Pid),
		envCloseConsoleReport+"="+report)
	posted := time.Now()
	out, err := helper.CombinedOutput()
	if b, readErr := os.ReadFile(report); readErr == nil {
		t.Logf("console-close helper: %s", strings.TrimSpace(string(b)))
	}
	if err != nil {
		t.Fatalf("the console-close helper failed: %v\n%s", err, out)
	}

	// The budget for this path is five seconds, because the operating system
	// terminates the process at about that point (0008-L 2.4.3).
	p.waitExit(t, 30*time.Second)
	t.Logf("the probe exited %v after the close was posted", time.Since(posted).Round(time.Millisecond))

	// No number of `child terminated` records is required on this path, and
	// asking for one was wrong: a console control event goes to every process
	// attached to the console, the children share the server's console, so they
	// receive it too and start dying the moment it is delivered. Step ③ then
	// finds them already gone and correctly skips them — all of them, in about
	// one run in twenty. (Measured: this test asked for at least one record and
	// failed with zero, having stopped both children cleanly.)
	//
	// What the judgement asks for is that no child is left running, and that is
	// checked against the pids rather than against the log — which is how
	// 0017-TR read it and why the reasoning survived the test being wrong.
	p.requireCleanTrace(t, "console_close", 0)
}

// (e) — killed outright, with nothing running in the process to notice.
//
// No trace is expected and none is required (0008-L 6.4 judgement 4). What is
// required is that the children are gone anyway, which is the whole reason the
// server group holds the kill-on-close property and is released last.
func TestExitPathForcedTermination(t *testing.T) {
	requireLive(t)
	p := startProbe(t, false)
	before := p.log()

	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("cannot kill the probe: %v", err)
	}
	p.waitExit(t, 30*time.Second)

	p.requireNoChildLeft(t)
	t.Logf("operations log after a forced termination:\n%s", p.log())
	// Nothing ran, so nothing can have been recorded. This is not a
	// requirement — it is a check that the previous assertion was not passing
	// because the sequence quietly ran after all, which would make this
	// measurement of the kernel backstop no measurement at all.
	if got := p.log(); strings.Contains(strings.TrimPrefix(got, before), "stopping") {
		t.Errorf("the shutdown sequence ran on a forced termination:\n%s", got)
	}
}

// (d) — logoff. It cannot be raised: Windows sends CTRL_LOGOFF_EVENT when a
// session ends, and there is no way to end one from inside a test without
// ending the session running the test.
//
// What can be shown is that it reaches the same code as (c), which is measured.
// internal/host maps close, logoff and shutdown onto one reason and one budget,
// and TestConsoleEventMapping pins that; this records the remaining gap rather
// than leaving it looking covered.
func TestExitPathLogoffIsNotMeasurable(t *testing.T) {
	requireLive(t)
	t.Skip("CTRL_LOGOFF_EVENT cannot be raised without ending the session; " +
		"the path is the same as the console-close path measured above, " +
		"and the mapping is pinned by internal/host TestConsoleEventMapping")
}

// (b) — a termination signal.
//
// Windows has no SIGTERM. The row it corresponds to in 0008-L 2.4.2 is the
// service control manager's stop request, and delivering one needs the service
// registered, which needs administrator rights this machine does not have. What
// is unmeasurable is therefore the delivery — the callback in internal/host
// that turns a control code into a reason and a budget.
//
// Everything on the far side of that callback is measured here: the whole
// sequence, driven with the service-control reason and its full budget, over
// real children in real job objects. It runs in this process rather than in the
// probe because that is the only way to reach the sequence without the service
// control manager.
func TestExitPathServiceControlSequence(t *testing.T) {
	requireLive(t)

	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	log, err := opslog.Open(logDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Host: "127.0.0.1", Port: freePort(t),
		DBPath: filepath.Join(dir, "spawnpoint.db"), LogDir: logDir,
		KillChildrenOnExit: true,
	}
	procs := runner.New(logDir, log, nil, true)

	// The order the steps ran in is recorded as they run, so ④ ⑤ ⑧ can be
	// checked against 0008-L 2.4.1 — the operations log says nothing about
	// them.
	var steps []string
	srv := lifecycle.New(cfg, log, lifecycle.Hooks{
		StopChildren: func(reason string, deadline time.Time) {
			steps = append(steps, "children")
			procs.StopChildren(reason, deadline)
		},
		StopCollectors: func() { steps = append(steps, "collectors"); procs.StopCollectors() },
		CloseDatabase:  func() { steps = append(steps, "database") },
		CloseServerJob: func() { steps = append(steps, "server job"); procs.CloseServerJob() },
	})

	var pids []int
	for i := 1; i <= 2; i++ {
		info, _ := procs.Register(fmt.Sprintf("child-%d", i), fmt.Sprintf(probeChild, i), nil, nil)
		if info.Status != "running" {
			t.Fatalf("child %d did not start: %s", i, info.Error)
		}
		pids = append(pids, *info.PID)
	}

	code := srv.Shutdown("service_control", 20*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	b, _ := os.ReadFile(filepath.Join(logDir, opslog.FileName))
	got := string(b)
	t.Logf("operations log after service_control:\n%s", got)
	if !strings.Contains(got, "INFO stopping reason=service_control") {
		t.Errorf("no `stopping reason=service_control` record:\n%s", got)
	}
	if !strings.Contains(got, "INFO stopped exit_code=0") {
		t.Errorf("no `stopped` record:\n%s", got)
	}
	if n := strings.Count(got, "INFO child terminated"); n != 2 {
		t.Errorf("`child terminated` recorded %d times, want 2:\n%s", n, got)
	}
	for _, pid := range pids {
		if !waitGone(pid, 10*time.Second) {
			t.Errorf("child %d survived the shutdown", pid)
		}
	}
	// The collectors are torn down after the children and the server group is
	// released after everything (0008-L 2.4.1 ③ ④ ⑤ ⑧). The second of those is
	// what keeps the kernel backstop covering every step before it.
	want := "children collectors database server job"
	if strings.Join(steps, " ") != want {
		t.Errorf("steps ran in the order %q, want %q", strings.Join(steps, " "), want)
	}
}

// --- Windows plumbing ------------------------------------------------------------

const (
	createNewConsole = 0x00000010
	stillActive      = 259
	queryProcess     = 0x0400
)

var (
	kernel32test = syscall.NewLazyDLL("kernel32.dll")
	user32       = syscall.NewLazyDLL("user32.dll")

	procFreeConsole        = kernel32test.NewProc("FreeConsole")
	procAttachConsole      = kernel32test.NewProc("AttachConsole")
	procGetConsoleWindow   = kernel32test.NewProc("GetConsoleWindow")
	procGetExitCodeProcess = kernel32test.NewProc("GetExitCodeProcess")
	procGetClassNameW      = user32.NewProc("GetClassNameW")
	procEndTask            = user32.NewProc("EndTask")
)

// envCloseConsoleOf puts this binary into helper mode: attach to the named
// process's console, post WM_CLOSE to its window, detach, exit.
const envCloseConsoleOf = "SPAWNPOINT_TEST_CLOSE_CONSOLE_OF"

// closeConsoleOf runs in the helper process. It cannot report anything in
// words — FreeConsole takes its output with it — so it speaks in exit codes and
// leaves what it saw in the file named by envCloseConsoleReport.
func closeConsoleOf(pid int) int {
	report := os.Getenv(envCloseConsoleReport)
	note := func(format string, args ...any) {
		if report != "" {
			f, err := os.OpenFile(report, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				fmt.Fprintf(f, format+"\n", args...)
				f.Close()
			}
		}
	}

	procFreeConsole.Call()
	if r, _, err := procAttachConsole.Call(uintptr(pid)); r == 0 {
		note("AttachConsole(%d) failed: %v", pid, err)
		return 11
	}
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		procFreeConsole.Call()
		note("the console has no window")
		return 12
	}
	// The class name says which console host owns the window, and the two
	// behave differently. Classic conhost gives a ConsoleWindowClass window
	// that closes on WM_CLOSE. Windows Terminal gives a PseudoConsoleWindow
	// placeholder that accepts WM_CLOSE — PostMessage reports success — and
	// then does nothing at all; measured, and the reason this uses EndTask
	// instead.
	class := make([]uint16, 128)
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
	note("hwnd=%#x class=%q", hwnd, syscall.UTF16ToString(class[:n]))

	// Detach before closing the window, not after. A process attached to a
	// console receives the close event too, and this one has no handler: left
	// attached, it is killed with STATUS_CONTROL_C_EXIT part way through the
	// call and cannot report what happened. The window handle stays valid.
	procFreeConsole.Call()

	// EndTask(hwnd, fShutDown=false, fForce=false) is the graceful close — the
	// same thing the window's close button does, which is the event being
	// measured. PostMessage(WM_CLOSE) is not enough: a PseudoConsoleWindow
	// accepts it, reports success, and does nothing (measured).
	if r, _, err := procEndTask.Call(hwnd, 0, 0); r == 0 {
		note("EndTask failed: %v", err)
		return 13
	}
	return 0
}

// envCloseConsoleReport names a file the helper appends its findings to. The
// helper has given up its console by the time it has anything to say.
const envCloseConsoleReport = "SPAWNPOINT_TEST_CLOSE_CONSOLE_REPORT"

func alive(pid int) bool {
	handle, err := syscall.OpenProcess(queryProcess, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if r, _, _ := procGetExitCodeProcess.Call(uintptr(handle), uintptr(unsafe.Pointer(&code))); r == 0 {
		return false
	}
	return code == stillActive
}

func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !alive(pid)
}
