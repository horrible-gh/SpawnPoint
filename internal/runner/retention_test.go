package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuietChildRotatesAfterMidnightWithoutEmptyDailyFiles(t *testing.T) {
	c, _ := testCollector(t, 100000)
	c.write([]byte("day-one-output\n"))
	today := time.Now()
	yesterday := today.Add(-24 * time.Hour)
	c.mu.Lock()
	c.lastDataAt = yesterday
	c.mu.Unlock()
	c.maintain(today)
	archive := archiveName(c.path, 1)
	if got := readFile(t, archive); !strings.Contains(got, "day-one-output") {
		t.Fatal("quiet prior day output was not archived")
	}
	before := readFile(t, c.path)
	if !strings.Contains(before, "--- rotated ") {
		t.Fatal("missing rotation marker")
	}
	// Next silent day should not generate a second header-only archive.
	c.maintain(today.Add(24 * time.Hour))
	if _, err := os.Stat(archiveName(c.path, 2)); !os.IsNotExist(err) {
		t.Fatal("silent day made an empty archive")
	}
	c.write([]byte("new-output\n"))
	if !strings.Contains(readFile(t, c.path), "new-output\n") {
		t.Fatal("new output lost")
	}
}

func TestStoppedProcessCleanupKeepsRegistration(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusStopped)
	path := m.LogPath(id)
	if err := os.WriteFile(path, []byte("historical output"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archiveName(path, 1), []byte("archived"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(archiveName(path, 1), old, old); err != nil {
		t.Fatal(err)
	}
	m.MaintainLogs(time.Now())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("31-day stopped log was not removed: %v", err)
	}
	if _, err := os.Stat(archiveName(path, 1)); !os.IsNotExist(err) {
		t.Fatalf("31-day archive was not removed: %v", err)
	}
	if _, ok := m.Get(id); !ok {
		t.Fatal("log retention removed process registration")
	}
}

func TestStoppedLogCrossingMidnightArchivesWithoutEmptyCurrent(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusStopped)
	path := filepath.Join(m.logDir, id+".log")
	if err := os.WriteFile(path, []byte("yesterday\n"), 0600); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}
	m.MaintainLogs(time.Now())
	if got := readFile(t, archiveName(path, 1)); got != "yesterday\n" {
		t.Fatalf("archive %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stopped process created empty current log")
	}
}

func TestThirtyOneDayOldActiveLogPrunedWithoutStoppingCollector(t *testing.T) {
	c, _ := testCollector(t, 100000)
	c.write([]byte("31-day-old-output\n"))
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	c.mu.Lock()
	c.lastDataAt = old
	if err := os.Chtimes(c.path, old, old); err != nil {
		c.mu.Unlock()
		t.Fatal(err)
	}
	c.mu.Unlock()
	c.maintain(now)
	if _, err := os.Stat(archiveName(c.path, 1)); !os.IsNotExist(err) {
		t.Fatalf("old active content remains archived: %v", err)
	}
	if _, err := os.Stat(c.path); err != nil {
		t.Fatalf("active collector has no writable current file: %v", err)
	}
	c.write([]byte("live-after-prune\n"))
	newText := readFile(t, c.path)
	if strings.Contains(newText, "31-day-old-output") || !strings.Contains(newText, "live-after-prune") {
		t.Fatalf("active 30-day cleanup failed: %q", newText)
	}
}
