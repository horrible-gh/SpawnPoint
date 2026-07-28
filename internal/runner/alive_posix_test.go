//go:build !windows

package runner

import (
	"os"
	"syscall"
)

// processAlive reports whether the process is still running. Signal 0 performs
// the existence and permission checks without delivering anything.
func processAlive(p *os.Process) bool {
	return p.Signal(syscall.Signal(0)) == nil
}
