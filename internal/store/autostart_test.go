package store

import (
	"path/filepath"
	"testing"
)

func TestAutoStartMigrationPreservesLegacyEntryAndDefaultsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s := openAt(t, path)
	if _, err := s.DB().Exec(migrationsDDL); err != nil {
		t.Fatal(err)
	}
	scripts, err := s.adapter.Migrations()
	if err != nil || len(scripts) != 4 {
		t.Fatalf("migration list: %d, %v", len(scripts), err)
	}
	for _, script := range scripts[:3] {
		if err := s.applyOne(script); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.DB().Exec(`INSERT INTO runner_entry (id, label, cmd, cwd, env, created_at, updated_at)
		VALUES ('proc_12345678', 'legacy', 'echo hello', NULL, '{"KEEP":"yes"}',
		'2026-08-01T00:00:00.000Z', '2026-08-01T00:00:00.000Z')`)
	if err != nil {
		t.Fatal(err)
	}
	already, applied, err := s.Migrate()
	if err != nil || already != 3 || applied != 1 {
		t.Fatalf("upgrade = %d, %d, %v", already, applied, err)
	}
	rows, err := s.ListEntries()
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %v, %v", rows, err)
	}
	row := rows[0]
	if row.AutoStart || row.Label != "legacy" || row.Cmd != "echo hello" || row.Cwd != nil || row.Env["KEEP"] != "yes" {
		t.Fatalf("legacy entry changed: %+v", row)
	}
	created := row.CreatedAt
	row.AutoStart = true
	if err := s.SaveEntry(row); err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened := openAt(t, path)
	already, applied, err = reopened.Migrate()
	if err != nil || already != 4 || applied != 0 {
		t.Fatalf("second migration = %d, %d, %v", already, applied, err)
	}
	rows, err = reopened.ListEntries()
	if err != nil || len(rows) != 1 || !rows[0].AutoStart || !rows[0].CreatedAt.Equal(created) {
		t.Fatalf("enabled preference did not survive reopen: %+v, %v", rows, err)
	}
	row = rows[0]
	row.AutoStart = false
	if err := reopened.SaveEntry(row); err != nil {
		t.Fatal(err)
	}
	rows, err = reopened.ListEntries()
	if err != nil || len(rows) != 1 || rows[0].AutoStart {
		t.Fatalf("could not persist off: %+v, %v", rows, err)
	}
}

func TestAutoStartFreshDatabaseAndConstraint(t *testing.T) {
	s := openTemp(t)
	already, applied, err := s.Migrate()
	if err != nil || already != 0 || applied != 4 {
		t.Fatalf("fresh migration = %d, %d, %v", already, applied, err)
	}
	if err := s.SaveEntry(RunnerEntry{ID: "proc_12345678", Label: "new", Cmd: "echo hi"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListEntries()
	if err != nil || len(rows) != 1 || rows[0].AutoStart {
		t.Fatalf("default = %+v, %v", rows, err)
	}
	if _, err := s.DB().Exec("UPDATE runner_entry SET auto_start = 2"); err == nil {
		t.Fatal("invalid boolean accepted by the database")
	}
}
