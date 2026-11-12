package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLogFilenameMigrationPreservesLegacyCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")
	s := openAt(t, path)
	defer s.Close()
	if _, err := s.DB().Exec(migrationsDDL); err != nil {
		t.Fatal(err)
	}
	migrations, err := s.adapter.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	// Pre-upgrade database already has all six earlier migrations.
	for _, migration := range migrations[:6] {
		if err := s.applyOne(migration); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.DB().Exec(`INSERT INTO runner_entry(id,label,cmd,env,created_at,updated_at,auto_start,run_as_json)
    VALUES('proc_aaaa0001','FlowGate-Dev','echo old','{}','2026-11-01T00:00:00.000Z','2026-11-01T00:00:00.000Z',0,'')`)
	if err != nil {
		t.Fatal(err)
	}
	a, b, err := s.Migrate()
	if err != nil || a != 6 || b != 2 {
		t.Fatalf("migration result: %d, %d, %v", a, b, err)
	}
	rows, err := s.ListEntries()
	if err != nil || len(rows) != 1 || rows[0].LogName != "" || rows[0].Label != "FlowGate-Dev" {
		t.Fatalf("old row changed: %+v %v", rows, err)
	}
	row := rows[0]
	row.LogName = "FlowGate-Dev"
	if err := s.SaveEntry(row); err != nil {
		t.Fatal(err)
	}
	copy, err := s.ListEntries()
	if err != nil || len(copy) != 1 || copy[0].LogName != "FlowGate-Dev" || !copy[0].CreatedAt.Equal(row.CreatedAt) {
		t.Fatalf("new binding: %+v %v", copy, err)
	}
	if !copy[0].CreatedAt.Before(time.Now()) {
		t.Fatal("creation timestamp was rewritten")
	}
}
