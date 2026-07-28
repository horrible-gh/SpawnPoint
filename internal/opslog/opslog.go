// Package opslog writes SpawnPoint's own operations log.
//
// This is the component the rewrite exists for. 0004-NR 1.4 found that when the
// current server disappears it leaves no trace at all — no reason, no time, not
// even the fact that it stopped. Everything here is shaped by that: the record
// format is fixed (0008-L 2.13), records are handed to the operating system one
// at a time with no user-space buffering, and the file is opened before the
// database so that a failure during startup still lands somewhere (0008-L 2.15).
//
// Record shape:
//
//	<response timestamp> <LEVEL> <event> <key>=<value> <key>=<value> ...
//	2026-07-28T18:12:41.006000+09:00 INFO stopping reason=service_control
//
// Field order is the declaration order and is deliberately stable: a later
// forensic tool may key off position, so callers pass fields in the order given
// by the event table in 0008-L 2.13.
package opslog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"spawnpoint/internal/timefmt"
)

// Rotation policy (0008-L 1.3). The operations log rotates at 16 MiB and keeps
// five archives, so a busy server retains roughly 96 MiB of history.
const (
	RotateBytes = 16 * 1024 * 1024
	ArchiveKeep = 5
)

// FileName is the operations log inside the configured log directory. Child
// logs in the same directory are named <id>.log where id is `proc_` plus eight
// hex digits (0007-P), so the names cannot collide.
const FileName = "spawnpoint.log"

// Level is the record severity. 0008-L 1.3 fixes the order and sets the minimum
// recorded level to INFO.
type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

// MinLevel is `ops_log_min_level` (0008-L 1.3).
const MinLevel = Info

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	default:
		return "INFO"
	}
}

// Field is one `key=value` pair of a record.
type Field struct {
	Key   string
	Value any
}

// F builds a keyed field.
func F(key string, value any) Field { return Field{Key: key, Value: value} }

// V builds a bare field, rendered as the value alone. Only `listening` uses it,
// which carries a single address and no key (0008-L 2.13, 0007-P [서비스 기동]).
func V(value any) Field { return Field{Value: value} }

// Logger appends records to the operations log file. It is safe for concurrent
// use; every exported method takes the same lock, which also serialises
// rotation against writes.
type Logger struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	once     map[string]struct{}
	closed   bool
	writeErr error

	// Injection points for the tests. Production values are set by Open.
	now         func() time.Time
	rotateBytes int64
	keep        int
}

// Open creates dir if needed and opens the operations log for appending.
//
// A failure here stops the process with exit_code_unrecoverable (0008-L 2.15,
// E-27): a SpawnPoint that cannot record why it died is the exact thing this
// rewrite is meant to remove, so it must not be left resident.
func Open(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, FileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open operations log %s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat operations log %s: %w", path, err)
	}
	return &Logger{
		path:        path,
		file:        file,
		size:        info.Size(),
		once:        make(map[string]struct{}),
		now:         time.Now,
		rotateBytes: RotateBytes,
		keep:        ArchiveKeep,
	}, nil
}

// Path is the operations log file, for start-up diagnostics on stderr.
func (l *Logger) Path() string { return l.path }

// Log appends one record. Records below MinLevel are dropped.
//
// Write failures are held rather than returned: a caller reacting to a failed
// log write has nowhere better to report it, and the alternative — dropping the
// shutdown sequence because its first record failed — loses the very trace the
// sequence exists to leave. Err surfaces the first failure at Close.
func (l *Logger) Log(level Level, event string, fields ...Field) {
	if level < MinLevel {
		return
	}
	record := Format(l.nowTime(), level, event, fields...)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.write(record)
}

// Once appends a record only the first time a given (event, id) pair is seen,
// for the whole life of the process — `ops_log_once` in 0008-L 2.13. A child
// whose every log write fails would otherwise fill the operations log with the
// same line (E-20).
func (l *Logger) Once(level Level, event, id string, fields ...Field) {
	if level < MinLevel {
		return
	}
	key := event + "\x00" + id
	record := Format(l.nowTime(), level, event, fields...)

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, seen := l.once[key]; seen {
		return
	}
	l.once[key] = struct{}{}
	l.write(record)
}

// Close flushes and closes the file and reports the first write error seen.
// It is idempotent so the shutdown sequence can run twice without complaint.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.writeErr
	}
	l.closed = true
	err := l.file.Close()
	if l.writeErr != nil {
		return l.writeErr
	}
	return err
}

// Err reports the first write failure, if any.
func (l *Logger) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeErr
}

func (l *Logger) nowTime() time.Time { return l.now() }

// write appends one already-formatted record. The caller holds l.mu.
//
// There is no user-space buffer and no explicit Sync. One write syscall per
// record is enough for the failure this design cares about: a process killed by
// the operating system loses nothing already handed to the kernel. Sync per
// record would only add power-loss durability, at a cost paid on every record.
func (l *Logger) write(record string) {
	if l.closed {
		return
	}
	size := int64(len(record))
	if l.size+size >= l.rotateBytes {
		l.rotate()
	}
	n, err := l.file.WriteString(record)
	l.size += int64(n)
	if err != nil && l.writeErr == nil {
		l.writeErr = err
	}
}

// rotate renames the current file aside and starts an empty one, keeping l.keep
// archives (0008-L 2.13, same scheme as the child logs in 2.5).
//
// Unlike a child log, no header line is written into the new file. Child logs
// get one so a person reading the viewer sees that a rotation happened (2.5);
// the operations log has no viewer and every line is expected to parse as a
// record, so a header would be the one line that does not.
//
// A failure to rotate is not fatal — the file simply keeps growing past the
// threshold, which is much better than losing records.
func (l *Logger) rotate() {
	if err := l.file.Close(); err != nil && l.writeErr == nil {
		l.writeErr = err
	}
	os.Remove(archiveName(l.path, l.keep))
	for n := l.keep - 1; n >= 1; n-- {
		from, to := archiveName(l.path, n), archiveName(l.path, n+1)
		if _, err := os.Stat(from); err == nil {
			os.Rename(from, to)
		}
	}
	if err := os.Rename(l.path, archiveName(l.path, 1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Keep going: reopening in append mode below still records, and a
		// stalled rotation is preferable to a silent gap in the log.
		if l.writeErr == nil {
			l.writeErr = err
		}
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		l.closed = true
		if l.writeErr == nil {
			l.writeErr = err
		}
		return
	}
	l.file = file
	l.size = 0
}

func archiveName(path string, n int) string {
	return fmt.Sprintf("%s.%d", path, n)
}

// Format renders one record, including the trailing newline. It is exported so
// tests and any later log reader share one definition of the shape.
func Format(at time.Time, level Level, event string, fields ...Field) string {
	var b strings.Builder
	b.WriteString(timefmt.Response(at))
	b.WriteByte(' ')
	b.WriteString(level.String())
	b.WriteByte(' ')
	b.WriteString(event)
	for _, f := range fields {
		b.WriteByte(' ')
		if f.Key != "" {
			b.WriteString(f.Key)
			b.WriteByte('=')
		}
		b.WriteString(FormatValue(f.Value))
	}
	b.WriteByte('\n')
	return b.String()
}

// FormatValue renders a field value per 0008-L 2.13: nil becomes `null`, and a
// value containing a space, a quote or a newline is quoted with those two
// characters escaped. Everything else is written bare.
func FormatValue(v any) string {
	if v == nil {
		return "null"
	}
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case error:
		s = t.Error()
	case fmt.Stringer:
		s = t.String()
	default:
		s = fmt.Sprint(v)
	}
	if !strings.ContainsAny(s, " \"\n") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
