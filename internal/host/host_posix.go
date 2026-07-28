//go:build !windows

package host

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"spawnpoint/internal/lifecycle"
)

// Run drives srv in the foreground, translating termination signals into the
// shutdown sequence.
//
// The mode is always Console here. SpawnPoint is deployed as a Windows service
// (0006-D 2.3) and no init system integration is in scope; this path exists so
// the sequences can be developed and tested on other platforms.
func Run(srv Server) (Mode, int) {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range signals {
			reason, budget := SignalEvent(s)
			go srv.Shutdown(reason, budget)
		}
	}()
	defer signal.Stop(signals)

	code, ok := srv.Startup()
	if !ok {
		return Console, code
	}
	return Console, srv.Serve()
}

// SignalEvent maps a signal to the reason and budget of 0008-L 2.4.2. SIGHUP is
// the terminal going away, which is the POSIX counterpart of the console window
// being closed, so it gets the reduced budget.
func SignalEvent(s os.Signal) (reason string, budget time.Duration) {
	switch s {
	case syscall.SIGINT:
		return lifecycle.ReasonConsoleCtrl, lifecycle.TotalBudget
	case syscall.SIGHUP:
		return lifecycle.ReasonConsoleClose, lifecycle.ConsoleCloseBudget
	default:
		return lifecycle.ReasonSignal, lifecycle.TotalBudget
	}
}
