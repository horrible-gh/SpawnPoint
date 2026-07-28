//go:build windows

package runner

import (
	"strings"
	"syscall"
	"testing"
)

// TestAutoQuotingPathWouldBreak proves the bypass is load-bearing: had we passed
// an argument vector to os/exec, syscall.EscapeArg would have rewritten the
// nested quotes as `\"` and cmd.exe would have read the backslashes literally
// (0004-NR 3.5). If this test ever stops failing to differ, the escape hazard
// changed and 0008-L 2.1 rule 2 needs revisiting.
func TestAutoQuotingPathWouldBreak(t *testing.T) {
	for _, entry := range loadFixture(t) {
		t.Run(entry.ID, func(t *testing.T) {
			ours := WindowsShellCommandLine(measuredShell, entry.Cmd)
			naive := naiveShellCommandLine(measuredShell, entry.Cmd)
			if naive == ours {
				t.Fatal("the auto-quoting path produced the same string; escape hazard assumption is stale")
			}
			if !strings.Contains(naive, `\"`) {
				t.Fatalf("expected the auto-quoting path to introduce backslash-quote, got %q", naive)
			}
		})
	}
}

// TestShellCommandProcessAttributes is absolute rule 2 and 3 of 0008-L 2.1: the
// finished string is handed over as SysProcAttr.CmdLine, and window hiding,
// process group and closed stdin are stated explicitly rather than inherited
// from a runtime's implicit behaviour.
func TestShellCommandProcessAttributes(t *testing.T) {
	t.Setenv("ComSpec", measuredShell)
	userCmd := `powershell -File "C:\tmp\x.ps1"`

	cmd := ShellCommand(userCmd)

	if cmd.Path != measuredShell {
		t.Fatalf("Path = %q, want the shell %q", cmd.Path, measuredShell)
	}
	attr := cmd.SysProcAttr
	if attr == nil {
		t.Fatal("SysProcAttr is nil; os/exec would auto-quote the arguments")
	}
	if want := WindowsShellCommandLine(measuredShell, userCmd); attr.CmdLine != want {
		t.Fatalf("CmdLine = %q, want %q", attr.CmdLine, want)
	}
	if !attr.HideWindow {
		t.Error("HideWindow is false; the child would flash a console window")
	}
	if attr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("CREATE_NEW_PROCESS_GROUP is not set")
	}
	if cmd.Stdin != nil {
		t.Error("Stdin is set; it must stay nil so os/exec attaches the null device")
	}
}
