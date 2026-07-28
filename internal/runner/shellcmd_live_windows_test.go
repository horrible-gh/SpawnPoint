//go:build windows

package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// probeScript reports the command line cmd.exe actually built for it.
// [Environment]::CommandLine is GetCommandLineW() for the powershell process, so
// this is the same value 0004-NR 3.2 read out of the live child PID.
const probeScript = "[Console]::Out.Write([Environment]::CommandLine)\r\n"

// TestLiveChildCommandLine is 0008-L 6.1 steps 3-4. It starts one shell and
// inspects what the shell handed to its own child.
//
// It is opt-in (SPAWNPOINT_LIVE_SPAWN=1) because it launches a real process. It
// launches a throwaway probe script, never one of the registered commands —
// those start production servers, and 0004-NR 1.5.1 warns that touching the
// live instance takes every managed child down with it.
func TestLiveChildCommandLine(t *testing.T) {
	if os.Getenv("SPAWNPOINT_LIVE_SPAWN") != "1" {
		t.Skip("set SPAWNPOINT_LIVE_SPAWN=1 to run the live spawn check")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "probe.ps1")
	if err := os.WriteFile(script, []byte(probeScript), 0o600); err != nil {
		t.Fatalf("write probe script: %v", err)
	}

	// Same shape as the registered commands: a quoted path, quotes nested inside
	// the shell's own quoting.
	userCmd := `powershell -ExecutionPolicy RemoteSigned -File "` + script + `"`

	cmd := ShellCommand(userCmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("probe exited with %v; output=%q", err, out.String())
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("probe did not finish within 60s")
	}

	childLine := strings.TrimSpace(out.String())
	t.Logf("child command line: %q", childLine)

	// Judgement 1: two spaces after `powershell` — the gap the outer quote left
	// when cmd.exe removed it. This is the measured criterion (0007-P 0.7).
	if !strings.Contains(childLine, "powershell  -ExecutionPolicy") {
		t.Errorf("expected two spaces after `powershell`, got %q", childLine)
	}
	// Judgement 2: the inner quotes around the path survived to the child.
	if !strings.Contains(childLine, `-File "`+script+`"`) {
		t.Errorf("inner quotes around the script path did not survive: %q", childLine)
	}
	// Judgement 3: no backslash-quote escape anywhere.
	if strings.Contains(childLine, `\"`) {
		t.Errorf("backslash-quote escape reached the child: %q", childLine)
	}
}

// TestLiveAutoQuotingPathBreaks is the negative control for the test above. It
// runs the same probe through os/exec's ordinary argument-vector path and shows
// the child does not come out intact — the concrete failure 0004-NR 3.5
// predicted for a straight port. Without this, "the strings differ" is only an
// argument that the difference matters.
func TestLiveAutoQuotingPathBreaks(t *testing.T) {
	if os.Getenv("SPAWNPOINT_LIVE_SPAWN") != "1" {
		t.Skip("set SPAWNPOINT_LIVE_SPAWN=1 to run the live spawn check")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "probe.ps1")
	if err := os.WriteFile(script, []byte(probeScript), 0o600); err != nil {
		t.Fatalf("write probe script: %v", err)
	}
	userCmd := `powershell -ExecutionPolicy RemoteSigned -File "` + script + `"`

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ResolveShellPath(), "/c", UTF8Prefix+userCmd)
	// Exit status is ignored on purpose: the point is what the child received,
	// and a mangled argument may or may not make powershell exit non-zero.
	out, _ := cmd.CombinedOutput()
	childLine := strings.TrimSpace(string(out))
	t.Logf("auto-quoted child output: %q", childLine)

	if strings.Contains(childLine, `-File "`+script+`"`) {
		t.Fatalf("the auto-quoting path delivered an intact path; 0008-L 2.1 rule 2 may no longer be needed: %q", childLine)
	}
}
