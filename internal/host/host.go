// Package host attaches the server to whatever is supervising the process.
//
// 0008-L 2.14 fixes how the mode is chosen: the executable asks the operating
// system whether it is connected to the service control channel and decides for
// itself. There is no `--service` flag and none may be added — the configuration
// contract is that environment variables are the only input (0006-D 2.1).
//
// The reason this package exists at all is 0004-NR 1.4: the current server dies
// with the terminal it was launched from and leaves no trace. Two things fix
// that. The service path keeps the process alive independently of any terminal.
// The console path registers the close and logoff events explicitly — the
// Python implementation handles only interrupt and break, so closing the window
// skips its cleanup entirely (0008-L 2.4.3) — and gives them the reduced budget
// the operating system actually allows.
package host

import "time"

// Server is the part of the lifecycle this package drives. lifecycle.Server
// implements it.
type Server interface {
	// Startup runs the startup sequence. When ok is false the process must
	// exit with the returned code; the reason is already in the log.
	Startup() (exitCode int, ok bool)
	// Serve blocks until a shutdown completes and returns the exit code.
	Serve() int
	// Shutdown runs the cleanup sequence and returns the exit code. It is
	// idempotent and safe to call from an operating system callback.
	Shutdown(reason string, budget time.Duration) int
}

// Mode reports how the process turned out to be supervised.
type Mode int

const (
	// Console means the process was started by a person or a script and is
	// tied to a terminal.
	Console Mode = iota
	// Service means the process is running under the operating system's
	// service control manager.
	Service
)

func (m Mode) String() string {
	if m == Service {
		return "service"
	}
	return "console"
}
