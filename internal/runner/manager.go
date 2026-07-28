package runner

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"spawnpoint/internal/jobs"
	"spawnpoint/internal/lifecycle"
	"spawnpoint/internal/opslog"
	"spawnpoint/internal/store"
	"spawnpoint/internal/textdec"
)

// Termination budgets (0008-L 1.2).
const (
	// ChildTerminateTimeout is the first wait after the group is told to stop.
	ChildTerminateTimeout = 5 * time.Second
	// ChildTerminateForceTimeout is the second wait, after the forced attempt.
	ChildTerminateForceTimeout = 3 * time.Second
	// collectorDrainTimeout bounds the wait for the collector once the child is
	// gone. Reaching it means a descendant outlived the group termination,
	// which is already reported as `child terminate timeout`.
	collectorDrainTimeout = 2 * time.Second
)

// IDHexDigits is `proc_id_hex_digits` (0008-L 1.2).
const IDHexDigits = 8

// Registry is the part of the store the runner uses. It is an interface so the
// runner's own tests do not need a database, and so the persistence failure
// path (E-21) can be exercised, which is otherwise only reachable by breaking a
// real file mid-run.
type Registry interface {
	SaveEntry(store.RunnerEntry) error
	DeleteEntry(id string) error
	ListEntries() ([]store.RunnerEntry, error)
}

// Manager owns the registered commands and the processes they start.
//
// It is the piece that makes the two containment layers of 0008-L 2.3 real: a
// group per child so one stop request reaches a whole tree, and one group for
// the server so that no exit path — including the ones that run no code —
// leaves a child behind.
type Manager struct {
	logDir     string
	log        *opslog.Logger
	registry   Registry
	killOnExit bool
	// decoder reads child log bytes back as text. It is a field rather than a
	// package call so the log contract tests can pin the candidate order of the
	// machine 0007-P was written against (0008-L 2.7).
	decoder *textdec.Decoder

	mu      sync.Mutex
	entries map[string]*entry

	// serverJob is created just before the first child, not at startup: a
	// server that never runs anything should not hold a job object, and the
	// current implementation defers it the same way (0008-L 2.3).
	serverJob     *jobs.Group
	serverJobDone bool
	// noChildJobs is set once the operating system has refused a nested
	// assignment. From then on the server group is the only one and stops go
	// through the tree walk (E-19).
	noChildJobs bool
}

// New builds a manager. log must be open; registry may be nil, in which case
// registrations live only in this process — which is what the current
// implementation does without a store.
func New(logDir string, log *opslog.Logger, registry Registry, killOnExit bool) *Manager {
	return &Manager{
		logDir:     logDir,
		log:        log,
		registry:   registry,
		killOnExit: killOnExit,
		decoder:    textdec.Default(),
		entries:    make(map[string]*entry),
	}
}

// LogPath is the child log for id. Child logs sit beside the operations log and
// cannot collide with it: an id is `proc_` plus eight hex digits.
func (m *Manager) LogPath(id string) string {
	return filepath.Join(m.logDir, id+".log")
}

// --- Registration --------------------------------------------------------------

// Register adds a command and starts it, in that order.
//
// The order is the contract (0007-P [새 명령 등록 — 정상]): the registration is
// saved before the process is started, so a server that dies during the start
// still has the command on the next boot. A save that fails does not stop the
// start — the user asked for the process, and refusing to run it because a row
// could not be written would trade a recoverable loss for an immediate one. The
// caller is told, through persisted, so the response can say so (E-21).
func (m *Manager) Register(label, cmd string, cwd *string, env map[string]string) (Info, bool) {
	now := time.Now()
	e := &entry{
		id:        m.newID(),
		label:     label,
		cmd:       cmd,
		cwd:       normaliseCwd(cwd),
		env:       copyEnv(env),
		createdAt: now,
		updatedAt: now,
		status:    StatusStopped,
	}

	m.mu.Lock()
	m.entries[e.id] = e
	m.mu.Unlock()

	persisted := m.persist(e)

	e.mu.Lock()
	m.spawn(e, "")
	info := e.infoLocked()
	e.mu.Unlock()
	return info, persisted
}

// Update replaces the registration and leaves the process alone.
//
// A running child keeps running with the command line it was started with; the
// new one applies from the next start (0008-L 3.1). Rewriting a live process's
// registration to say something it is not doing would make the log and the
// listing disagree with the machine.
//
// The middle result is whether the new registration reached the store, and it
// is reported for the same reason Register reports it: an edit that was applied
// in memory but not written disappears on the next restart, and the current
// server's silence about that is what produced entries the user could see but
// the database had never heard of (E-21, 0004-NR F7).
func (m *Manager) Update(id string, label, cmd *string, cwd **string, env map[string]string) (Info, bool, bool) {
	e, ok := m.entry(id)
	if !ok {
		return Info{}, false, false
	}
	e.mu.Lock()
	if label != nil {
		e.label = *label
	}
	if cmd != nil {
		e.cmd = *cmd
	}
	if cwd != nil {
		e.cwd = normaliseCwd(*cwd)
	}
	if env != nil {
		e.env = copyEnv(env)
	}
	e.updatedAt = time.Now()
	info := e.infoLocked()
	e.mu.Unlock()

	return info, m.persist(e), true
}

// Delete stops the process, removes the registration and removes the logs.
//
// The log and every archive go together. Leaving archives behind would have the
// next command to be given that identifier — identifiers are random, but the
// space is finite — append to a stranger's history (0008-L 2.5.1,
// 0007-P [삭제]).
func (m *Manager) Delete(id string) bool {
	e, ok := m.entry(id)
	if !ok {
		return false
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	m.stopChild(e, lifecycle.ReasonDeleteRequested, time.Time{})

	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()

	if m.registry != nil {
		if err := m.registry.DeleteEntry(id); err != nil {
			m.log.Log(opslog.Error, "runner entry persist failed",
				opslog.F("id", id), opslog.F("detail", err))
		}
	}
	removed := m.removeLogs(id)
	m.log.Log(opslog.Info, "entry deleted",
		opslog.F("id", id), opslog.F("logs_removed", removed))
	return true
}

// removeLogs deletes <id>.log and its archives, returning how many went.
func (m *Manager) removeLogs(id string) int {
	removed := 0
	base := m.LogPath(id)
	names := make([]string, 0, LogArchiveKeep+1)
	names = append(names, base)
	for n := 1; n <= LogArchiveKeep; n++ {
		names = append(names, archiveName(base, n))
	}
	for _, name := range names {
		if err := os.Remove(name); err == nil {
			removed++
		}
	}
	return removed
}

// Restore reloads the registrations as stopped and reports how many.
//
// Only the registration comes back. The pid recorded before a restart cannot be
// shown to still belong to the command it was recorded against — the operating
// system reuses numbers — so every live field is left null and the user resumes
// the entry with run (0006-D 3.1, 0008-L 2.15, 6.5).
func (m *Manager) Restore() (int, error) {
	if m.registry == nil {
		return 0, nil
	}
	rows, err := m.registry.ListEntries()
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range rows {
		e := &entry{
			id:        row.ID,
			label:     row.Label,
			cmd:       row.Cmd,
			cwd:       normaliseCwd(row.Cwd),
			env:       copyEnv(row.Env),
			createdAt: row.CreatedAt,
			updatedAt: row.UpdatedAt,
		}
		e.clearLive()
		m.entries[e.id] = e
	}
	return len(rows), nil
}

// persist saves the registration, reporting a failure without failing the
// operation (E-21).
func (m *Manager) persist(e *entry) bool {
	if m.registry == nil {
		return false
	}
	e.mu.Lock()
	row := store.RunnerEntry{
		ID: e.id, Label: e.label, Cmd: e.cmd, Cwd: e.cwd, Env: copyEnv(e.env),
		CreatedAt: e.createdAt, UpdatedAt: e.updatedAt,
	}
	e.mu.Unlock()

	if err := m.registry.SaveEntry(row); err != nil {
		m.log.Log(opslog.Error, "runner entry persist failed",
			opslog.F("id", row.ID), opslog.F("detail", err))
		return false
	}
	return true
}

// --- Lifecycle operations -------------------------------------------------------

// Run resumes a stopped entry. An entry that is already running is left exactly
// as it is and reported back unchanged — a second click is not an error and
// must not start a second process (0008-L 3.1, E-16).
func (m *Manager) Run(id string) (Info, bool) {
	e, ok := m.entry(id)
	if !ok {
		return Info{}, false
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status == StatusRunning {
		return e.infoLocked(), true
	}
	m.spawn(e, markerRun)
	return e.infoLocked(), true
}

// Restart stops the entry if it is running and starts it again.
//
// The log file is not truncated. A restart marker is written into it instead,
// so the history of every run stays in one place and the marker says which run
// the lines after it belong to (0008-L 3.1, 2.5.1).
func (m *Manager) Restart(id string) (Info, bool) {
	e, ok := m.entry(id)
	if !ok {
		return Info{}, false
	}
	// Held across both halves: a run arriving between the stop and the start
	// would otherwise start a process this one then replaces and loses track of.
	e.opMu.Lock()
	defer e.opMu.Unlock()

	m.stopChild(e, lifecycle.ReasonRestartRequest, time.Time{})

	e.mu.Lock()
	defer e.mu.Unlock()
	m.spawn(e, markerRestart)
	return e.infoLocked(), true
}

// Stop terminates the entry's process tree. Stopping something that is not
// running returns its current state and is not an error (0008-L 3.1, E-17).
func (m *Manager) Stop(id string) (Info, bool) {
	e, ok := m.entry(id)
	if !ok {
		return Info{}, false
	}
	e.opMu.Lock()
	m.stopChild(e, lifecycle.ReasonStopRequested, time.Time{})
	e.opMu.Unlock()
	return e.info(), true
}

// Get returns one entry.
func (m *Manager) Get(id string) (Info, bool) {
	e, ok := m.entry(id)
	if !ok {
		return Info{}, false
	}
	return e.info(), true
}

// List returns every entry in restore order: registration time ascending,
// identifier ascending on a tie. The live listing and the restored listing are
// sorted the same way so a restart does not reorder the screen (0008-L 6.5).
func (m *Manager) List() []Info {
	m.mu.Lock()
	all := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		all = append(all, e)
	}
	m.mu.Unlock()

	out := make([]Info, 0, len(all))
	for _, e := range all {
		out = append(out, e.info())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// --- Shutdown hooks --------------------------------------------------------------

// StopChildren is step ③ of the shutdown sequence (0008-L 2.4.1).
//
// The children are stopped in parallel and the deadline is shared. Serially,
// one child that refuses to die would spend the whole budget and every child
// behind it would be left to the kernel; in parallel a single refusal costs
// only itself.
func (m *Manager) StopChildren(reason string, deadline time.Time) {
	m.mu.Lock()
	all := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		all = append(all, e)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range all {
		e.mu.Lock()
		running := e.status == StatusRunning
		e.mu.Unlock()
		if !running {
			continue
		}
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			e.opMu.Lock()
			defer e.opMu.Unlock()
			m.stopChild(e, reason, deadline)
		}(e)
	}
	wg.Wait()
}

// StopCollectors is step ④. By this point the children are gone, so every pipe
// is at end of stream and the collectors are finishing on their own; this waits
// for them rather than cutting them off, because what a child wrote on its way
// down is the part worth having (0008-L 2.3, 2.4.1).
func (m *Manager) StopCollectors() {
	// The entry list is copied out before any entry lock is taken. Every other
	// path locks an entry first and the manager second, and holding both in the
	// other order here would be the one place the two can meet head on.
	m.mu.Lock()
	all := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		all = append(all, e)
	}
	m.mu.Unlock()

	colls := make([]*collector, 0, len(all))
	for _, e := range all {
		e.mu.Lock()
		if e.coll != nil {
			colls = append(colls, e.coll)
		}
		e.mu.Unlock()
	}

	for _, c := range colls {
		c.stop(collectorDrainTimeout)
	}
}

// CloseServerJob is step ⑧, the last thing the process does.
//
// Releasing this handle is what arms the kernel: anything still inside the
// group dies with it. Doing it last means every earlier step was covered — if
// the shutdown budget ran out three steps ago, the children still go
// (0008-L 2.3, 2.4.1).
func (m *Manager) CloseServerJob() {
	m.mu.Lock()
	job := m.serverJob
	m.serverJob = nil
	m.serverJobDone = true
	m.mu.Unlock()
	if job != nil {
		job.Close()
	}
}

// --- Spawning --------------------------------------------------------------------

// spawn starts the entry's command. The caller holds e.mu.
//
// marker is written to the log first when the start is a resume or a restart,
// and omitted for the initial registration, which has nothing to resume from
// (0008-L 2.2).
func (m *Manager) spawn(e *entry, marker string) {
	logPath := m.LogPath(e.id)
	if marker != "" {
		if err := writeMarker(logPath, marker, time.Now()); err != nil {
			m.log.Once(opslog.Error, "child log write failed", e.id,
				opslog.F("id", e.id), opslog.F("detail", err))
		}
	}

	// The working directory is checked here rather than left to the execution
	// layer. Handed down, a missing directory produces a different error shape
	// on every platform and, on Windows, a shell that starts anyway and fails
	// later — which would be reported as a run that died rather than a start
	// that never happened (0008-L 2.2, 4.5).
	if e.cwd != nil {
		if info, err := os.Stat(*e.cwd); err != nil || !info.IsDir() {
			reason := "cwd does not exist: " + *e.cwd
			e.markFailed(reason, time.Now())
			m.log.Log(opslog.Error, "child start failed",
				opslog.F("id", e.id), opslog.F("detail", reason))
			return
		}
	}

	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		reason := "cannot create the output pipe: " + err.Error()
		e.markFailed(reason, time.Now())
		m.log.Log(opslog.Error, "child start failed",
			opslog.F("id", e.id), opslog.F("detail", reason))
		return
	}

	cmd := ShellCommand(e.cmd)
	if e.cwd != nil {
		cmd.Dir = *e.cwd
	}
	cmd.Env = MergeEnv(os.Environ(), e.env)
	cmd.Stdout = pipeWrite
	cmd.Stderr = pipeWrite

	if err := cmd.Start(); err != nil {
		pipeRead.Close()
		pipeWrite.Close()
		e.markFailed(err.Error(), time.Now())
		m.log.Log(opslog.Error, "child start failed",
			opslog.F("id", e.id), opslog.F("detail", err))
		return
	}
	// The server's copy of the write end goes now. Holding it would keep the
	// pipe from ever reaching end of stream, so the collector would never
	// finish and every stop would spend its full drain budget.
	pipeWrite.Close()

	pid := cmd.Process.Pid
	childJob := m.contain(pid)

	e.status = StatusRunning
	e.errText = ""
	e.pid = intPtr(pid)
	e.exitCode = nil
	e.startedAt = timePtr(time.Now())
	e.endedAt = nil
	e.stopRequested = false
	e.job = childJob
	e.coll = startCollector(e.id, logPath, pipeRead, m.log)
	e.exited = make(chan struct{})
	e.gen++

	go m.watch(e, e.gen, cmd, e.exited)

	event := "child started"
	if marker == markerRestart {
		event = "child restarted"
	}
	m.log.Log(opslog.Info, event, opslog.F("id", e.id), opslog.F("pid", pid))
}

// contain puts the process into its groups and returns the child group, or nil
// if there is not one.
//
// The server group goes first and the child group second, which is the reverse
// of the order written in 0008-L 2.3 and is what actually produces the nesting
// the document asks for. Windows makes the *second* job the child of the first,
// so assigning the child group first makes the server group an inner job of
// that one child group — and the next child, whose own group has no
// relationship to it, cannot then be assigned to the server group at all.
// Measured: with the document's order the first child is contained and every
// child after it is refused with ERROR_ACCESS_DENIED.
//
// Both properties the document requires survive the swap, and both were
// measured rather than argued: terminating a child group still takes the whole
// subtree, and closing the server group's handle still kills every child and
// grandchild, including those whose child-group handle was already released.
//
// It is also the safer order under failure. If the child group cannot be
// assigned, the process is already inside the server group — the layer that
// cannot be rebuilt after the fact — and only the per-child stop degrades to a
// tree walk (0008-L 2.3 rule 3, E-19).
func (m *Manager) contain(pid int) *jobs.Group {
	if !m.killOnExit {
		// The operator asked for the children to outlive the server, so they
		// are not put into any group at all — a group would make the exit kill
		// them anyway (0008-L 2.4.1).
		return nil
	}
	if server := m.serverGroup(); server != nil {
		if err := server.Assign(pid); err != nil {
			m.log.Once(opslog.Warn, "nested_job_unavailable", "",
				opslog.F("detail", "assign to the server group: "+err.Error()))
		} else if in, err := jobs.Contains(pid); err == nil && !in {
			// The assignment reported success and the process is in no group.
			// That should not happen, and it means the kernel backstop is not
			// covering this child, so it is worth a record.
			m.log.Once(opslog.Warn, "nested_job_unavailable", "",
				opslog.F("detail", fmt.Sprintf("process %d is not contained", pid)))
		}
	}
	if !m.childJobsUsable() {
		return nil
	}
	childJob, err := jobs.NewChild()
	if err != nil {
		m.downgradeChildJobs("create a child group: " + err.Error())
		return nil
	}
	if err := childJob.Assign(pid); err != nil {
		childJob.Close()
		m.downgradeChildJobs("assign to a child group: " + err.Error())
		return nil
	}
	return childJob
}

// serverGroup returns the process-wide group, creating it on first use.
func (m *Manager) serverGroup() *jobs.Group {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serverJob != nil || m.serverJobDone {
		return m.serverJob
	}
	job, err := jobs.NewServer()
	if err != nil {
		// Without it the graceful paths still clean up; only the kill-from-
		// outside path loses its backstop. Recording it once is the most that
		// can be done about it.
		m.serverJobDone = true
		m.log.Once(opslog.Warn, "nested_job_unavailable", "",
			opslog.F("detail", err))
		return nil
	}
	m.serverJob = job
	return job
}

func (m *Manager) childJobsUsable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.killOnExit && !m.noChildJobs
}

func (m *Manager) downgradeChildJobs(detail string) {
	m.mu.Lock()
	m.noChildJobs = true
	m.mu.Unlock()
	m.log.Once(opslog.Warn, "nested_job_unavailable", "", opslog.F("detail", detail))
}

// watch records how a run ended.
//
// gen guards against a stale run finishing after the entry has already been
// started again: a restart terminates the old process and starts a new one, and
// the old waiter can be scheduled after the new one has set the entry running.
// Without the check it would overwrite a live process's state with a dead one's
// exit code.
func (m *Manager) watch(e *entry, gen int, cmd *exec.Cmd, exited chan struct{}) {
	defer close(exited)
	cmd.Wait()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gen != gen {
		return
	}
	e.exitCode = intPtr(code)
	e.endedAt = timePtr(time.Now())
	if e.stopRequested {
		// stopChild owns the final status, and the group handle with it. It is
		// `stopped`, not `killed`: the process ended because it was asked to
		// (0008-L 2.3).
		return
	}
	if code == 0 {
		e.status = StatusExited
	} else {
		e.status = StatusKilled
	}
	// A run that ends on its own still has to give its group back. Nobody else
	// will: stopChild is the other place that closes one and it returns
	// immediately for an entry that is no longer running, so without this the
	// handle is dropped on the floor and the next start overwrites the field.
	// Closing a child group terminates nothing — that property is deliberately
	// only on the server group (0008-L 2.3 rule 2).
	if e.job != nil {
		e.job.Close()
		e.job = nil
	}
}

// --- Stopping ----------------------------------------------------------------------

// stopChild is 0008-L 2.3, in its order: terminate the group, wait, force, wait
// again, drain the collector, release the group handle.
//
// The collector comes after the wait, never before. Closing it first loses the
// last thing the child wrote, which is the error message that explains why it
// was worth stopping.
//
// The caller holds e.opMu; this does not take it, so that an operation which
// both stops and restarts holds one lock for the pair.
//
// deadline caps the whole operation; a zero deadline means the full budget,
// which is what a user-initiated stop gets. During shutdown it is the sequence's
// deadline, shared with every other child.
func (m *Manager) stopChild(e *entry, reason string, deadline time.Time) {
	e.mu.Lock()
	if e.status != StatusRunning {
		e.mu.Unlock()
		return
	}
	e.stopRequested = true
	job, coll, exited := e.job, e.coll, e.exited
	pid := 0
	if e.pid != nil {
		pid = *e.pid
	}
	e.mu.Unlock()

	if job != nil {
		job.Terminate()
	} else if pid != 0 {
		// Downgraded path: no group, so the tree is walked instead (E-19).
		jobs.TerminateTree(pid)
	}
	waited := waitFor(exited, budget(ChildTerminateTimeout, deadline))
	if !waited {
		if job != nil {
			job.Kill()
		} else if pid != 0 {
			jobs.TerminateTree(pid)
		}
		waited = waitFor(exited, budget(ChildTerminateForceTimeout, deadline))
	}

	if coll != nil {
		coll.stop(collectorDrainTimeout)
	}
	if job != nil {
		job.Close()
	}

	e.mu.Lock()
	e.job = nil
	if !waited {
		// Nothing observed the process end, so no exit code can be claimed.
		// Reporting the entry as stopped anyway is deliberate: the group has
		// been terminated twice and the server has nothing further to try, so
		// leaving it as running would be a state nobody can ever leave (E-18).
		e.exitCode = nil
		m.log.Log(opslog.Error, "child terminate timeout",
			opslog.F("id", e.id), opslog.F("pid", pid),
			opslog.F("waited_ms", (ChildTerminateTimeout+ChildTerminateForceTimeout).Milliseconds()))
	}
	e.status = StatusStopped
	if e.endedAt == nil {
		e.endedAt = timePtr(time.Now())
	}
	code := e.exitCode
	e.mu.Unlock()

	m.log.Log(opslog.Info, "child terminated",
		opslog.F("id", e.id), opslog.F("pid", pid),
		opslog.F("exit_code", codeValue(code)), opslog.F("reason", reason))
}

// budget is how long to wait: want, or whatever is left of deadline, whichever
// is smaller. A zero deadline means no cap.
func budget(want time.Duration, deadline time.Time) time.Duration {
	if deadline.IsZero() {
		return want
	}
	left := time.Until(deadline)
	if left < 0 {
		left = 0
	}
	if left < want {
		return left
	}
	return want
}

func waitFor(done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// codeValue renders an exit code for the operations log, where an unknown code
// is `null` rather than a number nobody measured (0008-L 2.13).
func codeValue(code *int) any {
	if code == nil {
		return nil
	}
	return *code
}

// --- helpers ------------------------------------------------------------------------

func (m *Manager) entry(id string) (*entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	return e, ok
}

// newID is `proc_` plus eight lowercase hex digits (0008-L 1.2).
func (m *Manager) newID() string {
	buf := make([]byte, IDHexDigits/2)
	for {
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand does not fail on any supported platform; if it ever
			// does, spinning is better than handing out a predictable id.
			continue
		}
		id := "proc_" + hex.EncodeToString(buf)
		m.mu.Lock()
		_, taken := m.entries[id]
		m.mu.Unlock()
		if !taken {
			return id
		}
	}
}

// normaliseCwd turns an empty working directory into "not set". The two mean
// the same thing to a user and only one of them can be checked.
func normaliseCwd(cwd *string) *string {
	if cwd == nil || *cwd == "" {
		return nil
	}
	value := *cwd
	return &value
}

func copyEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}
