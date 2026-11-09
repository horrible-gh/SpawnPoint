package store

import (
	"path/filepath"
	"testing"
)

func TestRunAsMigrationKeepsExistingCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	s := openAt(t, path)
	if _, err := s.DB().Exec(migrationsDDL); err != nil {
		t.Fatal(err)
	}
	scripts, err := s.adapter.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range scripts[:4] {
		if err := s.applyOne(script); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO runner_entry(id,label,cmd,env,created_at,updated_at,auto_start) VALUES('proc_12345678','legacy','echo legacy','{"KEEP":"yes"}','2026-11-01T00:00:00.000Z','2026-11-01T00:00:00.000Z',1)`); err != nil {
		t.Fatal(err)
	}
	if a, b, err := s.Migrate(); err != nil || a != 4 || b != 1 {
		t.Fatalf("upgrade: %d %d %v", a, b, err)
	}
	rows, err := s.ListEntries()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	row := rows[0]
	if row.RunAsJSON != "" || !row.AutoStart || row.Env["KEEP"] != "yes" {
		t.Fatalf("legacy changed: %+v", row)
	}
	row.RunAsJSON = `{"username":"example","sealed":"encrypted-placeholder"}`
	if err := s.SaveEntry(row); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openAt(t, path)
	rows, err = s.ListEntries()
	if err != nil || len(rows) != 1 || rows[0].RunAsJSON != row.RunAsJSON {
		t.Fatalf("identity did not survive reopen: %v %v", rows, err)
	}
	row.RunAsJSON = ""
	if err := s.SaveEntry(row); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListEntries()
	if err != nil || rows[0].RunAsJSON != "" {
		t.Fatal("switch to service retained credential")
	}
}
