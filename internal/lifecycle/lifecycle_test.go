package lifecycle

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"spawnpoint/internal/config"
	"spawnpoint/internal/opslog"
)

// harness wires a server over a temporary log directory and records the order
// in which the hooks fire — the order is the contract here, not the effects.
type harness struct {
	t     *testing.T
	srv   *Server
	path  string
	mu    sync.Mutex
	calls []string
}

func newHarness(t *testing.T, cfg config.Config, build func(h *harness) Hooks) *harness {
	t.Helper()
	dir := t.TempDir()
	log, err := opslog.Open(dir)
	if err != nil {
		t.Fatalf("open ops log: %v", err)
	}
	t.Cleanup(func() { log.Close() }) // Close is idempotent; the tests that
	// finish a shutdown have already closed it at step ⑦.
	h := &harness{t: t, path: filepath.Join(dir, opslog.FileName)}
	h.srv = New(cfg, log, build(h))
	return h
}

func (h *harness) record(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, name)
}

func (h *harness) order() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

func (h *harness) logText() string {
	h.t.Helper()
	b, err := os.ReadFile(h.path)
	if err != nil {
		h.t.Fatalf("read ops log: %v", err)
	}
	return string(b)
}

func (h *harness) lines() []string {
	return strings.Split(strings.TrimSuffix(h.logText(), "\n"), "\n")
}

// eventOf strips the timestamp and returns "<LEVEL> <rest>".
func eventOf(line string) string {
	_, rest, _ := strings.Cut(line, " ")
	return rest
}

func (h *harness) events() []string {
	var out []string
	for _, line := range h.lines() {
		out = append(out, eventOf(line))
	}
	return out
}

// defaultConfig matches the values in the 0007-P [서비스 기동] transcript so
// the expected records can be copied from it verbatim.
func defaultConfig() config.Config {
	return config.Config{
		Host:               "0.0.0.0",
		Port:               8091,
		DBPath:             "spawnpoint.db",
		LogDir:             "logs",
		KillChildrenOnExit: true,
	}
}

// fullHooks records every stage of both sequences.
func fullHooks(h *harness) Hooks {
	return Hooks{
		ValidateAssets:  func() error { h.record("validate"); return nil },
		OpenDatabase:    func() error { h.record("open db"); return nil },
		ApplyMigrations: func() (int, int, error) { h.record("migrations"); return 2, 0, nil },
		RestoreEntries:  func() (int, error) { h.record("restore"); return 5, nil },
		Bind:            func() (string, error) { h.record("bind"); return "http://0.0.0.0:8091", nil },
		Serve: func(stop <-chan struct{}) error {
			h.record("serve")
			<-stop
			return nil
		},
		StopAccepting:  func() { h.record("stop accepting") },
		WaitInflight:   func(time.Duration) { h.record("wait inflight") },
		StopChildren:   func(string, time.Time) { h.record("stop children") },
		StopCollectors: func() { h.record("stop collectors") },
		CloseDatabase:  func() { h.record("close db") },
		CloseServerJob: func() { h.record("close server job") },
	}
}

// The completion test for this stage (0009-CH T2): after a shutdown the file
// carries `stopping reason=` and `stopped`.
func TestShutdownLeavesStoppingAndStopped(t *testing.T) {
	h := newHarness(t, defaultConfig(), fullHooks)
	if code, ok := h.srv.Startup(); !ok {
		t.Fatalf("Startup failed with %d", code)
	}
	if code := h.srv.Shutdown(ReasonServiceControl, TotalBudget); code != ExitNormal {
		t.Fatalf("Shutdown = %d, want %d", code, ExitNormal)
	}

	got := h.logText()
	if !strings.Contains(got, " INFO stopping reason=service_control\n") {
		t.Errorf("missing `stopping reason=service_control`:\n%s", got)
	}
	if !strings.Contains(got, " INFO stopped exit_code=0\n") {
		t.Errorf("missing `stopped exit_code=0`:\n%s", got)
	}
}

// 0008-L 2.15: the record order is the startup order, and 0007-P [서비스 기동]
// fixes the wording.
func TestStartupSequence(t *testing.T) {
	h := newHarness(t, defaultConfig(), fullHooks)
	if code, ok := h.srv.Startup(); !ok {
		t.Fatalf("Startup failed with %d", code)
	}
	want := []string{
		"INFO start host=0.0.0.0 port=8091 db=spawnpoint.db auth=disabled",
		"INFO migrations applied=2 pending=0",
		"INFO runner restored entries=5 status=stopped",
		"INFO listening http://0.0.0.0:8091",
	}
	if got := h.events(); !slices.Equal(got, want) {
		t.Fatalf("startup records =\n%s\nwant\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantCalls := []string{"validate", "open db", "migrations", "restore", "bind"}
	if got := h.order(); !slices.Equal(got, wantCalls) {
		t.Fatalf("hook order = %v, want %v", got, wantCalls)
	}
}

// 0008-L 2.4.1: the numbered order, with the two orderings the document calls
// out — collectors after the children (④ after ③) and the server job last (⑧).
func TestShutdownSequenceOrder(t *testing.T) {
	h := newHarness(t, defaultConfig(), fullHooks)
	h.srv.Startup()
	h.srv.Shutdown(ReasonServiceControl, TotalBudget)

	want := []string{
		"validate", "open db", "migrations", "restore", "bind",
		"stop accepting", "wait inflight", "stop children", "stop collectors",
		"close db", "close server job",
	}
	if got := h.order(); !slices.Equal(got, want) {
		t.Fatalf("shutdown order =\n%v\nwant\n%v", got, want)
	}
}

// ① is the whole point: the `stopping` record precedes every cleanup step, so a
// process killed part way through has still said why it went down
// (0004-NR 1.4, 0008-L 2.4.1).
func TestStoppingIsRecordedBeforeAnyCleanup(t *testing.T) {
	var seen string
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		capture := func(name string) func() {
			return func() {
				h.record(name)
				if seen == "" {
					seen = h.logText()
				}
			}
		}
		return Hooks{
			StopAccepting:  capture("stop accepting"),
			StopCollectors: capture("stop collectors"),
			CloseDatabase:  capture("close db"),
			CloseServerJob: capture("close server job"),
			StopChildren:   func(string, time.Time) { capture("stop children")() },
		}
	})
	h.srv.Startup()
	h.srv.Shutdown(ReasonConsoleClose, ConsoleCloseBudget)

	if !strings.Contains(seen, "INFO stopping reason=console_close") {
		t.Fatalf("first cleanup step ran before the stopping record was on disk:\n%s", seen)
	}
}

// 0008-L 2.4.1: with kill_children_on_exit off, step ③ is skipped entirely.
func TestChildrenLeftAloneWhenDisabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.KillChildrenOnExit = false
	h := newHarness(t, cfg, fullHooks)
	h.srv.Startup()
	h.srv.Shutdown(ReasonSignal, TotalBudget)

	if slices.Contains(h.order(), "stop children") {
		t.Fatalf("children were stopped although KillChildrenOnExit is false: %v", h.order())
	}
	if !slices.Contains(h.order(), "stop collectors") {
		t.Fatalf("the rest of the sequence was skipped too: %v", h.order())
	}
}

// The console close path gets the reduced budget (0008-L 2.4.3); the deadline
// handed to the children has to reflect it, or the reduction means nothing.
func TestBudgetReachesTheChildStep(t *testing.T) {
	var deadline time.Time
	var inflight time.Duration
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{
			WaitInflight: func(d time.Duration) { inflight = d },
			StopChildren: func(_ string, d time.Time) { deadline = d },
		}
	})
	start := time.Now()
	h.srv.Startup()
	h.srv.Shutdown(ReasonConsoleClose, ConsoleCloseBudget)

	// The deadline is taken inside the sequence, a moment after start, so it
	// sits just past the budget rather than exactly on it.
	if left := deadline.Sub(start); left > ConsoleCloseBudget+time.Second || left < ConsoleCloseBudget-time.Second {
		t.Errorf("child deadline is %v away, want about %v", left, ConsoleCloseBudget)
	}
	// The inflight grace can never exceed what is left of the total budget.
	if inflight > ConsoleCloseBudget {
		t.Errorf("inflight grace = %v, want at most the remaining budget %v",
			inflight, ConsoleCloseBudget)
	}
}

func TestInflightGraceIsCappedByTheBudget(t *testing.T) {
	var inflight time.Duration
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{WaitInflight: func(d time.Duration) { inflight = d }}
	})
	h.srv.Startup()
	h.srv.Shutdown(ReasonServiceControl, TotalBudget)
	if inflight > InflightGrace {
		t.Fatalf("inflight grace = %v, want at most %v", inflight, InflightGrace)
	}
}

// A signal handler and the serve loop can both reach Shutdown. Running the
// sequence twice would log `stopping` twice and tear the children down twice.
func TestShutdownIsIdempotentUnderConcurrency(t *testing.T) {
	h := newHarness(t, defaultConfig(), fullHooks)
	h.srv.Startup()

	codes := make(chan int, 8)
	for range 8 {
		go func() { codes <- h.srv.Shutdown(ReasonSignal, TotalBudget) }()
	}
	for range 8 {
		if code := <-codes; code != ExitNormal {
			t.Errorf("Shutdown = %d, want %d", code, ExitNormal)
		}
	}
	if n := strings.Count(h.logText(), "INFO stopping"); n != 1 {
		t.Errorf("stopping recorded %d times, want 1", n)
	}
	if n := strings.Count(h.logText(), "INFO stopped"); n != 1 {
		t.Errorf("stopped recorded %d times, want 1", n)
	}
	if n := slices.Index(h.order(), "stop children"); n < 0 {
		t.Error("children were never stopped")
	}
}

func TestServeReturnsAfterShutdown(t *testing.T) {
	h := newHarness(t, defaultConfig(), fullHooks)
	h.srv.Startup()

	done := make(chan int, 1)
	go func() { done <- h.srv.Serve() }()

	// Serve must be running before the shutdown, otherwise this proves nothing.
	waitFor(t, func() bool { return slices.Contains(h.order(), "serve") })
	h.srv.Shutdown(ReasonConsoleCtrl, TotalBudget)

	select {
	case code := <-done:
		if code != ExitNormal {
			t.Fatalf("Serve = %d, want %d", code, ExitNormal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
	if !strings.Contains(h.logText(), "stopping reason=console_ctrl") {
		t.Errorf("wrong reason recorded:\n%s", h.logText())
	}
}

// Serve must not return before the sequence has finished, or the process exits
// while the children are still being torn down.
func TestServeWaitsForTheSequenceToFinish(t *testing.T) {
	released := make(chan struct{})
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{
			Serve: func(stop <-chan struct{}) error { <-stop; return nil },
			StopChildren: func(string, time.Time) {
				<-released
				h.record("stop children")
			},
			CloseServerJob: func() { h.record("close server job") },
		}
	})
	h.srv.Startup()

	served := make(chan int, 1)
	go func() { served <- h.srv.Serve() }()
	go h.srv.Shutdown(ReasonSignal, TotalBudget)

	select {
	case <-served:
		t.Fatal("Serve returned while the shutdown sequence was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(released)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return once the sequence finished")
	}
	if got := h.order(); !slices.Equal(got, []string{"stop children", "close server job"}) {
		t.Fatalf("sequence did not complete: %v", got)
	}
}

// 0008-L 3.2: a panic still goes through the cleanup, is recorded, and produces
// a restartable exit code.
func TestPanicIsRecordedAndStillCleansUp(t *testing.T) {
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{
			Serve:          func(<-chan struct{}) error { panic("listener exploded") },
			StopChildren:   func(string, time.Time) { h.record("stop children") },
			CloseServerJob: func() { h.record("close server job") },
		}
	})
	h.srv.Startup()

	code := h.srv.Serve()
	if code != ExitStartFailed {
		t.Errorf("Serve = %d, want %d so the service restarts", code, ExitStartFailed)
	}
	got := h.logText()
	if !strings.Contains(got, `ERROR panic detail="listener exploded"`) {
		t.Errorf("panic not recorded:\n%s", got)
	}
	if !strings.Contains(got, "INFO stopping reason=internal_error") {
		t.Errorf("cleanup did not run:\n%s", got)
	}
	if !strings.Contains(got, "INFO stopped exit_code=1") {
		t.Errorf("wrong exit code recorded:\n%s", got)
	}
	if !slices.Contains(h.order(), "close server job") {
		t.Errorf("sequence did not complete: %v", h.order())
	}
}

// A front end that returns on its own was not asked to stop; the children must
// still be cleaned up rather than orphaned.
func TestServeReturningOnItsOwnIsTreatedAsAFailure(t *testing.T) {
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{
			Serve:        func(<-chan struct{}) error { return nil },
			StopChildren: func(string, time.Time) { h.record("stop children") },
		}
	})
	h.srv.Startup()
	if code := h.srv.Serve(); code != ExitStartFailed {
		t.Errorf("Serve = %d, want %d", code, ExitStartFailed)
	}
	if !slices.Contains(h.order(), "stop children") {
		t.Errorf("children were orphaned: %v", h.order())
	}
}

func TestServeErrorIsRecorded(t *testing.T) {
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{Serve: func(<-chan struct{}) error { return errors.New("accept failed") }}
	})
	h.srv.Startup()
	if code := h.srv.Serve(); code != ExitStartFailed {
		t.Errorf("Serve = %d, want %d", code, ExitStartFailed)
	}
	if !strings.Contains(h.logText(), `ERROR panic detail="accept failed"`) {
		t.Errorf("serve error not recorded:\n%s", h.logText())
	}
}

// 0008-L 3.2 / E-26: an occupied port is a recorded failure with exit code 1,
// never a silent pass.
func TestBindFailureRecordsBindFailedAndExits(t *testing.T) {
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{
			OpenDatabase:  func() error { h.record("open db"); return nil },
			CloseDatabase: func() { h.record("close db") },
			Bind: func() (string, error) {
				return "", errors.New("only one usage of each socket address is normally permitted")
			},
		}
	})
	code, ok := h.srv.Startup()
	if ok || code != ExitStartFailed {
		t.Fatalf("Startup = (%d, %v), want (%d, false)", code, ok, ExitStartFailed)
	}
	want := []string{
		"INFO start host=0.0.0.0 port=8091 db=spawnpoint.db auth=disabled",
		`ERROR bind_failed host=0.0.0.0 port=8091 detail="only one usage of each socket address is normally permitted"`,
		"ERROR exiting exit_code=1",
	}
	if got := h.events(); !slices.Equal(got, want) {
		t.Fatalf("records =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The database must not stay locked: the service restarts in five seconds.
	if got := h.order(); !slices.Equal(got, []string{"open db", "close db"}) {
		t.Fatalf("hook order = %v, want the database to be closed again", got)
	}
}

// 0008-L 1.4 / 3.2: an asset defect repeats on every retry, so it exits 2 and
// the service does not restart it.
func TestAssetFailureIsUnrecoverable(t *testing.T) {
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{
			ValidateAssets: func() error { return errors.New("001_create_spawn_tables.sql starts with a BOM") },
			OpenDatabase:   func() error { h.record("open db"); return nil },
		}
	})
	code, ok := h.srv.Startup()
	if ok || code != ExitUnrecoverable {
		t.Fatalf("Startup = (%d, %v), want (%d, false)", code, ok, ExitUnrecoverable)
	}
	if !strings.Contains(h.logText(), "ERROR exiting exit_code=2") {
		t.Errorf("exit code not recorded:\n%s", h.logText())
	}
	if !strings.Contains(h.logText(), "BOM") {
		t.Errorf("cause not recorded:\n%s", h.logText())
	}
	if slices.Contains(h.order(), "open db") {
		t.Error("the database was opened after the asset check failed")
	}
}

// 0008-L 3.2: a failed migration exits 1 — a locked or busy database may well
// succeed on the restart.
func TestMigrationFailureIsRestartable(t *testing.T) {
	h := newHarness(t, defaultConfig(), func(h *harness) Hooks {
		return Hooks{
			OpenDatabase:    func() error { h.record("open db"); return nil },
			CloseDatabase:   func() { h.record("close db") },
			ApplyMigrations: func() (int, int, error) { return 0, 1, errors.New("database is locked") },
		}
	})
	code, ok := h.srv.Startup()
	if ok || code != ExitStartFailed {
		t.Fatalf("Startup = (%d, %v), want (%d, false)", code, ok, ExitStartFailed)
	}
	if !strings.Contains(h.logText(), "ERROR exiting exit_code=1") {
		t.Errorf("exit code not recorded:\n%s", h.logText())
	}
	if got := h.order(); !slices.Equal(got, []string{"open db", "close db"}) {
		t.Fatalf("hook order = %v, want the database to be closed again", got)
	}
}

// A nil hook is skipped rather than crashing: T3 to T6 have not landed yet and
// the sequence has to run without them.
func TestNilHooksAreSkipped(t *testing.T) {
	h := newHarness(t, defaultConfig(), func(*harness) Hooks { return Hooks{} })
	if code, ok := h.srv.Startup(); !ok || code != ExitNormal {
		t.Fatalf("Startup = (%d, %v)", code, ok)
	}
	if code := h.srv.Shutdown(ReasonSignal, TotalBudget); code != ExitNormal {
		t.Fatalf("Shutdown = %d", code)
	}
	want := []string{
		"INFO start host=0.0.0.0 port=8091 db=spawnpoint.db auth=disabled",
		"INFO stopping reason=signal",
		"INFO stopped exit_code=0",
	}
	if got := h.events(); !slices.Equal(got, want) {
		t.Fatalf("records =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStoppingReportsState(t *testing.T) {
	h := newHarness(t, defaultConfig(), func(*harness) Hooks { return Hooks{} })
	h.srv.Startup()
	if h.srv.Stopping() {
		t.Fatal("Stopping is true before any shutdown")
	}
	h.srv.Shutdown(ReasonSignal, TotalBudget)
	if !h.srv.Stopping() {
		t.Fatal("Stopping is false after a shutdown")
	}
}

// Only the reasons in 0008-L 2.13 may reach the log.
func TestReasonsAreTheDocumentedSet(t *testing.T) {
	want := []string{
		"service_control", "console_ctrl", "console_close", "signal",
		"stop_requested", "restart_requested", "delete_requested", "internal_error",
	}
	got := []string{
		ReasonServiceControl, ReasonConsoleCtrl, ReasonConsoleClose, ReasonSignal,
		ReasonStopRequested, ReasonRestartRequest, ReasonDeleteRequested, ReasonInternalError,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("reasons = %v, want %v", got, want)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
