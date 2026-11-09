package access

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestTransportDoesNotTrustForwardingByDefault(t *testing.T) {
	t.Setenv("SPAWNPOINT_TRUST_LOOPBACK_PROXY", "")
	r := httptest.NewRequest("POST", "http://example.test/auth/login", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	if Secure(r) {
		t.Fatal("untrusted proxy accepted")
	}
	t.Setenv("SPAWNPOINT_TRUST_LOOPBACK_PROXY", "1")
	if !Secure(r) {
		t.Fatal("explicit loopback proxy rejected")
	}
	r.RemoteAddr = "10.0.0.5:1234"
	if Secure(r) {
		t.Fatal("remote proxy header accepted")
	}
	r.TLS = &tls.ConnectionState{}
	if !Secure(r) {
		t.Fatal("direct TLS rejected")
	}
}
func TestOriginAndLocalExceptions(t *testing.T) {
	r := httptest.NewRequest("POST", "http://localhost:7527/auth/login", nil)
	r.RemoteAddr = "127.0.0.1:1111"
	if !Secure(r) {
		t.Fatal("local denied")
	}
	r.Header.Set("Origin", "http://evil.test")
	if sameOrigin(r) {
		t.Fatal("cross-origin allowed")
	}
	r.Header.Set("Origin", "http://localhost:7527")
	if !sameOrigin(r) {
		t.Fatal("same origin denied")
	}
	r.Header.Set("Origin", "https://localhost:7527")
	if sameOrigin(r) {
		t.Fatal("scheme confusion")
	}
	r.Host = "evil.test"
	if Secure(r) {
		t.Fatal("DNS rebinding allowed")
	}
	r.Host = "localhost:7527"
	r.RemoteAddr = "10.0.0.5:1111"
	if Secure(r) {
		t.Fatal("remote Host localhost allowed")
	}
}
