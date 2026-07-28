//go:build !windows

package textdec

// defaultCandidates is UTF-8 alone.
//
// 0008-L 2.7 guards both of the other candidates with `if platform ==
// windows`, and so does the current implementation (spawnpoint/runner.py
// extends the list only under `_WINDOWS`). There is no ANSI code page outside
// Windows to stand in the middle, and CP949 is there because Windows commands
// emit it — a child on another platform that writes CP949 is not a case this
// server has ever handled, and inventing support for it here would mean
// carrying a character table for a notation nothing produces.
func defaultCandidates() []candidate {
	return []candidate{utf8Candidate}
}
