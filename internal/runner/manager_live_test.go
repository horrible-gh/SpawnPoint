package runner

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests start real processes and are opt-in, the same way the
// command-line spike's are:
//
//	SPAWNPOINT_LIVE_SPAWN=1 go test ./internal/runner/
//
// Everything they start is a throwaway shell line in a temporary directory.
// None of the registered commands is ever used: those start production servers,
// and 0004-NR 1.5.1 warns that touching the live instance takes every managed
// child down with it.

func requireLiveSpawn(t *testing.T) {
	t.Helper()
	if os.Getenv("SPAWNPOINT_LIVE_SPAWN") != "1" {
		t.Skip("set SPAWNPOINT_LIVE_SPAWN=1 to run the live spawn checks")
	}
}

// sleepFor is a shell line that stays alive for about d and produces no output.
func sleepFor(d time.Duration) string {
	if runtime.GOOS == "windows" {
		// ping is used rather than timeout because timeout needs a real console
		// and fails with "input redirection is not supported" under a pipe.
		return fmt.Sprintf("ping -n %d 127.0.0.1 >nul", int(d.Seconds())+1)
	}
	return fmt.Sprintf("sleep %d", int(d.Seconds()))
}

// echoThen prints text and then stays alive.
func echoThen(text string, d time.Duration) string {
	return "echo " + text + " & " + sleepFor(d)
}

// waitStatus polls until the entry reaches one of want.
func waitStatus(t *testing.T, m *Manager, id string, timeout time.Duration, want ...string) Info {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last Info
	for time.Now().Before(deadline) {
		info, ok := m.Get(id)
		if !ok {
			t.Fatalf("entry %s disappeared", id)
		}
		last = info
		for _, w := range want {
			if info.Status == w {
				return info
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("entry %s is %q after %v, want one of %v", id, last.Status, timeout, want)
	return last
}

func childLog(t *testing.T, m *Manager, id string) string {
	t.Helper()
	b, err := os.ReadFile(m.LogPath(id))
	if err != nil {
		return ""
	}
	return string(b)
}

// alive reports whether a pid is still a running process. It is the check that
// matters for the containment layers: the entry's own record of itself is
// exactly what a leak would not contradict.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return processAlive(p)
}

// 0008-L 2.2: a successful start produces a running entry with a pid and a
// start time, and says so in the operations log.
func TestLiveRegisterStartsTheChild(t *testing.T) {
	requireLiveSpawn(t)
	m, dir := testManager(t, nil)

	info, _ := m.Register("sleeper", sleepFor(30*time.Second), nil, nil)
	defer m.Stop(info.ID)

	if info.Status != StatusRunning {
		t.Fatalf("status = %q (%s), want %q", info.Status, info.Error, StatusRunning)
	}
	if info.PID == nil || !alive(*info.PID) {
		t.Fatalf("pid = %v, and it is not a running process", info.PID)
	}
	if info.StartedAt == nil || info.EndedAt != nil || info.ExitCode != nil {
		t.Errorf("started_at=%v ended_at=%v exit_code=%v, want a start time and nothing else",
			info.StartedAt, info.EndedAt, info.ExitCode)
	}
	want := fmt.Sprintf("INFO child started id=%s pid=%d", info.ID, *info.PID)
	if got := opsLog(t, dir); !strings.Contains(got, want) {
		t.Errorf("missing %q:\n%s", want, got)
	}
}

// 0008-L 2.5: the output arrives through a pipe the server owns, not by handing
// the child the log file. That is the change that makes the file the server's
// to manage (0006-D 2.1).
func TestLiveOutputIsCollected(t *testing.T) {
	requireLiveSpawn(t)
	m, _ := testManager(t, nil)

	info, _ := m.Register("talker", echoThen("hello-from-the-child", 30*time.Second), nil, nil)
	defer m.Stop(info.ID)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(childLog(t, m, info.ID), "hello-from-the-child") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the child's output never reached %s:\n%s", m.LogPath(info.ID), childLog(t, m, info.ID))
}

// 0008-L 3.1: a child that ends on its own with code 0 is `exited`, and one that
// ends with anything else is `killed`. Neither is `stopped` — nobody asked.
func TestLiveNaturalExitStates(t *testing.T) {
	requireLiveSpawn(t)
	m, _ := testManager(t, nil)

	for _, tc := range []struct {
		name   string
		cmd    string
		status string
		code   int
	}{
		{"zero", "exit 0", StatusExited, 0},
		{"non-zero", "exit 3", StatusKilled, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, _ := m.Register(tc.name, tc.cmd, nil, nil)
			got := waitStatus(t, m, info.ID, 20*time.Second, tc.status)
			if got.ExitCode == nil || *got.ExitCode != tc.code {
				t.Errorf("exit_code = %v, want %d", got.ExitCode, tc.code)
			}
			if got.EndedAt == nil {
				t.Error("ended_at is null for a child that ended")
			}
		})
	}
}

// 0008-L 2.3 / 3.1: a stop request produces `stopped`, not `killed`. The exit
// code cannot make that distinction — a process killed on request and one that
// failed on its own both come back non-zero — so the request is remembered.
func TestLiveStopProducesStopped(t *testing.T) {
	requireLiveSpawn(t)
	m, dir := testManager(t, nil)

	info, _ := m.Register("sleeper", sleepFor(60*time.Second), nil, nil)
	if info.Status != StatusRunning {
		t.Fatalf("the child did not start: %s", info.Error)
	}
	pid := *info.PID

	stopped, ok := m.Stop(info.ID)
	if !ok {
		t.Fatal("stop reported the entry as unknown")
	}
	if stopped.Status != StatusStopped {
		t.Errorf("status = %q, want %q", stopped.Status, StatusStopped)
	}
	if alive(pid) {
		t.Errorf("process %d is still running after the stop returned", pid)
	}
	if stopped.EndedAt == nil {
		t.Error("ended_at is null")
	}
	got := opsLog(t, dir)
	want := fmt.Sprintf("INFO child terminated id=%s pid=%d", info.ID, pid)
	if !strings.Contains(got, want) {
		t.Errorf("missing %q:\n%s", want, got)
	}
	if !strings.Contains(got, "reason=stop_requested") {
		t.Errorf("the reason was not recorded:\n%s", got)
	}
}

// 0008-L 2.3: the collector is torn down after the child is confirmed gone, so
// what the child wrote on its way down is in the file.
//
// The proof is indirect and stronger than reading the file: the collector only
// finishes when the pipe reaches end of stream, and the pipe only reaches end
// of stream when every handle to its write end has closed — the shell's and
// every descendant's. A log file that can be removed on Windows is a file
// nothing holds open, so a successful removal after a stop says the whole tree
// is gone and the collector saw it out.
func TestLiveStopDrainsTheWholeTree(t *testing.T) {
	requireLiveSpawn(t)
	m, _ := testManager(t, nil)

	// The registered line starts a second shell, so the process the server
	// holds is not the one doing the sleeping.
	inner := sleepFor(60 * time.Second)
	line := inner
	if runtime.GOOS == "windows" {
		line = `cmd /c "` + inner + `"`
	} else {
		line = "sh -c '" + inner + "'"
	}
	info, _ := m.Register("nested", line, nil, nil)
	if info.Status != StatusRunning {
		t.Fatalf("the child did not start: %s", info.Error)
	}
	m.Stop(info.ID)

	if !m.Delete(info.ID) {
		t.Fatal("delete reported the entry as unknown")
	}
	if _, err := os.Stat(m.LogPath(info.ID)); err == nil {
		t.Error("the child log is still open, so the pipe never reached end of stream: " +
			"a descendant outlived the group termination")
	}
}

// 0008-L 3.1 / E-16: run on something already running does nothing at all. A
// second click must not start a second process.
func TestLiveRunOnARunningEntryIsIgnored(t *testing.T) {
	requireLiveSpawn(t)
	m, dir := testManager(t, nil)

	info, _ := m.Register("sleeper", sleepFor(60*time.Second), nil, nil)
	defer m.Stop(info.ID)
	first := *info.PID

	again, _ := m.Run(info.ID)
	if again.PID == nil || *again.PID != first {
		t.Errorf("pid changed from %d to %v: a second process was started", first, again.PID)
	}
	if n := strings.Count(opsLog(t, dir), "INFO child started"); n != 1 {
		t.Errorf("`child started` recorded %d times, want 1", n)
	}
}

// 0008-L 3.1 / 2.5.1: restart stops and starts again, writes a marker carrying
// the time, and does not truncate the log. The history of every run stays in
// one file and the marker says which run the lines after it belong to.
func TestLiveRestartKeepsTheLogAndMarksIt(t *testing.T) {
	requireLiveSpawn(t)
	m, dir := testManager(t, nil)

	info, _ := m.Register("talker", echoThen("first-run", 60*time.Second), nil, nil)
	if info.Status != StatusRunning {
		t.Fatalf("the child did not start: %s", info.Error)
	}
	first := *info.PID
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(childLog(t, m, info.ID), "first-run") {
		time.Sleep(20 * time.Millisecond)
	}

	again, _ := m.Restart(info.ID)
	defer m.Stop(info.ID)
	if again.Status != StatusRunning {
		t.Fatalf("status after restart = %q (%s)", again.Status, again.Error)
	}
	if again.PID == nil || *again.PID == first {
		t.Errorf("pid = %v, want a new process", again.PID)
	}
	if alive(first) {
		t.Errorf("the previous process %d is still running", first)
	}

	log := childLog(t, m, info.ID)
	if !strings.Contains(log, "first-run") {
		t.Errorf("the log was truncated by the restart:\n%s", log)
	}
	if !strings.Contains(log, "--- restart 20") {
		t.Errorf("no restart marker with a timestamp:\n%s", log)
	}
	if got := opsLog(t, dir); !strings.Contains(got, "INFO child restarted") {
		t.Errorf("no `child restarted` record:\n%s", got)
	}
}

// 0008-L 2.4.1 step ③: shutdown stops every running child, in parallel, inside
// one shared deadline.
func TestLiveStopChildrenClearsEveryone(t *testing.T) {
	requireLiveSpawn(t)
	m, dir := testManager(t, nil)

	var pids []int
	for i := 0; i < 3; i++ {
		info, _ := m.Register(fmt.Sprintf("sleeper-%d", i), sleepFor(60*time.Second), nil, nil)
		if info.Status != StatusRunning {
			t.Fatalf("child %d did not start: %s", i, info.Error)
		}
		pids = append(pids, *info.PID)
	}
	// One entry that is not running: it must be skipped, not stopped.
	idle, _ := m.Register("gone", "exit 0", nil, nil)
	waitStatus(t, m, idle.ID, 20*time.Second, StatusExited, StatusKilled)

	started := time.Now()
	m.StopChildren("service_control", time.Now().Add(20*time.Second))
	elapsed := time.Since(started)

	for _, pid := range pids {
		if alive(pid) {
			t.Errorf("process %d survived the shutdown", pid)
		}
	}
	// Three children stopped in parallel take about as long as one. Serially
	// they would take three times as long, and under a shared budget the last
	// of them would be left to the kernel.
	if elapsed > 3*ChildTerminateTimeout {
		t.Errorf("stopping three children took %v; they were not stopped in parallel", elapsed)
	}
	if n := strings.Count(opsLog(t, dir), "INFO child terminated"); n != 3 {
		t.Errorf("`child terminated` recorded %d times, want 3 (the idle entry must be skipped)", n)
	}
}

// 0008-L 1.2 `forced_child_env` and `env_priority`, measured on a real child
// rather than on the merge function. This is the pair that must reach the
// process: without them the five registered commands change what they write
// (0004-NR 3.4).
func TestLiveChildReceivesTheMergedEnvironment(t *testing.T) {
	requireLiveSpawn(t)
	if runtime.GOOS != "windows" {
		t.Skip("the probe below is a cmd.exe line")
	}
	m, _ := testManager(t, nil)

	// The child prints the three values that decide the priority order: one
	// inherited, the forced pair, and one the entry overrides.
	t.Setenv("SPAWNPOINT_TEST_INHERITED", "from-parent")
	line := "echo utf8=%PYTHONUTF8% enc=%PYTHONIOENCODING% inherited=%SPAWNPOINT_TEST_INHERITED% own=%OWN% & " +
		sleepFor(30*time.Second)
	info, _ := m.Register("env", line, nil, map[string]string{"OWN": "mine", "PYTHONIOENCODING": "utf-16"})
	defer m.Stop(info.ID)

	deadline := time.Now().Add(15 * time.Second)
	var log string
	for time.Now().Before(deadline) {
		log = childLog(t, m, info.ID)
		if strings.Contains(log, "own=") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{
		"utf8=1",                // forced, and nothing overrode it
		"enc=utf-16",            // the entry's own value beats the forced one
		"inherited=from-parent", // the parent's environment came through
		"own=mine",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the child did not see %q:\n%s", want, log)
		}
	}
}

// 0008-L 3.1: concurrent operations on one entry must not leave two processes
// running under it. Restart is a stop followed by a start, and a run arriving
// between the two would start a process the restart then replaces and forgets —
// which is a leak the entry itself cannot show, because it only ever records
// one pid.
func TestLiveConcurrentOperationsLeaveOneProcess(t *testing.T) {
	requireLiveSpawn(t)
	m, _ := testManager(t, nil)

	info, _ := m.Register("sleeper", sleepFor(60*time.Second), nil, nil)
	if info.Status != StatusRunning {
		t.Fatalf("the child did not start: %s", info.Error)
	}
	defer m.Stop(info.ID)

	// Every pid the entry ever reports. All but the last must be dead at the
	// end; the last must be the one that is alive.
	var mu sync.Mutex
	seen := map[int]bool{*info.PID: true}
	record := func(i Info) {
		if i.PID == nil {
			return
		}
		mu.Lock()
		seen[*i.PID] = true
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				got, _ := m.Run(info.ID)
				record(got)
			case 1:
				got, _ := m.Restart(info.ID)
				record(got)
			default:
				got, _ := m.Stop(info.ID)
				record(got)
			}
		}(i)
	}
	wg.Wait()

	final, _ := m.Get(info.ID)
	record(final)

	mu.Lock()
	defer mu.Unlock()
	for pid := range seen {
		if final.Status == StatusRunning && final.PID != nil && pid == *final.PID {
			continue
		}
		if alive(pid) {
			t.Errorf("process %d is still running but the entry does not know about it "+
				"(entry is %s, pid %v)", pid, final.Status, final.PID)
		}
	}
}

// A run that ends on its own gives its process group back. Nothing observable
// says so, which is why it is easy to miss: the handle just accumulates, one
// per run, for as long as the server is up.
func TestLiveNaturalExitReleasesTheGroup(t *testing.T) {
	requireLiveSpawn(t)
	m, _ := testManager(t, nil)

	info, _ := m.Register("quick", "exit 0", nil, nil)
	waitStatus(t, m, info.ID, 20*time.Second, StatusExited)

	e, ok := m.entry(info.ID)
	if !ok {
		t.Fatal("the entry disappeared")
	}
	e.mu.Lock()
	job := e.job
	e.mu.Unlock()
	if job != nil {
		t.Error("the entry still holds a process group after its run ended")
	}
}
