// Package config resolves SpawnPoint's settings.
//
// Environment variables are the only source. There are no command-line options
// and none will be added: 0008-L 2.14 relies on the command line being free so
// the executable can decide service mode versus console mode by asking the
// operating system, and 0004-NR F10 recorded the env-only contract as current
// behaviour. Defaults and parsing rules match the previous entry point so an
// existing deployment keeps working unchanged.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Defaults, identical to the previous entry point's.
const (
	DefaultHost   = "127.0.0.1"
	DefaultPort   = 8091
	DefaultDBPath = "spawnpoint.db"
	DefaultLogDir = "logs"
)

// Environment variable names.
const (
	EnvHost               = "SPAWNPOINT_HOST"
	EnvPort               = "SPAWNPOINT_PORT"
	EnvDBPath             = "SPAWNPOINT_DB_PATH"
	EnvLogDir             = "SPAWNPOINT_LOG_DIR"
	EnvAPITokens          = "SPAWNPOINT_API_TOKENS"
	EnvKillChildrenOnExit = "SPAWNPOINT_KILL_CHILDREN_ON_EXIT"
)

// Config is the resolved settings. It is read-only after Load.
type Config struct {
	Host string
	Port int
	// DBPath is the SQLite file. The rewrite opens the existing file rather
	// than creating a new one (0006-D 2.3).
	DBPath string
	// LogDir holds both the child logs (<id>.log) and the operations log.
	LogDir string
	// APITokens is the allowed Bearer token list. Empty means authentication
	// is switched off — the local default (0007-P 0.1).
	APITokens []string
	// KillChildrenOnExit controls whether shutdown tears the runner's children
	// down (0008-L 2.4.1 step ③).
	KillChildrenOnExit bool
}

// Load resolves the configuration from the process environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

// load takes the lookup function so tests do not have to mutate the process
// environment.
func load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Host:               DefaultHost,
		Port:               DefaultPort,
		DBPath:             DefaultDBPath,
		LogDir:             DefaultLogDir,
		KillChildrenOnExit: true,
	}

	if v, ok := setting(getenv, EnvHost); ok {
		cfg.Host = v
	}
	if v, ok := setting(getenv, EnvPort); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %q is not a number", EnvPort, v)
		}
		// 0 would ask the operating system to pick a port, which makes the
		// service unreachable at a known address; reject it with the rest of
		// the out-of-range values.
		if port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("%s: %d is out of range 1-65535", EnvPort, port)
		}
		cfg.Port = port
	}
	if v, ok := setting(getenv, EnvDBPath); ok {
		cfg.DBPath = v
	}
	if v, ok := setting(getenv, EnvLogDir); ok {
		cfg.LogDir = v
	}
	cfg.APITokens = splitTokens(getenv(EnvAPITokens))
	if raw := strings.TrimSpace(getenv(EnvKillChildrenOnExit)); raw != "" {
		cfg.KillChildrenOnExit = truthy(raw)
	}
	return cfg, nil
}

// setting reports a variable as present only when it carries a non-blank value.
// A variable set to whitespace falls back to the default rather than producing
// an empty host or path, which would fail much later and less legibly.
func setting(getenv func(string) string, name string) (string, bool) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return "", false
	}
	return v, true
}

// splitTokens matches the previous `_tokens_from_env`: comma separated, trimmed,
// blanks dropped. Order is preserved but never relied upon.
func splitTokens(raw string) []string {
	var tokens []string
	for _, part := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(part); t != "" {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

// truthy matches the previous `_kill_children_on_exit`: anything except the five
// negative spellings enables the behaviour.
func truthy(raw string) bool {
	switch strings.ToLower(raw) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// AuthEnabled reports whether requests are checked against the token list.
func (c Config) AuthEnabled() bool { return len(c.APITokens) > 0 }

// AuthMode is the value of the `auth` field in the `start` operations record
// (0007-P [서비스 기동]).
func (c Config) AuthMode() string {
	if c.AuthEnabled() {
		return "enabled"
	}
	return "disabled"
}

// Address is the host:port the listener binds to.
func (c Config) Address() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}
