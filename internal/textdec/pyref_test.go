//go:build windows

package textdec

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The rewrite's decoder has to read back what the deployed one wrote, and the
// files it will be pointed at were written by the deployed one. So the contract
// is not "these bytes are CP949" but "this is the text the current
// implementation would show" — which is why the fixture is a capture of
// spawnpoint/runner.py read_log() rather than a table written by hand.
//
// tools/pyref/dump_decode.py regenerates it:
//
//	python tools/pyref/dump_decode.py > internal/textdec/testdata/python_reference.json

const referencePath = "testdata/python_reference.json"

// liveEnv gates the test that runs Python, the same switch the store's
// reference test uses. An ordinary `go test ./...` needs no Python.
const liveEnv = "SPAWNPOINT_LIVE_PYREF"

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

// TestReferenceIsCurrent catches the fixture drifting away from the
// implementation it was captured from. Without it every case above could keep
// passing against a stale file.
func TestReferenceIsCurrent(t *testing.T) {
	if os.Getenv(liveEnv) == "" {
		t.Skipf("set %s=1 to regenerate the reference and compare", liveEnv)
	}
	script := filepath.Join("..", "..", "tools", "pyref", "dump_decode.py")
	out, err := exec.Command("python", script).Output()
	if err != nil {
		t.Fatalf("run %s: %v", script, err)
	}
	var fresh reference
	if err := json.Unmarshal(out, &fresh); err != nil {
		t.Fatalf("parse the fresh capture: %v", err)
	}
	stored := loadReference(t)
	if fresh.ANSICodePage != stored.ANSICodePage {
		t.Skipf("captured on ANSI code page %d, stored is %d", fresh.ANSICodePage, stored.ANSICodePage)
	}
	if len(fresh.Cases) != len(stored.Cases) {
		t.Fatalf("the capture has %d cases, the stored file %d — regenerate it",
			len(fresh.Cases), len(stored.Cases))
	}
	for i, c := range fresh.Cases {
		s := stored.Cases[i]
		if c != s {
			t.Errorf("case %q drifted:\n fresh  %+v\n stored %+v", c.Name, c, s)
		}
	}
}
