package runas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M)      { HandleChild(); os.Exit(m.Run()) }
func keyDir(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "credentials") }
func TestProtectAndMetadata(t *testing.T) {
	dir := keyDir(t)
	input := Input{Mode: "user", Username: "example", Password: "not-a-real-password-!42"}
	spec, err := Apply(dir, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(spec.JSON(), input.Password) {
		t.Fatal("plaintext in storage")
	}
	got, err := spec.password(dir)
	if err != nil || string(got) != input.Password {
		t.Fatalf("decrypt: %v", err)
	}
	wipe(got)
	b, _ := json.Marshal(spec.Summary())
	if strings.Contains(string(b), input.Password) || strings.Contains(string(b), spec.Sealed) {
		t.Fatal("secret in public metadata")
	}
	parsed, err := Parse(spec.JSON())
	if err != nil || parsed.Username != spec.Username {
		t.Fatal("persistence round trip failed")
	}
	changed := Clone(spec)
	changed.Username = "another"
	if _, err = changed.password(dir); err == nil {
		t.Fatal("credential not bound to account")
	}
}
func TestKeepReplaceClear(t *testing.T) {
	dir := keyDir(t)
	input := Input{Mode: "user", Username: "example", Password: "first!42"}
	first, err := Apply(dir, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	input.Password = ""
	kept, err := Apply(dir, input, first)
	if err != nil || kept.Sealed != first.Sealed {
		t.Fatal("unchanged account did not retain credential")
	}
	input.Username = "other"
	if _, err = Apply(dir, input, first); err == nil {
		t.Fatal("changed account retained old password")
	}
	input.Username = "example"
	input.Password = "second!42"
	next, err := Apply(dir, input, first)
	if err != nil || next.Sealed == first.Sealed {
		t.Fatal("replacement failed")
	}
	cleared, err := Apply(dir, Input{Mode: "service"}, next)
	if err != nil || cleared != nil {
		t.Fatal("switch to service did not clear credential")
	}
}
func TestInvalidInputs(t *testing.T) {
	for _, i := range []Input{{Mode: "bogus"}, {Mode: "user"}, {Mode: "user", Username: "a\x00b"}, {Mode: "user", Username: "a", Password: strings.Repeat("x", 4097)}, {Mode: "service", Password: "secret"}} {
		if _, err := Normalize(i); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
	if runtime.GOOS != "windows" {
		if _, err := Normalize(Input{Mode: "user", Username: "-bad", Password: "x"}); err == nil {
			t.Fatal("accepted option-like username")
		}
	}
	if _, err := Parse("broken"); err == nil {
		t.Fatal("corrupt identity must not fall back to service")
	}
}
func TestThrottle(t *testing.T) {
	a := "unit-test-account"
	if err := acquire(a); err != nil {
		t.Fatal(err)
	}
	if err := acquire(a); err == nil {
		t.Fatal("concurrent authentication allowed")
	}
	release(a, true)
	if err := acquire(a); err == nil {
		t.Fatal("failed logon not throttled")
	}
	release(a, false)
	if err := acquire(a); err != nil {
		t.Fatal(err)
	}
	release(a, false)
}
