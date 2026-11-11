package opslog

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestOperationsRotateEvenAtLowVolumeAfterMidnight(t *testing.T) {
	dir := t.TempDir()
	log, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	now := time.Now()
	log.now = func() time.Time { return now.Add(-24 * time.Hour) }
	log.Log(Info, "day-one")
	log.MaintainLogs(now)
	b, err := os.ReadFile(archiveName(log.Path(), 1))
	if err != nil || !strings.Contains(string(b), "day-one") {
		t.Fatalf("missing archived output %q (%v)", b, err)
	}
	// An idle server with an empty current file does not rotate again.
	log.MaintainLogs(now.Add(24 * time.Hour))
	if _, err := os.Stat(archiveName(log.Path(), 2)); !os.IsNotExist(err) {
		t.Fatal("empty daily archive created")
	}
	log.now = func() time.Time { return now }
	log.Log(Info, "day-two")
	b, err = os.ReadFile(log.Path())
	if err != nil || !strings.Contains(string(b), "day-two") {
		t.Fatalf("new output missing %q: %v", b, err)
	}
}

func TestOperationsPruneArchivedFilesOlderThan30Days(t *testing.T) {
	dir := t.TempDir()
	log, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	log.Log(Info, "keep-current")
	archive := archiveName(log.Path(), 1)
	if err := os.WriteFile(archive, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(archive, old, old); err != nil {
		t.Fatal(err)
	}
	log.MaintainLogs(time.Now())
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("expired archive exists: %v", err)
	}
	if _, err := os.Stat(log.Path()); err != nil {
		t.Fatalf("active operations log removed: %v", err)
	}
}

func TestThirtyOneDayOldQuietOperationsLogExpires(t *testing.T) {
	dir := t.TempDir()
	log, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	log.now = func() time.Time { return old }
	log.Log(Info, "old-quiet-log")
	if err := os.Chtimes(log.Path(), old, old); err != nil {
		t.Fatal(err)
	}
	log.MaintainLogs(now)
	if _, err := os.Stat(archiveName(log.Path(), 1)); !os.IsNotExist(err) {
		t.Fatalf("31d old operations archive not deleted: %v", err)
	}
	log.now = func() time.Time { return now }
	log.Log(Info, "new-service-event")
	data, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "old-quiet-log") || !strings.Contains(string(data), "new-service-event") {
		t.Fatalf("incorrect kept operations output: %q", data)
	}
}
