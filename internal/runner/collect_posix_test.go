//go:build !windows

package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// blockRename takes the write bit off the directory. An open handle does not
// stop a rename here the way it does on Windows, and the directory permission
// is the nearest equivalent: the rename fails, everything else about the
// collector carries on.
//
// It does nothing for a process running as root, which is why the test that
// uses it checks whether the block took and skips if it did not.
func blockRename(t *testing.T, path string) func() {
	t.Helper()
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	if err := os.Chmod(dir, mode&^0o222); err != nil {
		t.Fatal(err)
	}
	restored := false
	release := func() {
		if !restored {
			restored = true
			os.Chmod(dir, mode)
		}
	}
	t.Cleanup(release)
	return release
}
