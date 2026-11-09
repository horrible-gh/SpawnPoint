package main

import (
	"spawnpoint/internal/access"
	"strings"
	"testing"
)

func TestSetupURLCannotSendCredentialsOffHost(t *testing.T) {
	for _, raw := range []string{"http://evil.test", "http://127.0.0.1.evil.test", "http://user:pw@localhost:7527", "http://localhost:7527/login", "http://localhost:7527?next=x", "file:///etc/passwd", "http://10.0.0.1:7527"} {
		if _, e := localSetupURL(raw); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"http://localhost:7527", "http://127.0.0.1:9527", "http://[::1]:7527", "https://localhost"} {
		if _, e := localSetupURL(raw); e != nil {
			t.Fatalf("rejected %s", raw)
		}
	}
}
func TestBootstrapProofDomainSeparation(t *testing.T) {
	nonce := strings.Repeat("12", 32)
	secret := "test token"
	a := access.BootstrapAccount{Login: "admin", Password: "long password", Nonce: nonce}
	if access.SetupServerProof(secret, nonce) == access.SetupAccountProof(secret, a) {
		t.Fatal("server/client proof interchangeable")
	}
	before := access.SetupAccountProof(secret, a)
	a.Password += "x"
	if before == access.SetupAccountProof(secret, a) {
		t.Fatal("password not bound")
	}
}
