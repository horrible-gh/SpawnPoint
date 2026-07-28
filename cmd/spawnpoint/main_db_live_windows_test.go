//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"spawnpoint/internal/opslog"
	"spawnpoint/internal/store"
)

// The database stages of startup, measured on the real executable. The unit
// tests cover the migration runner against a database it opens itself; these
// cover the part only the binary can answer — that the stages are wired into
// the startup sequence in the right order, with the right exit codes, and that
// what the operations log claims happened matches what is on disk.
//
// They share the gate with the rest of the live checks in this package:
//
//	SPAWNPOINT_LIVE_SHUTDOWN=1 go test ./cmd/spawnpoint/

// dbPath is the database the start helper configures, alongside its log
// directory.
func (i *instance) dbPath() string {
	return filepath.Join(filepath.Dir(i.logDir), "spawnpoint.db")
}

// TestLiveMigrationsRunOnceAcrossRestarts is 0008-L 6.3 item 3 seen from
// outside: the counts the server reports on a first start and on a second one
// against the same file. The second start must apply nothing.
//
// The operations record is checked as well as the database, because it is what
// an operator reads. A server that silently re-applied migrations while logging
// `pending=0` would be worse than one that failed.
func TestLiveMigrationsRunOnceAcrossRestarts(t *testing.T) {
	requireLive(t)
	port := freePort(t)

	first := start(t, port)
	first.waitForLog(t, "INFO listening", 20*time.Second)
	got := first.log()
	t.Logf("operations log of a first start:\n%s", got)
	if !strings.Contains(got, "INFO migrations applied=0 pending=3") {
		t.Errorf("first start did not report three migrations to apply:\n%s", got)
	}
	// Ordering is fixed by 0008-L 2.15: the database is dealt with before the
	// listener is bound, so a database failure never leaves a half-started
	// server holding the port.
	if strings.Index(got, "INFO migrations") > strings.Index(got, "INFO listening") {
		t.Errorf("migrations were applied after the listener was bound:\n%s", got)
	}
	sendConsoleCtrl(ctrlBreak, first.cmd.Process.Pid)
	first.waitExit(t, 30*time.Second)

	// The file the first run created is a real database with the full schema.
	assertSchema(t, first.dbPath())

	exe := build(t)
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"SPAWNPOINT_HOST=127.0.0.1",
		"SPAWNPOINT_PORT="+strconv.Itoa(port),
		"SPAWNPOINT_DB_PATH="+first.dbPath(),
		"SPAWNPOINT_LOG_DIR="+first.logDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("second start: %v", err)
	}
	second := &instance{cmd: cmd, logDir: first.logDir, logPath: first.logPath,
		port: port, stderr: &stderr}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	second.waitForCount(t, "INFO listening", 2, 20*time.Second)

	got = second.log()
	if !strings.Contains(got, "INFO migrations applied=3 pending=0") {
		t.Errorf("second start did not find the existing history:\n%s", got)
	}
	if n := strings.Count(got, "INFO migrations applied=0 pending=3"); n != 1 {
		t.Errorf("the first start's record appears %d times, want 1 — migrations ran again", n)
	}
	sendConsoleCtrl(ctrlBreak, cmd.Process.Pid)
	second.waitExit(t, 30*time.Second)

	assertSchema(t, first.dbPath())
}

// TestLiveUnopenableDatabaseExitsStartFailed checks the exit code the service
// restart policy reads.
//
// A database that cannot be opened is a start failure, not an unrecoverable
// one: the usual cause is transient — a file still held by the process that
// just went down, a volume not yet mounted — so the service must try again
// (0008-L 3.2). Exit code 2 here would leave the server down until somebody
// noticed.
func TestLiveUnopenableDatabaseExitsStartFailed(t *testing.T) {
	requireLive(t)
	exe := build(t)
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"SPAWNPOINT_HOST=127.0.0.1",
		"SPAWNPOINT_PORT="+strconv.Itoa(freePort(t)),
		// A directory that does not exist. SQLite will not create the path.
		"SPAWNPOINT_DB_PATH="+filepath.Join(dir, "no", "such", "directory", "spawnpoint.db"),
		"SPAWNPOINT_LOG_DIR="+logDir,
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("the server started with an unopenable database: %v\noutput:\n%s", err, out)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1 (a start failure is retried; 2 would not be)\noutput:\n%s",
			exitErr.ExitCode(), out)
	}

	log, readErr := os.ReadFile(filepath.Join(logDir, opslog.FileName))
	if readErr != nil {
		t.Fatalf("no operations log was written: %v", readErr)
	}
	got := string(log)
	t.Logf("operations log after an unopenable database:\n%s", got)
	if !strings.Contains(got, "ERROR exiting exit_code=1") {
		t.Errorf("no exiting record with the start-failure code:\n%s", got)
	}
	if !strings.Contains(got, "open database") {
		t.Errorf("the record does not say which stage failed:\n%s", got)
	}
	// Nothing may be reported as bound: the sequence stops before that point.
	if strings.Contains(got, "INFO listening") {
		t.Errorf("the listener was bound despite the database failing:\n%s", got)
	}
}

// assertSchema opens the file the server left behind and checks it is a
// complete database rather than an empty one that merely exists.
func assertSchema(t *testing.T, path string) {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("the server's database cannot be opened: %v", err)
	}
	defer s.Close()

	for _, table := range []string{"migrations", "spawn_instance", "spawn_daily_seq", "runner_entry"} {
		var name string
		err := s.DB().QueryRow(
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %q is missing from the server's database: %v", table, err)
		}
	}
	var history int
	if err := s.DB().QueryRow("SELECT count(*) FROM migrations").Scan(&history); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if history != 3 {
		t.Errorf("history has %d rows, want 3", history)
	}
}
