package requesttransport

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func proxyRequest() *http.Request {
	r := httptest.NewRequest("POST", "http://spawnpoint.prd/auth/login", nil)
	r.RemoteAddr = "127.0.0.1:32100"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "spawnpoint.prd")
	r.Header.Set("X-Forwarded-For", "192.0.2.25")
	r.Header.Set("Origin", "https://spawnpoint.prd")
	return r
}

func TestProxyHTTPSBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		edit func(*http.Request)
		want bool
	}{
		{name: "Caddy no configuration", want: true},
		{name: "explicit auto", mode: "auto", want: true},
		{name: "explicit legacy opt-in", mode: "1", want: true},
		{name: "explicit opt-out", mode: "0"},
		{name: "invalid setting", mode: "invalid"},
		{name: "IPv6 proxy", edit: func(r *http.Request) { r.RemoteAddr = "[::1]:32100" }, want: true},
		{name: "IPv4-mapped proxy", edit: func(r *http.Request) { r.RemoteAddr = "[::ffff:127.0.0.1]:32100" }, want: true},
		{name: "remote forged full tuple", edit: func(r *http.Request) { r.RemoteAddr = "192.0.2.25:32100" }},
		{name: "remote spoofed loopback for", edit: func(r *http.Request) { r.RemoteAddr = "192.0.2.25:32100"; r.Header.Set("X-Forwarded-For", "127.0.0.1") }},
		{name: "peer hostname is not proof", edit: func(r *http.Request) { r.RemoteAddr = "localhost:32100" }},
		{name: "plain HTTP proxy", edit: func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }},
		{name: "missing scheme", edit: func(r *http.Request) { r.Header.Del("X-Forwarded-Proto") }},
		{name: "duplicate scheme", edit: func(r *http.Request) { r.Header.Add("X-Forwarded-Proto", "http") }},
		{name: "identical duplicate scheme", edit: func(r *http.Request) { r.Header.Add("X-Forwarded-Proto", "https") }},
		{name: "scheme list", edit: func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https,http") }},
		{name: "missing host", edit: func(r *http.Request) { r.Header.Del("X-Forwarded-Host") }},
		{name: "wrong host", edit: func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "attacker.test") }},
		{name: "duplicate host", edit: func(r *http.Request) { r.Header.Add("X-Forwarded-Host", r.Host) }},
		{name: "missing client IP", edit: func(r *http.Request) { r.Header.Del("X-Forwarded-For") }},
		{name: "invalid client IP", edit: func(r *http.Request) { r.Header.Set("X-Forwarded-For", "attacker.test") }},
		{name: "duplicate client field", edit: func(r *http.Request) { r.Header.Add("X-Forwarded-For", "192.0.2.25") }},
		{name: "invalid client chain", edit: func(r *http.Request) { r.Header.Set("X-Forwarded-For", "192.0.2.25, unknown") }},
		{name: "valid client chain", edit: func(r *http.Request) { r.Header.Set("X-Forwarded-For", "2001:db8::1, 192.0.2.25") }, want: true},
		{name: "direct TLS", mode: "0", edit: func(r *http.Request) { r.TLS = &tls.ConnectionState{}; r.RemoteAddr = "192.0.2.25:32100" }, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(proxySetting, tc.mode)
			r := proxyRequest()
			if tc.edit != nil {
				tc.edit(r)
			}
			if got := HTTPS(r); got != tc.want {
				t.Fatalf("HTTPS=%v, want %v", got, tc.want)
			}
			if DirectLocal(r) {
				t.Fatal("proxy was granted the direct-local exception")
			}
		})
	}
}

func TestLocalHTTPAndProxyOrigin(t *testing.T) {
	t.Setenv(proxySetting, "")
	for _, addr := range []string{"localhost:7527", "127.0.0.1:7527", "[::1]:7527"} {
		r := httptest.NewRequest("POST", "http://"+addr+"/auth/login", nil)
		r.RemoteAddr = "127.0.0.1:32100"
		if !DirectLocal(r) || !Secure(r) || HTTPS(r) {
			t.Fatal("local HTTP changed")
		}
		// Rewritten Host must not turn a forwarded HTTP login into a local one.
		r.Header.Set("X-Forwarded-Proto", "http")
		if Secure(r) {
			t.Fatal("HTTP proxy bypassed transport guard using local Host")
		}
	}
	r := proxyRequest()
	if !SameOrigin(r) {
		t.Fatal("original HTTPS origin rejected behind Caddy")
	}
	for _, origin := range []string{"http://spawnpoint.prd", "https://attacker.test", "null", "https://spawnpoint.prd/path", "https://spawnpoint.prd:444"} {
		r.Header.Set("Origin", origin)
		if SameOrigin(r) {
			t.Fatalf("accepted wrong origin %q", origin)
		}
	}
	r.Header.Set("Origin", "https://spawnpoint.prd")
	r.Header.Add("Origin", "https://attacker.test")
	if SameOrigin(r) {
		t.Fatal("multiple Origin values accepted")
	}
}
