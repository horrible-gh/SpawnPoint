package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/store"
)

func TestLogNameIsHumanReadableWithoutInternalID(t *testing.T) {
	m, dir := testManager(t, nil)
	tests := []struct{ label, want string }{
		{"FlowGate-Dev", "FlowGate-Dev.log"},
		{"flowgate-dev", "flowgate-dev (2).log"},
		{"FlowGate-Dev", "FlowGate-Dev (3).log"},
		{"CON", "Process-CON.log"},
		{"SpawnPoint", "SpawnPoint (process).log"},
		{"a/../b\\c:*?", "a_.._b_c___.log"},
		{"개발 서버", "개발 서버.log"},
	}
	for _, tc := range tests {
		info, _ := m.RegisterOnly(tc.label, "echo hello", nil, nil)
		if got := filepath.Base(m.LogPath(info.ID)); got != tc.want {
			t.Errorf("label %q: %q, want %q", tc.label, got, tc.want)
		}
	}
	// An orphan from a deleted process must not be overwritten when its label
	// is reused (including a numbered archive without a current .log file).
	if err := os.WriteFile(filepath.Join(dir, "orphan.log.1"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := m.RegisterOnly("orphan", "echo hello", nil, nil)
	if got := filepath.Base(m.LogPath(info.ID)); got != "orphan (2).log" {
		t.Fatalf("orphan collision: %s", got)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "orphan.log.1")); err != nil || string(data) != "keep" {
		t.Fatal("orphan was altered")
	}
}

func TestLegacyLogAndArchivesMigrateAndStayBoundInDatabase(t *testing.T) {
	m, dir := testManager(t, nil)
	path := filepath.Join(t.TempDir(), "app.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	id := "proc_aa000001"
	row := store.RunnerEntry{ID: id, Label: "FlowGate-Dev", Cmd: "echo test", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.SaveEntry(row); err != nil {
		t.Fatal(err)
	}
	for suffix, text := range map[string]string{"": "current", ".1": "archive one", ".2": "archive two", ".10": "oldest archived", ".rotating": "pending"} {
		if err := os.WriteFile(filepath.Join(dir, id+".log"+suffix), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.registry = s
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	base := m.LogPath(id)
	if got := filepath.Base(base); got != "FlowGate-Dev.log" {
		t.Fatal(got)
	}
	for suffix, want := range map[string]string{"": "current", ".1": "archive one", ".2": "archive two", ".10": "oldest archived", ".rotating": "pending"} {
		data, err := os.ReadFile(base + suffix)
		if err != nil || string(data) != want {
			t.Errorf("migrated %s: %q, %v", suffix, data, err)
		}
		if _, err := os.Stat(filepath.Join(dir, id+".log"+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("old filename remains %s: %v", suffix, err)
		}
	}
	name := "FlowGate-Production"
	if _, persisted, ok := m.Update(id, &name, nil, nil, nil); !ok || !persisted {
		t.Fatalf("Update: %v %v", persisted, ok)
	}
	newBase := m.LogPath(id)
	if got := filepath.Base(newBase); got != "FlowGate-Production.log" {
		t.Fatal(got)
	}
	for _, suffix := range []string{"", ".1", ".2", ".10", ".rotating"} {
		if _, err := os.Stat(newBase + suffix); err != nil {
			t.Errorf("rename lost file %s: %v", suffix, err)
		}
		if _, err := os.Stat(base + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("old name remains %s", suffix)
		}
	}
	rows, err := s.ListEntries()
	if err != nil || len(rows) != 1 || rows[0].LogName != "FlowGate-Production" {
		t.Fatalf("DB log name: %+v %v", rows, err)
	}
	// A service restart resolves the same log even when the display name was
	// changed after the original registration.
	m2, _ := testManager(t, s)
	// Use the same log directory while keeping the operations log separate.
	m2.logDir = dir
	if _, err := m2.Restore(); err != nil {
		t.Fatal(err)
	}
	if m2.LogPath(id) != newBase {
		t.Fatalf("reboot log path drifted: %s", m2.LogPath(id))
	}
	if persisted, found := m2.Delete(id); !persisted || !found {
		t.Fatalf("delete %v %v", persisted, found)
	}
	for _, suffix := range []string{"", ".1", ".2", ".10", ".rotating"} {
		if _, err := os.Stat(newBase + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("delete left %s: %v", suffix, err)
		}
	}
}

func TestLegacyCollisionMigratesToNumberedFriendlyNames(t *testing.T) {
	reg := &fakeRegistry{rows: []store.RunnerEntry{
		{ID: "proc_00000001", Label: "job", Cmd: "echo one"},
		{ID: "proc_00000002", Label: "job", Cmd: "echo two"},
	}}
	m, dir := testManager(t, reg)
	for _, row := range reg.rows {
		if err := os.WriteFile(filepath.Join(dir, row.ID+".log"), []byte(row.ID), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	for i, row := range reg.rows {
		want := "job.log"
		if i == 1 {
			want = "job (2).log"
		}
		if got := filepath.Base(m.LogPath(row.ID)); got != want {
			t.Errorf("%s log name %s != %s", row.ID, got, want)
		}
		if b, err := os.ReadFile(m.LogPath(row.ID)); err != nil || string(b) != row.ID {
			t.Errorf("%s lost its own log: %q, %v", row.ID, b, err)
		}
	}
}

func TestDeferredRenameWhileProcessIsRunning(t *testing.T) {
	m, _ := testManager(t, nil)
	info, _ := m.RegisterOnly("OldWorker", "echo worker", nil, nil)
	oldPath := m.LogPath(info.ID)
	if err := os.WriteFile(oldPath, []byte("old log content"), 0600); err != nil {
		t.Fatal(err)
	}
	e, _ := m.entry(info.ID)
	e.mu.Lock()
	e.status = StatusRunning
	e.mu.Unlock()
	newLabel := "RenamedWorker"
	m.Update(info.ID, &newLabel, nil, nil, nil)
	if m.LogPath(info.ID) != oldPath {
		t.Fatal("running log path moved while collector may hold an open handle")
	}
	e.mu.Lock()
	e.status = StatusStopped
	e.mu.Unlock()
	m.MaintainLogs(time.Now())
	if got := filepath.Base(m.LogPath(info.ID)); got != "RenamedWorker.log" {
		t.Fatal(got)
	}
	if b, err := os.ReadFile(m.LogPath(info.ID)); err != nil || string(b) != "old log content" {
		t.Fatalf("history lost: %q %v", b, err)
	}
}

func TestLogNameMigrationRollsBackWhenDatabaseCannotPersist(t *testing.T) {
	reg := &fakeRegistry{rows: []store.RunnerEntry{{ID: "proc_00000001", Label: "SafeName", Cmd: "echo ok"}}, saveErr: errors.New("database locked")}
	m, dir := testManager(t, reg)
	old := filepath.Join(dir, "proc_00000001.log")
	if err := os.WriteFile(old, []byte("must survive"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if m.LogPath("proc_00000001") != old {
		t.Fatal("mapping changed despite persistence failure")
	}
	if b, err := os.ReadFile(old); err != nil || string(b) != "must survive" {
		t.Errorf("lost existing history: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "SafeName.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new name exists despite failed persistence")
	}
}

func TestUnsafeStoredLogNameNeverEscapesLogDirectory(t *testing.T) {
	reg := &fakeRegistry{rows: []store.RunnerEntry{{ID: "proc_00000001", Label: "Safe", LogName: "../../outside", Cmd: "echo ok"}}}
	m, dir := testManager(t, reg)
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.LogPath("proc_00000001"), dir+string(filepath.Separator)) {
		t.Fatal("unsafe log path")
	}
}
