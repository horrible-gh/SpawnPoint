package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/config"
	"spawnpoint/internal/opslog"
	"spawnpoint/internal/runner"
)

// The tests in this package compare whole response bodies against the strings
// in 0007-P rather than picking fields out of them. Field-by-field assertions
// pass a response that has quietly grown an extra key or lost `cwd`, and both
// of those are contract breaks: an added key is a promise the next version has
// to keep, and a dropped one is a caller reading undefined.
//
// Nothing here starts a process, opens a database or binds a socket. The runner
// and the issuer are fakes, which is what makes the failure paths — a save that
// did not happen, a start that never ran, an instance that could not be
// stored — reachable at all. They are otherwise only reachable by breaking a
// real file mid-test.

// fakeRunner is a scriptable Runner.
type fakeRunner struct {
	entries []runner.Info
	view    runner.LogView
	viewOK  bool

	// persisted is what Register and Update report as the save's outcome.
	persisted bool
	// registerAs, when set, replaces what Register returns, so the failed-start
	// and the storage-failure responses can be produced without a process.
	registerAs *runner.Info

	deleted  bool
	calls    []string
	lastLog  string
	lastEnv  map[string]string
	lastCwd  *string
	lastCmd  string
	lastName string
}

func (f *fakeRunner) List() []runner.Info { f.calls = append(f.calls, "list"); return f.entries }

func (f *fakeRunner) find(id string) (runner.Info, bool) {
	for _, e := range f.entries {
		if e.ID == id {
			return e, true
		}
	}
	return runner.Info{}, false
}

func (f *fakeRunner) Register(label, cmd string, cwd *string, env map[string]string) (runner.Info, bool) {
	f.calls = append(f.calls, "register")
	f.lastName, f.lastCmd, f.lastCwd, f.lastEnv = label, cmd, cwd, env
	if f.registerAs != nil {
		return *f.registerAs, f.persisted
	}
	info := runner.Info{
		ID: "proc_2bbd0c5b", Label: label, Cmd: cmd, Cwd: cwd, Env: env,
		Status: runner.StatusRunning, PID: intPtr(24356), StartedAt: timePtr(sampleStart),
	}
	f.entries = append(f.entries, info)
	return info, f.persisted
}

func (f *fakeRunner) Update(id string, label, cmd *string, cwd **string, env map[string]string) (runner.Info, bool, bool) {
	f.calls = append(f.calls, "update")
	info, ok := f.find(id)
	if !ok {
		return runner.Info{}, false, false
	}
	if label != nil {
		info.Label = *label
	}
	if cmd != nil {
		info.Cmd = *cmd
	}
	if cwd != nil {
		info.Cwd = *cwd
		f.lastCwd = *cwd
	}
	info.Env = env
	f.lastEnv = env
	return info, f.persisted, true
}

func (f *fakeRunner) Run(id string) (runner.Info, bool) {
	f.calls = append(f.calls, "run")
	return f.find(id)
}

func (f *fakeRunner) Stop(id string) (runner.Info, bool) {
	f.calls = append(f.calls, "stop")
	return f.find(id)
}

func (f *fakeRunner) Restart(id string) (runner.Info, bool) {
	f.calls = append(f.calls, "restart")
	return f.find(id)
}

func (f *fakeRunner) Delete(id string) bool {
	f.calls = append(f.calls, "delete")
	if _, ok := f.find(id); !ok {
		return false
	}
	f.deleted = true
	return true
}

func (f *fakeRunner) ReadLog(id, offsetParam string) (runner.LogView, bool) {
	f.calls = append(f.calls, "readlog")
	f.lastLog = offsetParam
	if _, ok := f.find(id); !ok {
		return runner.LogView{}, false
	}
	return f.view, f.viewOK
}

// fakeIssuer isolates the HTTP contract from the issuing service. It records the validated
// request so the tests can check what the front end decided before handing over.
type fakeIssuer struct {
	last InstanceRequest
	out  Instance
	ok   bool
}

func (f *fakeIssuer) Issue(req InstanceRequest) (Instance, bool) {
	f.last = req
	return f.out, f.ok
}

// --- harness ------------------------------------------------------------------

// sampleStart is the timestamp 0007-P uses throughout, so a rendered response
// can be compared against the document byte for byte.
var sampleStart = time.Date(2026, 7, 28, 6, 51, 0, 884930000, time.FixedZone("KST", 9*3600))

func intPtr(v int) *int              { return &v }
func strPtr(v string) *string        { return &v }
func timePtr(t time.Time) *time.Time { return &t }

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Log == nil {
		opts.Log = discardLog(t)
	}
	return New(opts)
}

// discardLog gives the server somewhere to write. The operations log is a file
// by design, so the tests give it a temporary directory rather than an
// interface — swapping in a writer here would mean this package tests something
// the server does not do.
func discardLog(t *testing.T) *opslog.Logger {
	t.Helper()
	log, err := opslog.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open the operations log: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

// do runs one request and returns the status and the exact body.
func do(t *testing.T, s *Server, method, target, body string, headers ...[2]string) (int, string, http.Header) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
		// A request with no body still has to look like one: httptest sets
		// ContentLength to 0, which is what a real GET carries.
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	for _, h := range headers {
		r.Header.Set(h[0], h[1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	return res.StatusCode, string(raw), res.Header
}

// wantBody compares against the contract's JSON, ignoring only whitespace
// between tokens: 0007-P prints its examples indented and the wire form is not.
// Key order is preserved by the comparison, because the contract fixes it.
func wantBody(t *testing.T, got, want string) {
	t.Helper()
	if compact(t, got) != compact(t, want) {
		t.Errorf("response body\n got: %s\nwant: %s", compact(t, got), compact(t, want))
	}
}

func compact(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, s)
	}
	return buf.String()
}

// authConfig is a configuration with one allowed token.
func authConfig(tokens ...string) config.Config {
	return config.Config{APITokens: tokens}
}
