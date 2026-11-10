package access

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

// Real TLS client -> TLS terminator -> HTTP backend, with a disposable SQLite
// policy and real session cookies. Native OS-password checks have separate CI.
func TestHTTPSProxyLoginSessionAndOrigin(t *testing.T) {
	t.Setenv("SPAWNPOINT_TRUST_LOOPBACK_PROXY", "")
	m, _ := bootstrapFixture(t)
	backend := httptest.NewServer(m)
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	proxy := httptest.NewTLSServer(&httputil.ReverseProxy{Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(target)
		p.Out.Host = p.In.Host // Caddy's normal HTTP upstream behavior
		p.SetXForwarded()
	}})
	defer proxy.Close()
	client := proxy.Client()
	client.Jar, _ = cookiejar.New(nil)
	call := func(path string, data any, origin, csrf string, want int) (*http.Response, map[string]any) {
		t.Helper()
		method := "GET"
		var body io.Reader
		if data != nil {
			b, _ := json.Marshal(data)
			body = bytes.NewReader(b)
			method = "POST"
		}
		r, err := http.NewRequest(method, proxy.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s: status=%d, want=%d", path, res.StatusCode, want)
		}
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return res, out
	}
	_, me := call("/auth/me", nil, "", "", 200)
	runtime := me["runtime"].(map[string]any)
	if runtime["https"] != true || runtime["secure"] != true {
		t.Fatal("TLS proxy was reported as remote HTTP")
	}
	in := map[string]string{"login": "proxy-admin", "password": "long-proxy-test-password", "setup_token": m.setupToken}
	call("/auth/setup", in, "http://attacker.invalid", "", 403)
	res, me := call("/auth/setup", in, proxy.URL, "", 200)
	if len(res.Cookies()) != 1 || !res.Cookies()[0].Secure || !res.Cookies()[0].HttpOnly || res.Cookies()[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("HTTPS session lost security attributes")
	}
	csrf := me["csrf"].(string)
	_, me = call("/auth/me", nil, "", "", 200)
	if me["user"] == nil {
		t.Fatal("HTTPS cookie did not authenticate next request")
	}
	call("/auth/heartbeat", map[string]any{}, "http://"+strings.TrimPrefix(proxy.URL, "https://"), csrf, 403)
	call("/auth/heartbeat", map[string]any{}, proxy.URL, "", 403)
	call("/auth/heartbeat", map[string]any{}, proxy.URL, csrf, 200)
	call("/auth/setup-info", nil, "", "", 403)
	call("/auth/logout", map[string]any{}, proxy.URL, csrf, 200)
	_, me = call("/auth/me", nil, "", "", 200)
	if me["user"] != nil {
		t.Fatal("logout did not invalidate proxied session")
	}
	call("/auth/login", map[string]string{"method": "local", "login": "proxy-admin", "password": in["password"]}, proxy.URL, "", 200)
}
