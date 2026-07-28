package runner

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/textdec"
)

// 0008-L 6.2, row by row. The numbers come from 0007-P's log scenarios, which
// were measured against a real 20.6 MiB log, so the sizes here are that log's
// sizes rather than convenient small ones — the line-boundary arithmetic is
// exactly what a rounder number would hide.

const (
	// pSize is the file 0007-P [로그 최초 조회] was measured on.
	pSize = 21553390
	// pStart is where that scenario's response begins: 262144 bytes back from
	// the end lands at 21291246, mid-line, and the read moves forward 67 bytes
	// to the start of the next one.
	pRawStart = pSize - LogTailDefaultBytes
	pStart    = 21291313
)

// registerQuietly puts an entry in the table without starting anything. The
// tests below are about the file, and Register would spawn a process.
func registerQuietly(t *testing.T, m *Manager, status string) string {
	t.Helper()
	id := m.newID()
	e := &entry{id: id, cmd: "noop", status: status, createdAt: time.Now(), updatedAt: time.Now()}
	m.mu.Lock()
	m.entries[id] = e
	m.mu.Unlock()
	return id
}

func writeLog(t *testing.T, m *Manager, id string, data []byte) {
	t.Helper()
	if err := os.WriteFile(m.LogPath(id), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// pLog is the 0007-P file: one line break placed so the tail read has to move
// forward exactly 67 bytes to reach a line start.
func pLog() []byte {
	data := bytes.Repeat([]byte("a"), pSize)
	data[pStart-1] = '\n'
	return data
}

// Row 1: 끝부분 기본.
func TestLogTailIsTheDefault(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusStopped)
	writeLog(t, m, id, pLog())

	got, ok := m.ReadLog(id, "")
	if !ok {
		t.Fatal("the entry was reported as unknown")
	}
	if got.StartOffset != pStart {
		t.Errorf("start_offset %d, want %d", got.StartOffset, pStart)
	}
	if got.StartOffset < pRawStart || got.StartOffset > pRawStart+LogLineScanLimitBytes {
		t.Errorf("start_offset %d is outside the window 6.2 allows (%d..%d)",
			got.StartOffset, pRawStart, pRawStart+LogLineScanLimitBytes)
	}
	// 6.2 states the property, not just the number: the read starts on a line.
	if b := pLog()[got.StartOffset-1]; b != '\n' {
		t.Errorf("the byte before start_offset is %#x, want a line break", b)
	}
	if got.NextOffset != pSize || got.Size != pSize {
		t.Errorf("next_offset %d / size %d, want %d / %d", got.NextOffset, got.Size, pSize, pSize)
	}
	if got.Truncated || got.Reset {
		t.Errorf("truncated %v / reset %v, want both false", got.Truncated, got.Reset)
	}
	if got.Encoding != textdec.NameUTF8 {
		t.Errorf("encoding %q, want %q", got.Encoding, textdec.NameUTF8)
	}
	if len(got.Text) != pSize-pStart {
		t.Errorf("text is %d bytes, want %d", len(got.Text), pSize-pStart)
	}
}

// Rows 2 and 3: 이어받기, then 변경 없음.
func TestLogResumeAndIdlePoll(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusRunning)
	writeLog(t, m, id, []byte("first line\n"))

	first, _ := m.ReadLog(id, "")
	if first.Text != "first line\n" {
		t.Fatalf("first read %q", first.Text)
	}

	file, err := os.OpenFile(m.LogPath(id), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("second line\n")
	file.Close()

	resumed, _ := m.ReadLog(id, offsetOf(first.NextOffset))
	if resumed.Text != "second line\n" {
		t.Errorf("resumed read %q, want only what was added", resumed.Text)
	}
	if resumed.StartOffset != first.NextOffset {
		t.Errorf("start_offset %d, want %d", resumed.StartOffset, first.NextOffset)
	}
	if resumed.NextOffset <= first.NextOffset {
		t.Errorf("next_offset %d did not advance past %d", resumed.NextOffset, first.NextOffset)
	}

	// Nothing new. Still a 200 with the same shape, not a 304 (0007-P).
	idle, ok := m.ReadLog(id, offsetOf(resumed.NextOffset))
	if !ok {
		t.Fatal("the entry was reported as unknown")
	}
	if idle.Text != "" {
		t.Errorf("text %q, want empty", idle.Text)
	}
	if idle.NextOffset != resumed.NextOffset || idle.StartOffset != resumed.NextOffset {
		t.Errorf("offsets moved on an unchanged file: %+v", idle)
	}
	if idle.Truncated || idle.Reset {
		t.Errorf("truncated %v / reset %v on an unchanged file", idle.Truncated, idle.Reset)
	}
}

// Row 4: 상한 절단. The response limit stops the read, and the cut is moved back
// off the character it landed in the middle of — 0007-P's next_offset of
// 1048573 against a limit of 1048576.
func TestLogResponseLimitCutsAtACharacter(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusRunning)

	const emojiStart = LogResponseMaxBytes - 3
	data := bytes.Repeat([]byte("a"), emojiStart)
	data = append(data, "\U0001F600"...) // four bytes, three of them inside the limit
	data = append(data, bytes.Repeat([]byte("b"), 1024)...)
	writeLog(t, m, id, data)

	got, _ := m.ReadLog(id, "0")
	if !got.Truncated {
		t.Error("truncated is false although the limit stopped the read")
	}
	if got.StartOffset != 0 {
		t.Errorf("start_offset %d, want 0", got.StartOffset)
	}
	if got.NextOffset != emojiStart {
		t.Errorf("next_offset %d, want %d — the character at the cut was split",
			got.NextOffset, emojiStart)
	}
	if got.NextOffset > LogResponseMaxBytes {
		t.Errorf("next_offset %d is past the response limit", got.NextOffset)
	}
	if got.Size != int64(len(data)) {
		t.Errorf("size %d, want %d", got.Size, len(data))
	}

	// And the rest arrives on the next request, character intact.
	rest, _ := m.ReadLog(id, offsetOf(got.NextOffset))
	if !strings.HasPrefix(rest.Text, "\U0001F600") {
		t.Errorf("the continuation starts %q, want the character that was held back",
			firstRunes(rest.Text, 2))
	}
	if rest.Truncated {
		t.Error("truncated is true on a read that reached the end of the file")
	}
}

// Row 5: 넘겨 쓰기. 0007-P's numbers exactly — an offset from before a rotation,
// against a file that is now 4096 bytes.
func TestLogRotationIsDetectedByOffsetPastTheEnd(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusRunning)
	data := bytes.Repeat([]byte("c"), 4095)
	data = append(data, '\n')
	writeLog(t, m, id, data)

	got, _ := m.ReadLog(id, "21553452")
	if !got.Reset {
		t.Error("reset is false although the offset is past the end of the file")
	}
	if got.StartOffset != 0 {
		// tail_start of a window larger than the file is 0, and it does not
		// advance to a line start: the whole file is inside the window.
		t.Errorf("start_offset %d, want 0", got.StartOffset)
	}
	if got.NextOffset != 4096 || got.Size != 4096 {
		t.Errorf("next_offset %d / size %d, want 4096 / 4096", got.NextOffset, got.Size)
	}
	if got.Truncated {
		t.Error("truncated is true on a read that reached the end of the file")
	}

	// The rule has exactly one form (0007-P, 0008-L 2.6.2): an offset that is
	// only equal to the size is a caller that is up to date, not a rotation.
	same, _ := m.ReadLog(id, "4096")
	if same.Reset {
		t.Error("reset is true for an offset equal to the file size")
	}
}

// Row 7: 파일 없음. Not a 404 — that is for an entry that is not registered
// (E-8, E-14).
func TestLogOfAnEntryThatHasNeverRun(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusStopped)

	got, ok := m.ReadLog(id, "")
	if !ok {
		t.Fatal("a registered entry with no log file was reported as unknown")
	}
	want := LogView{Encoding: textdec.NameUTF8}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	if _, ok := m.ReadLog("proc_deadbeef", ""); ok {
		t.Error("an entry that is not registered was reported as known")
	}
}

// 0008-L 4.2, the decision tree. Row 5 of it is the one that differs from the
// current implementation: anything unreadable falls to the recent end rather
// than to 0, which is what turned a malformed parameter into a full read of a
// 20 MiB file (0004-NR 1.7).
func TestOffsetParameter(t *testing.T) {
	cases := []struct {
		param string
		kind  offsetKind
		value int64
	}{
		{"", offsetTail, 0},
		{"tail", offsetTail, 0},
		{"TAIL", offsetTail, 0},
		{"tail:65536", offsetTailWindow, 65536},
		{"Tail:1", offsetTailWindow, 1},
		{"tail:99999999", offsetTailWindow, LogTailWindowMaxBytes},
		{"tail:0", offsetTail, 0},
		{"tail:-5", offsetTail, 0},
		{"tail:x", offsetTail, 0},
		{"tail:", offsetTail, 0},
		{"0", offsetAbsolute, 0},
		{"21553390", offsetAbsolute, 21553390},
		{"-1", offsetTail, 0},
		{"abc", offsetTail, 0},
		{"12.5", offsetTail, 0},
		{"99999999999999999999", offsetTail, 0},
	}
	for _, c := range cases {
		t.Run(c.param, func(t *testing.T) {
			kind, value := classifyOffset(c.param)
			if kind != c.kind || value != c.value {
				t.Errorf("classifyOffset(%q) = (%v, %d), want (%v, %d)",
					c.param, kind, value, c.kind, c.value)
			}
		})
	}
}

// A malformed parameter must not be answered with the whole file. This is the
// same rule as above, seen from the response.
func TestUnreadableOffsetReadsTheEndNotTheStart(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusRunning)
	writeLog(t, m, id, pLog())

	for _, param := range []string{"abc", "-1", ""} {
		got, _ := m.ReadLog(id, param)
		if got.StartOffset != pStart {
			t.Errorf("offset %q started the read at %d, want %d", param, got.StartOffset, pStart)
		}
	}
}

// tail:<n> reads n bytes back, and the window is clamped rather than refused.
func TestTailWindow(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusRunning)
	data := bytes.Repeat([]byte("line\n"), 4000) // 20000 bytes, a break every 5
	writeLog(t, m, id, data)

	got, _ := m.ReadLog(id, "tail:1000")
	if got.StartOffset != 20000-1000 {
		// 19000 is itself a line start, so there is nothing to advance past.
		t.Errorf("start_offset %d, want %d", got.StartOffset, 19000)
	}
	if len(got.Text) != 1000 {
		t.Errorf("text is %d bytes, want 1000", len(got.Text))
	}

	// A window larger than the file is the whole file, with no advance: a short
	// log must not lose its first line.
	whole, _ := m.ReadLog(id, "tail:999999")
	if whole.StartOffset != 0 {
		t.Errorf("start_offset %d for a window larger than the file, want 0", whole.StartOffset)
	}
}

// 0008-L 2.6.2: the advance is bounded. A single line longer than the scan
// limit is shown cut rather than skipped, because the alternative is dropping
// it.
func TestLineAdvanceGivesUpAfterTheScanLimit(t *testing.T) {
	m, _ := testManager(t, nil)
	id := registerQuietly(t, m, StatusRunning)
	// One line of 400000 bytes: no break anywhere near the tail start.
	writeLog(t, m, id, bytes.Repeat([]byte("z"), 400000))

	got, _ := m.ReadLog(id, "")
	if want := int64(400000 - LogTailDefaultBytes); got.StartOffset != want {
		t.Errorf("start_offset %d, want %d — the read moved although there was no line break",
			got.StartOffset, want)
	}
	if len(got.Text) != LogTailDefaultBytes {
		t.Errorf("text is %d bytes, want %d", len(got.Text), LogTailDefaultBytes)
	}
}

// E-11: a decode that consumes nothing is normal while the child is alive and
// permanent once it is not.
func TestStalledDecodeIsBrokenOnlyForADeadChild(t *testing.T) {
	m, _ := testManager(t, nil)
	partial := []byte{0xEC, 0x84} // two bytes of a three-byte character

	running := registerQuietly(t, m, StatusRunning)
	writeLog(t, m, running, partial)
	got, _ := m.ReadLog(running, "0")
	if got.NextOffset != 0 || got.Text != "" {
		t.Errorf("a live child's half-written character was consumed: %+v", got)
	}
	if got.Encoding != textdec.NameUTF8 {
		t.Errorf("encoding %q, want %q", got.Encoding, textdec.NameUTF8)
	}

	stopped := registerQuietly(t, m, StatusExited)
	writeLog(t, m, stopped, partial)
	got, _ = m.ReadLog(stopped, "0")
	if got.NextOffset != int64(len(partial)) {
		t.Errorf("next_offset %d, want %d — the query is stuck on a child that will never finish the character",
			got.NextOffset, len(partial))
	}
	if got.Encoding != textdec.NameReplace {
		t.Errorf("encoding %q, want %q", got.Encoding, textdec.NameReplace)
	}
}

// offsetOf renders a next_offset the way a caller sends it back.
func offsetOf(v int64) string { return strconv.FormatInt(v, 10) }

func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		runes = runes[:n]
	}
	return string(runes)
}
