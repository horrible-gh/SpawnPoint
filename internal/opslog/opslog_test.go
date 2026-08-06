package opslog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 7, 28, 18, 12, 41, 6000000, time.FixedZone("KST", 9*60*60))

// open returns a logger over a temporary directory with a frozen clock.
func open(t *testing.T) (*Logger, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	l.now = func() time.Time { return at }
	t.Cleanup(func() { l.Close() })
	return l, filepath.Join(dir, FileName)
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The two records that decide T2: 0009-CH's completion test for this stage is
// `stopping reason=` and `stopped` appearing in the file.
func TestShutdownRecordsMatchProtocolExample(t *testing.T) {
	l, path := open(t)
	l.Log(Info, "stopping", F("reason", "service_control"))
	l.Log(Info, "stopped", F("exit_code", 0))
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := "2026-07-28T18:12:41.006000+09:00 INFO stopping reason=service_control\n" +
		"2026-07-28T18:12:41.006000+09:00 INFO stopped exit_code=0\n"
	if got := read(t, path); got != want {
		t.Fatalf("log =\n%q\nwant\n%q", got, want)
	}
}

// 0007-P [서비스 기동] fixes the start record; `listening` carries a bare
// address with no key.
func TestStartupRecordsMatchProtocolExample(t *testing.T) {
	l, path := open(t)
	l.Log(Info, "start", F("host", "0.0.0.0"), F("port", 7527),
		F("db", "spawnpoint.db"), F("auth", "disabled"))
	l.Log(Info, "migrations", F("applied", 2), F("pending", 0))
	l.Log(Info, "runner restored", F("entries", 5), F("status", "stopped"))
	l.Log(Info, "listening", V("http://0.0.0.0:7527"))
	l.Close()

	got := read(t, path)
	for _, want := range []string{
		"INFO start host=0.0.0.0 port=7527 db=spawnpoint.db auth=disabled\n",
		"INFO migrations applied=2 pending=0\n",
		"INFO runner restored entries=5 status=stopped\n",
		"INFO listening http://0.0.0.0:7527\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q\ngot:\n%s", want, got)
		}
	}
}

// 0007-P [서비스 기동 실패 — 포트 점유] shows the quoted detail. Values with
// spaces must be quoted or a reader cannot tell where the field ends.
func TestValueQuoting(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{"service_control", "service_control"},
		{nil, "null"},
		{0, "0"},
		{7527, "7527"},
		{true, "true"},
		{"only one usage of each socket address", `"only one usage of each socket address"`},
		{`say "hi"`, `"say \"hi\""`},
		{"line\nbreak", `"line\nbreak"`},
		{`C:\var\logs`, `C:\var\logs`},
		{os.ErrNotExist, `"file does not exist"`},
	}
	for _, c := range cases {
		if got := FormatValue(c.value); got != c.want {
			t.Errorf("FormatValue(%v) = %s, want %s", c.value, got, c.want)
		}
	}
}

// A quoted value must not swallow the following field.
func TestQuotedValueKeepsRecordParseable(t *testing.T) {
	l, path := open(t)
	l.Log(Error, "bind_failed", F("host", "0.0.0.0"), F("port", 7527),
		F("detail", "only one usage of each socket address is normally permitted"))
	l.Close()

	line := strings.TrimSuffix(read(t, path), "\n")
	want := `2026-07-28T18:12:41.006000+09:00 ERROR bind_failed host=0.0.0.0 port=7527 ` +
		`detail="only one usage of each socket address is normally permitted"`
	if line != want {
		t.Fatalf("record = %q\nwant      %q", line, want)
	}
}

// 0008-L 2.13: field order is the declaration order and must not be reordered.
func TestFieldOrderIsDeclarationOrder(t *testing.T) {
	got := Format(at, Info, "child terminated",
		F("id", "proc_45c05b99"), F("pid", 22232), F("exit_code", 1),
		F("reason", "service_control"))
	want := "2026-07-28T18:12:41.006000+09:00 INFO child terminated " +
		"id=proc_45c05b99 pid=22232 exit_code=1 reason=service_control\n"
	if got != want {
		t.Fatalf("record = %q\nwant     %q", got, want)
	}
}

// ops_log_min_level is INFO (0008-L 1.3).
func TestDebugIsDropped(t *testing.T) {
	l, path := open(t)
	l.Log(Debug, "noise", F("k", "v"))
	l.Log(Info, "kept")
	l.Close()
	if got := read(t, path); strings.Contains(got, "noise") {
		t.Fatalf("DEBUG record was written: %q", got)
	} else if !strings.Contains(got, "kept") {
		t.Fatalf("INFO record missing: %q", got)
	}
}

// ops_log_once: a repeating failure must not fill the file (E-20).
func TestOnceDedupesPerEventAndID(t *testing.T) {
	l, path := open(t)
	for range 5 {
		l.Once(Error, "child log write failed", "proc_a", F("id", "proc_a"), F("detail", "disk full"))
	}
	l.Once(Error, "child log write failed", "proc_b", F("id", "proc_b"), F("detail", "disk full"))
	l.Once(Error, "other event", "proc_a", F("id", "proc_a"))
	l.Close()

	got := read(t, path)
	if n := strings.Count(got, "id=proc_a detail=\"disk full\""); n != 1 {
		t.Errorf("repeated (event,id) written %d times, want 1", n)
	}
	if n := strings.Count(got, "child log write failed"); n != 2 {
		t.Errorf("event written %d times, want 2 (one per id)", n)
	}
	if !strings.Contains(got, "other event id=proc_a") {
		t.Error("a different event with the same id was suppressed")
	}
}

// 0008-L 1.3 forbids buffering: the record must be in the file before the call
// returns, because the next thing to happen may be a forced termination.
func TestRecordIsOnDiskBeforeReturn(t *testing.T) {
	l, path := open(t)
	l.Log(Info, "stopping", F("reason", "console_close"))
	if got := read(t, path); !strings.Contains(got, "stopping reason=console_close") {
		t.Fatalf("record not visible before Close: %q", got)
	}
}

// Reopening must append rather than truncate, and must resume the size tracking
// from the existing file so rotation still triggers at the threshold.
func TestReopenAppendsAndResumesSizeTracking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	first, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first.now = func() time.Time { return at }
	first.Log(Info, "start", F("host", "127.0.0.1"))
	first.Close()

	second, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	second.now = func() time.Time { return at }
	if want := int64(len(read(t, path))); second.size != want {
		t.Errorf("resumed size = %d, want %d", second.size, want)
	}
	second.Log(Info, "stopped", F("exit_code", 0))
	second.Close()

	got := read(t, path)
	if !strings.Contains(got, "start") || !strings.Contains(got, "stopped") {
		t.Fatalf("reopen did not append: %q", got)
	}
}

func TestRotation(t *testing.T) {
	l, path := open(t)
	l.rotateBytes = 200
	l.keep = 3

	for i := range 40 {
		l.Log(Info, "child started", F("id", "proc_0000000"+string(rune('a'+i%20))), F("pid", 1000+i))
	}
	l.Close()

	// Current file plus keep archives, and nothing beyond keep.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	for n := 1; n <= 3; n++ {
		if _, err := os.Stat(archiveName(path, n)); err != nil {
			t.Errorf("archive .%d missing: %v", n, err)
		}
	}
	if _, err := os.Stat(archiveName(path, 4)); !os.IsNotExist(err) {
		t.Errorf("archive .4 exists, want at most keep=3")
	}

	// The newest archive must be newer than the oldest: .1 holds the records
	// written just before the current file, so rotation shifted in the right
	// direction rather than overwriting .1 every time.
	newest := read(t, archiveName(path, 1))
	oldest := read(t, archiveName(path, 3))
	if newest == oldest {
		t.Error("archives .1 and .3 are identical, shift order is wrong")
	}
	if !strings.Contains(read(t, path), "child started") {
		t.Error("current log is empty after rotation")
	}

	// Unlike a child log, the operations log gets no header line: every line
	// must parse as a record.
	for _, line := range strings.Split(strings.TrimSuffix(read(t, path), "\n"), "\n") {
		if !strings.HasPrefix(line, "2026-") {
			t.Errorf("non-record line after rotation: %q", line)
		}
	}
}

// Rotation must not lose the size accounting, or the file grows unbounded after
// the first rotation.
func TestSizeTrackingSurvivesRotation(t *testing.T) {
	l, path := open(t)
	l.rotateBytes = 300
	l.keep = 2
	for range 20 {
		l.Log(Info, "child started", F("id", "proc_45c05b99"), F("pid", 22232))
	}
	l.Close()
	if got, want := int64(len(read(t, path))), int64(300); got >= want {
		t.Fatalf("current log is %d bytes, want below the %d threshold", got, want)
	}
}

func TestOpenCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

// E-27: the caller needs a real error to turn into exit code 2.
func TestOpenFailsWhenDirectoryPathIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blocker); err == nil {
		t.Fatal("Open succeeded on a file path, want error")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	l, _ := open(t)
	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Logging after close is a no-op rather than a panic: the shutdown
	// sequence and a signal handler can race.
	l.Log(Info, "stopped", F("exit_code", 0))
}

func TestConcurrentLoggingIsSerialised(t *testing.T) {
	l, path := open(t)
	done := make(chan struct{})
	for w := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 50 {
				l.Log(Info, "child started", F("id", w), F("pid", i))
			}
		}()
	}
	for range 8 {
		<-done
	}
	l.Close()

	lines := strings.Split(strings.TrimSuffix(read(t, path), "\n"), "\n")
	if len(lines) != 400 {
		t.Fatalf("got %d lines, want 400", len(lines))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "2026-07-28T18:12:41.006000+09:00 INFO child started id=") {
			t.Fatalf("interleaved record: %q", line)
		}
	}
}
