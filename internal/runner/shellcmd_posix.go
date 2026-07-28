//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// ShellCommand prepares the child process on POSIX. The user command is handed
// to /bin/sh -c unparsed, with no code page prefix, and the child gets its own
// session so a stop request can reach its descendants (0008-L 2.1).
//
// stdin stays nil so os/exec attaches /dev/null, matching Python's DEVNULL.
func ShellCommand(userCmd string) *exec.Cmd {
	argv := POSIXArgv(userCmd)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
