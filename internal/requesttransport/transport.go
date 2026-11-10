// Package requesttransport provides the single transport trust boundary used by
// login, session cookies, origin checks and OS execution credentials.
package requesttransport

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const proxySetting = "SPAWNPOINT_TRUST_LOOPBACK_PROXY"

func loopback(s string) bool {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = strings.Trim(s, "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localHost(s string) bool {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	return strings.EqualFold(host, "localhost") || loopback(s)
}

// DirectLocal excludes forwarded requests, even when a proxy rewrites Host to
// localhost. A local proxy's HTTP frontend must never get the CLI exception.
func DirectLocal(r *http.Request) bool {
	if !loopback(r.RemoteAddr) || !localHost(r.Host) {
		return false
	}
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if len(r.Header.Values(name)) != 0 {
			return false
		}
	}
	return true
}

// scalar rejects duplicate/comma-joined proxy metadata instead of guessing
// which hop is authoritative. It never changes the request's Host or peer.
func scalar(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	return value, value != "" && !strings.ContainsAny(value, ",\r\n")
}

func validForwardedFor(r *http.Request) bool {
	values := r.Header.Values("X-Forwarded-For")
	if len(values) != 1 || len(values[0]) > 2048 {
		return false
	}
	ips := strings.Split(values[0], ",")
	if len(ips) > 32 {
		return false
	}
	for _, value := range ips {
		if net.ParseIP(strings.TrimSpace(value)) == nil {
			return false
		}
	}
	return true
}

// HTTPS recognizes direct TLS or a same-machine reverse proxy. With no setting,
// the complete Caddy-style tuple is required: one https scheme, the original
// Host unchanged, and valid client-IP metadata. The actual socket peer MUST be
// loopback; LAN/remote peers cannot turn HTTP into HTTPS with forged headers.
//
// This deliberately trusts local OS processes, not untrusted local tenants.
// The reverse proxy must overwrite these headers (Caddy does by default).
// "0" disables proxy trust; "1" preserves the explicit legacy opt-in for a
// proxy that only supplies the scheme. Unknown settings fail closed.
func HTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !loopback(r.RemoteAddr) {
		return false
	}
	mode := strings.TrimSpace(os.Getenv(proxySetting))
	if mode != "" && mode != "auto" && mode != "1" {
		return false
	}
	proto, ok := scalar(r, "X-Forwarded-Proto")
	if !ok || proto != "https" {
		return false
	}
	if mode != "1" || len(r.Header.Values("X-Forwarded-Host")) != 0 {
		host, valid := scalar(r, "X-Forwarded-Host")
		if !valid || !strings.EqualFold(host, r.Host) {
			return false
		}
	}
	if mode != "1" || len(r.Header.Values("X-Forwarded-For")) != 0 {
		if !validForwardedFor(r) {
			return false
		}
	}
	return true
}

func Secure(r *http.Request) bool { return DirectLocal(r) || HTTPS(r) }

func SameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	u, err := url.Parse(origins[0])
	scheme := "http"
	if HTTPS(r) {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}
