package config

import (
	"slices"
	"strings"
	"testing"
)

// env builds a lookup function over a literal map.
func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

func TestDefaultsMatchPythonEntryPoint(t *testing.T) {
	cfg, err := load(env(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 7527 {
		t.Errorf("address = %s, want 127.0.0.1:7527", cfg.Address())
	}
	if cfg.DBPath != "spawnpoint.db" || cfg.LogDir != "logs" {
		t.Errorf("paths = %q %q, want spawnpoint.db logs", cfg.DBPath, cfg.LogDir)
	}
	if len(cfg.APITokens) != 0 {
		t.Errorf("APITokens = %v, want empty (auth off by default)", cfg.APITokens)
	}
	if !cfg.KillChildrenOnExit {
		t.Error("KillChildrenOnExit = false, want true (the deployed default is 1)")
	}
}

func TestAllValuesRead(t *testing.T) {
	cfg, err := load(env(map[string]string{
		EnvHost:               "0.0.0.0",
		EnvPort:               "7527",
		EnvDBPath:             `C:\var\spawnpoint.db`,
		EnvLogDir:             `C:\var\logs`,
		EnvAPITokens:          "tok-a,tok-b",
		EnvKillChildrenOnExit: "0",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Address() != "0.0.0.0:7527" {
		t.Errorf("Address = %q", cfg.Address())
	}
	if cfg.DBPath != `C:\var\spawnpoint.db` || cfg.LogDir != `C:\var\logs` {
		t.Errorf("paths = %q %q", cfg.DBPath, cfg.LogDir)
	}
	if !slices.Equal(cfg.APITokens, []string{"tok-a", "tok-b"}) {
		t.Errorf("APITokens = %v", cfg.APITokens)
	}
	if cfg.KillChildrenOnExit {
		t.Error("KillChildrenOnExit = true, want false for \"0\"")
	}
}

// The deployed `_tokens_from_env` strips each entry and drops the empty ones.
// A trailing comma is common in shell configuration and must not create a
// blank token, which would otherwise authorise an empty Bearer value.
func TestTokenListTrimsAndDropsBlanks(t *testing.T) {
	cfg, err := load(env(map[string]string{EnvAPITokens: " tok-a , , tok-b ,"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !slices.Equal(cfg.APITokens, []string{"tok-a", "tok-b"}) {
		t.Fatalf("APITokens = %v", cfg.APITokens)
	}
	if !cfg.AuthEnabled() || cfg.AuthMode() != "enabled" {
		t.Errorf("AuthMode = %q, want enabled", cfg.AuthMode())
	}
}

// A variable set to only separators leaves no usable token, which is the same
// state as unset: authentication off (0007-P 0.1 local default).
func TestBlankTokenListDisablesAuth(t *testing.T) {
	for _, raw := range []string{"", "   ", ",", " , , "} {
		cfg, err := load(env(map[string]string{EnvAPITokens: raw}))
		if err != nil {
			t.Fatalf("load(%q): %v", raw, err)
		}
		if cfg.AuthEnabled() || cfg.AuthMode() != "disabled" {
			t.Errorf("AuthMode(%q) = %q, want disabled", raw, cfg.AuthMode())
		}
	}
}

// The negative spellings come from the previous entry point; everything else is true.
func TestKillChildrenSpellings(t *testing.T) {
	off := []string{"0", "false", "FALSE", "no", "Off", " off "}
	on := []string{"1", "true", "yes", "on", "anything"}
	for _, raw := range off {
		cfg, err := load(env(map[string]string{EnvKillChildrenOnExit: raw}))
		if err != nil {
			t.Fatalf("load(%q): %v", raw, err)
		}
		if cfg.KillChildrenOnExit {
			t.Errorf("%q: KillChildrenOnExit = true, want false", raw)
		}
	}
	for _, raw := range on {
		cfg, err := load(env(map[string]string{EnvKillChildrenOnExit: raw}))
		if err != nil {
			t.Fatalf("load(%q): %v", raw, err)
		}
		if !cfg.KillChildrenOnExit {
			t.Errorf("%q: KillChildrenOnExit = false, want true", raw)
		}
	}
}

// 0008-L 3.2: an unusable configuration stops the process with
// exit_code_unrecoverable before the operations log is even opened, so the
// error text is the only diagnostic. It has to name the variable.
func TestInvalidPortIsRejected(t *testing.T) {
	for _, raw := range []string{"eight-thousand", "0", "-1", "65536", "8091.5"} {
		_, err := load(env(map[string]string{EnvPort: raw}))
		if err == nil {
			t.Fatalf("load(%s=%q) succeeded, want error", EnvPort, raw)
		}
		if !strings.Contains(err.Error(), EnvPort) {
			t.Errorf("error %q does not name %s", err, EnvPort)
		}
	}
}

// A whitespace-only value must not become the effective setting: an empty log
// directory or database path fails far from its cause.
func TestBlankValuesFallBackToDefaults(t *testing.T) {
	cfg, err := load(env(map[string]string{
		EnvHost:   "   ",
		EnvPort:   " ",
		EnvDBPath: "\t",
		EnvLogDir: " ",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Host != DefaultHost || cfg.Port != DefaultPort ||
		cfg.DBPath != DefaultDBPath || cfg.LogDir != DefaultLogDir {
		t.Fatalf("blank values did not fall back: %+v", cfg)
	}
}

// Address has to bracket IPv6 literals or the listener gets a malformed target.
func TestAddressBracketsIPv6(t *testing.T) {
	cfg, err := load(env(map[string]string{EnvHost: "::1", EnvPort: "9000"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Address() != "[::1]:9000" {
		t.Fatalf("Address = %q", cfg.Address())
	}
}
