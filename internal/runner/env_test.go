package runner

import (
	"runtime"
	"strings"
	"testing"
)

// lookup finds a variable in a rendered environment block, the way the child
// would: case-insensitively on Windows.
func lookup(block []string, name string) (string, int) {
	found, count := "", 0
	for _, kv := range block {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if envKey(k) == envKey(name) {
			found, count = v, count+1
		}
	}
	return found, count
}

// 0008-L 1.2 `env_priority`: inherited < forced UTF-8 < the entry's own env.
func TestMergeEnvPriority(t *testing.T) {
	inherited := []string{"PATH=/bin", "PYTHONUTF8=0", "KEEP=yes"}
	user := map[string]string{"PYTHONIOENCODING": "cp949", "EXTRA=": "", "OWN": "1"}

	block := MergeEnv(inherited, user)

	// The forced pair beats what was inherited...
	if v, n := lookup(block, "PYTHONUTF8"); v != "1" || n != 1 {
		t.Errorf("PYTHONUTF8 = %q (%d entries), want \"1\" once", v, n)
	}
	// ...and the entry's own env beats the forced pair.
	if v, n := lookup(block, "PYTHONIOENCODING"); v != "cp949" || n != 1 {
		t.Errorf("PYTHONIOENCODING = %q (%d entries), want \"cp949\" once", v, n)
	}
	if v, _ := lookup(block, "PATH"); v != "/bin" {
		t.Errorf("PATH = %q, want the inherited value", v)
	}
	if v, _ := lookup(block, "KEEP"); v != "yes" {
		t.Errorf("KEEP = %q, want the inherited value", v)
	}
	if v, _ := lookup(block, "OWN"); v != "1" {
		t.Errorf("OWN = %q, want \"1\"", v)
	}
}

// 0008-L 1.2 `forced_child_env`: both variables are always present. Dropping
// them changes what every registered command writes, because all five of them
// start a Python server (0004-NR 3.4).
func TestForcedEnvIsAlwaysPresent(t *testing.T) {
	block := MergeEnv(nil, nil)
	for name, want := range ForcedChildEnv {
		if v, n := lookup(block, name); v != want || n != 1 {
			t.Errorf("%s = %q (%d entries), want %q once", name, v, n, want)
		}
	}
}

// A user entry differing only in case must override rather than sit beside the
// forced value. Two entries for one name leave the child to pick, and which one
// it picks is not specified anywhere.
func TestMergeEnvCaseHandlingMatchesThePlatform(t *testing.T) {
	block := MergeEnv([]string{"Path=/bin"}, map[string]string{"pythonutf8": "0", "PATH": "/usr/bin"})

	_, pathCount := lookup(block, "PATH")
	v, utf8Count := lookup(block, "PYTHONUTF8")

	if runtime.GOOS == "windows" {
		if pathCount != 1 {
			t.Errorf("PATH appears %d times, want 1: Windows treats Path and PATH as one variable", pathCount)
		}
		if utf8Count != 1 || v != "0" {
			t.Errorf("PYTHONUTF8 = %q (%d entries), want \"0\" once", v, utf8Count)
		}
		return
	}
	// POSIX names are case sensitive, so these really are different variables.
	if pathCount != 1 || utf8Count != 1 {
		t.Errorf("PATH ×%d, PYTHONUTF8 ×%d, want one each", pathCount, utf8Count)
	}
	if v != "1" {
		t.Errorf("PYTHONUTF8 = %q, want the forced \"1\": `pythonutf8` is a different name here", v)
	}
}

// Windows keeps per-drive working directories in the block as `=C:=C:\work`.
// They have no name before the first separator and must survive untouched;
// dropping them changes the child's current directory on every drive but one.
func TestMergeEnvKeepsDriveEntries(t *testing.T) {
	block := MergeEnv([]string{`=C:=C:\work`, "PATH=/bin"}, nil)
	for _, kv := range block {
		if kv == `=C:=C:\work` {
			return
		}
	}
	t.Errorf("the drive entry was dropped: %q", block)
}

// The block has to be the same on every run. Map iteration order would make two
// otherwise identical children differ, which turns any comparison between them
// into a coin toss.
func TestMergeEnvIsDeterministic(t *testing.T) {
	user := map[string]string{"A": "1", "B": "2", "C": "3", "D": "4", "E": "5"}
	first := strings.Join(MergeEnv([]string{"PATH=/bin"}, user), "\x00")
	for i := 0; i < 20; i++ {
		if got := strings.Join(MergeEnv([]string{"PATH=/bin"}, user), "\x00"); got != first {
			t.Fatalf("run %d produced a different block:\n%q\n%q", i, first, got)
		}
	}
}
