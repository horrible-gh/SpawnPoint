package runner

import (
	"bytes"
	"os"
	"strconv"
	"strings"

	"spawnpoint/internal/textdec"
)

// Log query limits (0008-L 1.3, from 0007-P 0.5).
const (
	// LogTailDefaultBytes is how far back from the end a request with no
	// `offset` starts.
	LogTailDefaultBytes = 262144
	// LogResponseMaxBytes caps one response.
	LogResponseMaxBytes = 1048576
	// LogTailWindowMaxBytes caps the `n` of `tail:<n>`; a larger n is cut to it
	// rather than refused.
	LogTailWindowMaxBytes = 1048576
	// LogLineScanLimitBytes is how far the start of a tail read looks for a line
	// break before giving up and starting mid-line (0008-L 2.6.2).
	LogLineScanLimitBytes = 65536
)

// LogView is one answer to a log query, in the shape 0007-P fixed.
type LogView struct {
	Text string
	// StartOffset is where the read actually began, which is not necessarily
	// where it was asked to begin: a tail read advances to a line boundary and
	// a stale offset is thrown away (Reset).
	StartOffset int64
	// NextOffset is what the caller sends next. It can be short of Size — the
	// tail of a half-written character is left behind on purpose (E-10).
	NextOffset int64
	Size       int64
	// Truncated means the response limit stopped the read, not that anything
	// was lost. The rest is behind NextOffset.
	Truncated bool
	// Reset means the requested offset was past the end of the file, so the
	// screen must throw away what it has and redraw (0007-P [로그 넘겨 쓰기 감지]).
	Reset    bool
	Encoding string
}

// emptyView is the answer for an entry that has never written anything. A
// missing log file is not an error and not a 404: 404 is reserved for an entry
// that is not registered (E-8, 0007-P [로그 조회 — 아직 아무것도 실행하지 않은 항목]).
func emptyView() LogView {
	return LogView{Encoding: textdec.NameUTF8}
}

// ReadLog answers a log query for one entry. The second result is false only
// when the entry is not registered.
//
// This is 0008-L 2.6.1 in order: resolve where to start, cap how much to read,
// decode what was read. The three are separate because each has its own way of
// stopping short — a line boundary, the response limit, a half-written
// character — and only the middle one counts as truncation.
func (m *Manager) ReadLog(id, offsetParam string) (LogView, bool) {
	e, ok := m.entry(id)
	if !ok {
		return LogView{}, false
	}

	file, err := os.Open(m.LogPath(id))
	if err != nil {
		// Not there yet, or not readable. Both come back as an empty result:
		// the entry exists, and a reader polling it should keep polling rather
		// than be told the entry is gone.
		return emptyView(), true
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return emptyView(), true
	}
	size := info.Size()

	start, reset := resolveStart(file, offsetParam, size)
	view := LogView{
		StartOffset: start,
		NextOffset:  start,
		Size:        size,
		Reset:       reset,
		Encoding:    textdec.NameUTF8,
	}

	readLen := size - start
	if readLen > LogResponseMaxBytes {
		readLen = LogResponseMaxBytes
	}
	if readLen <= 0 {
		return view, true
	}

	buf := make([]byte, readLen)
	// ReadAt returns io.EOF along with a short count when the file has been
	// truncated under us. The bytes it did return are still good.
	n, _ := file.ReadAt(buf, start)
	if n <= 0 {
		return view, true
	}
	data := buf[:n]
	view.Truncated = start+int64(n) < size

	res := m.decoder.Decode(data)
	if res.Consumed == 0 && !e.isRunning() {
		// Nothing could be consumed, so the reader would ask for the same bytes
		// forever. While the child is alive that is right — the character it is
		// half way through writing will be finished. Once it is gone nothing
		// will finish it, and the query has to move (E-11).
		res = textdec.Replace(data)
	}
	view.Text = res.Text
	view.NextOffset = start + int64(res.Consumed)
	view.Encoding = res.Encoding
	return view, true
}

// resolveStart is 0008-L 2.6.2.
func resolveStart(file *os.File, offsetParam string, size int64) (int64, bool) {
	kind, value := classifyOffset(offsetParam)
	if kind == offsetAbsolute {
		if value > size {
			// The only rotation test there is (0007-P fixed it, and 0008-L does
			// not extend it): asking for a position the file no longer reaches.
			return tailStart(file, LogTailDefaultBytes, size), true
		}
		return value, false
	}
	window := int64(LogTailDefaultBytes)
	if kind == offsetTailWindow {
		window = value
	}
	return tailStart(file, window, size), false
}

// tailStart is window bytes back from the end, moved forward to a line start.
func tailStart(file *os.File, window, size int64) int64 {
	raw := size - window
	if raw <= 0 {
		// The file is smaller than the window, so it is all of it. Advancing
		// here would drop the first line of a short log for no reason.
		return 0
	}
	return advanceToLineStart(file, raw, size)
}

// advanceToLineStart moves past the partial line pos lands in.
//
// It moves at most one line, and only if a line break turns up within
// LogLineScanLimitBytes. A single line longer than that is shown cut rather
// than skipped: the bytes are still in the file and still reachable by offset,
// which is the lesser of the two harms.
//
// Only LF is looked for. The CR of a CRLF ends up at the front of the text and
// the screen deals with it — searching for either would put the start of a line
// between the two halves of one break.
//
// The first thing it does is check whether there is a partial line at all.
// 0008-L 2.6.2's pseudocode does not: it scans forward unconditionally, so a
// window that happens to land exactly on a line start throws that whole line
// away. Nothing is lost — the line is still in the file, at an offset the
// caller can ask for — but a request for the last 1000 bytes coming back with
// 995 is a surprise with no purpose behind it, and the sentence the pseudocode
// is written from says to discard the line that was cut.
func advanceToLineStart(file *os.File, pos, size int64) int64 {
	var before [1]byte
	if n, _ := file.ReadAt(before[:], pos-1); n == 1 && before[0] == '\n' {
		return pos
	}
	scan := size - pos
	if scan > LogLineScanLimitBytes {
		scan = LogLineScanLimitBytes
	}
	if scan <= 0 {
		return pos
	}
	buf := make([]byte, scan)
	n, _ := file.ReadAt(buf, pos)
	i := bytes.IndexByte(buf[:n], '\n')
	if i < 0 {
		return pos
	}
	return pos + int64(i) + 1
}

// offsetKind is what an `offset` parameter turned out to mean (0008-L 4.2).
type offsetKind int

const (
	offsetTail offsetKind = iota
	offsetTailWindow
	offsetAbsolute
)

const tailPrefix = "tail:"

// classifyOffset is the decision tree of 0008-L 4.2.
//
// Everything it cannot read falls to a tail read. The current implementation
// falls to 0 instead, which turns one malformed parameter into a full read of a
// 20 MiB file (0004-NR 1.7); the safe side of that mistake is the recent end.
func classifyOffset(offsetParam string) (offsetKind, int64) {
	value := strings.TrimSpace(offsetParam)
	switch {
	case value == "":
		return offsetTail, 0
	case strings.EqualFold(value, "tail"):
		return offsetTail, 0
	case len(value) > len(tailPrefix) && strings.EqualFold(value[:len(tailPrefix)], tailPrefix):
		n, err := strconv.ParseInt(value[len(tailPrefix):], 10, 64)
		if err != nil || n < 1 {
			return offsetTail, 0
		}
		if n > LogTailWindowMaxBytes {
			n = LogTailWindowMaxBytes
		}
		return offsetTailWindow, n
	default:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			// Negative, or not a number at all (E-13).
			return offsetTail, 0
		}
		return offsetAbsolute, n
	}
}
