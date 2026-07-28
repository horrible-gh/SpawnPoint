package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The instance scenarios of 0007-P at the HTTP seam: the four field checks and
// their order, and the exact response the front end builds from the issuer.

var instanceCreated = time.Date(2026, 7, 28, 14, 32, 10, 482913000, time.FixedZone("KST", 9*3600))

func spawnServer(t *testing.T, issuer Issuer) *Server {
	t.Helper()
	return newTestServer(t, Options{Issuer: issuer})
}

// 0007-P [인스턴스 발급 — 정상]. expires_at, ttl_seconds and request_key are
// stored and not reported; `deduplicated` is absent, not false.
func TestSpawnResponse(t *testing.T) {
	label := "nightly-index"
	issuer := &fakeIssuer{ok: true, out: Instance{
		ID: "spwn_20260728_0001a3f2c1", Status: "created", Kind: "worker",
		Requester: "flowgate-worker-01", CreatedAt: instanceCreated, Label: &label,
	}}
	s := spawnServer(t, issuer)

	status, body, _ := do(t, s, http.MethodPost, "/spawn", `{
	  "requester": "flowgate-worker-01",
	  "kind": "worker",
	  "request_key": "job-2026-07-28-0042",
	  "options": {"label": "nightly-index", "ttl_seconds": 7200}
	}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true,
	  "instance": {
	    "id": "spwn_20260728_0001a3f2c1", "status": "created", "kind": "worker",
	    "requester": "flowgate-worker-01",
	    "created_at": "2026-07-28T14:32:10.482913+09:00",
	    "label": "nightly-index"
	  }
	}`)

	// The issuer receives a request that has already been decided: the ttl is
	// resolved, the key is present, and nothing it gets needs re-checking.
	if issuer.last.Requester != "flowgate-worker-01" || issuer.last.Kind != "worker" {
		t.Errorf("issuer received %+v", issuer.last)
	}
	if issuer.last.RequestKey == nil || *issuer.last.RequestKey != "job-2026-07-28-0042" {
		t.Errorf("request key %v", issuer.last.RequestKey)
	}
	if issuer.last.TTLSeconds != 7200 {
		t.Errorf("ttl %d, want 7200", issuer.last.TTLSeconds)
	}
}

// An omitted ttl arrives at the issuer already resolved to its default, so the
// issuer never has to know what the default is (0008-L 1.1).
func TestOmittedTTLIsResolvedBeforeTheIssuerSeesIt(t *testing.T) {
	issuer := &fakeIssuer{ok: true, out: Instance{ID: "x", Status: "created", Kind: "worker", Requester: "r", CreatedAt: instanceCreated}}
	s := spawnServer(t, issuer)

	do(t, s, http.MethodPost, "/spawn", `{"requester": "flowgate-worker-01", "kind": "worker"}`)
	if issuer.last.TTLSeconds != TTLDefault {
		t.Errorf("ttl %d, want %d", issuer.last.TTLSeconds, TTLDefault)
	}
	if issuer.last.RequestKey != nil {
		t.Errorf("a request key was invented: %q", *issuer.last.RequestKey)
	}
	if issuer.last.Label != nil {
		t.Errorf("a label was invented: %q", *issuer.last.Label)
	}
}

// An absent label is an absent key, not a null one (0007-P [인스턴스 발급 — 정상]).
func TestInstanceWithoutALabelOmitsTheKey(t *testing.T) {
	issuer := &fakeIssuer{ok: true, out: Instance{
		ID: "spwn_20260728_0002b7e04d", Status: "created", Kind: "worker",
		Requester: "flowgate-worker-01", CreatedAt: instanceCreated,
	}}
	s := spawnServer(t, issuer)

	_, body, _ := do(t, s, http.MethodPost, "/spawn", `{"requester": "flowgate-worker-01", "kind": "worker"}`)
	wantBody(t, body, `{
	  "ok": true,
	  "instance": {
	    "id": "spwn_20260728_0002b7e04d", "status": "created", "kind": "worker",
	    "requester": "flowgate-worker-01",
	    "created_at": "2026-07-28T14:32:10.482913+09:00"
	  }
	}`)
}

// 0007-P [인스턴스 발급 — 중복 요청]: 200, not 409. To the caller who sent the
// same key twice, "the one that already exists" is the correct answer, and
// `deduplicated` is the whole of the difference.
func TestDeduplicatedInstanceIsAlsoATwoHundred(t *testing.T) {
	label := "nightly-index"
	issuer := &fakeIssuer{ok: true, out: Instance{
		ID: "spwn_20260728_0001a3f2c1", Status: "created", Kind: "worker",
		Requester: "flowgate-worker-01", CreatedAt: instanceCreated, Label: &label,
		Deduplicated: true,
	}}
	s := spawnServer(t, issuer)

	status, body, _ := do(t, s, http.MethodPost, "/spawn", `{
	  "requester": "flowgate-worker-01", "kind": "worker",
	  "request_key": "job-2026-07-28-0042",
	  "options": {"label": "nightly-index", "ttl_seconds": 7200}
	}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true,
	  "deduplicated": true,
	  "instance": {
	    "id": "spwn_20260728_0001a3f2c1", "status": "created", "kind": "worker",
	    "requester": "flowgate-worker-01",
	    "created_at": "2026-07-28T14:32:10.482913+09:00",
	    "label": "nightly-index"
	  }
	}`)
}

// 0007-P [인스턴스 발급 — 검증 실패], in 0008-L 4.1's order.
func TestSpawnValidation(t *testing.T) {
	s := spawnServer(t, &fakeIssuer{ok: true})

	cases := []struct{ name, body, want string }{
		{"empty requester", `{"requester": "", "kind": "worker"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "requester", "message": "requester cannot be empty."}}`},
		{"missing requester", `{"kind": "worker"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "requester", "message": "requester cannot be empty."}}`},
		{"requester too long", `{"requester": "` + strings.Repeat("r", requesterMaxLen+1) + `", "kind": "worker"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "requester", "message": "requester is too long."}}`},
		{"unknown kind", `{"requester": "flowgate-worker-01", "kind": "daemon"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "kind", "message": "kind is not allowed."}}`},
		{"kind is case sensitive", `{"requester": "flowgate-worker-01", "kind": "Worker"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "kind", "message": "kind is not allowed."}}`},
		{"missing kind", `{"requester": "flowgate-worker-01"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "kind", "message": "kind cannot be empty."}}`},
		{"ttl below the range", `{"requester": "flowgate-worker-01", "kind": "worker", "options": {"ttl_seconds": 30}}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "options.ttl_seconds", "message": "ttl_seconds is outside the allowed range."}}`},
		{"ttl above the range", `{"requester": "flowgate-worker-01", "kind": "worker", "options": {"ttl_seconds": 86401}}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "options.ttl_seconds", "message": "ttl_seconds is outside the allowed range."}}`},
		{"ttl is a bool", `{"requester": "flowgate-worker-01", "kind": "worker", "options": {"ttl_seconds": true}}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "options.ttl_seconds", "message": "ttl_seconds is outside the allowed range."}}`},
		{"ttl is a quoted number", `{"requester": "flowgate-worker-01", "kind": "worker", "options": {"ttl_seconds": "3600"}}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "options.ttl_seconds", "message": "ttl_seconds is outside the allowed range."}}`},
		{"ttl is fractional", `{"requester": "flowgate-worker-01", "kind": "worker", "options": {"ttl_seconds": 3600.5}}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "options.ttl_seconds", "message": "ttl_seconds is outside the allowed range."}}`},
		{"label too long", `{"requester": "flowgate-worker-01", "kind": "worker", "options": {"label": "` + strings.Repeat("y", spawnLabelMaxLen+1) + `"}}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "options.label", "message": "label is too long."}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, _ := do(t, s, http.MethodPost, "/spawn", c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", status)
			}
			wantBody(t, body, c.want)
		})
	}
}

// The three allowed kinds, and nothing else (0008-L 1.1).
func TestAllowedKinds(t *testing.T) {
	issuer := &fakeIssuer{ok: true, out: Instance{ID: "x", Status: "created", Requester: "r", CreatedAt: instanceCreated}}
	s := spawnServer(t, issuer)
	for _, kind := range []string{"session", "worker", "task"} {
		if status, _, _ := do(t, s, http.MethodPost, "/spawn",
			`{"requester": "flowgate-worker-01", "kind": "`+kind+`"}`); status != http.StatusOK {
			t.Errorf("kind %q: status %d, want 200", kind, status)
		}
	}
}

// The label limit is 256 here — 0007-P 0.5 and 0008-L 1.1 both say so, and
// 0008-L 1.1 forbids L from changing a value it inherited from P.
//
// Migration 003 widens spawn_instance's old 128-character constraint to the
// same value, so labels in this range now pass the complete request-to-store
// path rather than failing after validation.
func TestSpawnLabelLimitFollowsTheProtocolNotTheSchema(t *testing.T) {
	issuer := &fakeIssuer{ok: true, out: Instance{ID: "x", Status: "created", Kind: "worker", Requester: "r", CreatedAt: instanceCreated}}
	s := spawnServer(t, issuer)

	for _, n := range []int{129, 200, spawnLabelMaxLen} {
		body := `{"requester": "r", "kind": "worker", "options": {"label": "` + strings.Repeat("y", n) + `"}}`
		if status, _, _ := do(t, s, http.MethodPost, "/spawn", body); status != http.StatusOK {
			t.Errorf("a %d-character label was refused with %d; the protocol allows %d", n, status, spawnLabelMaxLen)
		}
	}
}

// 0007-P [인스턴스 발급 — 저장 실패]: the only failure the issuer reports.
// An identifier collision is resolved behind that interface and never surfaces
// as one (0008-L 4.4).
func TestInstanceStorageFailure(t *testing.T) {
	s := spawnServer(t, &fakeIssuer{ok: false})

	status, body, _ := do(t, s, http.MethodPost, "/spawn",
		`{"requester": "flowgate-worker-01", "kind": "worker"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", status)
	}
	wantBody(t, body, `{"ok": false, "error": {"code": "storage_error", "message": "Failed to save instance."}}`)
}

// With no issuer wired the route still exists, still authenticates and still
// validates; it just has nothing to store into. Reporting that as a storage
// error is the honest answer and is the code 0007-P gives for a /spawn that
// fails after validation.
func TestSpawnWithoutAnIssuerStillValidates(t *testing.T) {
	s := newTestServer(t, Options{})

	if status, body, _ := do(t, s, http.MethodPost, "/spawn", `{"requester": "", "kind": "worker"}`); status != http.StatusBadRequest {
		t.Errorf("status %d, want 400 — %s", status, body)
	}
	if status, _, _ := do(t, s, http.MethodPost, "/spawn", `{"requester": "r", "kind": "worker"}`); status != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", status)
	}
}

// Authentication runs before validation on /spawn too: a bad token on a body
// that would also have failed gives 401, not 400 (0007-P [인증 실패]).
func TestSpawnAuthenticationComesFirst(t *testing.T) {
	s := newTestServer(t, Options{Config: authConfig("sp_live_7f3a9c21e480"), Issuer: &fakeIssuer{ok: true}})

	status, body, _ := do(t, s, http.MethodPost, "/spawn", `{"requester": "", "kind": "nonsense"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", status)
	}
	wantBody(t, body, `{"ok": false, "error": {"code": "unauthorized", "message": "Valid authentication token required."}}`)
}
