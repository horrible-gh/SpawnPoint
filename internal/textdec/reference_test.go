//go:build windows

package textdec

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// This decoder has to read back what the previous implementation wrote, and the
// files it will be pointed at were written by that one. So the contract is not
// "these bytes are CP949" but "this is the text the previous implementation
// showed" — which is why the fixture is a capture of its log reader rather than
// a table written by hand.
//
// The capture is a fixed contract value now. The implementation it was taken
// from has been removed, so the file is no longer regenerated: it records what
// the deployed decoder did, and that answer does not change.

const referencePath = "testdata/python_reference.json"

type reference struct {
	ANSICodePage uint32          `json:"ansi_code_page"`
	Python       string          `json:"python"`
	Cases        []referenceCase `json:"cases"`
}

type referenceCase struct {
	Name     string `json:"name"`
	DataHex  string `json:"data_hex"`
	Text     string `json:"text"`
	Consumed int    `json:"consumed"`
	// Replaced marks the cases where every candidate failed. Both
	// implementations consume the whole chunk there; neither is held to the
	// other's number of replacement characters (see Replace).
	Replaced bool `json:"replaced"`
}

// divergences are the cases where the rewrite deliberately answers differently,
// with the reason. A case not listed here has to match byte for byte.
var divergences = map[string]string{
	"byte that leads nothing": "" +
		"Python's mbcs decoder holds back both 0xff and the byte after it and " +
		"reports nothing consumed. Those two bytes are not a character being " +
		"written — no third byte completes them — so the reader is offered them " +
		"again on every poll and never gets past them while the child is alive. " +
		"0008-L 2.7 judges the pair and rejects the candidate, which is what " +
		"this build does: the chunk falls through to the substituting read and " +
		"the query moves.",
	"truncated lead that can never complete": "" +
		"The same stall from the other side: 0xe0 0x28 cannot become a UTF-8 " +
		"character and cannot become an mbcs one, but Python's mbcs decoder " +
		"buffers both bytes rather than rejecting them.",
}

func loadReference(t *testing.T) reference {
	t.Helper()
	raw, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatalf("read %s: %v", referencePath, err)
	}
	var ref reference
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("parse %s: %v", referencePath, err)
	}
	return ref
}

// TestMatchesThePythonReader compares the two decoders case by case.
//
// The candidate list depends on the ANSI code page — `mbcs` is whatever the
// machine says it is — so a fixture captured on one machine says nothing about
// another. Rather than pretend otherwise, the capture records the code page and
// this skips when it does not match.
func TestMatchesThePythonReader(t *testing.T) {
	ref := loadReference(t)
	if acp := systemCodePage(); acp != ref.ANSICodePage {
		t.Skipf("the reference was captured on a machine with ANSI code page %d; this one is %d",
			ref.ANSICodePage, acp)
	}
	d := Default()
	for _, c := range ref.Cases {
		t.Run(c.Name, func(t *testing.T) {
			data, err := hex.DecodeString(c.DataHex)
			if err != nil {
				t.Fatalf("case data: %v", err)
			}
			got := d.Decode(data)
			if why, known := divergences[c.Name]; known {
				if got.Text == c.Text && got.Consumed == c.Consumed {
					t.Errorf("this case is recorded as a deliberate difference but the two agree now — "+
						"the note needs removing:\n%s", why)
				}
				t.Logf("deliberate difference: python (%q, %d) against (%q, %d)\n%s",
					c.Text, c.Consumed, got.Text, got.Consumed, why)
				if got.Consumed < c.Consumed {
					t.Errorf("consumed %d, less than python's %d — the query moves slower, not faster",
						got.Consumed, c.Consumed)
				}
				return
			}
			if got.Consumed != c.Consumed {
				t.Errorf("consumed %d, want %d (python read %q, this read %q)",
					got.Consumed, c.Consumed, c.Text, got.Text)
			}
			if c.Replaced {
				// Only the byte count is contractual on this path.
				if got.Encoding != NameReplace {
					t.Errorf("encoding %q, want %q — python fell through to substitutions here",
						got.Encoding, NameReplace)
				}
				return
			}
			if got.Text != c.Text {
				t.Errorf("text %q, want %q", got.Text, c.Text)
			}
		})
	}
}
