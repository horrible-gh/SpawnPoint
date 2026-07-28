package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"spawnpoint/internal/opslog"
	"spawnpoint/internal/timefmt"
)

// PipeReadBuffer is `child_pipe_read_buffer_bytes` (0008-L 1.2).
const PipeReadBuffer = 64 * 1024

// LogArchiveKeep is `child_log_archive_keep` (0008-L 1.3). Deleting an entry
// removes the log and this many archives (0008-L 2.5.1).
const LogArchiveKeep = 3

// LogRotateBytes is `child_log_rotate_bytes` (0008-L 1.3). With the archives
// this bounds one entry at `child_log_max_total_bytes`, 128 MiB.
const LogRotateBytes = 32 * 1024 * 1024

// logRotateBytes is the threshold the collectors actually use. It is a variable
// only so the rotation tests do not have to write 32 MiB per case; nothing
// outside the tests assigns to it.
var logRotateBytes int64 = LogRotateBytes

// collector drains one child's output pipe into its log file.
//
// The change from the current implementation is that the pipe exists at all.
// Today the child's output handle is the log file itself, so the server has no
// say over what happens to it — which is why the file cannot be rotated and why
// nothing can be interposed (0006-D 2.1, 0008-L 2.5). Reading it here puts the
// server back in control.
//
// The collector owns the file for its whole life: it decides when the pipe is
// read, when the bytes are written, and when the file is handed aside for a new
// one (0008-L 2.5). The reader side of the same file is logread.go.
type collector struct {
	id   string
	path string
	log  *opslog.Logger

	pipe *os.File
	file *os.File

	mu sync.Mutex
	// size is `tracked_size`: what the collector has written, counted as it
	// writes rather than asked of the filesystem on every chunk (0008-L 2.5).
	size int64
	// failed marks an entry whose output is no longer reaching the disk (E-20).
	failed      bool
	rotateBytes int64
	keep        int

	done chan struct{}
}

// startCollector opens the log file and begins draining pipe.
//
// A log file that cannot be opened is not a reason to refuse to start the
// child: the child is the point and the log is the record of it. The pipe is
// drained either way, because a full pipe stops the child dead (0008-L 2.5).
func startCollector(id, path string, pipe *os.File, log *opslog.Logger) *collector {
	c := &collector{
		id: id, path: path, log: log, pipe: pipe,
		rotateBytes: logRotateBytes,
		keep:        LogArchiveKeep,
		done:        make(chan struct{}),
	}
	if file, err := openLog(path); err == nil {
		c.file = file
		if info, err := file.Stat(); err == nil {
			// tracked_size starts from what is already on disk. The collector
			// counts from here rather than asking the filesystem on every
			// write (0008-L 2.5).
			c.size = info.Size()
		}
	} else {
		c.failed = true
		c.reportWriteFailure(err)
	}
	go c.loop()
	return c
}

// openLog opens a child log for appending, creating the directory if a person
// removed it while the server was running (E-1).
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// loop reads until the write end is gone.
//
// End of stream arrives when every handle to the write end has closed — the
// child's, its descendants', and the server's own copy, which is dropped right
// after the child starts. So this returns once the whole tree is finished, not
// merely once the shell is, and that is what makes the output of a grandchild
// dying last still reach the file.
func (c *collector) loop() {
	defer close(c.done)
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.file != nil {
			c.file.Close()
			c.file = nil
		}
	}()

	buf := make([]byte, PipeReadBuffer)
	for {
		n, err := c.pipe.Read(buf)
		if n > 0 {
			c.write(buf[:n])
		}
		if err != nil {
			// io.EOF is the ordinary end. Any other error means the pipe is
			// unusable, and continuing would spin.
			if err != io.EOF {
				c.reportWriteFailure(err)
			}
			return
		}
	}
}

// write appends one chunk.
//
// A failed write is recorded once and then ignored: the read loop must keep
// going. Stopping it would fill the pipe buffer, and a child blocked on writing
// to a full pipe is a stopped service — the opposite of what a full disk should
// cost (0008-L 2.5, E-20).
func (c *collector) write(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return
	}
	if c.size+int64(len(chunk)) >= c.rotateBytes {
		c.rotate()
		if c.file == nil {
			return
		}
	}
	n, err := c.file.Write(chunk)
	c.size += int64(n)
	if err != nil {
		c.failed = true
		c.log.Once(opslog.Error, "child log write failed", c.id,
			opslog.F("id", c.id), opslog.F("detail", err))
	}
}

// rotate hands the current log aside and starts an empty one, keeping c.keep
// archives (0008-L 2.5). The caller holds c.mu.
//
// Rotation happens before the chunk that would cross the threshold is written,
// never in the middle of one, so a chunk is never split across two files.
//
// It is a best effort. Every step that fails leaves the collector writing
// somewhere — worst case the same file it started with, which then keeps
// growing and is rotated again on the next chunk. Losing output because a
// rename failed would be the wrong trade: the point of the log is that it is
// there afterwards.
func (c *collector) rotate() {
	archived := c.size
	if err := c.file.Close(); err != nil {
		c.reportWriteFailure(err)
	}
	c.file = nil

	os.Remove(archiveName(c.path, c.keep))
	for n := c.keep - 1; n >= 1; n-- {
		from, to := archiveName(c.path, n), archiveName(c.path, n+1)
		if _, err := os.Stat(from); err == nil {
			os.Rename(from, to)
		}
	}
	rotated := true
	if err := os.Rename(c.path, archiveName(c.path, 1)); err != nil {
		// On Windows this is what a reader holding the file open looks like:
		// os.Open takes no share-delete right, so a query that overlaps the
		// rotation blocks the rename for as long as it runs. Nothing is lost —
		// the file is reopened below with its real size, so the next chunk
		// tries again.
		rotated = false
		if !errors.Is(err, os.ErrNotExist) {
			c.reportWriteFailure(err)
		}
	}

	file, err := openLog(c.path)
	if err != nil {
		// No file to write to. The read loop keeps draining regardless, which
		// is the one thing that must not stop (E-20).
		c.failed = true
		c.reportWriteFailure(err)
		return
	}
	c.file = file
	// From the file itself, not from zero: if the rename did not happen, the
	// size is still the old one and the threshold is still crossed, so the
	// attempt repeats instead of granting another full round of growth.
	c.size = 0
	if info, err := file.Stat(); err == nil {
		c.size = info.Size()
	}
	if !rotated {
		return
	}

	// The header goes in even though a reader whose offset survived the
	// rotation will not see a `reset`. It is the only thing that tells that
	// reader the file underneath it changed (0008-L 2.5, E-9).
	//
	// The count is what was archived, not `child_log_rotate_bytes`. The
	// pseudocode in 2.5 writes the threshold, but the archived file is always
	// somewhat short of it — rotation happens before the chunk that would cross
	// it — so the threshold would be a number that is never true.
	header := "--- rotated " + timefmt.Response(time.Now()) +
		" (previous " + strconv.FormatInt(archived, 10) + " bytes archived) ---\n"
	n, err := c.file.WriteString(header)
	c.size += int64(n)
	if err != nil {
		c.failed = true
		c.reportWriteFailure(err)
	}
	c.log.Log(opslog.Info, "child log rotated",
		opslog.F("id", c.id), opslog.F("keep", c.keep))
}

// reportWriteFailure records one child's log trouble once for the life of the
// process. An entry whose every write fails would otherwise crowd the
// operations log out with one line per chunk (E-20).
func (c *collector) reportWriteFailure(err error) {
	c.log.Once(opslog.Error, "child log write failed", c.id,
		opslog.F("id", c.id), opslog.F("detail", err))
}

// archiveName is `<path>.<n>`, the same scheme the operations log uses.
func archiveName(path string, n int) string {
	return path + "." + strconv.Itoa(n)
}

// stop waits for the drain to finish, up to timeout.
//
// It is called after the child is confirmed gone, never before: a collector
// closed early loses whatever the child wrote on its way down, and that is the
// part a person goes looking for (0008-L 2.3).
//
// On timeout the goroutine is abandoned rather than interrupted. The only way
// to reach it is a descendant still holding the pipe open after the group was
// terminated, which is already reported as `child terminate timeout`; forcing
// the read end shut underneath a blocked read would race the read itself for no
// gain, since the process is on its way out in every case that gets here.
func (c *collector) stop(timeout time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// writeMarker appends a resume or restart marker to a child log (0008-L 2.5.1).
//
// The timestamp is required. The current implementation writes a bare
// `--- run ---`, which makes it impossible to tell afterwards which run
// produced which lines (0004-NR U3, 0007-P [실행 재개]). The line is ASCII only,
// so it survives being read back under either encoding.
func writeMarker(path, kind string, at time.Time) error {
	file, err := openLog(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString("\n--- " + kind + " " + timefmt.Response(at) + " ---\n")
	return err
}
