//go:build linux

package access

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetupTokenRejectsPublicFileAndSymlink(t *testing.T) {
	p := filepath.Join(t.TempDir(), "setup")
	f, e := openSetupToken(p, true)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	f, e = openSetupToken(p, false)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	if e = os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
	if f, e = openSetupToken(p, false); e == nil {
		f.Close()
		t.Fatal("public token accepted")
	}
	link := p + "-link"
	if e = os.Symlink(p, link); e != nil {
		t.Fatal(e)
	}
	if f, e = openSetupToken(link, false); e == nil {
		f.Close()
		t.Fatal("symlink accepted")
	}
}
