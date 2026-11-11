package runner

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// noisy is a shell line that writes lines of about 40 bytes and exits.
func noisy(lines int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("for /l %%i in (1,1,%d) do @echo line-%%i-%s", lines, strings.Repeat("x", 24))
	}
	return fmt.Sprintf("for i in $(seq 1 %d); do echo line-$i-%s; done", lines, strings.Repeat("x", 24))
}

// The three stages of T5 meet here on output a process actually produced:
// the collector writes it, the threshold rotates it, and the reader reads it
// back. Everything else in this package works from a file written by the test.
func TestLiveOutputSurvivesRotationAndReadsBack(t *testing.T) {
	requireLiveSpawn(t)
	m, dir := testManager(t, nil)

	// A threshold a shell line can reach. The real one is 32 MiB.
	restore := logRotateBytes
	logRotateBytes = 4096
	t.Cleanup(func() { logRotateBytes = restore })

	const lines = 200
	info, _ := m.Register("noisy", noisy(lines), nil, nil)
	if info.Status == StatusFailed {
		t.Fatalf("the child did not start: %s", info.Error)
	}
	defer m.Stop(info.ID)
	waitStatus(t, m, info.ID, 60*time.Second, StatusExited, StatusKilled)

	// The child is gone; the collector finishes on its own once the pipe
	// reaches end of stream.
	last := fmt.Sprintf("line-%d-", lines)
	var view LogView
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		view, _ = m.ReadLog(info.ID, "")
		if strings.Contains(view.Text, last) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(view.Text, last) {
		t.Fatalf("the tail of the log does not reach %q:\n%s", last, view.Text)
	}

	// It rotated, and said so.
	if _, err := os.Stat(archiveName(m.LogPath(info.ID), 1)); err != nil {
		t.Fatalf("nothing was archived: %v", err)
	}
	if ops := opsLog(t, dir); !strings.Contains(ops, "child log rotated id="+info.ID) {
		t.Errorf("the rotation is not in the operations log:\n%s", ops)
	}
	if !strings.Contains(view.Text, "--- rotated ") {
		t.Error("the reader cannot see that the file was rotated")
	}

	// Nothing was lost across the rotation: every line the child wrote is in
	// one of the files, exactly once.
	whole := childLog(t, m, info.ID)
	for n := 1; n <= LogArchiveKeep; n++ {
		if b, err := os.ReadFile(archiveName(m.LogPath(info.ID), n)); err == nil {
			whole = string(b) + whole
		}
	}
	for _, n := range []int{1, lines / 2, lines} {
		want := fmt.Sprintf("line-%d-", n)
		if got := strings.Count(whole, want); got != 1 {
			t.Errorf("%q appears %d times across the log and its archives, want 1", want, got)
		}
	}

	// Plain ASCII output, read back as such.
	if view.Encoding != "utf-8" {
		t.Errorf("encoding %q, want utf-8", view.Encoding)
	}
	if view.Truncated || view.Reset {
		t.Errorf("truncated %v / reset %v on a tail read of a small file", view.Truncated, view.Reset)
	}
}

// A restart preserves the historical file and timestamp marker, but the
// console shows only the current run and hides the marker itself.
func TestLiveRestartMarkerIsReadable(t *testing.T) {
	requireLiveSpawn(t)
	m, _ := testManager(t, nil)

	info, _ := m.Register("talker", echoThen("first-run", 30*time.Second), nil, nil)
	defer m.Stop(info.ID)
	waitFor := func(text string) LogView {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		var view LogView
		for time.Now().Before(deadline) {
			view, _ = m.ReadLog(info.ID, "")
			if strings.Contains(view.Text, text) {
				return view
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%q never appeared in the log:\n%s", text, view.Text)
		return view
	}
	waitFor("first-run")

	old, _ := m.ReadLog(info.ID, "")
	m.Restart(info.ID)
	// The visible output must be empty until the new child produces output.
	// The same echo line is emitted by both runs, so wait for it to appear.
	view := waitFor("first-run")
	if view.Generation == old.Generation || view.Generation == "" {
		t.Errorf("restart did not change the console generation: %q", view.Generation)
	}
	if strings.Count(view.Text, "first-run") != 1 || strings.Contains(view.Text, "--- restart ") {
		t.Errorf("previous run or marker was not hidden from console: %q", view.Text)
	}
	if continued, _ := m.ReadLog(info.ID, offsetOf(old.NextOffset)); !continued.Reset {
		t.Error("old byte cursor did not reset after a restart")
	}
	// Audit history on disk remains intact and keeps the timestamped marker.
	full := childLog(t, m, info.ID)
	if strings.Count(full, "first-run") != 2 || !strings.Contains(full, "--- restart ") {
		t.Errorf("previous run or restart marker lost from file: %q", full)
	}
	marker := full[strings.Index(full, "--- restart "):]
	if !strings.Contains(marker[:min(len(marker), 45)], "T") {
		t.Errorf("the on-disk marker carries no timestamp: %q", firstRunes(marker, 45))
	}
}
