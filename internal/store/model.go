package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"spawnpoint/internal/timefmt"
)

// Instance is one row of spawn_instance.
//
// The nullable columns are pointers rather than empty strings. The distinction
// matters: request_key participates in the duplicate check, and a row that
// stored "" instead of NULL would be found by a lookup for the empty key.
type Instance struct {
	ID         string
	Requester  string
	Kind       string
	Status     string
	RequestKey *string
	Label      *string
	TTLSeconds int
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// RunnerEntry is one row of runner_entry: a registered command, without any
// live state.
type RunnerEntry struct {
	ID        string
	Label     string
	Cmd       string
	Cwd       *string
	Env       map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// storageTime renders a timestamp for a database column.
func storageTime(t time.Time) string { return timefmt.Storage(t) }

// storageNow is storageTime for the columns the store fills in itself.
func storageNow() string { return timefmt.Storage(time.Now()) }

// readLayouts are the shapes a timestamp column can hold when it is handed
// back as text.
//
// The first is what both implementations write. The second is what SQLite's own
// CURRENT_TIMESTAMP produces, which is the format of every applied_at value in
// the history table. Neither is guessed: they are the two writers that exist.
var readLayouts = []string{
	"2006-01-02T15:04:05.999Z",
	"2006-01-02 15:04:05",
}

// timestamp reads a TIMESTAMP or DATETIME column back into a time.Time.
//
// It is a Scanner rather than a string parse because the driver gets there
// first. modernc.org/sqlite inspects the declared column type and, for DATE,
// DATETIME and TIMESTAMP, parses the stored text into a time.Time before the
// value reaches Scan (rows.go). Scanning such a column into a Go string
// therefore does not return what is on disk — database/sql re-renders the
// time.Time as RFC3339Nano, which drops trailing zeros, so a stored
// `...:10.480Z` comes back as `...:10.48Z` and a parse expecting three fraction
// digits fails on exactly one value in ten.
//
// Accepting both forms also means the code does not depend on the driver
// keeping that behaviour, in either direction.
type timestamp struct{ Time time.Time }

func (ts *timestamp) Scan(src any) error {
	switch v := src.(type) {
	case time.Time:
		ts.Time = v.UTC()
		return nil
	case string:
		return ts.parse(v)
	case []byte:
		return ts.parse(string(v))
	case nil:
		return fmt.Errorf("timestamp column is NULL")
	default:
		return fmt.Errorf("timestamp column has unexpected type %T", src)
	}
}

// parse reads the text forms. A value in neither shape is reported rather than
// silently left at the zero time, which would present an instance as created in
// year 1 and therefore already expired.
func (ts *timestamp) parse(s string) error {
	for _, layout := range readLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			ts.Time = t.UTC()
			return nil
		}
	}
	return fmt.Errorf("%q is not a stored timestamp", s)
}

func nullString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func fromNull(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// encodeEnv renders the environment for the env column.
//
// HTML escaping is switched off. Go's default turns `&`, `<` and `>` into
// escape sequences, which would make the stored text differ from what the
// current implementation writes for the same map — the values decode the same
// either way, but a column that two implementations write differently is one
// that cannot be compared.
func encodeEnv(env map[string]string) (string, error) {
	if env == nil {
		env = map[string]string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// decodeEnv parses the env column, degrading to an empty map on anything it
// cannot read. Matching spawnpoint/storage.py _decode_env: a damaged value
// costs one entry its environment, not the whole restore.
func decodeEnv(raw string) map[string]string {
	if raw == "" {
		return map[string]string{}
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		s, ok := v.(string)
		if !ok {
			// The current implementation coerces with str(), which would turn a
			// number into text. Values are only ever written as strings, so a
			// non-string here means the column was tampered with; dropping the
			// key is closer to "unreadable value" than inventing a rendering.
			continue
		}
		out[k] = s
	}
	return out
}
