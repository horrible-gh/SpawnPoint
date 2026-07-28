//go:build windows

package textdec

import (
	"reflect"
	"strings"
	"testing"
)

// koreanCP949 is the 0007-P [로그 조회 — 여러 표기 혼재] sample, in the notation
// the scenario says a Korean Windows child writes.
var koreanCP949 = []byte{
	0xBC, 0xAD, 0xB9, 0xF6, 0xB8, 0xA6, 0x20, // 서버를␠
	0xBD, 0xC3, 0xC0, 0xDB, 0xC7, 0xD5, 0xB4, 0xCF, 0xB4, 0xD9, 0x0A, // 시작합니다\n
}

// koreanPinned is the decoder of the machine 0007-P was written against: a
// Korean Windows, where the system code page is CP949 and 2.7's dedup leaves
// two candidates.
func koreanPinned() *Decoder { return CodePages(codePageCP949) }

// 0008-L 6.2 row "표기 혼재": CP949 output comes back as CP949, the Korean is
// intact, and next_offset does not run past the file.
func TestCP949Chunk(t *testing.T) {
	got := koreanPinned().Decode(koreanCP949)
	if got.Encoding != NameCP949 {
		t.Errorf("encoding %q, want %q", got.Encoding, NameCP949)
	}
	if want := "서버를 시작합니다\n"; got.Text != want {
		t.Errorf("text %q, want %q", got.Text, want)
	}
	if got.Consumed != len(koreanCP949) {
		t.Errorf("consumed %d of %d bytes", got.Consumed, len(koreanCP949))
	}
}

// The trailing byte of a character the child has only half written is held
// back, and the next read completes it — the property 0007-P states as
// next_offset 4159 against size 4162.
func TestCP949HalfWrittenCharacterIsCarried(t *testing.T) {
	d := koreanPinned()
	cut := len(koreanCP949) - 2 // stops after the lead byte of the last character
	first := d.Decode(koreanCP949[:cut])
	if first.Encoding != NameCP949 {
		t.Fatalf("encoding %q, want %q", first.Encoding, NameCP949)
	}
	if first.Consumed >= cut {
		t.Fatalf("consumed %d of %d bytes — the half-written character was not held back",
			first.Consumed, cut)
	}
	if carry := cut - first.Consumed; carry > CarryMax {
		t.Errorf("carried %d bytes, more than %d", carry, CarryMax)
	}
	// What the reader does next: resume from where it stopped.
	second := d.Decode(koreanCP949[first.Consumed:])
	if joined := first.Text + second.Text; joined != "서버를 시작합니다\n" {
		t.Errorf("the two reads join to %q", joined)
	}
}

// Rule 2 again, on the code page path: a byte that does not fit rejects the
// notation for the whole chunk rather than for the part after it.
func TestCP949MidChunkFaultRejectsTheCandidate(t *testing.T) {
	data := append([]byte{}, koreanCP949...)
	data[3] = 0x20 // a lead byte followed by a space is no character
	if _, _, ok := decodeCodePageFor(t, codePageCP949, data); ok {
		t.Error("a chunk with a broken character in the middle was accepted as CP949")
	}
}

// A child's output is bytes, not a string; NUL is one of them and must survive
// the round trip. This is the reason the conversion does not go through
// syscall.UTF16ToString.
func TestNulBytesSurviveTheCodePagePath(t *testing.T) {
	data := []byte("before\x00after\n")
	text, consumed, ok := decodeCodePageFor(t, codePageCP949, data)
	if !ok || consumed != len(data) {
		t.Fatalf("decode reported ok=%v consumed=%d", ok, consumed)
	}
	if text != "before\x00after\n" {
		t.Errorf("text %q lost the NUL or what follows it", text)
	}
}

// A code page the machine does not have is left out of the list rather than
// added as a candidate that always fails.
func TestUnknownCodePageIsNotACandidate(t *testing.T) {
	got := CodePages(60000).Names()
	if !reflect.DeepEqual(got, []string{NameUTF8}) {
		t.Errorf("candidates %v, want %v", got, []string{NameUTF8})
	}
}

// 0008-L 2.7 rule 1: UTF-8, then the system code page, then CP949 — with the
// repeat removed when the system code page is CP949.
func TestDefaultOrderFollowsTheSystemCodePage(t *testing.T) {
	system := SystemCodePageName()
	var want []string
	switch system {
	case NameUTF8:
		want = []string{NameUTF8, NameCP949}
	case NameCP949:
		want = []string{NameUTF8, NameCP949}
	default:
		want = []string{NameUTF8, system, NameCP949}
	}
	got := Default().Names()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidates %v, want %v (system code page %s)", got, want, system)
	}
}

// A machine whose ANSI code page is some other double-byte code page answers
// some CP949 chunks under that code page instead, because it is tried first and
// the two share byte ranges.
//
// This is the fixed order of 2.7 doing what it says, and it is what the current
// implementation does too (`mbcs` before `cp949` in spawnpoint/runner.py). On
// the deployment target — a Korean Windows, where the system code page is
// CP949 — the middle candidate does not exist and the question never arises.
//
// The test is here so the behaviour is on the record rather than discovered by
// someone reading a log of mojibake. It fails if the overlap ever stops
// existing, at which point this note needs rewriting rather than the code.
func TestAnotherDoubleByteCodePageCanClaimCP949Bytes(t *testing.T) {
	system := SystemCodePageName()
	if system == NameUTF8 || system == NameCP949 {
		t.Skipf("system code page is %s: CP949 is the only double-byte candidate", system)
	}
	if _, ok := codePageInfo(systemCodePage()); !ok {
		t.Skipf("system code page %s is not a double-byte code page", system)
	}
	got := Default().Decode(koreanCP949)
	if got.Encoding != system {
		t.Skipf("system code page %s rejected this chunk, so CP949 got it: %q", system, got.Text)
	}
	pinned := koreanPinned().Decode(koreanCP949)
	t.Logf("system code page %s claims the chunk as %q; as CP949 it reads %q",
		system, got.Text, pinned.Text)
	if got.Text == pinned.Text {
		t.Errorf("the two notations produced the same text %q, which they should not", got.Text)
	}
}

// decodeCodePageFor runs one code page directly, skipping if the machine does
// not have it.
func decodeCodePageFor(t *testing.T, cp uint32, data []byte) (string, int, bool) {
	t.Helper()
	info, ok := codePageInfo(cp)
	if !ok {
		t.Skipf("code page %d is not installed", cp)
	}
	if !strictlyJudgeable(cp) {
		t.Skipf("code page %d does not support strict conversion", cp)
	}
	return decodeCodePage(cp, info, data)
}

// The name that goes on the wire. 0007-P spells two of them and Windows numbers
// the rest.
func TestCodePageNames(t *testing.T) {
	for cp, want := range map[uint32]string{
		codePageUTF8:  NameUTF8,
		codePageCP949: NameCP949,
		932:           "cp932",
		1252:          "cp1252",
	} {
		if got := codePageName(cp); got != want {
			t.Errorf("codePageName(%d) = %q, want %q", cp, got, want)
		}
	}
	if strings.ContainsAny(SystemCodePageName(), " \"") {
		t.Errorf("system code page name %q would need quoting in a log record", SystemCodePageName())
	}
}
