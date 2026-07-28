// Package textdec turns a slice of a child's log file into text.
//
// A child writes bytes; a reader needs characters, and on Windows the bytes
// arrive in more than one notation — the registered commands are PowerShell
// scripts and a Korean Windows emits CP949 alongside UTF-8. 0007-P
// [로그 조회 — 여러 표기 혼재] fixed that support range as an external contract
// (it is explicitly not narrowed to UTF-8), and 0008-L 2.7 restated the current
// implementation's incremental decoder as the algorithm implemented here.
//
// Two properties carry the design.
//
//   - A candidate is judged over the whole slice. One byte that does not fit
//     rejects the notation outright; there is no partial success (2.7 rule 2).
//     Accepting a prefix would let a stretch of CP949 be reported as UTF-8
//     because its first few bytes happened to be ASCII.
//   - A character whose tail has not arrived yet is not consumed. Those bytes
//     stay behind, the caller's next_offset stops short of the file size, and
//     the next read completes them — so a character is never split across two
//     responses (2.7 rule 3, 0007-P next_offset 4159 against size 4162).
//
// The verdict is per response, not per file. The same log can come back as
// UTF-8 now and CP949 next time, which is a true report of what the child wrote
// rather than a defect (E-32).
package textdec

import (
	"strings"
	"unicode/utf8"
)

// CarryMax is `decode_carry_max_bytes` (0008-L 1.3): the most bytes a decode
// may leave unconsumed at the tail. Three, because a UTF-8 character can be
// four bytes long and a double-byte code page can be two.
const CarryMax = 3

// Notation names as they appear in the `encoding` field of a log response
// (0007-P). `replace` is not a notation — it is the report that no candidate
// fit and the bytes were read with substitutions.
const (
	NameUTF8    = "utf-8"
	NameCP949   = "cp949"
	NameReplace = "replace"
)

// Windows code page numbers used by name above.
const (
	codePageUTF8  = 65001
	codePageCP949 = 949
)

// Result is one decode: the text, how many of the input bytes it accounts for,
// and which notation produced it.
type Result struct {
	Text     string
	Consumed int
	Encoding string
}

// candidate is one notation to try, in order.
type candidate struct {
	name string
	// decode reports ok=false when the notation does not fit the whole slice.
	// A short consumed count with ok=true is a truncated tail, not a failure.
	decode func([]byte) (text string, consumed int, ok bool)
}

// Decoder is an ordered candidate list.
//
// It is a value rather than a package function because the order depends on the
// machine — the system code page sits between UTF-8 and CP949 (2.7 rule 1) —
// and the contract tests have to be able to pin the order of the machine the
// contract was written for, which is not necessarily the one running them.
type Decoder struct {
	candidates []candidate
}

// Default is the decoder for this machine, per 0008-L 2.7.
func Default() *Decoder { return &Decoder{candidates: defaultCandidates()} }

// Decode reads as much of data as it can account for.
//
// The bytes are never rejected. If every candidate fails, the last resort reads
// them as UTF-8 with substitutions and consumes all of them, so a reader
// polling the log can always make progress past a stretch of bytes that is not
// text at all (2.7 rule 4).
func (d *Decoder) Decode(data []byte) Result {
	if len(data) == 0 {
		return Result{Encoding: NameUTF8}
	}
	for _, c := range d.candidates {
		if text, consumed, ok := c.decode(data); ok {
			return Result{Text: text, Consumed: consumed, Encoding: c.name}
		}
	}
	return Replace(data)
}

// Names lists the candidates in order. For tests and for diagnostics.
func (d *Decoder) Names() []string {
	out := make([]string, 0, len(d.candidates))
	for _, c := range d.candidates {
		out = append(out, c.name)
	}
	return out
}

// Replace is the last resort, and also the way out of a stall: it consumes
// everything it is given (E-11).
//
// Runs of bytes that are not UTF-8 collapse into one replacement character
// each. The current implementation, going through Python's `errors="replace"`,
// substitutes per maximal subpart and so can produce a different number of
// replacement characters for the same input. Neither count is contractual —
// 0007-P fixes `encoding: "replace"` and full consumption, not the shape of the
// damage.
func Replace(data []byte) Result {
	return Result{
		Text:     strings.ToValidUTF8(string(data), "�"),
		Consumed: len(data),
		Encoding: NameReplace,
	}
}

// utf8Candidate is first on every platform (2.7 rule 1).
var utf8Candidate = candidate{name: NameUTF8, decode: decodeUTF8}

// decodeUTF8 is try_decode_strict for UTF-8.
//
// The length is taken from the leading byte before the sequence is read, so a
// sequence that runs off the end can be told apart from one that is malformed.
// That distinction is the whole point: the first is carried to the next read,
// the second rejects the notation.
func decodeUTF8(data []byte) (string, int, bool) {
	consumed := 0
	for consumed < len(data) {
		b := data[consumed]
		if b < utf8.RuneSelf {
			consumed++
			continue
		}
		need := utf8SequenceLen(b)
		if need == 0 {
			return "", 0, false
		}
		if consumed+need > len(data) {
			// The tail of a character, still being written — but only if what
			// is already here could still turn into one. See partialUTF8OK.
			if !partialUTF8OK(data[consumed:]) {
				return "", 0, false
			}
			break
		}
		// DecodeRune reports one byte consumed for anything malformed, which
		// covers overlong forms, surrogate halves and bad continuation bytes.
		if _, size := utf8.DecodeRune(data[consumed : consumed+need]); size != need {
			return "", 0, false
		}
		consumed += need
	}
	return string(data[:consumed]), consumed, true
}

// partialUTF8OK reports whether seq — shorter than the character its leading
// byte announces — could still become that character.
//
// 0008-L 2.7's pseudocode stops at the length check and asks nothing of the
// bytes already in hand. That is one byte short of correct, and the shortfall
// stalls the reader: a chunk ending in 0xE0 0x28 is not a character being
// written, because no third byte makes it one. Left as a carry it is offered to
// the next query unchanged, which carries it again, for as long as the child
// lives — and the way out in E-11 is only open once the child is dead.
//
// Judging the tail as far as it goes closes that. It is also what the current
// implementation does: Python's incremental decoder validates each byte as it
// arrives and raises on this input, which sends it to the next candidate
// (spawnpoint/runner.py read_log). The stall is a defect in the pseudocode, not
// a behaviour being preserved.
func partialUTF8OK(seq []byte) bool {
	for i := 1; i < len(seq); i++ {
		lo, hi := utf8ContinuationRange(seq[0], i)
		if seq[i] < lo || seq[i] > hi {
			return false
		}
	}
	return true
}

// utf8ContinuationRange is the range the i-th byte of a sequence may take.
//
// The second byte is narrower than 0x80-0xBF for four of the leading bytes:
// those are the ranges that would otherwise admit an overlong form (0xE0, 0xF0),
// a surrogate half (0xED) or a value above U+10FFFF (0xF4). utf8.DecodeRune
// applies the same rules to a complete sequence; this applies them early so a
// partial one is judged by the same standard.
func utf8ContinuationRange(lead byte, i int) (byte, byte) {
	if i == 1 {
		switch lead {
		case 0xE0:
			return 0xA0, 0xBF
		case 0xED:
			return 0x80, 0x9F
		case 0xF0:
			return 0x90, 0xBF
		case 0xF4:
			return 0x80, 0x8F
		}
	}
	return 0x80, 0xBF
}

// utf8SequenceLen is the sequence length a leading byte announces, or 0 if it
// cannot lead one.
//
// Zero covers three groups: continuation bytes (0x80-0xBF), the two leading
// bytes that only ever produce an overlong form (0xC0, 0xC1), and the bytes
// that would announce a value above U+10FFFF or a length UTF-8 no longer has
// (0xF5-0xFF). Each of them has to be rejected here rather than left to the
// completed-sequence check, because a chunk that ends on one would otherwise be
// carried forward forever.
func utf8SequenceLen(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b < 0xC2:
		return 0
	case b < 0xE0:
		return 2
	case b < 0xF0:
		return 3
	case b < 0xF5:
		return 4
	default:
		return 0
	}
}
