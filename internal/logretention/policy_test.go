package logretention

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDifferentDayUsesCalendarDate(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 11, 10, 23, 59, 59, 0, loc)
	after := before.Add(2 * time.Second)
	if !DifferentDay(before, after) {
		t.Fatal("midnight was ignored")
	}
	if DifferentDay(before, before.Add(500*time.Millisecond)) {
		t.Fatal("same day rotated")
	}
	if DifferentDay(time.Time{}, after) {
		t.Fatal("no data rotated")
	}
}

func TestTenGenerationsAndAgePruning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc_12345678.log")
	for i := 0; i < ArchiveKeep+3; i++ {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("generation %d", i)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ShiftCurrent(path, ArchiveKeep); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= ArchiveKeep; i++ {
		content, err := os.ReadFile(fmt.Sprintf("%s.%d", path, i))
		if err != nil {
			t.Fatal(err)
		}
		expected := fmt.Sprintf("generation %d", ArchiveKeep+3-i)
		if string(content) != expected {
			t.Fatalf("generation %d = %q, want %q", i, content, expected)
		}
	}
	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, ArchiveKeep+1)); !os.IsNotExist(err) {
		t.Fatalf("too many generations: %v", err)
	}
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	recent := now.Add(-29 * 24 * time.Hour)
	if err := os.Chtimes(path+".3", old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path+".4", recent, recent); err != nil {
		t.Fatal(err)
	}
	Prune(path, ArchiveKeep, now)
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("31d archive remains: %v", err)
	}
	if _, err := os.Stat(path + ".4"); err != nil {
		t.Fatalf("29d archive removed: %v", err)
	}
}

func TestInterruptedRotationRecoversWithoutLostOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.log")
	if err := os.WriteFile(path, []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".10", []byte("oldest"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".rotating", []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ShiftCurrent(path, ArchiveKeep); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path + ".1")
	if err != nil || string(b) != "current" {
		t.Fatalf("current was not archived: %q: %v", b, err)
	}
	b, err = os.ReadFile(path + ".2")
	if err != nil || string(b) != "interrupted" {
		t.Fatalf("pending archive lost: %q: %v", b, err)
	}
	if _, err := os.Stat(path + ".rotating"); !os.IsNotExist(err) {
		t.Fatal("pending archive was not recovered")
	}
}

func TestBlockedCurrentRenameKeepsOldest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.log")
	if err := os.WriteFile(path, []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".10", []byte("oldest"), 0600); err != nil {
		t.Fatal(err)
	}
	// Pre-existing directory at the pending path prevents any rename.
	if err := os.Mkdir(path+".rotating", 0700); err != nil {
		t.Fatal(err)
	}
	if err := ShiftCurrent(path, ArchiveKeep); err == nil {
		t.Fatal("unexpected rotation over directory")
	}
	if b, err := os.ReadFile(path + ".10"); err != nil || string(b) != "oldest" {
		t.Fatal("oldest lost on blocked rotate")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "current" {
		t.Fatal("current log lost on blocked rotate")
	}
}
