package access

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"spawnpoint/internal/store"
)

func sessionFixture(t *testing.T) (*Manager, string, *time.Time) {
	t.Helper()
	m, path := bootstrapFixture(t)
	if e := m.commit(fixturePolicy(), "test", "fixture", "", ""); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Truncate(time.Second)
	m.now = func() time.Time { return now }
	return m, path, &now
}
func sessionRequest(method, path string, c *http.Cookie, csrf string, body any) *http.Request {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "https://localhost:7527"+path, bytes.NewReader(b))
	r.RemoteAddr = "127.0.0.1:44321"
	r.Header.Set("Content-Type", "application/json")
	if c != nil {
		r.AddCookie(c)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	return r
}
func sessionIssue(t *testing.T, m *Manager, remember bool, old *http.Cookie) (Principal, *http.Cookie) {
	t.Helper()
	w := httptest.NewRecorder()
	p, e := m.issueSession(w, sessionRequest("POST", "/auth/login", old, "", nil), *m.state.user("admin"), remember)
	if e != nil {
		t.Fatal(e)
	}
	if len(w.Result().Cookies()) != 1 {
		t.Fatal("missing session cookie")
	}
	return p, w.Result().Cookies()[0]
}
func sessionCall(m *Manager, method, path string, c *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	m.ServeHTTP(w, sessionRequest(method, path, c, csrf, body))
	return w
}
func sessionUser(t *testing.T, w *httptest.ResponseRecorder) any {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var v map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v["user"]
}
func TestRememberedSessionSurvivesIdleAndBrowserReturn(t *testing.T) {
	m, _, now := sessionFixture(t)
	p, c := sessionIssue(t, m, true, nil)
	if c.MaxAge != int(rememberedSessionLifetime) || !c.Expires.Equal(now.Add(30*24*time.Hour)) || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("incorrect persistent cookie: %+v", c)
	}
	*now = now.Add(7 * 24 * time.Hour)
	w := sessionCall(m, "GET", "/auth/me", c, "", nil)
	if sessionUser(t, w) == nil {
		t.Fatal("remembered session lost after inactivity")
	}
	if len(w.Result().Cookies()) != 1 {
		t.Fatal("return did not rotate due token")
	}
	if m.currentPrincipal(p) == nil {
		t.Fatal("stable session ID invalidated by renewal")
	}
	var remembered bool
	var tokenHash string
	if e := m.db.QueryRow("SELECT remembered,token_digest FROM access_session WHERE digest=?", p.Session).Scan(&remembered, &tokenHash); e != nil {
		t.Fatal(e)
	}
	if !remembered || tokenHash != digest(w.Result().Cookies()[0].Value) || tokenHash == c.Value {
		t.Fatal("persistent mode or token hash not stored")
	}
}
func TestOrdinarySessionStillExpires(t *testing.T) {
	for _, boundary := range []string{"idle", "absolute"} {
		t.Run(boundary, func(t *testing.T) {
			m, _, now := sessionFixture(t)
			p, c := sessionIssue(t, m, false, nil)
			if c.MaxAge != 0 || !c.Expires.IsZero() {
				t.Fatal("ordinary session became a persistent cookie")
			}
			if boundary == "idle" {
				*now = now.Add(time.Hour)
			} else {
				*now = now.Add(12 * time.Hour)
				if _, e := m.db.Exec("UPDATE access_session SET last_seen=? WHERE digest=?", now.Unix(), p.Session); e != nil {
					t.Fatal(e)
				}
			}
			if sessionUser(t, sessionCall(m, "GET", "/auth/me", c, "", nil)) != nil {
				t.Fatal("ordinary expiry was bypassed")
			}
			if m.currentPrincipal(p) != nil {
				t.Fatal("secondary authorization bypassed ordinary expiry")
			}
		})
	}
}
func TestRememberedSessionRotationPreservesDeadlineAndReauth(t *testing.T) {
	for _, endpoint := range []string{"/auth/me", "/auth/heartbeat"} {
		t.Run(endpoint, func(t *testing.T) {
			m, _, now := sessionFixture(t)
			p, old := sessionIssue(t, m, true, nil)
			deadline := old.Expires
			*now = now.Add(12 * time.Hour)
			method, body := "GET", any(nil)
			if endpoint == "/auth/heartbeat" {
				method = "POST"
				body = map[string]any{}
			}
			w := sessionCall(m, method, endpoint, old, p.CSRF, body)
			if w.Code != 200 || len(w.Result().Cookies()) != 1 {
				t.Fatalf("renewal: %d %s", w.Code, w.Body.String())
			}
			fresh := w.Result().Cookies()[0]
			if fresh.Value == old.Value || fresh.Expires != deadline || fresh.MaxAge != int(deadline.Unix()-now.Unix()) {
				t.Fatal("rotation changed absolute lifetime or kept token")
			}
			next, e := m.principal(sessionRequest("GET", "/auth/me", fresh, "", nil))
			if e != nil || next.CSRF != p.CSRF || next.Session != p.Session || next.AuthAt != p.AuthAt {
				t.Fatal("rotation changed CSRF, session ID or password verification age")
			}
			stale := sessionCall(m, method, endpoint, old, p.CSRF, body)
			if stale.Code != 200 || len(stale.Result().Cookies()) != 0 {
				t.Fatal("in-flight predecessor request replaced new cookie")
			}
			// A newly rotated login is not a newly verified password.
			admin := sessionCall(m, "POST", "/api/access/admin", fresh, p.CSRF, map[string]any{"action": "guest", "revision": m.revision})
			if admin.Code != 403 || !bytes.Contains(admin.Body.Bytes(), []byte("reauth_required")) {
				t.Fatal("rotation bypassed sensitive-action reauthentication")
			}
			*now = now.Add(time.Duration(sessionRenewalOverlap) * time.Second)
			stale = sessionCall(m, "GET", "/auth/me", old, "", nil)
			if sessionUser(t, stale) != nil || len(stale.Result().Cookies()) != 0 {
				t.Fatal("old token survived overlap or cleared a newer cookie")
			}
			if sessionUser(t, sessionCall(m, "GET", "/auth/me", fresh, "", nil)) == nil {
				t.Fatal("current token rejected after overlap")
			}
		})
	}
}
func TestRememberedSessionConcurrentTabsRotateOnce(t *testing.T) {
	m, _, now := sessionFixture(t)
	p, old := sessionIssue(t, m, true, nil)
	*now = now.Add(12 * time.Hour)
	const count = 24
	results := make(chan *httptest.ResponseRecorder, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- sessionCall(m, "POST", "/auth/heartbeat", old, p.CSRF, map[string]any{})
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for w := range results {
		if w.Code != 200 {
			t.Fatalf("parallel request: %d %s", w.Code, w.Body.String())
		}
		winners += len(w.Result().Cookies())
	}
	if winners != 1 {
		t.Fatalf("got %d successor cookies, want one", winners)
	}
	if m.currentPrincipal(p) == nil {
		t.Fatal("rotation invalidated already-authorized in-flight principal")
	}
}
func TestRememberedSessionAbsoluteDeadline(t *testing.T) {
	m, _, now := sessionFixture(t)
	p, c := sessionIssue(t, m, true, nil)
	deadline := now.Add(30 * 24 * time.Hour)
	for day := 0; day < 29; day++ {
		*now = now.Add(24 * time.Hour)
		w := sessionCall(m, "GET", "/auth/me", c, "", nil)
		if sessionUser(t, w) == nil {
			t.Fatal("remembered session expired early")
		}
		if cookies := w.Result().Cookies(); len(cookies) > 0 {
			c = cookies[0]
		}
	}
	var expires int64
	if e := m.db.QueryRow("SELECT expires_at FROM access_session WHERE digest=?", p.Session).Scan(&expires); e != nil {
		t.Fatal(e)
	}
	if expires != deadline.Unix() || !c.Expires.Equal(deadline) {
		t.Fatal("active use extended absolute lifetime")
	}
	*now = deadline
	if sessionUser(t, sessionCall(m, "GET", "/auth/me", c, "", nil)) != nil || m.currentPrincipal(p) != nil {
		t.Fatal("30-day deadline bypassed")
	}
}
func TestRememberedSessionRestartAndRevocation(t *testing.T) {
	m, path, now := sessionFixture(t)
	p, old := sessionIssue(t, m, true, nil)
	*now = now.Add(24 * time.Hour)
	w := sessionCall(m, "GET", "/auth/me", old, "", nil)
	fresh := w.Result().Cookies()[0]
	// Reopen SQLite, not just the Manager; no in-memory renewal state is required.
	if e := m.db.Close(); e != nil {
		t.Fatal(e)
	}
	db, e := store.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	reopened, e := New(db.DB(), nil, http.NotFoundHandler(), nil, path)
	if e != nil {
		t.Fatal(e)
	}
	reopened.now = func() time.Time { return *now }
	if sessionUser(t, sessionCall(reopened, "GET", "/auth/me", fresh, "", nil)) == nil {
		t.Fatal("server restart lost remembered session")
	}
	out := sessionCall(reopened, "POST", "/auth/logout", fresh, p.CSRF, map[string]any{})
	if out.Code != 200 || len(out.Result().Cookies()) != 1 || out.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout did not clear cookie")
	}
	for _, c := range []*http.Cookie{old, fresh} {
		if sessionUser(t, sessionCall(reopened, "GET", "/auth/me", c, "", nil)) != nil {
			t.Fatal("logout left a token valid")
		}
	}
}
func TestRememberedSessionSecurityInvalidation(t *testing.T) {
	for _, change := range []string{"version", "paused", "auth_disabled"} {
		t.Run(change, func(t *testing.T) {
			m, _, now := sessionFixture(t)
			p, c := sessionIssue(t, m, true, nil)
			*now = now.Add(24 * time.Hour)
			switch change {
			case "version":
				m.state.user(p.UserID).Version++
			case "paused":
				m.state.user(p.UserID).Status = "paused"
			case "auth_disabled":
				m.state.Settings.LocalLogin = false
			}
			if sessionUser(t, sessionCall(m, "GET", "/auth/me", c, "", nil)) != nil || m.currentPrincipal(p) != nil {
				t.Fatal("remembered login survived security invalidation")
			}
		})
	}
}
func TestRememberedSessionRevokeOthersAndOptOut(t *testing.T) {
	m, _, now := sessionFixture(t)
	p, first := sessionIssue(t, m, true, nil)
	_, second := sessionIssue(t, m, true, nil)
	*now = now.Add(12 * time.Hour)
	first = sessionCall(m, "GET", "/auth/me", first, "", nil).Result().Cookies()[0]
	second = sessionCall(m, "GET", "/auth/me", second, "", nil).Result().Cookies()[0]
	w := sessionCall(m, "POST", "/auth/revoke-sessions", first, p.CSRF, map[string]any{})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if sessionUser(t, sessionCall(m, "GET", "/auth/me", second, "", nil)) != nil {
		t.Fatal("other rotated session survived revocation")
	}
	if sessionUser(t, sessionCall(m, "GET", "/auth/me", first, "", nil)) == nil {
		t.Fatal("current session revoked with others")
	}
	_, ordinary := sessionIssue(t, m, false, first)
	if ordinary.MaxAge != 0 {
		t.Fatal("opting out retained persistent cookie")
	}
	if sessionUser(t, sessionCall(m, "GET", "/auth/me", first, "", nil)) != nil {
		t.Fatal("replaced rotated session still valid")
	}
	var count int
	if e := m.db.QueryRow("SELECT COUNT(*) FROM access_session").Scan(&count); e != nil || count != 1 {
		t.Fatalf("session replacement left extra rows: %d %v", count, e)
	}
}
func TestRememberedSessionDoesNotBypassOriginCSRFOrBearer(t *testing.T) {
	m, _, now := sessionFixture(t)
	p, c := sessionIssue(t, m, true, nil)
	*now = now.Add(12 * time.Hour)
	for _, kind := range []string{"origin", "csrf", "bearer"} {
		t.Run(kind, func(t *testing.T) {
			r := sessionRequest("POST", "/auth/heartbeat", c, p.CSRF, map[string]any{})
			want := 403
			switch kind {
			case "origin":
				r.Header.Set("Origin", "https://attacker.invalid")
			case "csrf":
				r.Header.Del("X-CSRF-Token")
			case "bearer":
				r.Header.Set("Authorization", "Bearer invalid")
				want = 401
			}
			w := httptest.NewRecorder()
			m.ServeHTTP(w, r)
			if w.Code != want || len(w.Result().Cookies()) != 0 {
				t.Fatalf("%s bypassed guard: %d", kind, w.Code)
			}
		})
	}
}
func TestSessionDatabaseErrorDoesNotLogOut(t *testing.T) {
	m, _, _ := sessionFixture(t)
	_, c := sessionIssue(t, m, true, nil)
	if e := m.db.Close(); e != nil {
		t.Fatal(e)
	}
	w := sessionCall(m, "GET", "/auth/me", c, "", nil)
	if w.Code != 503 || len(w.Result().Cookies()) != 0 {
		t.Fatal("storage failure erased login instead of a retryable failure")
	}
}
