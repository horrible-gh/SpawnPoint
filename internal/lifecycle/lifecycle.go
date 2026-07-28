// Package lifecycle runs SpawnPoint's startup and shutdown sequences.
//
// Both sequences are ordered contracts, not conveniences:
//
//   - Startup (0008-L 2.15) opens the operations log before the database, so a
//     failure anywhere after that point is recorded rather than lost.
//   - Shutdown (0008-L 2.4.1) writes `stopping` first, before any cleanup. If
//     the budget runs out and the operating system kills the process mid-way,
//     when and why it went down is already in the file. 0004-NR 1.4 found that
//     the current server leaves nothing at all, and that is the gap this closes.
//
// The stages that belong to later work items are injected as Hooks. A nil hook
// is skipped, which keeps the sequence itself runnable and testable before the
// database (T3), the runner (T4/T5) and the request front end (T6) exist.
package lifecycle

import (
	"fmt"
	"sync"
	"time"

	"spawnpoint/internal/config"
	"spawnpoint/internal/opslog"
)

// Process exit codes (0008-L 1.4). The service restart policy reads them:
// 0 and 2 are not restarted, everything else is.
const (
	ExitNormal        = 0
	ExitStartFailed   = 1
	ExitUnrecoverable = 2
)

// Shutdown reasons (0008-L 2.13). This is the complete allowed set for the
// `reason` field; nothing outside it may be logged.
const (
	ReasonServiceControl  = "service_control"
	ReasonConsoleCtrl     = "console_ctrl"
	ReasonConsoleClose    = "console_close"
	ReasonSignal          = "signal"
	ReasonStopRequested   = "stop_requested"
	ReasonRestartRequest  = "restart_requested"
	ReasonDeleteRequested = "delete_requested"
	ReasonInternalError   = "internal_error"
)

// Budgets (0008-L 1.4).
const (
	// InflightGrace is how long requests already being served are given after
	// the listener stops accepting.
	InflightGrace = 5 * time.Second
	// TotalBudget covers the whole cleanup on the service-control, interrupt
	// and signal paths.
	TotalBudget = 20 * time.Second
	// ConsoleCloseBudget is the reduced budget for the console close and
	// logoff paths; the operating system terminates the process at about that
	// point anyway (0004-NR 1.5). Only the `stopping` record has to fit
	// (0008-L 2.4.3) — the rest is delegated to the kernel through the server
	// job object.
	ConsoleCloseBudget = 5 * time.Second
)

// Hooks are the stages the sequences drive. Each is filled in by the work item
// that owns it; a nil hook is skipped.
type Hooks struct {
	// ValidateAssets checks the embedded assets — filename convention, BOM,
	// compound statements (0008-L 2.15, 2.9). Failure is unrecoverable. [T3]
	ValidateAssets func() error
	// OpenDatabase opens the existing SQLite file. [T3]
	OpenDatabase func() error
	// ApplyMigrations applies pending files and reports the counts for the
	// `migrations` record. [T3]
	ApplyMigrations func() (applied, pending int, err error)
	// RestoreEntries reloads the registered commands as stopped and reports
	// how many. Live state is never restored (0008-L 2.15). [T4/T6]
	RestoreEntries func() (int, error)
	// Bind acquires the listening address and returns it for the `listening`
	// record. Address reuse is not enabled, so an occupied port fails here
	// (0004-NR F3, E-26). [T6]
	Bind func() (address string, err error)
	// Serve handles requests until stop is closed. [T6]
	Serve func(stop <-chan struct{}) error

	// StopAccepting closes the listener. Step ② of 0008-L 2.4.1.
	StopAccepting func()
	// WaitInflight waits for requests already in progress, up to budget.
	WaitInflight func(budget time.Duration)
	// StopChildren tears down the running children, honouring deadline. Only
	// called when KillChildrenOnExit is set. Step ③. [T4]
	StopChildren func(reason string, deadline time.Time)
	// StopCollectors drains and closes the child log collectors. Step ④. [T5]
	StopCollectors func()
	// CloseDatabase closes the database. Step ⑤. [T3]
	CloseDatabase func()
	// CloseServerJob releases the server-wide process job handle. Step ⑧, and
	// deliberately the very last thing to happen: while it is held, the kernel
	// remains the backstop for any child the earlier steps failed to clean up
	// (0008-L 2.3, 2.4.1). [T4]
	CloseServerJob func()
}

// Server drives one process through startup, serving and shutdown.
type Server struct {
	cfg   config.Config
	log   *opslog.Logger
	hooks Hooks

	// stop is closed at step ② and releases Serve.
	stop chan struct{}
	// done is closed once the shutdown sequence has finished. Callers that
	// arrive during a shutdown wait on it instead of running a second one.
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	exitCode int
}

// New builds a server. log must already be open: opening it is step ① of
// startup and a failure there stops the process before this point.
func New(cfg config.Config, log *opslog.Logger, hooks Hooks) *Server {
	return &Server{
		cfg:   cfg,
		log:   log,
		hooks: hooks,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// Log exposes the operations log so the caller can record events of its own.
func (s *Server) Log() *opslog.Logger { return s.log }

// Startup runs 0008-L 2.15 in order and reports the exit code to use when it
// fails. On failure the operations log already carries the reason and an
// `exiting` record; the caller only has to exit.
func (s *Server) Startup() (exitCode int, ok bool) {
	s.log.Log(opslog.Info, "start",
		opslog.F("host", s.cfg.Host),
		opslog.F("port", s.cfg.Port),
		opslog.F("db", s.cfg.DBPath),
		opslog.F("auth", s.cfg.AuthMode()))

	// Embedded assets are checked before the database is touched: a build
	// defect produces the same result on every retry, so it must not be
	// restarted (0008-L 1.4, 3.2).
	if s.hooks.ValidateAssets != nil {
		if err := s.hooks.ValidateAssets(); err != nil {
			return s.startupFailed(ExitUnrecoverable, "embedded assets", err)
		}
	}
	dbOpen := false
	// unwind releases what startup already acquired. The process is about to
	// exit anyway, but leaving the database file locked makes the immediate
	// restart that the service policy performs fail for a second reason.
	unwind := func() {
		if dbOpen && s.hooks.CloseDatabase != nil {
			s.hooks.CloseDatabase()
		}
	}

	if s.hooks.OpenDatabase != nil {
		if err := s.hooks.OpenDatabase(); err != nil {
			return s.startupFailed(ExitStartFailed, "open database", err)
		}
		dbOpen = true
	}
	if s.hooks.ApplyMigrations != nil {
		applied, pending, err := s.hooks.ApplyMigrations()
		if err != nil {
			unwind()
			return s.startupFailed(ExitStartFailed, "apply migrations", err)
		}
		s.log.Log(opslog.Info, "migrations",
			opslog.F("applied", applied), opslog.F("pending", pending))
	}
	if s.hooks.RestoreEntries != nil {
		entries, err := s.hooks.RestoreEntries()
		if err != nil {
			unwind()
			return s.startupFailed(ExitStartFailed, "restore runner entries", err)
		}
		s.log.Log(opslog.Info, "runner restored",
			opslog.F("entries", entries), opslog.F("status", "stopped"))
	}
	if s.hooks.Bind != nil {
		address, err := s.hooks.Bind()
		if err != nil {
			unwind()
			// bind_failed carries the host and port so an occupied port is
			// identifiable from the log alone (0007-P [서비스 기동 실패]).
			s.log.Log(opslog.Error, "bind_failed",
				opslog.F("host", s.cfg.Host),
				opslog.F("port", s.cfg.Port),
				opslog.F("detail", err))
			s.log.Log(opslog.Error, "exiting", opslog.F("exit_code", ExitStartFailed))
			return ExitStartFailed, false
		}
		s.log.Log(opslog.Info, "listening", opslog.V(address))
	}
	return ExitNormal, true
}

// startupFailed records the cause and the exit code, in that order, so a reader
// sees what went wrong before seeing what the process did about it.
func (s *Server) startupFailed(code int, stage string, err error) (int, bool) {
	s.log.Log(opslog.Error, "exiting",
		opslog.F("exit_code", code),
		opslog.F("detail", fmt.Sprintf("%s: %v", stage, err)))
	return code, false
}

// Serve runs the request front end until a shutdown is requested, then returns
// the process exit code. It must only be called after a successful Startup.
//
// An unhandled failure inside Serve is not allowed to skip the cleanup: it is
// recorded as `panic` and then goes through the normal sequence (0008-L 2.4.2,
// 3.2), because the children of a crashed server are exactly the orphans this
// design is meant to prevent.
func (s *Server) Serve() (exitCode int) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Log(opslog.Error, "panic", opslog.F("detail", fmt.Sprint(r)))
			exitCode = s.shutdown(ReasonInternalError, TotalBudget, ExitStartFailed)
		}
	}()

	if s.hooks.Serve != nil {
		if err := s.hooks.Serve(s.stop); err != nil {
			s.log.Log(opslog.Error, "panic", opslog.F("detail", err))
			return s.shutdown(ReasonInternalError, TotalBudget, ExitStartFailed)
		}
		if !s.Stopping() {
			// The front end gave up without anyone asking it to. Treated as an
			// internal error so the children are still cleaned up and the
			// service restart policy sees a non-zero code.
			s.log.Log(opslog.Error, "panic",
				opslog.F("detail", "serve returned without a shutdown request"))
			return s.shutdown(ReasonInternalError, TotalBudget, ExitStartFailed)
		}
	} else {
		<-s.stop
	}
	// Serve returned because a shutdown closed s.stop. Wait for that sequence
	// to finish rather than racing it to the exit.
	<-s.done
	return s.code()
}

// Shutdown runs the cleanup sequence for reason within budget and returns the
// process exit code. It is safe to call from a signal handler, safe to call
// concurrently, and safe to call more than once — later callers wait for the
// first sequence and get its exit code.
func (s *Server) Shutdown(reason string, budget time.Duration) int {
	return s.shutdown(reason, budget, ExitNormal)
}

func (s *Server) shutdown(reason string, budget time.Duration, code int) int {
	s.once.Do(func() {
		s.setCode(code)
		s.run(reason, budget)
		close(s.done)
	})
	<-s.done
	return s.code()
}

// run is 0008-L 2.4.1 step by step. The numbering in the comments is the
// document's.
func (s *Server) run(reason string, budget time.Duration) {
	deadline := time.Now().Add(budget)

	// ① The trace comes first. Everything after this may be cut short by the
	// operating system; this record must not be.
	s.log.Log(opslog.Info, "stopping", opslog.F("reason", reason))

	// ② Stop accepting. Closing s.stop releases Serve and any hook waiting on
	// it, so the front end winds down alongside the explicit hook.
	close(s.stop)
	if s.hooks.StopAccepting != nil {
		s.hooks.StopAccepting()
	}
	if s.hooks.WaitInflight != nil {
		s.hooks.WaitInflight(min(InflightGrace, remaining(deadline)))
	}

	// ③ Children. Skipped entirely when the operator asked for the children to
	// outlive the server (0008-L 2.4.1); in that case they were never put into
	// the job objects either.
	if s.cfg.KillChildrenOnExit && s.hooks.StopChildren != nil {
		s.hooks.StopChildren(reason, deadline)
	}

	// ④ Collectors, after the children are gone: a collector closed earlier
	// would lose the output a child produced on its way down, which is the
	// part worth keeping (0008-L 2.3).
	if s.hooks.StopCollectors != nil {
		s.hooks.StopCollectors()
	}

	// ⑤ Database.
	if s.hooks.CloseDatabase != nil {
		s.hooks.CloseDatabase()
	}

	// ⑥ ⑦ The closing record, then the log itself.
	s.log.Log(opslog.Info, "stopped", opslog.F("exit_code", s.code()))
	s.log.Close()

	// ⑧ The kernel backstop is released last, so it covers every step above.
	if s.hooks.CloseServerJob != nil {
		s.hooks.CloseServerJob()
	}
}

// Stopping reports whether a shutdown has begun. The front end uses it to stop
// accepting new work.
func (s *Server) Stopping() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *Server) code() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

func (s *Server) setCode(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exitCode = code
}

// remaining is the time left before deadline, never negative.
func remaining(deadline time.Time) time.Duration {
	if d := time.Until(deadline); d > 0 {
		return d
	}
	return 0
}
