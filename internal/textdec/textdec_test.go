package textdec

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The UTF-8 candidate is the only one every platform has, so its rules are
// tested here and the code page candidates in textdec_windows_test.go.

func TestUTF8IsJudgedOverTheWholeSlice(t *testing.T) {
	korean := "서버를 시작합니다\n"
	cases := []struct {
		name     string
		data     []byte
		text     string
		consumed int
		ok       bool
	}{
		{"empty", nil, "", 0, true},
		{"ascii", []byte("hello\n"), "hello\n", 6, true},
		{"nul bytes are text", []byte("a\x00b"), "a\x00b", 3, true},
		{"complete", []byte(korean), korean, len(korean), true},
		{
			// 0008-L 2.7 rule 3: the tail is left for the next read.
			"one byte of three", append([]byte("ok "), korean[0]), "ok ", 3, true,
		},
		{"two bytes of three", append([]byte("ok "), korean[0], korean[1]), "ok ", 3, true},
		{"three bytes of four", append([]byte("ok "), "\U0001F600"[:3]...), "ok ", 3, true},
		// Rule 2: one byte that does not fit rejects the notation, and the
		// bytes before it go with it.
		{"lone continuation byte", []byte("abc\x80"), "", 0, false},
		{"bad continuation", []byte("abc\xc3\x28\n"), "", 0, false},
		{"overlong form", []byte("abc\xc0\xaf"), "", 0, false},
		{"surrogate half", []byte("abc\xed\xa0\x80"), "", 0, false},
		{"above the last code point", []byte("abc\xf5\x80\x80\x80"), "", 0, false},
		// A tail that has already ruled itself out is not a tail. Without this
		// the reader would carry these bytes forever — see partialUTF8OK.
		{"truncated but already impossible", []byte("abc\xe0\x28"), "", 0, false},
		{"truncated overlong lead", []byte("abc\xc0"), "", 0, false},
		{"truncated surrogate lead", []byte("abc\xed\xa0"), "", 0, false},
		{"truncated above the last code point", []byte("abc\xf4\x90"), "", 0, false},
		{"truncated but still possible", []byte("abc\xed\x9f"), "abc", 3, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, consumed, ok := decodeUTF8(c.data)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (text %q, consumed %d)", ok, c.ok, text, consumed)
			}
			if text != c.text || consumed != c.consumed {
				t.Errorf("got (%q, %d), want (%q, %d)", text, consumed, c.text, c.consumed)
			}
		})
	}
}

// The carry is what a caller's next_offset falls short by. 0008-L 1.3 puts a
// number on it, and the log response contract relies on that number being small
// enough that a reader always catches up (0007-P next_offset 4159, size 4162).
func TestCarryNeverExceedsTheStatedMaximum(t *testing.T) {
	// Every prefix of every sequence length, with a valid leading byte.
	seqs := []string{"a", "é", "한", "\U0001F600"}
	for _, s := range seqs {
		for cut := 1; cut <= len(s); cut++ {
			data := append([]byte("ok "), s[:cut]...)
			_, consumed, ok := decodeUTF8(data)
			if !ok {
				t.Fatalf("%q cut to %d bytes was rejected", s, cut)
			}
			if carry := len(data) - consumed; carry > CarryMax {
				t.Errorf("%q cut to %d bytes carried %d bytes, more than %d",
					s, cut, carry, CarryMax)
			}
		}
	}
}

// Rule 4: the last resort always moves. A reader that could not get past a
// stretch of bytes would never see anything written after it.
func TestReplaceConsumesEverything(t *testing.T) {
	data := []byte("abc\xff\xfe\xfd")
	got := Replace(data)
	if got.Consumed != len(data) {
		t.Errorf("consumed %d of %d bytes", got.Consumed, len(data))
	}
	if got.Encoding != NameReplace {
		t.Errorf("encoding %q, want %q", got.Encoding, NameReplace)
	}
	if !utf8.ValidString(got.Text) {
		t.Errorf("text %q is not valid UTF-8", got.Text)
	}
	if !strings.HasPrefix(got.Text, "abc") {
		t.Errorf("text %q lost the part that was readable", got.Text)
	}
}

func TestEmptyInputIsUTF8(t *testing.T) {
	got := Default().Decode(nil)
	if got.Text != "" || got.Consumed != 0 || got.Encoding != NameUTF8 {
		t.Errorf("got %+v, want an empty utf-8 result", got)
	}
}

// 0008-L 2.7 rule 1 fixes UTF-8 first everywhere, and rule 1's dedup keeps the
// same notation from being tried twice.
func TestCandidateOrder(t *testing.T) {
	names := Default().Names()
	if len(names) == 0 || names[0] != NameUTF8 {
		t.Fatalf("candidates %v do not start with %q", names, NameUTF8)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("candidate %q appears twice in %v", n, names)
		}
		seen[n] = true
	}
}

// Decoding a chunk twice must give the same answer. The verdict is allowed to
// differ between chunks (E-32) but not between two reads of one.
func TestDecodeIsStable(t *testing.T) {
	d := Default()
	data := []byte("서버 시작\n")
	first := d.Decode(data)
	second := d.Decode(data)
	if first != second {
		t.Errorf("two decodes of the same bytes differ:\n %+v\n %+v", first, second)
	}
}
