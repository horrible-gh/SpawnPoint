package runner

import (
	"sync"
	"time"

	"spawnpoint/internal/jobs"
)

// The five states of 0008-L 3.1. `stopped` covers three different histories —
// registered and never run, stopped on request, restored after a restart — on
// purpose: from the outside they are the same thing, an entry that is not
// running and can be run.
const (
	StatusStopped = "stopped"
	StatusRunning = "running"
	StatusExited  = "exited"
	StatusKilled  = "killed"
	StatusFailed  = "failed"
)

// Marker kinds for the child log (0008-L 2.5.1).
const (
	markerRun     = "run"
	markerRestart = "restart"
)

// Info is one entry as the rest of the server sees it: a snapshot, safe to hold
// and to render, with no handles in it.
type Info struct {
	ID    string
	Label string
	Cmd   string
	Cwd   *string
	Env   map[string]string

	Status   string
	PID      *int
	ExitCode *int
	// Error carries the reason a start failed. It is only meaningful — and only
	// put on the wire — when Status is failed (0008-L 4.5).
	Error     string
	StartedAt *time.Time
	EndedAt   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// entry is one registered command plus whatever of it is currently alive.
//
// The registration half survives a restart; the live half never does. Nothing
// can prove that the pid recorded before a restart still belongs to the command
// that was recorded with it — the operating system reuses numbers — so a
// restored entry comes back as stopped with every live field cleared
// (0006-D 3.1, 0008-L 3.1).
type entry struct {
	// opMu serialises whole operations against each other; mu guards the
	// fields. Two locks because an operation cannot hold mu for its duration —
	// stopping waits for a process to die, and the waiter that records the
	// death needs mu to do it. Without opMu a restart's stop and its start are
	// two separate moments, and a run arriving between them starts a second
	// process that the first one then overwrites.
	//
	// The order is opMu, then mu, then the manager's. Nothing takes opMu while
	// holding either of the others.
	opMu sync.Mutex

	mu sync.Mutex

	id        string
	label     string
	cmd       string
	cwd       *string
	env       map[string]string
	createdAt time.Time
	updatedAt time.Time

	status    string
	errText   string
	pid       *int
	exitCode  *int
	startedAt *time.Time
	endedAt   *time.Time

	// stopRequested distinguishes "we stopped it" from "it stopped". The exit
	// code cannot: a process killed on request and a process that failed on its
	// own both come back non-zero (0008-L 3.1).
	stopRequested bool

	job  *jobs.Group
	coll *collector
	// exited closes when the waiter has recorded this run's outcome.
	exited chan struct{}
	// gen counts starts. A waiter carries the generation it was started for and
	// drops its result if the entry has been started again since, which is what
	// keeps a restart from having the outgoing process overwrite the incoming
	// one's state.
	gen int
}

// isRunning reports whether a process is alive for this entry. The log reader
// asks because a stalled decode is normal for a live child and permanent for a
// dead one (E-11).
func (e *entry) isRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status == StatusRunning
}

// info snapshots the entry. The caller must not hold e.mu.
func (e *entry) info() Info {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.infoLocked()
}

func (e *entry) infoLocked() Info {
	out := Info{
		ID:        e.id,
		Label:     e.label,
		Cmd:       e.cmd,
		Status:    e.status,
		Error:     e.errText,
		CreatedAt: e.createdAt,
		UpdatedAt: e.updatedAt,
		Env:       make(map[string]string, len(e.env)),
	}
	if e.cwd != nil {
		cwd := *e.cwd
		out.Cwd = &cwd
	}
	for k, v := range e.env {
		out.Env[k] = v
	}
	// Copied, not aliased: the caller holds this after the lock is released and
	// the next state change would otherwise rewrite it underneath them.
	if e.pid != nil {
		pid := *e.pid
		out.PID = &pid
	}
	if e.exitCode != nil {
		code := *e.exitCode
		out.ExitCode = &code
	}
	if e.startedAt != nil {
		at := *e.startedAt
		out.StartedAt = &at
	}
	if e.endedAt != nil {
		at := *e.endedAt
		out.EndedAt = &at
	}
	return out
}

// clearLive resets the live half. Used before a start attempt and by the
// restore path.
func (e *entry) clearLive() {
	e.status = StatusStopped
	e.errText = ""
	e.pid = nil
	e.exitCode = nil
	e.startedAt = nil
	e.endedAt = nil
	e.stopRequested = false
	e.job = nil
	e.coll = nil
	e.exited = nil
}

// markFailed records a start that never produced a process (0008-L 4.5).
//
// started_at stays null. The entry never started, and a timestamp there would
// read as "it ran and ended instantly", which is a different fault with a
// different cause.
func (e *entry) markFailed(reason string, at time.Time) {
	e.status = StatusFailed
	e.errText = reason
	e.pid = nil
	e.exitCode = nil
	e.startedAt = nil
	ended := at
	e.endedAt = &ended
}

func timePtr(t time.Time) *time.Time { return &t }
func intPtr(v int) *int              { return &v }
