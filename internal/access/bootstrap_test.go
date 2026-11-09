package access

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"spawnpoint/internal/store"
)

func bootstrapFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	t.Setenv("SPAWNPOINT_SETUP_TOKEN", "")
	path := filepath.Join(t.TempDir(), "actual database.db")
	db, e := store.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	if _, _, e = db.Migrate(); e != nil {
		t.Fatal(e)
	}
	m, e := New(db.DB(), nil, http.NotFoundHandler(), nil, path)
	if e != nil {
		t.Fatal(e)
	}
	return m, path
}
func bootstrapCall(m *Manager, path string, in any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(in)
	r := httptest.NewRequest("POST", "http://localhost:7527"+path, bytes.NewReader(b))
	r.RemoteAddr = "127.0.0.1:44321"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}
func TestBootstrapOSRequiresVerifiedAdminAndClosesEveryMethod(t *testing.T) {
	m, path := bootstrapFixture(t)
	token := m.setupToken
	var calls int
	m.verifyAdmin = func(ctx context.Context, user, password string) (string, error) {
		calls++
		if user != "osadmin" || password != "os-administrator-secret" {
			return "", errors.New("denied")
		}
		return "linux:uid:1234", nil
	}
	for _, in := range []loginInput{{Login: "regular", Password: "valid-user-password"}, {Login: "osadmin", Password: "wrong"}} {
		if w := bootstrapCall(m, "/auth/setup-os", in); w.Code != 403 {
			t.Fatalf("ordinary/wrong credential accepted: %d", w.Code)
		}
	}
	in := loginInput{Login: "osadmin", Password: "os-administrator-secret"}
	w := bootstrapCall(m, "/auth/setup-os", in)
	if w.Code != 200 {
		t.Fatalf("bootstrap: %d %s", w.Code, w.Body.String())
	}
	if len(m.state.Users) != 1 || m.state.Users[0].Auth != "os" || m.state.Users[0].Role != "admin" || m.state.Users[0].Hash != "" {
		t.Fatal("incorrect administrator binding")
	}
	if _, e := os.Stat(path + ".setup-token"); !os.IsNotExist(e) {
		t.Fatal("setup token not removed")
	}
	for _, route := range []string{"/auth/setup", "/auth/setup-os", "/auth/setup-cli"} {
		in.SetupToken = token
		if w = bootstrapCall(m, route, in); w.Code != 403 {
			t.Fatalf("bootstrap reopened: %s %d", route, w.Code)
		}
	}
	if calls != 3 {
		t.Fatal("OS authenticator called after bootstrap closed")
	}
	var raw string
	if e := m.db.QueryRow("SELECT document FROM access_state WHERE id=1").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(raw, in.Password) {
		t.Fatal("OS password persisted")
	}
	loaded, e := New(m.db, nil, http.NotFoundHandler(), nil, path)
	if e != nil || len(loaded.state.Users) != 1 {
		t.Fatalf("restart lost administrator: %v", e)
	}
}
func TestBootstrapConcurrentOSOnlyOneAdministrator(t *testing.T) {
	m, _ := bootstrapFixture(t)
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	m.verifyAdmin = func(context.Context, string, string) (string, error) {
		ready <- struct{}{}
		<-release
		return "linux:uid:22", nil
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			w := bootstrapCall(m, "/auth/setup-os", loginInput{Login: n, Password: "password"})
			if w.Code == 200 {
				successes.Add(1)
			} else if w.Code != 403 {
				t.Errorf("unexpected status %d", w.Code)
			}
		}(name)
	}
	<-ready
	<-ready
	close(release)
	wg.Wait()
	if successes.Load() != 1 || len(m.state.Users) != 1 {
		t.Fatal("multiple bootstrap administrators")
	}
}
func TestBootstrapCLIProofBindsAccountAndDoesNotExposeSecret(t *testing.T) {
	m, path := bootstrapFixture(t)
	token := m.setupToken
	nonce := strings.Repeat("a1", 32)
	r := httptest.NewRequest("GET", "http://localhost/auth/setup-info?nonce="+nonce, nil)
	r.RemoteAddr = "127.0.0.1:123"
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	var info SetupInfo
	if e := json.Unmarshal(w.Body.Bytes(), &info); e != nil {
		t.Fatal(e)
	}
	if info.DBPath != path || info.TokenPath != path+".setup-token" || info.Proof != SetupServerProof(token, nonce) {
		t.Fatal("incorrect actual paths/server proof")
	}
	if strings.Contains(w.Body.String(), token) {
		t.Fatal("secret leaked to HTTP diagnostics")
	}
	for _, alter := range []func(*http.Request){func(r *http.Request) { r.RemoteAddr = "10.0.0.5:123" }, func(r *http.Request) { r.Header.Set("X-Forwarded-For", "10.0.0.5") }, func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }} {
		copy := r.Clone(context.Background())
		copy.Header = r.Header.Clone()
		alter(copy)
		w = httptest.NewRecorder()
		m.ServeHTTP(w, copy)
		if w.Code != 403 || strings.Contains(w.Body.String(), path) {
			t.Fatal("paths disclosed to remote/proxy/origin")
		}
	}
	a := BootstrapAccount{Login: "cli-admin", Name: "CLI admin", Password: "new administrator password", Nonce: nonce}
	a.Proof = SetupAccountProof(token, a)
	forged := a
	forged.Login = "attacker"
	if w = bootstrapCall(m, "/auth/setup-cli", forged); w.Code != 403 {
		t.Fatal("unsigned account change accepted")
	}
	if w = bootstrapCall(m, "/auth/setup-cli", a); w.Code != 200 {
		t.Fatalf("CLI rejected: %s", w.Body.String())
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("CLI unnecessarily issued browser session")
	}
	if w = bootstrapCall(m, "/auth/setup-cli", a); w.Code != 403 {
		t.Fatal("CLI replay reopened setup")
	}
}
func TestBootstrapOSWorksWithoutWritableTokenFile(t *testing.T) {
	m, path := bootstrapFixture(t)
	os.Remove(path + ".setup-token")
	if e := os.Mkdir(path+".setup-token", 0700); e != nil {
		t.Fatal(e)
	}
	loaded, e := New(m.db, nil, http.NotFoundHandler(), nil, path)
	if e != nil || loaded.setupError == "" || loaded.setupToken != "" {
		t.Fatalf("optional token error not isolated: %v", e)
	}
	loaded.verifyAdmin = func(context.Context, string, string) (string, error) { return "linux:uid:45", nil }
	if w := bootstrapCall(loaded, "/auth/setup-os", loginInput{Login: "os-admin", Password: "password"}); w.Code != 200 {
		t.Fatalf("OS fallback failed: %s", w.Body.String())
	}
}
func TestLiveOSAdminBootstrapAuthentication(t *testing.T) {
	admin, password := os.Getenv("SPAWNPOINT_ADMIN_TEST_USER"), os.Getenv("SPAWNPOINT_ADMIN_TEST_PASSWORD")
	if admin == "" || password == "" {
		t.Skip("requires disposable native admin account")
	}
	expected, e := resolveOS(admin)
	if e != nil {
		t.Fatal(e)
	}
	got, e := verifyOSAdmin(context.Background(), admin, password)
	if e != nil || got != expected {
		t.Fatalf("admin proof failed: %v", e)
	}
	if _, e = verifyOSAdmin(context.Background(), admin, password+"wrong"); e == nil {
		t.Fatal("wrong admin password accepted")
	}
	ordinary, pw := os.Getenv("SPAWNPOINT_AUTH_TEST_USER"), os.Getenv("SPAWNPOINT_AUTH_TEST_PASSWORD")
	if ordinary == "" || pw == "" {
		t.Fatal("missing standard-user negative fixture")
	}
	if _, e = verifyOSAdmin(context.Background(), ordinary, pw); e == nil {
		t.Fatal("non-admin account accepted")
	}
}

func TestLiveOSAdminRootAuthentication(t *testing.T) {
	name, pw := os.Getenv("SPAWNPOINT_ROOT_TEST_USER"), os.Getenv("SPAWNPOINT_ROOT_TEST_PASSWORD")
	if name == "" || pw == "" {
		t.Skip("requires disposable UID-0 test alias")
	}
	got, e := verifyOSAdmin(context.Background(), name, pw)
	if e != nil || got != "linux:uid:0" {
		t.Fatalf("UID-0 bootstrap authentication: %v", e)
	}
	if _, e = verifyOSAdmin(context.Background(), name, pw+"-wrong"); e == nil {
		t.Fatal("UID-0 wrong password accepted")
	}
}
