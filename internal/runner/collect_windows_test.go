//go:build windows

package runner

import (
	"os"
	"testing"
)

// blockRename holds path open the way a log query does. Windows refuses to
// rename a file another handle has open without share-delete rights, and
// os.Open asks for none — so this is the real collision between a rotation and
// a reader, not a simulation of one.
func blockRename(t *testing.T, path string) func() {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	release := func() {
		if !closed {
			closed = true
			file.Close()
		}
	}
	t.Cleanup(release)
	return release
}
