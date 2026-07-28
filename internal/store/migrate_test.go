package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/dialect"
)

// schemaObject mirrors one row of the reference dump produced by
// tools/pyref/dump_schema.py.
type schemaObject struct {
	Type    string  `json:"type"`
	Name    string  `json:"name"`
	TblName string  `json:"tbl_name"`
	SQL     *string `json:"sql"`
}

type schemaDump struct {
	Objects    []schemaObject `json:"objects"`
	Migrations []string       `json:"migrations"`
}

// TestMigrateProducesThePythonSchema is item 4 of 0008-L 6.3.
//
// The reference is not a description of the schema, it is the schema the
// current implementation actually produced, captured by running it
// (tools/pyref/dump_schema.py). Comparing against it means the check covers
// things nobody thought to assert: the exact text of every constraint, the
// partial index predicate, the column defaults, and the definition of the
// history table itself.
func TestMigrateProducesThePythonSchema(t *testing.T) {
	want := loadReference(t)

	s := openTemp(t)
	already, applied, err := s.Migrate()
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if already != 0 {
		t.Errorf("already applied = %d on an empty database, want 0", already)
	}
	if applied != len(want.Migrations) {
		t.Errorf("applied = %d, want %d", applied, len(want.Migrations))
	}

	got := dumpSchema(t, s.DB())
	if !reflect.DeepEqual(got.Objects, want.Objects) {
		reportSchemaDifference(t, got.Objects, want.Objects)
	}
	if !reflect.DeepEqual(got.Migrations, want.Migrations) {
		t.Errorf("history rows:\n got %v\nwant %v", got.Migrations, want.Migrations)
	}
}

// TestMigrateDoesNotReapply is item 3 of 0008-L 6.3, and the rule the whole
// compatibility section exists to protect (0006-D 3.6).
//
// A second run against a database that already carries the history must apply
// nothing at all. The check is not just the row count: applied_at is compared
// too, because a re-applied migration that used INSERT OR REPLACE would keep
// the count identical while quietly rewriting history.
func TestMigrateDoesNotReapply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.db")

	first := openAt(t, path)
	if _, applied, err := first.Migrate(); err != nil || applied != 3 {
		t.Fatalf("first Migrate: applied=%d err=%v", applied, err)
	}
	before := historyRows(t, first.DB())
	first.Close()

	// Reopened rather than reused, so this is the startup path of a server
	// finding a database somebody else already migrated.
	second := openAt(t, path)
	defer second.Close()
	already, applied, err := second.Migrate()
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if already != 3 {
		t.Errorf("already applied = %d, want 3", already)
	}
	if applied != 0 {
		t.Errorf("applied = %d on a database that was already migrated, want 0", applied)
	}
	after := historyRows(t, second.DB())
	if !reflect.DeepEqual(before, after) {
		t.Errorf("history changed:\n before %v\n after  %v", before, after)
	}
}

// TestHistoryRecordsBareFilenames is 0004-NR R3. If a path were recorded, the
// rows the current implementation wrote would not match and all migrations
// would run again — against a database that already has the tables.
func TestHistoryRecordsBareFilenames(t *testing.T) {
	s := openTemp(t)
	if _, _, err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, row := range historyRows(t, s.DB()) {
		if filepath.Base(row.filename) != row.filename {
			t.Errorf("history row %q is a path, not a filename", row.filename)
		}
	}
}

// TestAppliedAtComesFromTheColumnDefault is 0004-NR R8. The rewrite must not
// supply the value: the current implementation lets SQLite's CURRENT_TIMESTAMP
// fill it, and a value written in this codebase's own timestamp format would
// sit alongside the existing rows in a different notation.
func TestAppliedAtComesFromTheColumnDefault(t *testing.T) {
	s := openTemp(t)
	if _, _, err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, row := range historyRows(t, s.DB()) {
		if row.appliedAt == "" {
			t.Errorf("%s: applied_at is empty — the column default did not run", row.filename)
		}
		// CURRENT_TIMESTAMP renders as `YYYY-MM-DD HH:MM:SS`: a space, no
		// fraction, no zone. The format this codebase writes elsewhere has a
		// `T`, three fraction digits and a `Z`, so its presence here would mean
		// the value was supplied rather than defaulted.
		if len(row.appliedAt) != 19 || row.appliedAt[10] != ' ' {
			t.Errorf("%s: applied_at = %q, which is not the column default's format",
				row.filename, row.appliedAt)
		}
	}
}

// TestExpandSpawnLabelMigrationPreservesRows applies 003 over the exact old
// two-migration state. Rebuilding a SQLite table is the only way to widen a
// CHECK constraint, so this pins the property that matters most: existing
// instance data survives the rebuild unchanged.
func TestExpandSpawnLabelMigrationPreservesRows(t *testing.T) {
	s := openTemp(t)
	if _, err := s.DB().Exec(migrationsDDL); err != nil {
		t.Fatalf("create migration history: %v", err)
	}
	scripts, err := s.adapter.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 3 {
		t.Fatalf("migration count = %d, want 3", len(scripts))
	}
	for _, script := range scripts[:2] {
		if err := s.applyOne(script); err != nil {
			t.Fatalf("apply old migration %s: %v", script.Name, err)
		}
	}

	now := time.Date(2026, 7, 28, 5, 32, 10, 482000000, time.UTC)
	before := instance("spwn_20260728_0001f00d", now, ptr("keep-me"))
	before.Label = ptr(strings.Repeat("x", 128))
	if err := s.Insert(before); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	if err := s.applyOne(scripts[2]); err != nil {
		t.Fatalf("apply %s: %v", scripts[2].Name, err)
	}

	got, err := s.FindActiveByKey("keep-me", now.Add(time.Second), 300*time.Second)
	if err != nil || got == nil {
		t.Fatalf("read migrated row: got=%v err=%v", got, err)
	}
	if got.ID != before.ID || got.Label == nil || *got.Label != *before.Label {
		t.Errorf("migrated row = %+v, want %+v", got, before)
	}
}

// TestFailedMigrationLeavesNothingBehind is 0004-NR R6, the window the rewrite
// closes.
//
// The current implementation commits the statements and then records the
// history separately, so a failure between the two leaves a database whose
// schema has changed and whose history does not say so. Here the statements and
// the history entry share one transaction: a script that fails half way through
// must leave the database exactly as it was.
func TestFailedMigrationLeavesNothingBehind(t *testing.T) {
	s := openTemp(t)
	if _, _, err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	before := historyRows(t, s.DB())

	broken := dialect.Migration{
		Name: "003_broken.sql",
		Text: "CREATE TABLE half_applied (a INT);\nTHIS IS NOT SQL;\n",
	}
	if err := s.applyOne(broken); err == nil {
		t.Fatal("a script with an invalid statement was applied without error")
	}

	if tableExists(t, s.DB(), "half_applied") {
		t.Error("the first statement survived the rollback")
	}
	if after := historyRows(t, s.DB()); !reflect.DeepEqual(before, after) {
		t.Errorf("history changed after a failed migration:\n before %v\n after %v", before, after)
	}
}

// TestApplyScriptWithAwkwardQuoting is item 5 of 0008-L 6.3, taken further than
// the item asks: the script is not merely split, it is executed. Counting
// fragments would prove the splitter counted right; executing proves the pieces
// it produced are the statements the engine was meant to receive.
func TestApplyScriptWithAwkwardQuoting(t *testing.T) {
	s := openTemp(t)
	if _, _, err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	script, err := os.ReadFile(filepath.Join("testdata", "awkward_quoting.sql"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := s.applyOne(dialect.Migration{Name: "003_awkward.sql", Text: string(script)}); err != nil {
		t.Fatalf("applyOne: %v", err)
	}

	// Each of these exists only if the semicolon inside a literal, a comment or
	// a quoted identifier was not treated as a separator.
	for _, name := range []string{"quoting_probe", "semi;colon", "note_probe"} {
		if !tableExists(t, s.DB(), name) {
			t.Errorf("table %q was not created — a statement was cut in the wrong place", name)
		}
	}

	var value string
	if err := s.DB().QueryRow(`SELECT note FROM quoting_probe WHERE id = 1`).Scan(&value); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "a;b -- not a comment /* nor this */"; value != want {
		t.Errorf("stored value = %q, want %q", value, want)
	}
}

// --- helpers ------------------------------------------------------------------

func openTemp(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "test.db"))
}

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func loadReference(t *testing.T) schemaDump {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "python_schema.json"))
	if err != nil {
		t.Fatalf("read reference: %v (regenerate with tools/pyref/dump_schema.py)", err)
	}
	var dump schemaDump
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if len(dump.Objects) == 0 || len(dump.Migrations) == 0 {
		t.Fatal("reference is empty")
	}
	return dump
}

// dumpSchema reads the schema back in the same shape, and the same order, as
// the Python generator.
func dumpSchema(t *testing.T, db *sql.DB) schemaDump {
	t.Helper()
	rows, err := db.Query(
		"SELECT type, name, tbl_name, sql FROM sqlite_master" +
			" WHERE name NOT LIKE 'sqlite_autoindex%'" +
			" ORDER BY type, name")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer rows.Close()

	var dump schemaDump
	for rows.Next() {
		var (
			o    schemaObject
			text sql.NullString
		)
		if err := rows.Scan(&o.Type, &o.Name, &o.TblName, &text); err != nil {
			t.Fatalf("read schema: %v", err)
		}
		if text.Valid {
			s := text.String
			o.SQL = &s
		}
		dump.Objects = append(dump.Objects, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	for _, row := range historyRows(t, db) {
		dump.Migrations = append(dump.Migrations, row.filename)
	}
	return dump
}

type historyRow struct {
	filename  string
	appliedAt string
}

// historyRows reads the history table.
//
// applied_at is cast to TEXT on purpose. The column is declared DATETIME, and
// the driver parses such a column into a time.Time before Scan sees it, so
// reading it directly would show the driver's rendering rather than the bytes
// on disk — and the bytes on disk are what has to match the current
// implementation. Casting produces an expression, which has no declared type,
// which is delivered as the raw string.
func historyRows(t *testing.T, db *sql.DB) []historyRow {
	t.Helper()
	rows, err := db.Query("SELECT filename, CAST(applied_at AS TEXT) FROM migrations ORDER BY filename")
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	defer rows.Close()

	var out []historyRow
	for rows.Next() {
		var r historyRow
		if err := rows.Scan(&r.filename, &r.appliedAt); err != nil {
			t.Fatalf("read history: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read history: %v", err)
	}
	return out
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found string
	err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&found)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("look up table %q: %v", name, err)
	}
	return true
}

// reportSchemaDifference names the object that differs instead of printing two
// schemas and leaving the reader to diff them.
func reportSchemaDifference(t *testing.T, got, want []schemaObject) {
	t.Helper()
	index := func(objs []schemaObject) map[string]schemaObject {
		m := make(map[string]schemaObject, len(objs))
		for _, o := range objs {
			m[o.Type+" "+o.Name] = o
		}
		return m
	}
	gotByName, wantByName := index(got), index(want)
	for key, w := range wantByName {
		g, ok := gotByName[key]
		if !ok {
			t.Errorf("%s: missing from the rewrite's schema", key)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s differs:\n got  %s\n want %s", key, text(g.SQL), text(w.SQL))
		}
	}
	for key := range gotByName {
		if _, ok := wantByName[key]; !ok {
			t.Errorf("%s: present in the rewrite's schema but not in the reference", key)
		}
	}
}

func text(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}
