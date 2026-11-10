package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"spawnpoint/internal/runas"
	"spawnpoint/internal/runner"
)

// Embed the legacy interface for unused methods, while implementing the real
// identity extension. The guard must inspect this runner before any action.
type guardedIdentityRunner struct {
	Runner
	identity *runas.Summary
	reads    int
}

func (f *guardedIdentityRunner) Get(string) (runner.Info, bool) {
	f.reads++
	return runner.Info{RunAs: f.identity}, true
}
func (*guardedIdentityRunner) RegisterIdentity(string, string, *string, map[string]string, runas.Input, bool, ...bool) (runner.Info, bool, error) {
	panic("unexpected registration")
}
func (*guardedIdentityRunner) UpdateIdentity(string, *string, *string, **string, map[string]string, *runas.Input, ...bool) (runner.Info, bool, bool, error) {
	panic("unexpected update")
}
func TestExistingIdentityRequiresProtectedTransport(t *testing.T) {
	f := &guardedIdentityRunner{identity: &runas.Summary{Mode: "user", Username: "alice"}}
	s := &Server{runner: f}
	r := httptest.NewRequest("POST", "http://server.example/processes/proc_test/run", nil)
	r.RemoteAddr = "192.0.2.4:1234"
	w := httptest.NewRecorder()
	if s.guardExistingIdentity(w, r, "proc_test", false) || w.Code != http.StatusForbidden || f.reads != 1 {
		t.Fatal("remote identity action was not rejected")
	}
	f.identity = nil
	if !s.guardExistingIdentity(httptest.NewRecorder(), r, "proc_test", false) {
		t.Fatal("legacy service action unexpectedly restricted")
	}
	f.identity = &runas.Summary{Mode: "user", Username: "alice"}
	r = httptest.NewRequest("POST", "http://localhost:7527/processes/proc_test/run", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	if !s.guardExistingIdentity(httptest.NewRecorder(), r, "proc_test", false) {
		t.Fatal("local identity action rejected")
	}
	r.Header.Set("Origin", "http://attacker.example")
	if s.guardExistingIdentity(httptest.NewRecorder(), r, "proc_test", false) {
		t.Fatal("cross-origin identity action allowed")
	}
}

func TestAutomaticLocalProxyAlsoProtectsExecution(t *testing.T) {
	t.Setenv("SPAWNPOINT_TRUST_LOOPBACK_PROXY", "")
	s := &Server{tokens: map[string]struct{}{"test-token": {}}}
	r := httptest.NewRequest("POST", "http://spawnpoint.prd/processes", nil)
	r.RemoteAddr = "127.0.0.1:32100"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "spawnpoint.prd")
	r.Header.Set("X-Forwarded-For", "192.0.2.25")
	r.Header.Set("Origin", "https://spawnpoint.prd")
	r.Header.Set("Content-Type", "application/json")
	if s.identityTransport(r) {
		t.Fatal("proxy grants execution without authentication")
	}
	r.Header.Set("Authorization", "Bearer test-token")
	if !s.identityGuard(httptest.NewRecorder(), r, true) {
		t.Fatal("authenticated HTTPS execution rejected behind Caddy")
	}
	r.Header.Set("Origin", "http://spawnpoint.prd")
	if s.identityGuard(httptest.NewRecorder(), r, true) {
		t.Fatal("wrong origin scheme accepted")
	}
	r.Header.Set("Origin", "https://spawnpoint.prd")
	r.RemoteAddr = "192.0.2.25:32100"
	if s.identityTransport(r) {
		t.Fatal("remote forged proxy accepted")
	}
	r.RemoteAddr = "127.0.0.1:32100"
	r.Host = "localhost:7527"
	r.Header.Set("X-Forwarded-Host", r.Host)
	r.Header.Set("X-Forwarded-Proto", "http")
	if s.identityTransport(r) {
		t.Fatal("HTTP proxy acquired the local execution exception")
	}
}
