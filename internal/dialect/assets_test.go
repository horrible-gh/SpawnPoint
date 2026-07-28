package dialect

import (
	"strings"
	"testing"

	"spawnpoint/internal/sqlsplit"
)

// TestMigrationStatementCounts is items 1 and 2 of 0008-L 6.3, run against the
// scripts that are actually embedded rather than against a copy of them.
//
// The numbers are the ones the design fixed after recounting: five in 001, not
// the six 0004-NR U4 first reported. The sixth was the empty tail after the
// file's last semicolon, which the executable-statement filter removes. If this
// test ever reads six, the filter has stopped working and the migration runner
// is about to hand the engine an empty string.
func TestMigrationStatementCounts(t *testing.T) {
	want := map[string]int{
		"001_create_spawn_tables.sql": 5,
		"002_create_runner_entry.sql": 2,
		"003_expand_spawn_label.sql":  7,
	}

	adapter := mustAdapter(t)
	migrations, err := adapter.Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if len(migrations) != len(want) {
		t.Fatalf("embedded %d migration scripts, expected %d", len(migrations), len(want))
	}
	for _, m := range migrations {
		expected, ok := want[m.Name]
		if !ok {
			t.Errorf("unexpected migration script %q", m.Name)
			continue
		}
		if got := len(sqlsplit.Statements(m.Text)); got != expected {
			t.Errorf("%s: %d executable statements, want %d", m.Name, got, expected)
		}
	}
}

// TestMigrationsAreOrderedByFilename fixes 0004-NR R4. The order is a plain
// string comparison, which is only the right order because every name carries
// three padded digits — the property Validate enforces.
func TestMigrationsAreOrderedByFilename(t *testing.T) {
	migrations, err := mustAdapter(t).Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	for i := 1; i < len(migrations); i++ {
		if migrations[i-1].Name >= migrations[i].Name {
			t.Errorf("out of order: %q before %q", migrations[i-1].Name, migrations[i].Name)
		}
	}
}

// TestMigrationNamesAreBare guards 0004-NR R3 at the source. The name this
// struct carries is written straight into the history table, so if a path ever
// crept in here every existing row would stop matching and all migrations
// would be applied again to a live database.
func TestMigrationNamesAreBare(t *testing.T) {
	migrations, err := mustAdapter(t).Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	for _, m := range migrations {
		if strings.ContainsAny(m.Name, `/\`) {
			t.Errorf("migration name %q carries a path", m.Name)
		}
	}
}

// TestValidateAcceptsEmbeddedAssets is the startup check running against the
// real assets. It has to pass, or the executable cannot start at all.
func TestValidateAcceptsEmbeddedAssets(t *testing.T) {
	if err := mustAdapter(t).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestQueryLoader checks the .json indirection the current implementation uses,
// including that a key which is not listed fails loudly rather than returning
// an empty query — an empty query would be a successful no-op write.
func TestQueryLoader(t *testing.T) {
	adapter := mustAdapter(t)

	cases := []struct {
		group, key string
		contains   string
	}{
		{"spawn_instance", "insert", "INSERT INTO spawn_instance"},
		{"spawn_instance", "find_active_by_key", "created_at > ?"},
		{"spawn_daily_seq", "upsert", "ON CONFLICT(date_part)"},
		{"spawn_daily_seq", "select_last_seq", "SELECT last_seq"},
		{"runner_entry", "upsert", "ON CONFLICT(id)"},
		{"runner_entry", "list", "ORDER BY created_at, id"},
		{"runner_entry", "delete", "DELETE FROM runner_entry"},
	}
	for _, tc := range cases {
		got, err := adapter.Query(tc.group, tc.key)
		if err != nil {
			t.Errorf("Query(%q, %q): %v", tc.group, tc.key, err)
			continue
		}
		if !strings.Contains(got, tc.contains) {
			t.Errorf("Query(%q, %q) does not contain %q:\n%s", tc.group, tc.key, tc.contains, got)
		}
	}

	if _, err := adapter.Query("spawn_instance", "no_such_key"); err == nil {
		t.Error("an unlisted key returned a query instead of an error")
	}
	if _, err := adapter.Query("no_such_group", "insert"); err == nil {
		t.Error("an unknown group returned a query instead of an error")
	}
}

// TestSelectRejectsDriverlessDialects keeps the failure legible. The error
// tables for PostgreSQL and MySQL are complete and tested, but no driver for
// either is linked, and the difference has to be stated rather than discovered
// inside database/sql.
func TestSelectRejectsDriverlessDialects(t *testing.T) {
	for _, kind := range []Kind{PostgreSQL, MySQL} {
		if _, err := Select(kind); err == nil {
			t.Errorf("Select(%q) succeeded without a driver", kind)
		}
	}
	if _, err := Select("nonesuch"); err == nil {
		t.Error("Select of an unknown dialect succeeded")
	}
}

func mustAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := Select(Default)
	if err != nil {
		t.Fatalf("Select(%q): %v", Default, err)
	}
	return a
}
