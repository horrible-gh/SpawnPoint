//go:build linux

package runas

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateKeyAndLostKey(t *testing.T) {
	dir := keyDir(t)
	s, e := Apply(dir, Input{Mode: "user", Username: "test", Password: "ephemeral-test-password"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(filepath.Join(dir, "master.key"))
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal("key permissions")
	}
	if e = os.Remove(filepath.Join(dir, "master.key")); e != nil {
		t.Fatal(e)
	}
	if _, e = s.password(dir); e == nil {
		t.Fatal("missing key silently recreated")
	}
}
func TestUnsafeKeyRejected(t *testing.T) {
	dir := keyDir(t)
	os.Mkdir(dir, 0755)
	if _, e := Apply(dir, Input{Mode: "user", Username: "test", Password: "ephemeral-test-password"}, nil); e == nil {
		t.Fatal("world-readable key directory accepted")
	}
}
