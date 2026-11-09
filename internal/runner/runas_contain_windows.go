//go:build windows

package runner

import (
	"errors"
	"spawnpoint/internal/jobs"
	"spawnpoint/internal/runas"
)

func (m *Manager) containIdentityChild(c *runas.Child) (*jobs.Group, error) {
	// Cross-user processes are assigned using the native creator handle, not
	// reopened by PID under the service account. Assignment precedes ResumeThread.
	if m.killOnExit {
		server := m.serverGroup()
		if server == nil {
			return nil, errors.New("cannot create execution containment")
		}
		if err := server.AssignHandle(c.Handle); err != nil {
			return nil, err
		}
	}
	child, err := jobs.NewChild()
	if err != nil {
		return nil, err
	}
	if err = child.AssignHandle(c.Handle); err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}
