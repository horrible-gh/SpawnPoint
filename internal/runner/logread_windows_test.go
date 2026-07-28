//go:build windows

package runner

import (
	"encoding/hex"
	"testing"

	"spawnpoint/internal/textdec"
)

// 0008-L 6.2, row 표기 혼재: a child that writes CP949 is read as CP949, the
// Korean is intact, and next_offset stops short of the file because the last
// character is only half written (0007-P next_offset 4159 against size 4162).
//
// The decoder is pinned to the candidate list of a Korean Windows, which is the
// machine 0007-P was measured on. On a machine whose ANSI code page is some
// other double-byte code page that code page is tried first and can claim these
// bytes — see TestAnotherDoubleByteCodePageCanClaimCP949Bytes in
// internal/textdec, where the behaviour is on the record. Pinning is what keeps
// this row checkable anywhere rather than only in Korea.
func TestLogWithMixedNotations(t *testing.T) {
	m, _ := testManager(t, nil)
	m.decoder = textdec.CodePages(949)

	id := registerQuietly(t, m, StatusRunning)
	// The 0007-P sample in CP949, ending on the leading byte of a character
	// whose second byte has not been written yet.
	data, err := hex.DecodeString(
		"bcadb9f6b8a620bdc3c0dbc7d5b4cfb4d90ac6f7c6ae203831303020b4ebb1e220c1dfbc")
	if err != nil {
		t.Fatal(err)
	}
	writeLog(t, m, id, data)

	got, ok := m.ReadLog(id, "0")
	if !ok {
		t.Fatal("the entry was reported as unknown")
	}
	if got.Encoding != textdec.NameCP949 {
		t.Errorf("encoding %q, want %q", got.Encoding, textdec.NameCP949)
	}
	if want := "서버를 시작합니다\n포트 8100 대기 중"; got.Text != want {
		t.Errorf("text %q, want %q", got.Text, want)
	}
	if got.NextOffset >= got.Size {
		t.Errorf("next_offset %d against size %d — the half-written character was consumed",
			got.NextOffset, got.Size)
	}
	if carry := got.Size - got.NextOffset; carry > textdec.CarryMax {
		t.Errorf("carried %d bytes, more than %d", carry, textdec.CarryMax)
	}
	// E-10: falling short of the size is not truncation. Only the response
	// limit is.
	if got.Truncated {
		t.Error("truncated is true although nothing hit the response limit")
	}

	// The child finishes the character it was in the middle of, and the poll
	// that follows picks it up whole.
	writeLog(t, m, id, append(data, 0xAD))
	rest, _ := m.ReadLog(id, offsetOf(got.NextOffset))
	if rest.Text != "서" {
		t.Errorf("the continuation read %q, want the character that was held back", rest.Text)
	}
	if rest.NextOffset != rest.Size {
		t.Errorf("the continuation stopped at %d of %d", rest.NextOffset, rest.Size)
	}
}
