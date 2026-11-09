package httpapi

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestExecutionCredentialParsing(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{}`, `{"mode":"admin"}`, `{"mode":"user","username":""}`, `{"mode":"service","password":"secret"}`, `{"mode":"user","username":"alice","unknown":true}`} {
		if _, err := parseRunAs([]byte(raw)); err == nil {
			t.Errorf("accepted malformed identity: %s", raw)
		}
	}
	for _, raw := range []string{`{"mode":"service"}`, `{"mode":"user","username":"alice","password":"test"}`} {
		if _, err := parseRunAs([]byte(raw)); err != nil {
			t.Errorf("valid identity rejected: %s", raw)
		}
	}
	if in, err := parseRunAs(nil); in != nil || err != nil {
		t.Fatal("omission must preserve existing identity")
	}
}
func TestExecutionTransportBoundary(t *testing.T) {
	s := &Server{tokens: map[string]struct{}{"test-token": {}}}
	tests := []struct {
		name, host, peer, proto, token string
		tls, trust, want               bool
	}{
		{name: "local", host: "localhost:7527", peer: "127.0.0.1:1234", want: true},
		{name: "IPv6 local", host: "[::1]:7527", peer: "[::1]:1234", want: true},
		{name: "DNS rebinding", host: "attacker.example", peer: "127.0.0.1:1234", want: false},
		{name: "remote forged host", host: "localhost:7527", peer: "192.0.2.10:1234", want: false},
		{name: "plain remote", host: "server.example", peer: "192.0.2.10:1234", token: "test-token", want: false},
		{name: "TLS no token", host: "server.example", peer: "192.0.2.10:1234", tls: true, want: false},
		{name: "TLS token", host: "server.example", peer: "192.0.2.10:1234", tls: true, token: "test-token", want: true},
		{name: "untrusted forwarded", host: "server.example", peer: "127.0.0.1:1234", proto: "https", token: "test-token", want: false},
		{name: "trusted proxy", host: "server.example", peer: "127.0.0.1:1234", proto: "https", token: "test-token", trust: true, want: true},
		{name: "remote cannot spoof proxy", host: "server.example", peer: "192.0.2.10:1234", proto: "https", token: "test-token", trust: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SPAWNPOINT_TRUST_LOOPBACK_PROXY", "0")
			if tc.trust {
				t.Setenv("SPAWNPOINT_TRUST_LOOPBACK_PROXY", "1")
			}
			r := httptest.NewRequest("POST", "http://"+tc.host+"/processes", nil)
			r.RemoteAddr = tc.peer
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			r.Header.Set("X-Forwarded-Proto", tc.proto)
			if got := s.identityTransport(r); got != tc.want {
				t.Fatalf("transport=%v want=%v", got, tc.want)
			}
		})
	}
}
func TestExecutionCrossOrigin(t *testing.T) {
	r := httptest.NewRequest("POST", "http://localhost:7527/processes", nil)
	for _, origin := range []string{"null", "https://attacker.example", "http://localhost:7527.evil.test", "http://localhost:7527/path"} {
		r.Header.Set("Origin", origin)
		if sameOrigin(r) {
			t.Errorf("accepted origin %s", origin)
		}
	}
	r.Header.Set("Origin", "http://localhost:7527")
	if !sameOrigin(r) {
		t.Fatal("same-origin rejected")
	}
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if sameOrigin(r) {
		t.Fatal("cross-site fetch accepted")
	}
}
