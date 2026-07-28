//go:build windows

package runner

import (
	"os/exec"
	"syscall"
)

// ShellCommand prepares the child process exactly as 0008-L 2.1 requires.
//
// Path is set to the shell and SysProcAttr.CmdLine carries the finished string,
// which bypasses os/exec's automatic argument quoting. Going through
// exec.Command(shell, "/c", inner) instead would run every argument through
// syscall.EscapeArg and turn the nested quotes into `\"`, which cmd.exe reads
// as a literal backslash — all five registered commands would fail to launch
// (0004-NR 3.5).
//
// The three explicit process properties (rule 3) are: HideWindow reproduces the
// STARTF_USESHOWWINDOW|SW_HIDE that Python's shell=True path adds implicitly,
// CREATE_NEW_PROCESS_GROUP matches the current creationflags, and stdin stays
// nil so os/exec attaches the null device (Python's DEVNULL).
func ShellCommand(userCmd string) *exec.Cmd {
	shell := ResolveShellPath()
	return &exec.Cmd{
		Path: shell,
		Args: []string{shell},
		SysProcAttr: &syscall.SysProcAttr{
			CmdLine:       WindowsShellCommandLine(shell, userCmd),
			HideWindow:    true,
			CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		},
	}
}

// naiveShellCommandLine reproduces what os/exec would have built from an
// argument vector. It exists only so the contract test can assert that the
// auto-quoting path really does differ from the required output.
func naiveShellCommandLine(shell, userCmd string) string {
	return syscall.EscapeArg(shell) + " " + syscall.EscapeArg("/c") + " " +
		syscall.EscapeArg(UTF8Prefix+userCmd)
}
