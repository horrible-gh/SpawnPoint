//go:build !windows

package runner

import (
	"spawnpoint/internal/jobs"
	"spawnpoint/internal/runas"
)

func (m *Manager) containIdentityChild(c *runas.Child) (*jobs.Group, error) {
	return m.contain(c.PID), nil
}
