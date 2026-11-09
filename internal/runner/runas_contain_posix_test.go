//go:build !windows

package runner

import (
	"testing"

	"spawnpoint/internal/runas"
)

func TestIdentityKeepsExplicitStopGroupWhenExitCleanupDisabled(t *testing.T) {
	m := &Manager{killOnExit: false}
	group, err := m.containIdentityChild(&runas.Child{PID: 2147483647})
	if err != nil || group == nil {
		t.Fatalf("explicit stop group unavailable: %v", err)
	}
	if m.serverJob != nil {
		t.Fatal("keep-alive mode created a server kill group")
	}
	// Close of a child group releases metadata without signalling its PID.
	if err := group.Close(); err != nil {
		t.Fatal(err)
	}
}
