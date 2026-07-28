package runner

import (
	"runtime"
	"sort"
	"strings"
)

// ForcedChildEnv is `forced_child_env` (0008-L 1.2).
//
// It is not a convenience and must not be dropped. Every command currently
// registered starts a Python server, and without these two variables the child
// falls back to the console code page for its own output — which changes what
// lands in the child log and what those servers write to their own files
// (0004-NR 3.4).
var ForcedChildEnv = map[string]string{
	"PYTHONIOENCODING": "utf-8",
	"PYTHONUTF8":       "1",
}

// MergeEnv builds the child's environment block in the order fixed by
// `env_priority` (0008-L 1.2): inherited, then the forced UTF-8 pair, then the
// user's own entries. Later wins.
//
// Inheriting the whole parent environment is deliberate. E-30 records that a
// child which is itself a SpawnPoint would pick up the server's SPAWNPOINT_*
// settings, and keeps the behaviour anyway: it is what the current
// implementation does, nothing depends on it being otherwise, and a user who
// needs a different value sets it in the entry's own env.
//
// Names are matched the way the platform matches them — case-insensitively on
// Windows — so a user entry spelled `pythonutf8` overrides the forced
// `PYTHONUTF8` rather than being appended beside it. Appending both would leave
// the child to pick one, and which one it picks is not specified anywhere.
func MergeEnv(inherited []string, userEnv map[string]string) []string {
	var block []string
	index := make(map[string]int, len(inherited)+len(userEnv)+len(ForcedChildEnv))

	set := func(name, value string) {
		entry := name + "=" + value
		key := envKey(name)
		if at, ok := index[key]; ok {
			// The position is the inherited block's; the spelling and the value
			// are the last writer's. That is what an override means.
			block[at] = entry
			return
		}
		index[key] = len(block)
		block = append(block, entry)
	}

	for _, kv := range inherited {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			// Windows keeps per-drive working directories in the block as
			// `=C:=C:\work`, which has no name before the first separator.
			// They are passed through untouched rather than parsed; dropping
			// them would change the child's idea of the current directory on
			// every drive but one.
			block = append(block, kv)
			continue
		}
		set(name, value)
	}
	for _, name := range sortedKeys(ForcedChildEnv) {
		set(name, ForcedChildEnv[name])
	}
	for _, name := range sortedKeys(userEnv) {
		set(name, userEnv[name])
	}
	return block
}

// envKey is the identity under which two variable names are the same variable.
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// sortedKeys keeps the output deterministic. Map iteration order would make the
// block differ between runs, which turns any comparison of two child
// environments into a coin toss.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
