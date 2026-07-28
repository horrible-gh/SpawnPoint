package issuer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/dialect"
	"spawnpoint/internal/httpapi"
	"spawnpoint/internal/store"
)

var fixedNow = time.Date(2026, 7, 28, 14, 32, 10, 482913000, time.FixedZone("KST", 9*3600))

type fakeRegistry struct {
	seq         int
	nextErr     error
	findResults []*store.Instance
	findErrors  []error
	insertErr   []error
	inserted    []store.Instance
	findKeys    []string
}

func (f *fakeRegistry) NextSeq(string) (int, error) { return f.seq, f.nextErr }

func (f *fakeRegistry) FindActiveByKey(key string, _ time.Time, _ time.Duration) (*store.Instance, error) {
	f.findKeys = append(f.findKeys, key)
	var out *store.Instance
	var err error
	if len(f.findResults) > 0 {
		out = f.findResults[0]
		f.findResults = f.findResults[1:]
	}
	if len(f.findErrors) > 0 {
		err = f.findErrors[0]
		f.findErrors = f.findErrors[1:]
	}
	return out, err
}

func (f *fakeRegistry) Insert(inst store.Instance) error {
	f.inserted = append(f.inserted, inst)
	if len(f.insertErr) == 0 {
		return nil
	}
	err := f.insertErr[0]
	f.insertErr = f.insertErr[1:]
	return err
}

func request() httpapi.InstanceRequest {
	key, label := "job-42", "nightly-index"
	return httpapi.InstanceRequest{
		Requester:  "flowgate-worker-01",
		Kind:       "worker",
		RequestKey: &key,
		Label:      &label,
		TTLSeconds: 7200,
	}
}

func service(reg Registry, tails ...string) *Service {
	s := New(reg, nil)
	s.now = func() time.Time { return fixedNow }
	n := 0
	s.randomHex = func() (string, error) {
		if n >= len(tails) {
			return "", errors.New("no random fixture")
		}
		out := tails[n]
		n++
		return out, nil
	}
	return s
}

func duplicateError() error {
	return &dialect.WriteError{Class: dialect.DuplicateKey, Err: errors.New("duplicate id")}
}

func TestIssueCreatesAndPersistsTheProtocolInstance(t *testing.T) {
	reg := &fakeRegistry{seq: 1}
	got, ok := service(reg, "a3f2c1").Issue(request())
	if !ok {
		t.Fatal("Issue reported a storage failure")
	}
	if got.ID != "spwn_20260728_0001a3f2c1" || got.Deduplicated {
		t.Errorf("public instance = %+v", got)
	}
	if len(reg.inserted) != 1 {
		t.Fatalf("inserted %d instances, want 1", len(reg.inserted))
	}
	saved := reg.inserted[0]
	if saved.Status != "created" || saved.TTLSeconds != 7200 {
		t.Errorf("stored instance = %+v", saved)
	}
	if !saved.CreatedAt.Equal(fixedNow) || !saved.ExpiresAt.Equal(fixedNow.Add(7200*time.Second)) {
		t.Errorf("stored times = %s .. %s", saved.CreatedAt, saved.ExpiresAt)
	}
}

func TestIssueReturnsAnEarlyDuplicateWithoutAllocating(t *testing.T) {
	existing := store.Instance{
		ID: "spwn_20260728_0001aaaaaa", Status: "created", Kind: "worker",
		Requester: "flowgate-worker-01", CreatedAt: fixedNow.Add(-time.Minute),
	}
	reg := &fakeRegistry{seq: 99, findResults: []*store.Instance{&existing}}
	got, ok := service(reg).Issue(request())
	if !ok || !got.Deduplicated || got.ID != existing.ID {
		t.Fatalf("Issue = (%+v, %v)", got, ok)
	}
	if len(reg.inserted) != 0 {
		t.Errorf("an early duplicate caused %d inserts", len(reg.inserted))
	}
}

func TestIssueRetriesOnlyTheRandomTailAfterAnIdentifierCollision(t *testing.T) {
	reg := &fakeRegistry{
		seq:       7,
		insertErr: []error{duplicateError(), duplicateError(), nil},
	}
	req := request()
	req.RequestKey = nil
	got, ok := service(reg, "aaaaaa", "bbbbbb", "cccccc").Issue(req)
	if !ok || got.ID != "spwn_20260728_0007cccccc" {
		t.Fatalf("Issue = (%+v, %v)", got, ok)
	}
	want := []string{
		"spwn_20260728_0007aaaaaa",
		"spwn_20260728_0007bbbbbb",
		"spwn_20260728_0007cccccc",
	}
	for i := range want {
		if reg.inserted[i].ID != want[i] {
			t.Errorf("attempt %d id = %s, want %s", i, reg.inserted[i].ID, want[i])
		}
	}
}

func TestIssueStopsAfterThreeCollisionRetries(t *testing.T) {
	reg := &fakeRegistry{
		seq: 1,
		insertErr: []error{
			duplicateError(), duplicateError(), duplicateError(), duplicateError(),
		},
	}
	req := request()
	req.RequestKey = nil
	_, ok := service(reg, "000001", "000002", "000003", "000004").Issue(req)
	if ok {
		t.Fatal("four colliding identifiers were reported as a success")
	}
	if len(reg.inserted) != collisionRetries+1 {
		t.Errorf("insert attempts = %d, want %d", len(reg.inserted), collisionRetries+1)
	}
}

func TestIssueResolvesACollisionToTheRequestKeysExistingInstance(t *testing.T) {
	existing := store.Instance{ID: "spwn_20260728_0001bbbbbb", Status: "created", CreatedAt: fixedNow}
	reg := &fakeRegistry{
		seq:         2,
		insertErr:   []error{duplicateError()},
		findResults: []*store.Instance{nil, &existing},
	}
	got, ok := service(reg, "aaaaaa").Issue(request())
	if !ok || !got.Deduplicated || got.ID != existing.ID {
		t.Fatalf("Issue = (%+v, %v)", got, ok)
	}
}

func TestIssuePreservesAnExplicitEmptyRequestKey(t *testing.T) {
	reg := &fakeRegistry{seq: 1}
	req := request()
	empty := ""
	req.RequestKey = &empty
	if _, ok := service(reg, "abcdef").Issue(req); !ok {
		t.Fatal("Issue failed")
	}
	if len(reg.findKeys) != 1 || reg.findKeys[0] != "" {
		t.Errorf("duplicate lookups = %q, want one lookup for the empty key", reg.findKeys)
	}
	if reg.inserted[0].RequestKey == nil || *reg.inserted[0].RequestKey != "" {
		t.Errorf("stored request key = %v", reg.inserted[0].RequestKey)
	}
}

func TestIssueExpandsTheSequencePastFourDigitsAndRejectsOverflow(t *testing.T) {
	reg := &fakeRegistry{seq: 10000}
	got, ok := service(reg, "abcdef").Issue(request())
	if !ok || got.ID != "spwn_20260728_10000abcdef" {
		t.Fatalf("Issue = (%+v, %v)", got, ok)
	}

	reg = &fakeRegistry{seq: sequenceLimit + 1}
	if _, ok := service(reg).Issue(request()); ok {
		t.Fatal("an overflowing daily sequence was accepted")
	}
}

func TestIssueTreatsNonDuplicateWritesAsStorageFailures(t *testing.T) {
	for _, class := range []dialect.Class{dialect.Constraint, dialect.ClassError} {
		t.Run(string(class), func(t *testing.T) {
			reg := &fakeRegistry{
				seq: 1,
				insertErr: []error{&dialect.WriteError{
					Class: class, Err: fmt.Errorf("%s failure", class),
				}},
			}
			if _, ok := service(reg, strings.Repeat("a", randomHexDigits)).Issue(request()); ok {
				t.Fatalf("%s was reported as success", class)
			}
			if len(reg.inserted) != 1 {
				t.Errorf("%d insert attempts, want 1", len(reg.inserted))
			}
		})
	}
}
