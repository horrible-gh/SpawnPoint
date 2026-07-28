//go:build windows

package textdec

import (
	"strconv"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// Windows entry points, resolved lazily out of the standard syscall package.
// internal/host and internal/jobs already establish this shape: the rewrite
// ships as one executable and takes no third-party dependency for something the
// operating system already answers (0006-D 2.3).
//
// Using the operating system's tables rather than a bundled one is what keeps
// `cp949` here meaning the same thing it means to every other program on the
// machine — and it is also the reason 0008-L 2.7's warning about needing a
// character table outside the standard set does not apply to this build.
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetACP              = kernel32.NewProc("GetACP")
	procGetCPInfo           = kernel32.NewProc("GetCPInfo")
	procMultiByteToWideChar = kernel32.NewProc("MultiByteToWideChar")
)

// mbErrInvalidChars makes MultiByteToWideChar fail on the first byte that has
// no mapping instead of substituting for it. That failure is the whole judgment
// (0008-L 2.7 rule 2): without the flag every code page would "succeed" and the
// first candidate would always win.
const mbErrInvalidChars = 0x00000008

// cpInfo mirrors the CPINFO structure.
type cpInfo struct {
	MaxCharSize uint32
	DefaultChar [2]byte
	LeadByte    [12]byte
}

// isLead reports whether b starts a two-byte sequence in this code page.
//
// LeadByte holds up to six ranges as low/high pairs, terminated by a zero pair.
// Asking the operating system is what makes the sequence-length rule of 2.7
// work for a code page this file has never heard of.
func (c *cpInfo) isLead(b byte) bool {
	for i := 0; i+1 < len(c.LeadByte); i += 2 {
		lo, hi := c.LeadByte[i], c.LeadByte[i+1]
		if lo == 0 && hi == 0 {
			return false
		}
		if b >= lo && b <= hi {
			return true
		}
	}
	return false
}

var (
	defaultOnce sync.Once
	defaultList []candidate
)

// defaultCandidates is UTF-8, then the system code page, then CP949
// (0008-L 2.7 rule 1, the order the current implementation uses).
//
// The list is fixed for the life of the process because the ANSI code page is.
// Duplicates are dropped rather than tried twice: on a Korean Windows the
// system code page is CP949, and 2.7 removes that repeat explicitly — it costs
// a second full scan of every chunk and cannot change the answer.
func defaultCandidates() []candidate {
	defaultOnce.Do(func() {
		out := []candidate{utf8Candidate}
		if acp := systemCodePage(); acp != codePageUTF8 {
			out = appendCodePage(out, acp)
		}
		out = appendCodePage(out, codePageCP949)
		defaultList = out
	})
	return defaultList
}

// CodePages builds a decoder over UTF-8 and the given code pages, in order.
//
// It exists for the contract tests. 0007-P's log scenarios were written against
// a Korean Windows, where the candidate list is UTF-8 then CP949; a machine
// with a different ANSI code page has a third candidate wedged in the middle
// and can answer a Korean chunk differently — see the note in
// textdec_windows_test.go. Pinning the list is how those scenarios stay
// checkable anywhere.
func CodePages(pages ...uint32) *Decoder {
	out := []candidate{utf8Candidate}
	for _, cp := range pages {
		out = appendCodePage(out, cp)
	}
	return &Decoder{candidates: out}
}

// appendCodePage adds a code page candidate unless it is already in the list or
// the operating system cannot judge it strictly.
func appendCodePage(list []candidate, cp uint32) []candidate {
	name := codePageName(cp)
	for _, c := range list {
		if c.name == name {
			return list
		}
	}
	info, ok := codePageInfo(cp)
	if !ok {
		// An unknown code page, or one whose characters run longer than two
		// bytes — GB18030 and the ISO-2022 family among them. The sequence
		// length rule of 2.7 is written for one- and two-byte characters, and
		// guessing at the rest would consume bytes it cannot account for.
		// Leaving the candidate out means such a chunk falls through to the
		// substituting read, which is visibly wrong rather than quietly wrong.
		return list
	}
	if !strictlyJudgeable(cp) {
		// Some code pages reject MB_ERR_INVALID_CHARS outright. Without it
		// every chunk would "decode" and this candidate would swallow
		// everything that reached it.
		return list
	}
	return append(list, candidate{
		name: name,
		decode: func(data []byte) (string, int, bool) {
			return decodeCodePage(cp, info, data)
		},
	})
}

// codePageName is the value that goes in the `encoding` field. The two the
// contract names are spelled the way it spells them; anything else is `cp` and
// the number, which is how Windows itself refers to them.
func codePageName(cp uint32) string {
	switch cp {
	case codePageUTF8:
		return NameUTF8
	case codePageCP949:
		return NameCP949
	default:
		return "cp" + strconv.FormatUint(uint64(cp), 10)
	}
}

// decodeCodePage is try_decode_strict for a Windows code page.
//
// It runs in two parts, matching 2.7: the sequence lengths decide how much of
// the slice is complete, and the conversion decides whether that much is valid.
// Splitting it this way is what separates "the child is mid-character" from
// "these bytes are not this notation" — one carries, the other rejects — and it
// is the reason the trailing bytes of a chunk never come back as replacement
// characters.
func decodeCodePage(cp uint32, info *cpInfo, data []byte) (string, int, bool) {
	consumed := len(data)
	if info.MaxCharSize > 1 {
		consumed = 0
		for consumed < len(data) {
			if !info.isLead(data[consumed]) {
				consumed++
				continue
			}
			if consumed+2 > len(data) {
				break
			}
			consumed += 2
		}
	}
	if consumed == 0 {
		// Everything in hand is the start of one character. Valid so far, and
		// nothing to show for it yet.
		return "", 0, true
	}
	text, ok := strictConvert(cp, data[:consumed])
	if !ok {
		return "", 0, false
	}
	return text, consumed, true
}

// strictConvert converts data with no substitutions, reporting failure instead.
func strictConvert(cp uint32, data []byte) (string, bool) {
	need := multiByteToWideChar(cp, data, nil)
	if need <= 0 {
		return "", false
	}
	buf := make([]uint16, need)
	got := multiByteToWideChar(cp, data, buf)
	if got <= 0 {
		return "", false
	}
	// utf16.Decode rather than syscall.UTF16ToString: a child's output can
	// contain NUL bytes, and UTF16ToString would cut the text at the first one.
	return string(utf16.Decode(buf[:got])), true
}

// multiByteToWideChar returns the number of UTF-16 units, or a non-positive
// number if the bytes do not fit the code page. Passing the length explicitly
// (rather than -1 for a NUL-terminated string) is what lets NUL bytes through.
func multiByteToWideChar(cp uint32, data []byte, out []uint16) int32 {
	if len(data) == 0 {
		return 0
	}
	var outPtr *uint16
	if len(out) > 0 {
		outPtr = &out[0]
	}
	n, _, _ := procMultiByteToWideChar.Call(
		uintptr(cp),
		uintptr(mbErrInvalidChars),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(int32(len(data))),
		uintptr(unsafe.Pointer(outPtr)),
		uintptr(int32(len(out))),
	)
	return int32(n)
}

// strictlyJudgeable probes whether the code page accepts MB_ERR_INVALID_CHARS.
// A code page that refuses the flag fails with ERROR_INVALID_FLAGS on input it
// would otherwise convert without complaint.
func strictlyJudgeable(cp uint32) bool {
	return multiByteToWideChar(cp, []byte{'A'}, nil) > 0
}

var (
	infoMu    sync.Mutex
	infoCache = map[uint32]*cpInfo{}
)

// codePageInfo asks the operating system about a code page once and remembers
// the answer. ok is false for a code page that is not installed, and for one
// whose characters can exceed two bytes.
func codePageInfo(cp uint32) (*cpInfo, bool) {
	infoMu.Lock()
	defer infoMu.Unlock()
	if info, seen := infoCache[cp]; seen {
		return info, info != nil
	}
	var info cpInfo
	ok, _, _ := procGetCPInfo.Call(uintptr(cp), uintptr(unsafe.Pointer(&info)))
	if ok == 0 || info.MaxCharSize == 0 || info.MaxCharSize > 2 {
		infoCache[cp] = nil
		return nil, false
	}
	infoCache[cp] = &info
	return &info, true
}

// systemCodePage is `system_default_codepage_name()` of 2.7 — the ANSI code
// page, which is what the current implementation's `mbcs` codec resolves to.
func systemCodePage() uint32 {
	acp, _, _ := procGetACP.Call()
	return uint32(acp)
}

// SystemCodePageName is the system code page as it would appear in an
// `encoding` field. Reported by the tests so a failure says which machine
// produced it.
func SystemCodePageName() string { return codePageName(systemCodePage()) }
