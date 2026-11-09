//go:build !windows

package runner

import (
	"spawnpoint/internal/jobs"
	"spawnpoint/internal/runas"
)

func (m *Manager) containIdentityChild(c *runas.Child) (*jobs.Group, error) {
	if m.killOnExit {
		return m.contain(c.PID), nil
	}
	// Keeping commands alive on server exit does not disable explicit Stop.
	// su must receive SIGTERM and forward it to the authenticated supervisor;
	// the generic no-group fallback sends SIGKILL and cannot forward cleanup.
	child, err := jobs.NewChild()
	if err != nil {
		return nil, err
	}
	if err = child.Assign(c.PID); err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}
