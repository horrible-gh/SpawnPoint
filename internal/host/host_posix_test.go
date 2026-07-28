//go:build !windows

package host

import (
	"syscall"
	"testing"

	"spawnpoint/internal/lifecycle"
)

// 0008-L 2.4.2: each entry signal maps to one reason and one budget.
//
// This is the POSIX half of the exit-path measurement of 0008-L 6.4. Path (b),
// a termination signal, has no Windows counterpart that can be delivered
// without registering a service, so this is where the mapping it depends on is
// pinned.
func TestSignalEventMapping(t *testing.T) {
	for _, tc := range []struct {
		signal syscall.Signal
		reason string
		budget interface{ String() string }
	}{
		{syscall.SIGINT, lifecycle.ReasonConsoleCtrl, lifecycle.TotalBudget},
		{syscall.SIGTERM, lifecycle.ReasonSignal, lifecycle.TotalBudget},
		// SIGHUP is the terminal going away, which is the POSIX counterpart of
		// a console window being closed, so it gets the reduced budget.
		{syscall.SIGHUP, lifecycle.ReasonConsoleClose, lifecycle.ConsoleCloseBudget},
	} {
		reason, budget := SignalEvent(tc.signal)
		if reason != tc.reason {
			t.Errorf("%v: reason = %q, want %q", tc.signal, reason, tc.reason)
		}
		if budget.String() != tc.budget.String() {
			t.Errorf("%v: budget = %v, want %v", tc.signal, budget, tc.budget)
		}
	}
}
