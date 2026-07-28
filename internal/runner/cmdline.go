// Package runner assembles the command line that SpawnPoint hands to the
// operating system shell.
//
// The single hard requirement is byte identity with the current Python
// implementation (spawnpoint/runner.py `_spawn`, which relies on
// subprocess.Popen(shell=True)). The registered commands contain nested,
// unescaped double quotes that happen to survive cmd.exe's /c quote rules;
// any re-quoting breaks all of them (0004-NR 3.5, 0006-D 3.4, 0008-L 2.1).
//
// The four absolute rules from 0008-L 2.1 are implemented here and in the
// platform files next to this one:
//
//  1. `inner` is never escaped — the user's quotes stay nested as typed.
//  2. The execution layer receives a finished command-line string, never an
//     argument vector (which would be auto-quoted).
//  3. The child is started with window hidden, new process group, stdin closed.
//  4. The UTF-8 prefix separator is `&` (plain sequencing), not `&&`.
package runner

import (
	"os"
	"runtime"
)

// UTF8Prefix is prepended to every user command on Windows so the child console
// runs in code page 65001. The separator is `&` and not `&&` on purpose: chcp
// may fail and the user command must still run (0008-L 1.2, 0004-NR 3.2).
const UTF8Prefix = "chcp 65001 >nul & "

// posixShellPath is the shell used on non-Windows platforms. POSIX has no code
// page concept, so no prefix is added there (0008-L 2.1).
const posixShellPath = "/bin/sh"

// shellFallbackSuffix is appended to %SystemRoot% when %ComSpec% is unset. The
// fallback order matches the current implementation.
const shellFallbackSuffix = `\System32\cmd.exe`

// ResolveShellPath returns the Windows shell to delegate to: %ComSpec% when
// set, otherwise %SystemRoot%\System32\cmd.exe (0008-L 2.1).
func ResolveShellPath() string {
	if path := os.Getenv("ComSpec"); path != "" {
		return path
	}
	return os.Getenv("SystemRoot") + shellFallbackSuffix
}

// WindowsShellCommandLine builds the exact lpCommandLine string passed to
// CreateProcess on Windows. userCmd is inserted verbatim — no escaping, no
// quote normalisation, no splitting.
//
// It takes shell as a parameter rather than calling ResolveShellPath so the
// contract test can pin the measured value and run on any platform.
func WindowsShellCommandLine(shell, userCmd string) string {
	return shell + ` /c "` + UTF8Prefix + userCmd + `"`
}

// POSIXArgv returns the argument vector used on non-Windows platforms. The
// user command still reaches a shell unparsed, as the third element.
func POSIXArgv(userCmd string) []string {
	return []string{posixShellPath, "-c", userCmd}
}

// BuildCommandLine returns the finished command line for the current platform.
// ok is false on POSIX, where the argv path (POSIXArgv) is used instead — this
// mirrors `build_command_line` returning null there (0008-L 2.1).
func BuildCommandLine(userCmd string) (line string, ok bool) {
	if runtime.GOOS != "windows" {
		return "", false
	}
	return WindowsShellCommandLine(ResolveShellPath(), userCmd), true
}
