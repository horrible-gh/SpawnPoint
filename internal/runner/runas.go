package runner

import (
	"errors"
	"path/filepath"
	"time"

	"spawnpoint/internal/opslog"
	"spawnpoint/internal/runas"
	"spawnpoint/internal/store"
)

// SetCredentialDirectory must be called before Restore or serving requests.
// The key stays separate from the SQLite file; it must be backed up with it.
func (m *Manager) SetCredentialDirectory(path string) { m.credentialDir = path }
func (m *Manager) credentialDirectory() string {
	if m.credentialDir != "" {
		return m.credentialDir
	}
	return filepath.Join(m.logDir, ".credentials")
}

func (m *Manager) RegisterIdentity(label, command string, cwd *string, env map[string]string, input runas.Input, start bool, autoStart ...bool) (Info, bool, error) {
	spec, err := runas.Apply(m.credentialDirectory(), input, nil)
	if err != nil {
		return Info{}, false, err
	}
	info, persisted := m.registerConfigured(label, command, cwd, env, start, spec, autoStart...)
	return info, persisted, nil
}

// UpdateIdentity writes a complete configuration before publishing it in memory.
// On a storage error the old registration/credential remains intact and no
// restart can accidentally use a partially updated account.
func (m *Manager) UpdateIdentity(id string, label, command *string, cwd **string, env map[string]string, input *runas.Input, autoStart ...bool) (Info, bool, bool, error) {
	e, ok := m.entry(id)
	if !ok {
		return Info{}, false, false, nil
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	spec := runas.Clone(e.runAs)
	var err error
	if input != nil {
		spec, err = runas.Apply(m.credentialDirectory(), *input, spec)
		if err != nil {
			return e.infoLocked(), false, true, err
		}
	}
	row := store.RunnerEntry{ID: e.id, Label: e.label, Cmd: e.cmd, Cwd: normaliseCwd(e.cwd), Env: copyEnv(e.env), AutoStart: e.autoStart, RunAsJSON: spec.JSON(), CreatedAt: e.createdAt, UpdatedAt: time.Now()}
	if label != nil {
		row.Label = *label
	}
	if command != nil {
		row.Cmd = *command
	}
	if cwd != nil {
		row.Cwd = normaliseCwd(*cwd)
	}
	if env != nil {
		row.Env = copyEnv(env)
	}
	if len(autoStart) > 0 {
		row.AutoStart = autoStart[0]
	}
	if m.registry == nil {
		return e.infoLocked(), false, true, errors.New("cannot save execution identity without persistent storage")
	}
	if err = m.registry.SaveEntry(row); err != nil {
		return e.infoLocked(), false, true, errors.New("configuration could not be saved; the previous settings are unchanged")
	}
	e.label = row.Label
	e.cmd = row.Cmd
	e.cwd = row.Cwd
	e.env = row.Env
	e.autoStart = row.AutoStart
	e.runAs = spec
	e.updatedAt = row.UpdatedAt
	return e.infoLocked(), true, true, nil
}

func (m *Manager) spawnAs(e *entry, marker string) {
	// m.spawn already wrote the marker and established the new console window.
	logPath := m.LogPath(e.id)
	fail := func(err error) {
		e.markFailed(err.Error(), time.Now())
		m.log.Log(opslog.Error, "child start failed", opslog.F("id", e.id), opslog.F("detail", err))
	}
	cwd := ""
	if e.cwd != nil {
		cwd = *e.cwd
	}
	child, err := runas.Launch(m.credentialDirectory(), e.runAs, e.cmd, cwd, e.env)
	if err != nil {
		fail(err)
		return
	}
	job, err := m.containIdentityChild(child)
	if err != nil {
		child.Abort()
		fail(err)
		return
	}
	if err = child.Ready(); err != nil {
		child.Abort()
		if job != nil {
			job.Kill()
			job.Close()
		}
		fail(err)
		return
	}
	e.status = StatusRunning
	e.errText = ""
	e.pid = intPtr(child.PID)
	e.exitCode = nil
	e.startedAt = timePtr(time.Now())
	e.endedAt = nil
	e.stopRequested = false
	e.job = job
	e.coll = startCollector(e.id, logPath, child.Output, m.log)
	e.exited = make(chan struct{})
	e.gen++
	gen, exited := e.gen, e.exited
	go func() { defer close(exited); m.finishRun(e, gen, child.Wait()) }()
	event := "child started"
	if marker == markerRestart {
		event = "child restarted"
	}
	m.log.Log(opslog.Info, event, opslog.F("id", e.id), opslog.F("pid", child.PID))
}
