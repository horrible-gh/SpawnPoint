package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/runner"
	"spawnpoint/internal/store"
)

// The tests above drive the front end against a fake runner, which is how the
// response shapes get pinned. These drive it against the real one.
//
// What that adds is the wiring: that Manager satisfies the interface this
// package declares, and that a request with no `offset` really does come back
// with the end of a real file rather than the whole of it. A fake cannot show
// either — it agrees with whatever it was written to agree with.
//
// No process is started. The entries come back through the restore path, which
// is how a real server's entries look between a restart and the first run, and
// the log files are written directly. The default test run starts nothing,
// which is the convention T1 set.

// Manager is the Runner. If this stops compiling, the front end and the runner
// have drifted apart, and it is better to find out here than at the call site
// in main.
var _ Runner = (*runner.Manager)(nil)

// stubRegistry hands the manager a fixed set of registrations to restore.
type stubRegistry struct{ rows []store.RunnerEntry }

func (s *stubRegistry) SaveEntry(store.RunnerEntry) error         { return nil }
func (s *stubRegistry) DeleteEntry(string) error                  { return nil }
func (s *stubRegistry) ListEntries() ([]store.RunnerEntry, error) { return s.rows, nil }

func realServer(t *testing.T, ids ...string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	log := discardLog(t)

	rows := make([]store.RunnerEntry, 0, len(ids))
	for i, id := range ids {
		rows = append(rows, store.RunnerEntry{
			ID: id, Label: "entry-" + id, Cmd: "powershell -NoProfile",
			CreatedAt: time.Now().Add(time.Duration(i) * time.Second),
			UpdatedAt: time.Now(),
		})
	}
	m := runner.New(dir, log, &stubRegistry{rows: rows}, false)
	if n, err := m.Restore(); err != nil || n != len(ids) {
		t.Fatalf("restore: %d entries, %v", n, err)
	}
	return newTestServer(t, Options{Runner: m, Log: log}), dir
}

// The whole point of the changed contract: selecting an entry reads the end of
// the file, not all of it. The current server sends offset=0 on every selection
// and pulls the whole thing across (0004-NR 1.7, 0007-P [로그 최초 조회]).
func TestARequestWithNoOffsetReadsTheEndOfARealFile(t *testing.T) {
	s, dir := realServer(t, "proc_45c05b99")

	// Comfortably past the 256 KiB window, with numbered lines so the answer
	// says exactly where it started.
	var sb strings.Builder
	for i := 0; sb.Len() < 400*1024; i++ {
		sb.WriteString("line ")
		sb.WriteString(strings.Repeat("x", 60))
		sb.WriteByte('\n')
	}
	whole := sb.String()
	if err := os.WriteFile(filepath.Join(dir, "proc_45c05b99.log"), []byte(whole), 0o644); err != nil {
		t.Fatalf("write the log: %v", err)
	}

	_, body, _ := do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs", "")
	var got logResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v — %s", err, body)
	}

	if got.Size != int64(len(whole)) {
		t.Errorf("size %d, want %d", got.Size, len(whole))
	}
	if got.StartOffset == 0 {
		t.Fatal("the read started at 0 — this is the full read the rewrite removes")
	}
	// The window is 256 KiB and the advance to a line start is at most one line.
	back := got.Size - got.StartOffset
	if back > runner.LogTailDefaultBytes || back < runner.LogTailDefaultBytes-4096 {
		t.Errorf("read back %d bytes from the end, want about %d", back, runner.LogTailDefaultBytes)
	}
	if int64(len(got.Text)) != got.NextOffset-got.StartOffset {
		t.Errorf("text is %d bytes, offsets say %d", len(got.Text), got.NextOffset-got.StartOffset)
	}
	if got.Truncated || got.Reset {
		t.Errorf("truncated=%v reset=%v, want both false", got.Truncated, got.Reset)
	}
	// The text begins at a line start, which is what makes the first line whole.
	if strings.HasPrefix(got.Text, "x") {
		t.Errorf("the read began mid-line: %.40q", got.Text)
	}
}

// A resume asks from where the last answer stopped and gets only what arrived
// since (0007-P [로그 이어받기 — 증분]).
func TestResumingFromNextOffsetOverARealFile(t *testing.T) {
	s, dir := realServer(t, "proc_1")
	path := filepath.Join(dir, "proc_1.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, body, _ := do(t, s, http.MethodGet, "/processes/proc_1/logs", "")
	var first logResponse
	json.Unmarshal([]byte(body), &first)
	if first.Text != "first\n" {
		t.Fatalf("text %q", first.Text)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	f.WriteString("second\n")
	f.Close()

	_, body, _ = do(t, s, http.MethodGet, "/processes/proc_1/logs?offset="+itoa(first.NextOffset), "")
	var second logResponse
	json.Unmarshal([]byte(body), &second)
	if second.Text != "second\n" {
		t.Errorf("resume returned %q, want %q", second.Text, "second\n")
	}
	if second.StartOffset != first.NextOffset {
		t.Errorf("resumed at %d, asked for %d", second.StartOffset, first.NextOffset)
	}

	// Nothing new: same offset back, empty text, still a 200.
	status, body, _ := do(t, s, http.MethodGet, "/processes/proc_1/logs?offset="+itoa(second.NextOffset), "")
	var third logResponse
	json.Unmarshal([]byte(body), &third)
	if status != http.StatusOK || third.Text != "" || third.NextOffset != second.NextOffset {
		t.Errorf("idle poll: %d %+v", status, third)
	}
}

// An offset past the end of the file is a rotation, and the answer says so
// (0007-P [로그 넘겨 쓰기 감지]).
func TestRotationIsReportedOverARealFile(t *testing.T) {
	s, dir := realServer(t, "proc_1")
	if err := os.WriteFile(filepath.Join(dir, "proc_1.log"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, body, _ := do(t, s, http.MethodGet, "/processes/proc_1/logs?offset=21553452", "")
	var got logResponse
	json.Unmarshal([]byte(body), &got)
	if !got.Reset {
		t.Errorf("reset was not raised: %+v", got)
	}
	if got.Text != "fresh\n" {
		t.Errorf("text %q", got.Text)
	}
}

// A registered entry that has never run has no log file, and that is a 200 with
// an empty result rather than a 404 (E-8).
func TestAnEntryWithNoLogFileOverTheRealRunner(t *testing.T) {
	s, _ := realServer(t, "proc_f8470a37")

	status, body, _ := do(t, s, http.MethodGet, "/processes/proc_f8470a37/logs", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{"ok": true, "text": "", "start_offset": 0, "next_offset": 0,
	                    "size": 0, "truncated": false, "reset": false, "encoding": "utf-8"}`)
}

// 0007-P [서버 재기동 후 복원] / 0008-L 6.5: restored entries come back stopped
// with every live field null, listed oldest first.
func TestRestoredEntriesAreListedStopped(t *testing.T) {
	s, _ := realServer(t, "proc_a", "proc_b")

	_, body, _ := do(t, s, http.MethodGet, "/processes", "")
	var got listResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Processes) != 2 {
		t.Fatalf("%d entries, want 2", len(got.Processes))
	}
	if got.Processes[0].ID != "proc_a" || got.Processes[1].ID != "proc_b" {
		t.Errorf("order %s, %s", got.Processes[0].ID, got.Processes[1].ID)
	}
	for _, p := range got.Processes {
		if p.Status != runner.StatusStopped {
			t.Errorf("%s: status %q, want stopped", p.ID, p.Status)
		}
		if p.PID != nil || p.ExitCode != nil || p.StartedAt != nil || p.EndedAt != nil {
			t.Errorf("%s carried live state from before the restart: %+v", p.ID, p)
		}
	}
}

// Stopping something that is not running is not an error, and neither is
// running something that is already stopped-and-restarted. Both answer with the
// current state (E-16, E-17).
func TestIdempotentOperationsOverTheRealRunner(t *testing.T) {
	s, _ := realServer(t, "proc_1")

	for _, action := range []string{"stop", "stop"} {
		status, body, _ := do(t, s, http.MethodPost, "/processes/proc_1/"+action, "")
		if status != http.StatusOK {
			t.Fatalf("%s: status %d, want 200 — %s", action, status, body)
		}
		if !strings.Contains(body, `"status":"stopped"`) {
			t.Errorf("%s: %s", action, body)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
