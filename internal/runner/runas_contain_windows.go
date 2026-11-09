//go:build windows

package runner

import (
	"errors"
	"fmt"
	"time"

	"spawnpoint/internal/jobs"
	"spawnpoint/internal/runas"
)

func (m *Manager) containIdentityChild(c *runas.Child) (*jobs.Group, error) {
	// Logon-created processes can start in a different job hierarchy from
	// ordinary children (for example when SpawnPoint itself is a CI job).
	// Never attach them to a shared, already-nested server job. Each launch
	// gets an empty outer job, then an independent per-command stop job.
	var outer *jobs.Group
	if m.killOnExit {
		var err error
		outer, err = jobs.NewServer()
		if err != nil {
			return nil, err
		}
		if err = outer.AssignHandle(c.Handle); err != nil {
			outer.Close()
			return nil, fmt.Errorf("assign execution lifetime job: %w", err)
		}
	}
	child, err := jobs.NewChild()
	if err == nil {
		err = child.AssignHandle(c.Handle)
	}
	if err != nil {
		if child != nil {
			child.Close()
		}
		if outer != nil {
			outer.Close()
		}
		return nil, fmt.Errorf("assign execution stop job: %w", err)
	}
	if outer != nil {
		m.mu.Lock()
		if m.serverJobDone {
			m.mu.Unlock()
			child.Close()
			outer.Close()
			return nil, errors.New("execution containment is shutting down")
		}
		if m.identityServerJobs == nil {
			m.identityServerJobs = make(map[*jobs.Group]struct{})
		}
		m.identityServerJobs[outer] = struct{}{}
		m.mu.Unlock()
		// Retain kill-on-close coverage even after the supervisor exits while
		// detached descendants remain. Reap only after the entire job is empty.
		go m.reapIdentityJob(outer)
	}
	return child, nil
}

func (m *Manager) reapIdentityJob(job *jobs.Group) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		_, retained := m.identityServerJobs[job]
		m.mu.Unlock()
		if !retained {
			return
		}
		active, err := job.ActiveProcesses()
		if err != nil || active != 0 {
			// On query failure keep the kernel backstop until server shutdown.
			continue
		}
		m.mu.Lock()
		delete(m.identityServerJobs, job)
		m.mu.Unlock()
		job.Close()
		return
	}
}
