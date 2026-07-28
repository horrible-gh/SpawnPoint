package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// measuredShell is the %ComSpec% observed on the production host (0004-NR 3.2).
// The fixtures use it as their {C} placeholder so this file's expectations are
// byte-exact regardless of where the test happens to run.
const measuredShell = `C:\windows\system32\cmd.exe`

type fixtureEntry struct {
	ID       string `json:"id"`
	Cmd      string `json:"cmd"`
	Expected string `json:"expected"`
}

func loadFixture(t *testing.T) []fixtureEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "registered_commands.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc struct {
		Entries []fixtureEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(doc.Entries) != 5 {
		t.Fatalf("fixture holds %d entries, want the 5 registered commands", len(doc.Entries))
	}
	return doc.Entries
}

func loadPythonReference(t *testing.T) (comspec string, lines map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "python_reference.json"))
	if err != nil {
		t.Fatalf("read python reference: %v", err)
	}
	var doc struct {
		Comspec      string            `json:"comspec"`
		CommandLines map[string]string `json:"command_lines"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse python reference: %v", err)
	}
	return doc.Comspec, doc.CommandLines
}

// TestByteIdentityAgainstDesignTable is 0008-L 6.1 steps 1-2: every registered
// command must assemble to the string fixed by the design, byte for byte.
func TestByteIdentityAgainstDesignTable(t *testing.T) {
	for _, entry := range loadFixture(t) {
		t.Run(entry.ID, func(t *testing.T) {
			want := strings.Replace(entry.Expected, "{C}", measuredShell, 1)
			got := WindowsShellCommandLine(measuredShell, entry.Cmd)
			if got != want {
				t.Fatalf("command line differs\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestByteIdentityAgainstPythonReference compares against what the previous
// implementation actually handed to CreateProcess, captured by intercepting the
// call. The design table is a transcription; this is the measurement, and it is
// the one that decides portability. The capture tool is gone with the
// implementation it measured — testdata/python_reference.json is a fixed
// contract value now, not a regenerated one.
func TestByteIdentityAgainstPythonReference(t *testing.T) {
	comspec, reference := loadPythonReference(t)
	if comspec == "" {
		t.Fatal("python reference has no comspec recorded")
	}
	entries := loadFixture(t)
	if len(reference) != len(entries) {
		t.Fatalf("reference holds %d command lines, fixture holds %d", len(reference), len(entries))
	}
	for _, entry := range entries {
		t.Run(entry.ID, func(t *testing.T) {
			want, ok := reference[entry.ID]
			if !ok {
				t.Fatalf("no python reference for %s", entry.ID)
			}
			got := WindowsShellCommandLine(comspec, entry.Cmd)
			if got != want {
				t.Fatalf("command line differs from the running implementation\n go: %q\n py: %q", got, want)
			}
		})
	}
}

// TestNoEscapingApplied is absolute rule 1: the user's quotes stay nested and
// unescaped. A `\"` anywhere means cmd.exe would receive a literal backslash
// and every registered command would fail to launch (0004-NR 3.5).
func TestNoEscapingApplied(t *testing.T) {
	for _, entry := range loadFixture(t) {
		t.Run(entry.ID, func(t *testing.T) {
			got := WindowsShellCommandLine(measuredShell, entry.Cmd)
			if strings.Contains(got, `\"`) {
				t.Fatalf("backslash-quote escape present: %q", got)
			}
			if strings.Count(got, `"`) != strings.Count(entry.Cmd, `"`)+2 {
				t.Fatalf("quote count changed: cmd has %d, line has %d (%q)",
					strings.Count(entry.Cmd, `"`), strings.Count(got, `"`), got)
			}
			inner := UTF8Prefix + entry.Cmd
			if !strings.Contains(got, inner) {
				t.Fatalf("user command was not carried verbatim: %q", got)
			}
		})
	}
}

// TestPrefixSeparatorIsSingleAmpersand is absolute rule 4: chcp may fail and the
// user command must still run.
func TestPrefixSeparatorIsSingleAmpersand(t *testing.T) {
	if UTF8Prefix != "chcp 65001 >nul & " {
		t.Fatalf("prefix changed: %q", UTF8Prefix)
	}
	if strings.Contains(UTF8Prefix, "&&") {
		t.Fatalf("prefix uses conditional sequencing: %q", UTF8Prefix)
	}
	line := WindowsShellCommandLine(measuredShell, "whoami")
	if want := measuredShell + ` /c "chcp 65001 >nul & whoami"`; line != want {
		t.Fatalf("got %q, want %q", line, want)
	}
}

func TestResolveShellPath(t *testing.T) {
	t.Run("comspec wins", func(t *testing.T) {
		t.Setenv("ComSpec", `D:\other\cmd.exe`)
		t.Setenv("SystemRoot", `C:\windows`)
		if got := ResolveShellPath(); got != `D:\other\cmd.exe` {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("falls back to systemroot", func(t *testing.T) {
		t.Setenv("ComSpec", "")
		t.Setenv("SystemRoot", `C:\windows`)
		if got := ResolveShellPath(); got != `C:\windows\System32\cmd.exe` {
			t.Fatalf("got %q", got)
		}
	})
}

// TestPOSIXArgv pins the non-Windows path: shell delegation is kept, the code
// page prefix is not added (0008-L 2.1).
func TestPOSIXArgv(t *testing.T) {
	cmd := `echo "hi there"`
	want := []string{"/bin/sh", "-c", cmd}
	if got := POSIXArgv(cmd); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(POSIXArgv(cmd), " "), "chcp") {
		t.Fatal("code page prefix leaked into the POSIX argv")
	}
}
