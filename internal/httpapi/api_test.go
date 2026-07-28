package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/config"
	"spawnpoint/internal/opslog"
	"spawnpoint/internal/runner"
)

// 0007-P [화면 최초 진입]: one screen, served from the executable, on both
// paths, and never cached.
func TestScreenIsServedFromTheExecutable(t *testing.T) {
	page := []byte("<html><body>screen</body></html>")
	s := newTestServer(t, Options{Index: page})

	for _, path := range []string{"/", "/index.html"} {
		status, body, header := do(t, s, http.MethodGet, path, "")
		if status != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, status)
		}
		if body != string(page) {
			t.Errorf("GET %s: body %q, want the embedded screen", path, body)
		}
		if got := header.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("GET %s: Content-Type %q", path, got)
		}
		// A screen compiled into the binary outlives an upgrade if it is cached,
		// and then talks to a server whose responses it no longer understands.
		if got := header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s: Cache-Control %q, want no-store", path, got)
		}
	}
}

// The screen is unauthenticated even when the rest of the server is not: a
// browser has no way to attach a Bearer token to a top-level navigation, so
// requiring one would make the screen unreachable exactly when it is needed
// (0007-P 0.1).
func TestScreenAndHealthNeedNoToken(t *testing.T) {
	s := newTestServer(t, Options{Config: authConfig("sp_live_7f3a9c21e480"), Index: []byte("<html></html>")})

	for _, path := range []string{"/", "/index.html", "/healthz"} {
		if status, _, _ := do(t, s, http.MethodGet, path, ""); status != http.StatusOK {
			t.Errorf("GET %s without a token: status %d, want 200", path, status)
		}
	}
	// And everything else does need one.
	if status, _, _ := do(t, s, http.MethodGet, "/processes", ""); status != http.StatusUnauthorized {
		t.Errorf("GET /processes without a token: status %d, want 401", status)
	}
}

// 0007-P [생존 확인]. version and started_at are new; ok and status are what
// existing supervisors read, and they are unchanged.
func TestHealthReportsWhenTheServiceCameUp(t *testing.T) {
	started := time.Date(2026, 7, 28, 6, 50, 4, 402000000, time.FixedZone("KST", 9*3600))
	s := newTestServer(t, Options{StartedAt: started})

	status, body, header := do(t, s, http.MethodGet, "/healthz", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	if got := header.Get("Content-Type"); got != contentTypeJSON {
		t.Errorf("Content-Type %q", got)
	}
	wantBody(t, body, `{"ok": true, "status": "healthy", "version": "1.0.0",
	                    "started_at": "2026-07-28T06:50:04.402000+09:00"}`)
}

// 0007-P [프로세스 목록 조회 — 정상], with the five entries 0004-NR 3.1
// measured. Compared whole: the point of this test is that no field has been
// added, renamed or dropped.
func TestProcessListMatchesTheContract(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	rn := &fakeRunner{entries: []runner.Info{
		{
			ID: "proc_45c05b99", Label: "FlowGate",
			Cmd:    `powershell -ExecutionPolicy RemoteSigned -File "C:\workspace\projects\webservices\stg\flowgate.ps1"`,
			Env:    map[string]string{},
			Status: runner.StatusRunning, PID: intPtr(22232),
			StartedAt: timePtr(time.Date(2026, 7, 28, 6, 50, 52, 117482000, kst)),
		},
		{
			ID: "proc_f6aa7819", Label: "FlowGate-Dev",
			Cmd:    `powershell -ExecutionPolicy RemoteSigned -File "C:\workspace\projects\webservices\dev\preview-flowgate.ps1" 0340`,
			Env:    map[string]string{},
			Status: runner.StatusStopped,
		},
	}}
	s := newTestServer(t, Options{Runner: rn})

	status, body, _ := do(t, s, http.MethodGet, "/processes", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true,
	  "processes": [
	    {"id": "proc_45c05b99", "label": "FlowGate",
	     "cmd": "powershell -ExecutionPolicy RemoteSigned -File \"C:\\workspace\\projects\\webservices\\stg\\flowgate.ps1\"",
	     "cwd": null, "env": {}, "pid": 22232, "status": "running", "exit_code": null,
	     "started_at": "2026-07-28T06:50:52.117482+09:00", "ended_at": null},
	    {"id": "proc_f6aa7819", "label": "FlowGate-Dev",
	     "cmd": "powershell -ExecutionPolicy RemoteSigned -File \"C:\\workspace\\projects\\webservices\\dev\\preview-flowgate.ps1\" 0340",
	     "cwd": null, "env": {}, "pid": null, "status": "stopped", "exit_code": null,
	     "started_at": null, "ended_at": null}
	  ]
	}`)
}

// An empty registry is an empty array, not null. The screen renders
// processes.length without checking, and null would be a blank page plus a
// console error rather than the "no registered commands" panel.
func TestEmptyProcessListIsAnArray(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{}})
	_, body, _ := do(t, s, http.MethodGet, "/processes", "")
	wantBody(t, body, `{"ok": true, "processes": []}`)
}

// 0007-P [새 명령 등록 — 정상].
func TestRegisterAnswersWithTheRegistration(t *testing.T) {
	rn := &fakeRunner{persisted: true}
	s := newTestServer(t, Options{Runner: rn})

	status, body, _ := do(t, s, http.MethodPost, "/processes", `{
	  "label": "MirageGlass-Dev",
	  "cmd": "powershell -ExecutionPolicy RemoteSigned -File \"C:\\workspace\\projects\\webservices\\dev\\preview-mirage.ps1\"",
	  "cwd": null,
	  "env": {}
	}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true,
	  "persisted": true,
	  "process": {
	    "id": "proc_2bbd0c5b", "label": "MirageGlass-Dev",
	    "cmd": "powershell -ExecutionPolicy RemoteSigned -File \"C:\\workspace\\projects\\webservices\\dev\\preview-mirage.ps1\"",
	    "cwd": null, "env": {}, "pid": 24356, "status": "running", "exit_code": null,
	    "started_at": "2026-07-28T06:51:00.884930+09:00", "ended_at": null
	  }
	}`)
}

// 0007-P [새 명령 등록 — label 생략]: the command names itself, and an omitted
// cwd and env come back as null and {}.
func TestRegisterWithoutALabelUsesTheFirstWord(t *testing.T) {
	rn := &fakeRunner{persisted: true}
	s := newTestServer(t, Options{Runner: rn})

	do(t, s, http.MethodPost, "/processes",
		`{"cmd": "powershell -ExecutionPolicy RemoteSigned -File \"C:\\stg\\flowgate.ps1\""}`)

	if rn.lastName != "powershell" {
		t.Errorf("label %q, want %q", rn.lastName, "powershell")
	}
	if rn.lastCwd != nil {
		t.Errorf("cwd %v, want nil", *rn.lastCwd)
	}
	if rn.lastEnv == nil || len(rn.lastEnv) != 0 {
		t.Errorf("env %v, want an empty map", rn.lastEnv)
	}
}

// An empty working directory means the same thing as omitting it. Only one of
// the two can be checked for existence, so they are made the same on the way in
// (0007-P [새 명령 등록 — label 생략]).
func TestEmptyWorkingDirectoryIsTheSameAsNone(t *testing.T) {
	rn := &fakeRunner{persisted: true}
	s := newTestServer(t, Options{Runner: rn})
	do(t, s, http.MethodPost, "/processes", `{"cmd": "powershell", "cwd": ""}`)
	if rn.lastCwd != nil {
		t.Errorf("cwd %q, want nil", *rn.lastCwd)
	}
}

// 0007-P [새 명령 등록 — 실행 시작 실패]: the registration succeeded, so this is
// a 200. The start's failure is carried by `status` and `error`, which is the
// state the current server had no way to express (0006-D 3.3).
func TestAStartThatNeverRanIsStillATwoHundred(t *testing.T) {
	ended := time.Date(2026, 7, 28, 14, 7, 5, 220417000, time.FixedZone("KST", 9*3600))
	rn := &fakeRunner{persisted: true, registerAs: &runner.Info{
		ID: "proc_9d20fe41", Label: "Broken", Cmd: "nonexistent-program --run",
		Cwd: strPtr(`C:\no\such\directory`), Env: map[string]string{},
		Status: runner.StatusFailed, Error: `cwd does not exist: C:\no\such\directory`,
		EndedAt: timePtr(ended),
	}}
	s := newTestServer(t, Options{Runner: rn})

	status, body, _ := do(t, s, http.MethodPost, "/processes",
		`{"label": "Broken", "cmd": "nonexistent-program --run", "cwd": "C:\\no\\such\\directory", "env": {}}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true, "persisted": true,
	  "process": {
	    "id": "proc_9d20fe41", "label": "Broken", "cmd": "nonexistent-program --run",
	    "cwd": "C:\\no\\such\\directory", "env": {}, "pid": null, "status": "failed",
	    "exit_code": null, "started_at": null,
	    "ended_at": "2026-07-28T14:07:05.220417+09:00",
	    "error": "cwd does not exist: C:\\no\\such\\directory"
	  }
	}`)
}

// `error` belongs to `failed` and to nothing else. A running entry carrying a
// stale reason would have the screen show a fault that is over.
func TestErrorIsOnlyCarriedByAFailedStart(t *testing.T) {
	rn := &fakeRunner{persisted: true, registerAs: &runner.Info{
		ID: "proc_1", Label: "x", Cmd: "x", Env: map[string]string{},
		Status: runner.StatusRunning, PID: intPtr(1), Error: "left over from an earlier attempt",
	}}
	s := newTestServer(t, Options{Runner: rn})
	_, body, _ := do(t, s, http.MethodPost, "/processes", `{"cmd": "x"}`)
	if strings.Contains(body, "error") {
		t.Errorf("a running process reported an error: %s", body)
	}
}

// 0007-P [새 명령 등록 — 저장 실패]: the row could not be written and the
// process was started anyway. `persisted: false` is the whole point — the
// current server drops this silently and the entry vanishes on the next restart
// (0004-NR F7, E-21).
func TestASaveThatFailedIsReportedRatherThanSwallowed(t *testing.T) {
	rn := &fakeRunner{persisted: false}
	s := newTestServer(t, Options{Runner: rn})

	status, body, _ := do(t, s, http.MethodPost, "/processes", `{"label": "FlowGate", "cmd": "powershell"}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	if !strings.Contains(body, `"persisted":false`) {
		t.Errorf("the failed save was not reported: %s", body)
	}
}

// 0007-P [새 명령 등록 — 검증 실패] and 0008-L 4.1: cmd, then label, then cwd,
// then env, and only the first failure is reported.
func TestRegistrationValidation(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{persisted: true}})

	cases := []struct {
		name string
		body string
		want string
	}{
		{"blank cmd", `{"label": "FlowGate", "cmd": "   "}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "cmd", "message": "cmd cannot be empty."}}`},
		{"missing cmd", `{"label": "FlowGate"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "cmd", "message": "cmd cannot be empty."}}`},
		{"cmd is not a string", `{"cmd": 12345}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "cmd", "message": "cmd cannot be empty."}}`},
		{"cmd too long", `{"cmd": "` + strings.Repeat("x", cmdMaxLen+1) + `"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "cmd", "message": "cmd is too long."}}`},
		{"label too long", `{"label": "` + strings.Repeat("y", labelMaxLen+1) + `", "cmd": "powershell -NoProfile"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "label", "message": "label is too long."}}`},
		{"label is not a string", `{"label": 5, "cmd": "powershell"}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "label", "message": "label must be a string."}}`},
		{"cwd is not a string", `{"cmd": "powershell -NoProfile", "cwd": 12345}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "cwd", "message": "cwd must be a string."}}`},
		{"env value is not a string", `{"cmd": "powershell -NoProfile", "env": {"PORT": 8080}}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "env", "message": "env must be an object with string keys and values."}}`},
		{"env is not an object", `{"cmd": "powershell -NoProfile", "env": []}`,
			`{"ok": false, "error": {"code": "invalid_request", "field": "env", "message": "env must be an object with string keys and values."}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, _ := do(t, s, http.MethodPost, "/processes", c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", status)
			}
			wantBody(t, body, c.want)
		})
	}
}

// The order matters as much as the checks. A body that is wrong in three places
// reports the first one, so a caller fixing them one at a time makes progress
// instead of receiving a different complaint each time.
func TestOnlyTheFirstBadFieldIsReported(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{persisted: true}})
	_, body, _ := do(t, s, http.MethodPost, "/processes", `{"cmd": "", "label": 1, "cwd": 2, "env": 3}`)
	wantBody(t, body,
		`{"ok": false, "error": {"code": "invalid_request", "field": "cmd", "message": "cmd cannot be empty."}}`)
}

// Length limits count characters, not bytes: the database's CHECK constraints
// count characters, and a byte-based limit here would accept a label the store
// then rejects (0008-L 1.2).
func TestLengthLimitsCountCharacters(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{persisted: true}})
	// 128 Korean characters is 384 bytes and is inside the limit.
	label := strings.Repeat("가", labelMaxLen)
	if status, body, _ := do(t, s, http.MethodPost, "/processes",
		`{"label": "`+label+`", "cmd": "powershell"}`); status != http.StatusOK {
		t.Errorf("a %d-character label was refused: %d %s", labelMaxLen, status, body)
	}
	if status, _, _ := do(t, s, http.MethodPost, "/processes",
		`{"label": "`+label+`가", "cmd": "powershell"}`); status != http.StatusBadRequest {
		t.Errorf("a %d-character label was accepted: %d", labelMaxLen+1, status)
	}
}

// 0007-P [등록 정보 수정]: the registration changes and the process does not.
func TestUpdateReplacesTheRegistration(t *testing.T) {
	rn := &fakeRunner{persisted: true, entries: []runner.Info{{
		ID: "proc_f6aa7819", Label: "old", Cmd: "old", Env: map[string]string{},
		Status: runner.StatusStopped,
	}}}
	s := newTestServer(t, Options{Runner: rn})

	status, body, _ := do(t, s, http.MethodPut, "/processes/proc_f6aa7819", `{
	  "label": "FlowGate-Dev",
	  "cmd": "powershell -ExecutionPolicy RemoteSigned -File \"C:\\dev\\preview-flowgate.ps1\" 0341",
	  "cwd": null, "env": {}
	}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true, "persisted": true,
	  "process": {
	    "id": "proc_f6aa7819", "label": "FlowGate-Dev",
	    "cmd": "powershell -ExecutionPolicy RemoteSigned -File \"C:\\dev\\preview-flowgate.ps1\" 0341",
	    "cwd": null, "env": {}, "pid": null, "status": "stopped", "exit_code": null,
	    "started_at": null, "ended_at": null
	  }
	}`)
}

// The three lifecycle operations, each answering with the entry's current state.
func TestLifecycleOperations(t *testing.T) {
	base := runner.Info{
		ID: "proc_45c05b99", Label: "FlowGate", Cmd: "powershell", Env: map[string]string{},
		Status: runner.StatusRunning, PID: intPtr(22232), StartedAt: timePtr(sampleStart),
	}
	for _, action := range []string{"run", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			rn := &fakeRunner{entries: []runner.Info{base}}
			s := newTestServer(t, Options{Runner: rn})
			status, body, _ := do(t, s, http.MethodPost, "/processes/proc_45c05b99/"+action, "")
			if status != http.StatusOK {
				t.Fatalf("status %d, want 200", status)
			}
			if !strings.HasPrefix(body, `{"ok":true,"process":{`) {
				t.Errorf("body %s", body)
			}
			// No `persisted` on a lifecycle operation: nothing was written.
			if strings.Contains(body, "persisted") {
				t.Errorf("%s carried a persisted field: %s", action, body)
			}
			if len(rn.calls) != 1 || rn.calls[0] != action {
				t.Errorf("runner calls %v, want [%s]", rn.calls, action)
			}
		})
	}
}

// 0007-P [삭제]: no `process` block. There is no longer a process to describe.
func TestDeleteAnswersWithTheIdentifierAlone(t *testing.T) {
	rn := &fakeRunner{entries: []runner.Info{{ID: "proc_f8470a37", Env: map[string]string{}}}}
	s := newTestServer(t, Options{Runner: rn})

	status, body, _ := do(t, s, http.MethodDelete, "/processes/proc_f8470a37", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{"ok": true, "deleted_id": "proc_f8470a37"}`)
	if !rn.deleted {
		t.Error("the runner was not asked to delete anything")
	}
}

// 0007-P [존재하지 않는 항목 조작] / E-14: every operation on an unknown
// identifier gives the same answer, and it carries no `field`.
func TestOperationsOnAnUnknownEntry(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{}})
	want := `{"ok": false, "error": {"code": "not_found", "message": "Process not found."}}`

	cases := []struct{ method, target, body string }{
		{http.MethodPost, "/processes/proc_00000000/run", ""},
		{http.MethodPost, "/processes/proc_00000000/stop", ""},
		{http.MethodPost, "/processes/proc_00000000/restart", ""},
		{http.MethodGet, "/processes/proc_00000000/logs", ""},
		{http.MethodPut, "/processes/proc_00000000", `{"cmd": "powershell"}`},
		{http.MethodDelete, "/processes/proc_00000000", ""},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			status, body, _ := do(t, s, c.method, c.target, c.body)
			if status != http.StatusNotFound {
				t.Fatalf("status %d, want 404", status)
			}
			wantBody(t, body, want)
		})
	}
}

// E-15: an operation name that is not one of the three is a path that does not
// exist. The message is the only thing that tells the two 404s apart, which is
// why it is compared rather than the status.
func TestAnUndefinedOperationIsAnUndefinedPath(t *testing.T) {
	rn := &fakeRunner{entries: []runner.Info{{ID: "proc_45c05b99", Env: map[string]string{}}}}
	s := newTestServer(t, Options{Runner: rn})

	status, body, _ := do(t, s, http.MethodPost, "/processes/proc_45c05b99/pause", "")
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
	wantBody(t, body, `{"ok": false, "error": {"code": "not_found", "message": "Unknown endpoint."}}`)
	if len(rn.calls) != 0 {
		t.Errorf("the runner was consulted for an undefined operation: %v", rn.calls)
	}
}

// 0007-P [알 수 없는 경로 / 지원하지 않는 메서드].
func TestUnknownPathsAndWrongMethods(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{}, Index: []byte("<html></html>")})

	notFound := `{"ok": false, "error": {"code": "not_found", "message": "Unknown endpoint."}}`
	notAllowed := `{"ok": false, "error": {"code": "method_not_allowed", "message": "Method not allowed."}}`

	cases := []struct {
		method, target string
		status         int
		want           string
	}{
		{http.MethodGet, "/api/v2/processes", http.StatusNotFound, notFound},
		{http.MethodGet, "/nope", http.StatusNotFound, notFound},
		{http.MethodPatch, "/processes/proc_45c05b99", http.StatusMethodNotAllowed, notAllowed},
		{http.MethodDelete, "/spawn", http.StatusMethodNotAllowed, notAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed, notAllowed},
		{http.MethodPut, "/processes", http.StatusMethodNotAllowed, notAllowed},
		{http.MethodGet, "/processes/proc_45c05b99", http.StatusMethodNotAllowed, notAllowed},
		{http.MethodPost, "/", http.StatusMethodNotAllowed, notAllowed},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			status, body, header := do(t, s, c.method, c.target, "")
			if status != c.status {
				t.Fatalf("status %d, want %d", status, c.status)
			}
			wantBody(t, body, c.want)
			// No Allow header on a 405: the current server sends none and
			// nothing reads it (0007-P [알 수 없는 경로]).
			if got := header.Get("Allow"); got != "" {
				t.Errorf("Allow: %q, want no header", got)
			}
		})
	}
}

// 0007-P [인증 실패]. Authentication comes before validation, so a bad token on
// a malformed body is 401 and not 400 — a caller who cannot authenticate is not
// told what the server thinks of their JSON.
func TestAuthentication(t *testing.T) {
	const token = "sp_live_7f3a9c21e480"
	s := newTestServer(t, Options{Config: authConfig(token), Runner: &fakeRunner{}})
	want := `{"ok": false, "error": {"code": "unauthorized", "message": "Valid authentication token required."}}`

	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong token", "Bearer wrong-token"},
		{"wrong scheme", "Basic " + token},
		{"scheme only", "Bearer"},
		{"blank token", "Bearer    "},
		{"token alone", token},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var headers [][2]string
			if c.header != "" {
				headers = append(headers, [2]string{"Authorization", c.header})
			}
			status, body, header := do(t, s, http.MethodGet, "/processes", "", headers...)
			if status != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", status)
			}
			wantBody(t, body, want)
			// A WWW-Authenticate header makes the browser open its own
			// credentials dialog over the screen, which cannot supply a Bearer
			// token and cannot be dismissed back to the page.
			if got := header.Get("WWW-Authenticate"); got != "" {
				t.Errorf("WWW-Authenticate: %q, want no header", got)
			}
		})
	}

	// The scheme name is compared without case; the token is not.
	for _, h := range []string{"Bearer " + token, "bearer " + token, "BEARER " + token} {
		if status, _, _ := do(t, s, http.MethodGet, "/processes", "",
			[2]string{"Authorization", h}); status != http.StatusOK {
			t.Errorf("%q: status %d, want 200", h, status)
		}
	}
	if status, _, _ := do(t, s, http.MethodGet, "/processes", "",
		[2]string{"Authorization", "Bearer " + strings.ToUpper(token)}); status != http.StatusUnauthorized {
		t.Errorf("an upper-cased token was accepted: %d", status)
	}
}

// An empty allow list passes everything, header or no header. This is the local
// default and every existing deployment depends on it (0007-P 0.1).
func TestNoTokensConfiguredMeansEverythingPasses(t *testing.T) {
	s := newTestServer(t, Options{Config: config.Config{}, Runner: &fakeRunner{}})
	for _, h := range []string{"", "Bearer anything", "garbage"} {
		var headers [][2]string
		if h != "" {
			headers = append(headers, [2]string{"Authorization", h})
		}
		if status, _, _ := do(t, s, http.MethodGet, "/processes", "", headers...); status != http.StatusOK {
			t.Errorf("Authorization %q: status %d, want 200", h, status)
		}
	}
}

// 0007-P [알 수 없는 경로]: 404 wins over 401. There is no reason to tell an
// unauthenticated caller which paths exist.
func TestAnUnknownPathIsFourOhFourEvenWithoutAToken(t *testing.T) {
	s := newTestServer(t, Options{Config: authConfig("secret"), Runner: &fakeRunner{}})
	status, body, _ := do(t, s, http.MethodGet, "/api/v2/processes", "")
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
	wantBody(t, body, `{"ok": false, "error": {"code": "not_found", "message": "Unknown endpoint."}}`)
}

// 0007-P [본문 초과]: the length is judged before anything is read, and before
// authentication. A caller who cannot authenticate must still not be able to
// make the server hold 64 KiB of their choosing.
func TestOversizedBodiesAreRefusedBeforeAnythingElse(t *testing.T) {
	s := newTestServer(t, Options{Config: authConfig("secret"), Runner: &fakeRunner{}})
	big := `{"label": "Big", "cmd": "` + strings.Repeat("x", RequestBodyMaxBytes) + `"}`

	status, body, _ := do(t, s, http.MethodPost, "/processes", big)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", status)
	}
	wantBody(t, body, `{"ok": false, "error": {"code": "payload_too_large", "message": "Request body is too large."}}`)

	// A body exactly at the limit is accepted — the limit is inclusive.
	filler := RequestBodyMaxBytes - len(`{"cmd":""}`)
	exact := `{"cmd":"` + strings.Repeat("x", filler) + `"}`
	if len(exact) != RequestBodyMaxBytes {
		t.Fatalf("test built a %d-byte body, wanted %d", len(exact), RequestBodyMaxBytes)
	}
	// It is refused for being too long a command, not for being too large a
	// body — which is the distinction being checked.
	if status, body, _ := do(t, s, http.MethodPost, "/processes", exact,
		[2]string{"Authorization", "Bearer secret"}); status != http.StatusBadRequest {
		t.Errorf("a body at exactly the limit: status %d, want 400 — %s", status, body)
	}
}

// 0007-P [잘못된 본문].
func TestMalformedBodies(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{}, Issuer: &fakeIssuer{}})

	cases := []struct {
		name, target, body, want string
	}{
		{"truncated JSON", "/processes", `{"label": "Broken", "cmd":`,
			`{"ok": false, "error": {"code": "invalid_request", "message": "Request body is not valid JSON."}}`},
		{"an array", "/spawn", `["flowgate-worker-01", "worker"]`,
			`{"ok": false, "error": {"code": "invalid_request", "message": "Request body must be an object."}}`},
		{"a bare string", "/processes", `"powershell"`,
			`{"ok": false, "error": {"code": "invalid_request", "message": "Request body must be an object."}}`},
		{"a number", "/processes", `42`,
			`{"ok": false, "error": {"code": "invalid_request", "message": "Request body must be an object."}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, _ := do(t, s, http.MethodPost, c.target, c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", status)
			}
			wantBody(t, body, c.want)
		})
	}
}

// An empty body is an empty object, and the field checks take it from there.
// The complaint differs by route, which is the evidence that the body reached
// validation rather than being rejected on the way in (0007-P [잘못된 본문]).
func TestAnEmptyBodyIsAnEmptyObject(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{}, Issuer: &fakeIssuer{}})

	_, body, _ := do(t, s, http.MethodPost, "/processes", "")
	wantBody(t, body, `{"ok": false, "error": {"code": "invalid_request", "field": "cmd", "message": "cmd cannot be empty."}}`)

	_, body, _ = do(t, s, http.MethodPost, "/spawn", "")
	wantBody(t, body, `{"ok": false, "error": {"code": "invalid_request", "field": "requester", "message": "requester cannot be empty."}}`)
}

// HEAD answers with its GET's headers and no body (0007-P 0.1). Content-Length
// is the one that matters: left to net/http a HEAD whose body is never written
// carries none at all, and a caller sizing a response would read zero.
func TestHeadSendsTheHeadersAndNoBody(t *testing.T) {
	rn := &fakeRunner{entries: []runner.Info{{ID: "proc_1", Env: map[string]string{}}}}
	s := newTestServer(t, Options{Runner: rn, Index: []byte("<html>screen</html>")})

	for _, target := range []string{"/", "/index.html", "/healthz", "/processes", "/processes/proc_1/logs"} {
		getStatus, getBody, getHeader := do(t, s, http.MethodGet, target, "")
		headStatus, headBody, headHeader := do(t, s, http.MethodHead, target, "")

		if headStatus != getStatus {
			t.Errorf("HEAD %s: status %d, GET gave %d", target, headStatus, getStatus)
		}
		if headBody != "" {
			t.Errorf("HEAD %s returned a body: %q", target, headBody)
		}
		if got, want := headHeader.Get("Content-Length"), getHeader.Get("Content-Length"); got != want {
			t.Errorf("HEAD %s: Content-Length %q, GET gave %q", target, got, want)
		}
		if got, want := headHeader.Get("Content-Type"), getHeader.Get("Content-Type"); got != want {
			t.Errorf("HEAD %s: Content-Type %q, GET gave %q", target, got, want)
		}
		_ = getBody
	}
}

// HEAD is not accepted where GET is not. POST-only and PUT/DELETE-only routes
// answer 405, because 0007-P grants HEAD on the GET paths and on no others.
func TestHeadIsNotAcceptedOnWriteRoutes(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{}})
	for _, target := range []string{"/spawn", "/processes/proc_1/run"} {
		if status, _, _ := do(t, s, http.MethodHead, target, ""); status != http.StatusMethodNotAllowed {
			t.Errorf("HEAD %s: status %d, want 405", target, status)
		}
	}
}

// 0007-P 표기 규칙: non-ASCII travels as itself, and the characters Go escapes
// for HTML by default do not. Every registered command contains `>`, so a
// server that left the default on would return a command that is not the one
// that was registered.
func TestJSONIsNotEscapedForHTML(t *testing.T) {
	rn := &fakeRunner{persisted: true, registerAs: &runner.Info{
		ID: "proc_1", Label: "한글 <이름> & 기호", Cmd: `cmd /c "chcp 65001 >nul & echo <hi>"`,
		Env: map[string]string{}, Status: runner.StatusRunning, PID: intPtr(1),
	}}
	s := newTestServer(t, Options{Runner: rn})

	_, body, _ := do(t, s, http.MethodPost, "/processes", `{"cmd": "x"}`)
	if !strings.Contains(body, `"한글 <이름> & 기호"`) {
		t.Errorf("the label was escaped or transliterated: %s", body)
	}
	if strings.Contains(body, `\u003c`) || strings.Contains(body, `\u0026`) {
		t.Errorf("HTML escaping is still on: %s", body)
	}
	// Backslashes and quotes are JSON escapes and must survive as such.
	if !strings.Contains(body, `chcp 65001 >nul & echo <hi>`) {
		t.Errorf("the command was altered: %s", body)
	}
}

// panickingRunner fails the one way a handler cannot answer for.
type panickingRunner struct{ fakeRunner }

func (p *panickingRunner) List() []runner.Info { panic("the registry vanished") }

// A handler that panics leaves a record and then carries on panicking. The
// record is the point of the rewrite (0004-NR 1.4); not answering is deliberate,
// because 0007-P lists six error codes and a seventh invented here would be a
// contract this server has to keep.
func TestAPanickingHandlerIsRecorded(t *testing.T) {
	dir := t.TempDir()
	log, err := opslog.Open(dir)
	if err != nil {
		t.Fatalf("open the operations log: %v", err)
	}
	s := New(Options{Runner: &panickingRunner{}, Log: log})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed instead of being passed on")
			}
		}()
		do(t, s, http.MethodGet, "/processes", "")
	}()
	log.Close()

	raw, err := os.ReadFile(filepath.Join(dir, opslog.FileName))
	if err != nil {
		t.Fatalf("read the operations log: %v", err)
	}
	record := string(raw)
	for _, want := range []string{"request failed", "method=GET", "path=/processes", "the registry vanished"} {
		if !strings.Contains(record, want) {
			t.Errorf("the operations log does not mention %q:\n%s", want, record)
		}
	}
}

// Once the shutdown has begun, every answer tells the caller not to reuse the
// connection. The screen polls on a kept-alive one, and without this each poll
// resets the idle timer the inflight wait is watching (E-29).
func TestShutdownAsksCallersToCloseTheConnection(t *testing.T) {
	s := newTestServer(t, Options{Runner: &fakeRunner{}})

	if _, _, header := do(t, s, http.MethodGet, "/processes", ""); header.Get("Connection") != "" {
		t.Errorf("Connection: %q before the shutdown, want no header", header.Get("Connection"))
	}
	s.BeginShutdown()
	if _, _, header := do(t, s, http.MethodGet, "/processes", ""); header.Get("Connection") != "close" {
		t.Errorf("Connection: %q during the shutdown, want close", header.Get("Connection"))
	}
}

// Every JSON response says what it is, and nothing else claims to be JSON.
func TestContentTypeIsJSONEverywhereButTheScreen(t *testing.T) {
	rn := &fakeRunner{entries: []runner.Info{{ID: "proc_1", Env: map[string]string{}}}}
	s := newTestServer(t, Options{Runner: rn, Index: []byte("<html></html>")})

	for _, target := range []string{"/healthz", "/processes", "/processes/proc_1/logs", "/nope"} {
		_, _, header := do(t, s, http.MethodGet, target, "")
		if got := header.Get("Content-Type"); got != contentTypeJSON {
			t.Errorf("GET %s: Content-Type %q, want %q", target, got, contentTypeJSON)
		}
	}
}
