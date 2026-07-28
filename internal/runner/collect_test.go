package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"spawnpoint/internal/opslog"
)

// Rotation is tested through a collector built by hand rather than through a
// live child. What matters is which bytes end up in which file, and driving
// that with 32 MiB of real output would take a hundred megabytes of disk to
// prove something the threshold already decides.

// attachCollector opens path and returns a collector writing to it at the given
// threshold. Whatever file the collector holds at the end — the original or one
// it rotated to — is closed on cleanup, or the temporary directory cannot be
// removed on Windows.
func attachCollector(t *testing.T, id, path string, log *opslog.Logger, rotateBytes int64) *collector {
	t.Helper()
	file, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{
		id: id, path: path, log: log, file: file,
		rotateBytes: rotateBytes,
		keep:        LogArchiveKeep,
		done:        make(chan struct{}),
	}
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.file != nil {
			c.file.Close()
			c.file = nil
		}
		file.Close()
	})
	return c
}

// testCollector is attachCollector plus a log directory of its own.
func testCollector(t *testing.T, rotateBytes int64) (*collector, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := opslog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	id := "proc_00000001"
	return attachCollector(t, id, filepath.Join(dir, id+".log"), log, rotateBytes), dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// 0008-L 2.5: at the threshold the file is handed aside and a new one is
// started with a header saying so.
func TestChildLogRotatesAtTheThreshold(t *testing.T) {
	c, _ := testCollector(t, 100)

	c.write(bytes.Repeat([]byte("a"), 60))
	if _, err := os.Stat(archiveName(c.path, 1)); err == nil {
		t.Fatal("the log was rotated before the threshold")
	}
	c.write(bytes.Repeat([]byte("b"), 60)) // 60 + 60 >= 100

	archived := readFile(t, archiveName(c.path, 1))
	if archived != strings.Repeat("a", 60) {
		t.Errorf("the archive holds %d bytes, want the 60 written before the threshold", len(archived))
	}

	current := readFile(t, c.path)
	if !strings.HasPrefix(current, "--- rotated ") {
		t.Fatalf("the new file starts %q, want a rotation header", firstRunes(current, 20))
	}
	// The chunk that crossed the threshold is written whole into the new file,
	// never split across the two.
	if !strings.HasSuffix(current, strings.Repeat("b", 60)) {
		t.Error("the chunk that triggered the rotation is not whole in the new file")
	}
	if int64(len(current)) != c.size {
		t.Errorf("tracked size %d, file %d bytes", c.size, len(current))
	}
}

// The header carries what was archived, not the threshold. The pseudocode in
// 2.5 writes `child_log_rotate_bytes`, which is a number the archived file
// never actually has: rotation happens before the chunk that would cross it.
func TestRotationHeaderReportsWhatWasArchived(t *testing.T) {
	c, _ := testCollector(t, 100)
	c.write(bytes.Repeat([]byte("a"), 60))
	c.write(bytes.Repeat([]byte("b"), 60))

	header := strings.SplitN(readFile(t, c.path), "\n", 2)[0]
	if !strings.Contains(header, " (previous 60 bytes archived)") {
		t.Errorf("header %q does not say how much was archived", header)
	}
	// The timestamp is the one every other record uses, so a person can line
	// the rotation up against the operations log (0008-L 1.6).
	if !strings.HasPrefix(header, "--- rotated 20") || !strings.HasSuffix(header, " ---") {
		t.Errorf("header %q is not in the shape 2.5 fixes", header)
	}
}

// The archives shift and the oldest goes. `child_log_archive_keep` is what
// bounds one entry's disk to `child_log_max_total_bytes`.
func TestRotationKeepsTheStatedNumberOfArchives(t *testing.T) {
	c, dir := testCollector(t, 100)

	for round := 0; round < LogArchiveKeep+2; round++ {
		c.write(bytes.Repeat([]byte(strconv.Itoa(round)), 99))
	}

	for n := 1; n <= LogArchiveKeep; n++ {
		if _, err := os.Stat(archiveName(c.path, n)); err != nil {
			t.Errorf("archive %d is missing: %v", n, err)
		}
	}
	if _, err := os.Stat(archiveName(c.path, LogArchiveKeep+1)); err == nil {
		t.Errorf("archive %d exists, more than the %d that are kept", LogArchiveKeep+1, LogArchiveKeep)
	}
	// Newest first: .1 holds the round before the current file.
	if got := readFile(t, archiveName(c.path, 1)); !strings.Contains(got, "3333") {
		t.Errorf("archive 1 holds %q, want the most recent round", firstRunes(got, 60))
	}

	ops := opsLog(t, dir)
	if got := strings.Count(ops, "child log rotated"); got != LogArchiveKeep+1 {
		t.Errorf("the operations log records %d rotations, want %d", got, LogArchiveKeep+1)
	}
	if !strings.Contains(ops, "child log rotated id=proc_00000001 keep=3") {
		t.Errorf("the rotation record is not in the shape 2.5 fixes:\n%s", ops)
	}
}

// A rotation is not a reason to lose the reader's place: the file it is reading
// keeps its name, and the header is what tells the reader the file underneath
// it changed.
func TestRotationLeavesTheLogReadable(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusRunning)
	c := attachCollector(t, id, m.LogPath(id), m.log, 100)

	c.write([]byte(strings.Repeat("old ", 15))) // 60 bytes
	c.write([]byte(strings.Repeat("new ", 12) + "output\n"))

	got, ok := m.ReadLog(id, "")
	if !ok {
		t.Fatal("the entry was reported as unknown")
	}
	if !strings.Contains(got.Text, "--- rotated ") {
		t.Error("the reader cannot see that the file was rotated")
	}
	if !strings.HasSuffix(got.Text, "new output\n") {
		t.Errorf("the reader sees %q, want the output written after the rotation", got.Text)
	}
	if got.Reset {
		// E-9: reset has one cause and this is not it. The header exists
		// precisely because reset does not fire here.
		t.Error("reset is true although the offset was not past the end")
	}
}

// A reader holding an offset from before the rotation asks for a position the
// new file does not reach, which is the one thing that raises reset
// (0007-P [로그 넘겨 쓰기 감지]).
func TestReaderFromBeforeARotationIsToldToStartOver(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusRunning)
	c := attachCollector(t, id, m.LogPath(id), m.log, 100)

	// Fill to just under the threshold, then read: the offset the reader takes
	// away is larger than the file a rotation leaves behind.
	c.write(bytes.Repeat([]byte("o"), 99))
	before, _ := m.ReadLog(id, "0")
	c.write([]byte("x"))

	after, _ := m.ReadLog(id, offsetOf(before.NextOffset))
	if !after.Reset {
		t.Errorf("reset is false for an offset of %d against a file of %d",
			before.NextOffset, after.Size)
	}
	if after.StartOffset != 0 {
		t.Errorf("start_offset %d, want 0 — the new file is smaller than the tail window",
			after.StartOffset)
	}
}

// A rotation that cannot happen must not cost the output that was going to be
// written, must not pretend it happened, and must be tried again rather than
// granting another full round of growth.
//
// The way to get there is the way it happens in service: something else holding
// the file. On Windows that is a log query overlapping the rotation — os.Open
// takes no share-delete right, so a read of up to a megabyte blocks the rename
// for as long as it runs.
func TestBlockedRotationLosesNothing(t *testing.T) {
	c, dir := testCollector(t, 100)
	c.write(bytes.Repeat([]byte("a"), 60))

	release := blockRename(t, c.path)
	c.write(bytes.Repeat([]byte("b"), 60))

	if _, err := os.Stat(archiveName(c.path, 1)); err == nil {
		release()
		t.Skip("the rename went through despite the block: this machine cannot exercise the path")
	}
	current := readFile(t, c.path)
	if strings.Contains(current, "--- rotated ") {
		t.Error("a rotation header was written although nothing was archived")
	}
	if current != strings.Repeat("a", 60)+strings.Repeat("b", 60) {
		t.Errorf("the file holds %d bytes, want the 120 that were written", len(current))
	}
	if c.size != 120 {
		t.Errorf("tracked size %d, want 120 — the next write would not try again", c.size)
	}

	// With the way clear, the next chunk rotates.
	release()
	c.write([]byte("c"))
	if _, err := os.Stat(archiveName(c.path, 1)); err != nil {
		t.Errorf("the rotation was not tried again: %v", err)
	}
	if ops := opsLog(t, dir); !strings.Contains(ops, "child log rotated") {
		t.Error("the retried rotation left no record")
	}
}

// A write that fails is recorded once and does not stop the drain. The pipe
// must keep emptying or the child stops on its next line of output (E-20).
func TestWriteFailureIsRecordedOnceAndDoesNotStopTheCollector(t *testing.T) {
	c, dir := testCollector(t, 1<<30)

	c.mu.Lock()
	c.file.Close() // every write from here on fails
	c.mu.Unlock()

	for i := 0; i < 5; i++ {
		c.write([]byte("output\n"))
	}
	if !c.failed {
		t.Error("the entry is not marked as having lost its output")
	}
	if got := strings.Count(opsLog(t, dir), "child log write failed"); got != 1 {
		t.Errorf("the operations log has %d write-failure records, want 1", got)
	}
}
