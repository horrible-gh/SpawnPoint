package store

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"spawnpoint/internal/dialect"
)

func migrated(t *testing.T) *Store {
	t.Helper()
	s := openTemp(t)
	if _, _, err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func ptr(s string) *string { return &s }

func instance(id string, created time.Time, requestKey *string) Instance {
	return Instance{
		ID:         id,
		Requester:  "tester",
		Kind:       "session",
		Status:     "created",
		RequestKey: requestKey,
		Label:      ptr("fixture"),
		TTLSeconds: 3600,
		CreatedAt:  created,
		ExpiresAt:  created.Add(3600 * time.Second),
	}
}

// TestInsertAndReadBack checks the round trip, including that a truncated
// timestamp survives it. The stored form keeps three fraction digits, so the
// value that comes back is the written value truncated to milliseconds — not
// rounded, which is what the dedup window's string comparison depends on
// (0008-L 1.6).
func TestInsertAndReadBack(t *testing.T) {
	s := migrated(t)
	created := time.Date(2026, 7, 28, 5, 32, 10, 482_999_000, time.UTC)
	inst := instance("spwn_20260728_0001abcd", created, ptr("rk-1"))

	if err := s.Insert(inst); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.FindActiveByKey("rk-1", created.Add(time.Second), 300*time.Second)
	if err != nil {
		t.Fatalf("FindActiveByKey: %v", err)
	}
	if got == nil {
		t.Fatal("the instance just written was not found")
	}

	want := created.Truncate(time.Millisecond)
	if !got.CreatedAt.Equal(want) {
		t.Errorf("created_at = %s, want %s (truncated, not rounded)", got.CreatedAt, want)
	}
	if got.ID != inst.ID || got.Requester != inst.Requester || got.Kind != inst.Kind {
		t.Errorf("identity fields differ: %+v", got)
	}
	if got.RequestKey == nil || *got.RequestKey != "rk-1" {
		t.Errorf("request_key = %v, want rk-1", got.RequestKey)
	}
	if got.Label == nil || *got.Label != "fixture" {
		t.Errorf("label = %v, want fixture", got.Label)
	}
	if got.TTLSeconds != 3600 {
		t.Errorf("ttl_seconds = %d, want 3600", got.TTLSeconds)
	}
}

// TestDuplicateIdentifierIsClassified is what the error interpreter exists for.
// The caller retries with a fresh random tail only when it gets this class back
// (0008-L 2.11); anything else and the request fails.
func TestDuplicateIdentifierIsClassified(t *testing.T) {
	s := migrated(t)
	now := time.Now().UTC()
	inst := instance("spwn_20260728_0001abcd", now, ptr("rk-1"))

	if err := s.Insert(inst); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	err := s.Insert(inst)
	if err == nil {
		t.Fatal("the same identifier was accepted twice")
	}
	we, ok := dialect.AsWriteError(err)
	if !ok {
		t.Fatalf("Insert returned a bare error, not a classified one: %v", err)
	}
	if we.Class != dialect.DuplicateKey {
		t.Errorf("class = %q, want %q", we.Class, dialect.DuplicateKey)
	}
	if we.Note != "" {
		t.Errorf("unexpected note %q", we.Note)
	}
}

// TestConstraintViolationIsNotADuplicate keeps the two apart. A rejected value
// must not be retried with a new identifier: the identifier was never the
// problem and the retry would fail identically three more times.
func TestConstraintViolationIsNotADuplicate(t *testing.T) {
	s := migrated(t)
	now := time.Now().UTC()

	bad := instance("spwn_20260728_0002abcd", now, nil)
	bad.Kind = "nonesuch" // violates ck_kind

	err := s.Insert(bad)
	if err == nil {
		t.Fatal("a value outside the allowed kinds was accepted")
	}
	we, ok := dialect.AsWriteError(err)
	if !ok {
		t.Fatalf("Insert returned a bare error: %v", err)
	}
	if we.Class != dialect.Constraint {
		t.Errorf("class = %q, want %q", we.Class, dialect.Constraint)
	}
}

// TestDedupWindowBoundaryIsExclusive fixes the comparison of 0008-L 2.12 as
// strict. A row created exactly at the boundary is outside the window. The
// distinction is one millisecond wide and decides whether a caller gets a new
// instance or is handed an old one, so it is stated in a test rather than left
// to whoever next reads the SQL.
func TestDedupWindowBoundaryIsExclusive(t *testing.T) {
	s := migrated(t)
	const window = 300 * time.Second
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	// Exactly at the boundary: created_at equals the threshold, and the query
	// asks for strictly greater.
	atBoundary := instance("spwn_20260728_0001aaaa", now.Add(-window), ptr("rk-boundary"))
	if err := s.Insert(atBoundary); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.FindActiveByKey("rk-boundary", now, window)
	if err != nil {
		t.Fatalf("FindActiveByKey: %v", err)
	}
	if got != nil {
		t.Errorf("a row exactly at the window boundary was treated as a duplicate: %s", got.ID)
	}

	// One millisecond inside it.
	inside := instance("spwn_20260728_0002aaaa", now.Add(-window).Add(time.Millisecond), ptr("rk-inside"))
	if err := s.Insert(inside); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err = s.FindActiveByKey("rk-inside", now, window)
	if err != nil {
		t.Fatalf("FindActiveByKey: %v", err)
	}
	if got == nil {
		t.Error("a row one millisecond inside the window was not treated as a duplicate")
	}
}

// TestDedupReturnsTheMostRecent covers the tie-break of 0008-L 2.12: several
// rows can share a request key, and the newest is the answer.
func TestDedupReturnsTheMostRecent(t *testing.T) {
	s := migrated(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	older := instance("spwn_20260728_0001bbbb", now.Add(-120*time.Second), ptr("rk-multi"))
	newer := instance("spwn_20260728_0002bbbb", now.Add(-30*time.Second), ptr("rk-multi"))
	for _, inst := range []Instance{older, newer} {
		if err := s.Insert(inst); err != nil {
			t.Fatalf("Insert %s: %v", inst.ID, err)
		}
	}

	got, err := s.FindActiveByKey("rk-multi", now, 300*time.Second)
	if err != nil {
		t.Fatalf("FindActiveByKey: %v", err)
	}
	if got == nil || got.ID != newer.ID {
		t.Errorf("got %v, want the most recent row %s", got, newer.ID)
	}
}

// TestDedupIgnoresRowsWithoutAKey is the first line of 0008-L 2.12: with no
// request key there is no duplicate judgement to make. A row stored with a NULL
// key must not be reachable by looking up the empty string, which is why the
// field is a pointer.
func TestDedupIgnoresRowsWithoutAKey(t *testing.T) {
	s := migrated(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	if err := s.Insert(instance("spwn_20260728_0001cccc", now, nil)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.FindActiveByKey("", now, 300*time.Second)
	if err != nil {
		t.Fatalf("FindActiveByKey: %v", err)
	}
	if got != nil {
		t.Errorf("an empty request key matched a row with no key: %s", got.ID)
	}
}

// TestExplicitEmptyRequestKeyParticipatesInDeduplication keeps "missing" and
// "present but empty" distinct. The HTTP contract permits any string here and
// only omission disables duplicate detection.
func TestExplicitEmptyRequestKeyParticipatesInDeduplication(t *testing.T) {
	s := migrated(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	empty := ""
	inst := instance("spwn_20260728_0001dddd", now.Add(-time.Minute), &empty)
	if err := s.Insert(inst); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.FindActiveByKey("", now, 300*time.Second)
	if err != nil {
		t.Fatalf("FindActiveByKey: %v", err)
	}
	if got == nil || got.ID != inst.ID {
		t.Errorf("empty request key lookup = %v, want %s", got, inst.ID)
	}
}

// TestSpawnLabelConstraintMatchesProtocol closes the mismatch recorded by T6:
// 256 code points are accepted by both validation and storage, while the next
// one is still a classified constraint failure.
func TestSpawnLabelConstraintMatchesProtocol(t *testing.T) {
	s := migrated(t)
	now := time.Now().UTC()

	allowed := instance("spwn_20260728_0001eeee", now, nil)
	allowed.Label = ptr(strings.Repeat("가", 256))
	if err := s.Insert(allowed); err != nil {
		t.Fatalf("256-character label: %v", err)
	}

	tooLong := instance("spwn_20260728_0002eeee", now, nil)
	tooLong.Label = ptr(strings.Repeat("가", 257))
	err := s.Insert(tooLong)
	writeErr, ok := dialect.AsWriteError(err)
	if !ok || writeErr.Class != dialect.Constraint {
		t.Fatalf("257-character label error = %v, want classified constraint", err)
	}
}

// TestNextSeqIncrements checks the counter and that separate dates do not share
// one.
func TestNextSeqIncrements(t *testing.T) {
	s := migrated(t)

	for want := 1; want <= 3; want++ {
		got, err := s.NextSeq("20260728")
		if err != nil {
			t.Fatalf("NextSeq: %v", err)
		}
		if got != want {
			t.Fatalf("NextSeq = %d, want %d", got, want)
		}
	}
	got, err := s.NextSeq("20260729")
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	if got != 1 {
		t.Errorf("a new date started at %d, want 1", got)
	}
}

// TestNextSeqIsAtomic is the reason the upsert and the read are one
// transaction (0008-L 2.11). Split apart, two concurrent callers can both read
// the counter after both increments and be handed the same sequence number —
// which becomes two instances with the same identifier.
func TestNextSeqIsAtomic(t *testing.T) {
	s := migrated(t)
	const callers = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make(map[int]int)
		failed  error
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, err := s.NextSeq("20260728")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = err
				return
			}
			results[seq]++
		}()
	}
	wg.Wait()

	if failed != nil {
		t.Fatalf("NextSeq: %v", failed)
	}
	for seq, count := range results {
		if count > 1 {
			t.Errorf("sequence %d was handed out %d times", seq, count)
		}
	}
	if len(results) != callers {
		t.Errorf("%d distinct sequences from %d callers", len(results), callers)
	}
	for want := 1; want <= callers; want++ {
		if results[want] == 0 {
			t.Errorf("sequence %d was never handed out — the counter skipped", want)
		}
	}
}

// TestRunnerEntryRoundTrip covers save, list and delete, and the restore order
// 0008-L 6.5 requires: registration time ascending, identifier ascending on a
// tie. The entries are written in an order that is neither, so a list that
// simply echoed insertion order would fail.
func TestRunnerEntryRoundTrip(t *testing.T) {
	s := migrated(t)

	entries := []RunnerEntry{
		{ID: "proc_0000000c", Label: "third", Cmd: "echo 3"},
		{ID: "proc_0000000a", Label: "first", Cmd: "echo 1", Cwd: ptr(`C:\work`)},
		{ID: "proc_0000000b", Label: "second", Cmd: "echo 2", Env: map[string]string{
			"PYTHONUTF8": "1",
			"NOTE":       "a & b <c> 한글",
		}},
	}
	for _, e := range entries {
		if err := s.SaveEntry(e); err != nil {
			t.Fatalf("SaveEntry %s: %v", e.ID, err)
		}
	}

	// Registration times are assigned here rather than left to the clock, so
	// the ordering contract is exercised rather than observed: `b` and `c` are
	// given the same instant, which is the tie the identifier has to break, and
	// `a` is given a later one even though it was written second. Neither
	// insertion order nor identifier order alone produces the expected result.
	times := map[string]string{
		"proc_0000000a": "2026-07-28T12:00:02.000Z",
		"proc_0000000b": "2026-07-28T12:00:01.000Z",
		"proc_0000000c": "2026-07-28T12:00:01.000Z",
	}
	for id, ts := range times {
		if _, err := s.DB().Exec("UPDATE runner_entry SET created_at = ? WHERE id = ?", ts, id); err != nil {
			t.Fatalf("set created_at for %s: %v", id, err)
		}
	}

	got, err := s.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("listed %d entries, want 3", len(got))
	}
	wantOrder := []string{"proc_0000000b", "proc_0000000c", "proc_0000000a"}
	for i, id := range wantOrder {
		if got[i].ID != id {
			t.Errorf("position %d is %s, want %s", i, got[i].ID, id)
		}
	}

	byID := map[string]RunnerEntry{}
	for _, e := range got {
		byID[e.ID] = e
	}
	if cwd := byID["proc_0000000a"].Cwd; cwd == nil || *cwd != `C:\work` {
		t.Errorf("cwd = %v, want C:\\work", cwd)
	}
	if cwd := byID["proc_0000000c"].Cwd; cwd != nil {
		t.Errorf("cwd = %v, want nil for an entry saved without one", cwd)
	}
	wantEnv := map[string]string{"PYTHONUTF8": "1", "NOTE": "a & b <c> 한글"}
	if env := byID["proc_0000000b"].Env; !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("env = %v, want %v", env, wantEnv)
	}
	if env := byID["proc_0000000a"].Env; env == nil || len(env) != 0 {
		t.Errorf("env = %v, want an empty map for an entry saved without one", env)
	}

	if err := s.DeleteEntry("proc_0000000b"); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}
	got, err = s.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("listed %d entries after a delete, want 2", len(got))
	}
	// Deleting something that is not there is not an error: the runner keeps
	// its own state, and a delete arriving twice must not fail the second time.
	if err := s.DeleteEntry("proc_0000000b"); err != nil {
		t.Errorf("deleting an absent entry: %v", err)
	}
}

// TestSaveEntryUpdatesInPlace checks the upsert half. An update must not move
// the entry in the restore order, which means created_at has to survive it.
func TestSaveEntryUpdatesInPlace(t *testing.T) {
	s := migrated(t)

	original := RunnerEntry{ID: "proc_0000000a", Label: "before", Cmd: "echo 1"}
	if err := s.SaveEntry(original); err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	first, err := s.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}

	// A later timestamp, so a created_at that was overwritten would be visible.
	time.Sleep(2 * time.Millisecond)
	updated := RunnerEntry{ID: "proc_0000000a", Label: "after", Cmd: "echo 2", Cwd: ptr(`C:\new`)}
	if err := s.SaveEntry(updated); err != nil {
		t.Fatalf("SaveEntry (update): %v", err)
	}

	got, err := s.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d entries after an update, want 1", len(got))
	}
	if got[0].Label != "after" || got[0].Cmd != "echo 2" {
		t.Errorf("the update did not take: %+v", got[0])
	}
	if !got[0].CreatedAt.Equal(first[0].CreatedAt) {
		t.Errorf("created_at moved from %s to %s", first[0].CreatedAt, got[0].CreatedAt)
	}
	if got[0].UpdatedAt.Before(first[0].UpdatedAt) {
		t.Errorf("updated_at went backwards: %s then %s", first[0].UpdatedAt, got[0].UpdatedAt)
	}
}

// TestDamagedEnvDoesNotBlockRestore mirrors spawnpoint/storage.py _decode_env.
// A value that cannot be parsed costs that entry its environment; it must not
// cost the server its start, because the entry list is read during startup.
func TestDamagedEnvDoesNotBlockRestore(t *testing.T) {
	s := migrated(t)
	if err := s.SaveEntry(RunnerEntry{ID: "proc_0000000a", Label: "x", Cmd: "echo 1"}); err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	for _, damaged := range []string{"not json", "[1,2,3]", `{"a":`, "null"} {
		if _, err := s.DB().Exec("UPDATE runner_entry SET env = ? WHERE id = ?", damaged, "proc_0000000a"); err != nil {
			t.Fatalf("damage env: %v", err)
		}
		got, err := s.ListEntries()
		if err != nil {
			t.Fatalf("ListEntries with env=%q: %v", damaged, err)
		}
		if len(got) != 1 {
			t.Fatalf("env=%q: listed %d entries, want 1", damaged, len(got))
		}
		if len(got[0].Env) != 0 {
			t.Errorf("env=%q decoded to %v, want an empty map", damaged, got[0].Env)
		}
	}
}

// TestEnvEncodingIsNotHTMLEscaped keeps the stored text comparable with what
// the current implementation writes. Go escapes `&`, `<` and `>` by default;
// Python with ensure_ascii=False does not.
func TestEnvEncodingIsNotHTMLEscaped(t *testing.T) {
	got, err := encodeEnv(map[string]string{"K": "a & b <c> 한글"})
	if err != nil {
		t.Fatalf("encodeEnv: %v", err)
	}
	want := `{"K":"a & b <c> 한글"}`
	if got != want {
		t.Errorf("encodeEnv = %s, want %s", got, want)
	}
}
