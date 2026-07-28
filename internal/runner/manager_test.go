package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/opslog"
	"spawnpoint/internal/store"
)

// These tests start no processes. The ones that do are in
// manager_live_test.go, behind the same switch the command-line spike
// established.

// fakeRegistry stands in for the store. Using the real one here would make
// every registry test a database test, and the failure path (E-21) is only
// reachable on a real store by breaking a file mid-run.
type fakeRegistry struct {
	rows    []store.RunnerEntry
	saved   []store.RunnerEntry
	deleted []string
	saveErr error
	listErr error
}

func (f *fakeRegistry) SaveEntry(e store.RunnerEntry) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, e)
	return nil
}

func (f *fakeRegistry) DeleteEntry(id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeRegistry) ListEntries() ([]store.RunnerEntry, error) {
	return f.rows, f.listErr
}

func testManager(t *testing.T, registry Registry) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := opslog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return New(dir, log, registry, true), dir
}

func opsLog(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, opslog.FileName))
	if err != nil {
		return ""
	}
	return string(b)
}

// 0008-L 1.2: `proc_` plus eight lowercase hex digits.
func TestIdentifierShape(t *testing.T) {
	m, _ := testManager(t, nil)
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id := m.newID()
		if len(id) != len("proc_")+IDHexDigits {
			t.Fatalf("id %q is %d characters, want %d", id, len(id), len("proc_")+IDHexDigits)
		}
		if !strings.HasPrefix(id, "proc_") {
			t.Fatalf("id %q does not start with proc_", id)
		}
		for _, r := range id[len("proc_"):] {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("id %q has a non lowercase-hex digit %q", id, r)
			}
		}
		if seen[id] {
			t.Fatalf("id %q was handed out twice", id)
		}
		seen[id] = true
	}
}

// 0008-L 2.2 / 4.5: a missing working directory is caught before the shell is
// started. Handed down instead, the failure shape differs per platform and, on
// Windows, the shell starts anyway — which would be reported as a run that died
// rather than a start that never happened.
func TestMissingWorkingDirectoryFailsBeforeTheShellStarts(t *testing.T) {
	m, dir := testManager(t, nil)
	missing := filepath.Join(dir, "no", "such", "directory")

	info, _ := m.Register("bad cwd", "echo hello", &missing, nil)

	if info.Status != StatusFailed {
		t.Errorf("status = %q, want %q", info.Status, StatusFailed)
	}
	if want := "cwd does not exist: " + missing; info.Error != want {
		t.Errorf("error = %q, want %q", info.Error, want)
	}
	// started_at stays null: a timestamp there reads as "it ran and ended
	// instantly", which is a different fault with a different cause.
	if info.StartedAt != nil {
		t.Errorf("started_at = %v, want null", info.StartedAt)
	}
	if info.PID != nil || info.ExitCode != nil {
		t.Errorf("pid = %v, exit_code = %v, want both null", info.PID, info.ExitCode)
	}
	if info.EndedAt == nil {
		t.Error("ended_at is null; the attempt did end")
	}
	if got := opsLog(t, dir); !strings.Contains(got, "ERROR child start failed") {
		t.Errorf("no `child start failed` record:\n%s", got)
	}
	// Nothing ran, so nothing can have been logged for it.
	if _, err := os.Stat(m.LogPath(info.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a child log was created for a start that never happened: %v", err)
	}
}

// 0008-L 2.2 / E-21: the registration is written before the process is started,
// and a write that fails does not stop the start. The user asked for the
// process; refusing to run it because a row could not be written trades a
// recoverable loss for an immediate one.
func TestPersistFailureIsReportedButDoesNotBlockTheStart(t *testing.T) {
	registry := &fakeRegistry{saveErr: errors.New("database is locked")}
	m, dir := testManager(t, registry)
	missing := filepath.Join(dir, "gone")

	// A failing cwd keeps this test process-free; the ordering under test is
	// persist-then-start, and the start's outcome is beside the point.
	_, persisted := m.Register("x", "echo hello", &missing, nil)

	if persisted {
		t.Error("persisted = true, want false")
	}
	got := opsLog(t, dir)
	if !strings.Contains(got, "ERROR runner entry persist failed") {
		t.Errorf("no `runner entry persist failed` record:\n%s", got)
	}
	// The start was attempted regardless, which the start-failure record shows.
	if !strings.Contains(got, "ERROR child start failed") {
		t.Errorf("the start was not attempted after the save failed:\n%s", got)
	}
	if i := strings.Index(got, "persist failed"); i > strings.Index(got, "child start failed") {
		t.Errorf("the save was attempted after the start:\n%s", got)
	}
}

// 0008-L 6.5 / 2.15: restored entries come back registered and nothing more.
// The pid recorded before a restart cannot be shown to still belong to the
// command it was recorded against, so none of it is restored.
func TestRestoreBringsBackRegistrationOnly(t *testing.T) {
	base := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	cwd := "C:/work"
	registry := &fakeRegistry{rows: []store.RunnerEntry{
		{ID: "proc_00000002", Label: "b", Cmd: "cmd b", CreatedAt: base, UpdatedAt: base},
		{ID: "proc_00000001", Label: "a", Cmd: "cmd a", Cwd: &cwd,
			Env: map[string]string{"K": "V"}, CreatedAt: base, UpdatedAt: base},
		{ID: "proc_00000003", Label: "c", Cmd: "cmd c",
			CreatedAt: base.Add(-time.Hour), UpdatedAt: base},
	}}
	m, _ := testManager(t, registry)

	n, err := m.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("restored %d entries, want 3", n)
	}

	got := m.List()
	// Registration time ascending, identifier ascending on a tie (0008-L 6.5).
	wantOrder := []string{"proc_00000003", "proc_00000001", "proc_00000002"}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Errorf("position %d = %s, want %s", i, got[i].ID, want)
		}
	}
	for _, info := range got {
		if info.Status != StatusStopped {
			t.Errorf("%s status = %q, want %q", info.ID, info.Status, StatusStopped)
		}
		if info.PID != nil || info.ExitCode != nil || info.StartedAt != nil || info.EndedAt != nil {
			t.Errorf("%s carries live state: pid=%v exit_code=%v started_at=%v ended_at=%v",
				info.ID, info.PID, info.ExitCode, info.StartedAt, info.EndedAt)
		}
	}
	if got[1].Cwd == nil || *got[1].Cwd != cwd || got[1].Env["K"] != "V" {
		t.Errorf("registration fields were not restored: %+v", got[1])
	}
}

// 0008-L 3.1: update touches the registration and nothing else. Rewriting a
// live process's registration to say something it is not doing would make the
// listing disagree with the machine.
func TestUpdateDoesNotTouchLiveState(t *testing.T) {
	registry := &fakeRegistry{}
	m, _ := testManager(t, registry)
	// The entry is placed directly so this stays process-free. What is under
	// test is that update leaves the live half exactly as it found it, and a
	// hand-built running entry states that half more plainly than a real
	// process would.
	id, pid := "proc_00000001", 4242
	started := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	m.mu.Lock()
	m.entries[id] = &entry{
		id: id, label: "first", cmd: "echo one",
		status: StatusRunning, pid: &pid, startedAt: &started,
	}
	m.mu.Unlock()

	label, cmd := "second", "echo two"
	after, persisted, ok := m.Update(id, &label, &cmd, nil, map[string]string{"A": "1"})
	if !ok {
		t.Fatal("update reported the entry as unknown")
	}
	if !persisted {
		t.Error("the updated registration was not reported as saved")
	}
	if after.Label != "second" || after.Cmd != "echo two" || after.Env["A"] != "1" {
		t.Errorf("registration was not updated: %+v", after)
	}
	if after.Status != StatusRunning || after.PID == nil || *after.PID != pid {
		t.Errorf("live state changed: status=%q pid=%v", after.Status, after.PID)
	}
	if after.StartedAt == nil || !after.StartedAt.Equal(started) {
		t.Errorf("started_at changed to %v", after.StartedAt)
	}
	if len(registry.saved) != 1 {
		t.Errorf("saved %d times, want 1", len(registry.saved))
	}
}

// 0008-L 2.5.1 / 0007-P [삭제]: the log and every archive go, and how many went
// is recorded. An archive left behind would have the next command given that
// identifier append to a stranger's history.
func TestDeleteRemovesEveryLogFile(t *testing.T) {
	registry := &fakeRegistry{}
	m, dir := testManager(t, registry)
	id := "proc_deadbeef"
	m.mu.Lock()
	m.entries[id] = &entry{id: id, label: "x", cmd: "x", status: StatusStopped}
	m.mu.Unlock()

	base := m.LogPath(id)
	// The log plus two of the three archives; the third is absent on purpose,
	// so the count reports what was really removed rather than the ceiling.
	for _, name := range []string{base, base + ".1", base + ".2"} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A file that is not this entry's must survive.
	other := filepath.Join(dir, "proc_cafebabe.log")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !m.Delete(id) {
		t.Fatal("delete reported the entry as unknown")
	}
	for _, name := range []string{base, base + ".1", base + ".2"} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists", filepath.Base(name))
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("an unrelated log was removed: %v", err)
	}
	if registry.deleted[0] != id {
		t.Errorf("deleted %v from the registry, want %s", registry.deleted, id)
	}
	if got := opsLog(t, dir); !strings.Contains(got, "INFO entry deleted id="+id+" logs_removed=3") {
		t.Errorf("no `entry deleted ... logs_removed=3` record:\n%s", got)
	}
}

// E-14: operations on an identifier that is not registered report not found
// rather than inventing an entry.
func TestUnknownIdentifier(t *testing.T) {
	m, _ := testManager(t, nil)
	if _, ok := m.Get("proc_00000000"); ok {
		t.Error("Get found an entry that was never registered")
	}
	if _, ok := m.Run("proc_00000000"); ok {
		t.Error("Run found an entry that was never registered")
	}
	if _, ok := m.Stop("proc_00000000"); ok {
		t.Error("Stop found an entry that was never registered")
	}
	if _, ok := m.Restart("proc_00000000"); ok {
		t.Error("Restart found an entry that was never registered")
	}
	if m.Delete("proc_00000000") {
		t.Error("Delete found an entry that was never registered")
	}
}

// E-17: stopping something that is not running is not an error. It returns the
// state it already had, and records nothing — there was no child to terminate.
func TestStoppingAnIdleEntryIsNotAnError(t *testing.T) {
	m, dir := testManager(t, nil)
	id := "proc_00000001"
	m.mu.Lock()
	m.entries[id] = &entry{id: id, label: "x", cmd: "x", status: StatusStopped}
	m.mu.Unlock()

	info, ok := m.Stop(id)
	if !ok {
		t.Fatal("stop reported the entry as unknown")
	}
	if info.Status != StatusStopped {
		t.Errorf("status = %q, want %q", info.Status, StatusStopped)
	}
	if got := opsLog(t, dir); strings.Contains(got, "child terminated") {
		t.Errorf("a termination was recorded for an entry that was not running:\n%s", got)
	}
}

// 0008-L 2.5.1 / 0004-NR U3: the marker carries the time. The current
// implementation writes a bare `--- run ---`, which makes it impossible to tell
// afterwards which run produced which lines.
func TestMarkerCarriesTheTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proc_00000001.log")
	at := time.Date(2026, 7, 28, 18, 12, 41, 6000, time.FixedZone("KST", 9*3600))

	if err := writeMarker(path, markerRun, at); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(path, markerRestart, at); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	want := "\n--- run 2026-07-28T18:12:41.000006+09:00 ---\n" +
		"\n--- restart 2026-07-28T18:12:41.000006+09:00 ---\n"
	if got != want {
		t.Errorf("markers =\n%q\nwant\n%q", got, want)
	}
	// ASCII only, so the line survives being read back under either encoding.
	for i := 0; i < len(got); i++ {
		if got[i] > 0x7f {
			t.Fatalf("marker byte %d is not ASCII: %q", i, got)
		}
	}
}

// budget caps a wait at whatever is left of the shutdown deadline. Without the
// cap one child could spend the whole sequence's budget on its own.
func TestBudget(t *testing.T) {
	if got := budget(5*time.Second, time.Time{}); got != 5*time.Second {
		t.Errorf("no deadline: got %v, want the full 5s", got)
	}
	if got := budget(5*time.Second, time.Now().Add(-time.Second)); got != 0 {
		t.Errorf("expired deadline: got %v, want 0", got)
	}
	got := budget(5*time.Second, time.Now().Add(time.Second))
	if got <= 0 || got > time.Second {
		t.Errorf("nearly expired deadline: got %v, want (0, 1s]", got)
	}
}
