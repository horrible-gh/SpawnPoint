//go:build windows

package runas

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Read the bootstrap protocol directly in this isolated test so setup errors
// are diagnosed without making the production API expose pre-handshake output.
func TestLivePrivateDesktopBootstrap(t *testing.T) {
	user, password := os.Getenv("SPAWNPOINT_RUNAS_TEST_USER"), os.Getenv("SPAWNPOINT_RUNAS_TEST_PASSWORD")
	if user == "" || password == "" {
		t.Skip("requires an isolated OS test account")
	}
	dir := keyDir(t)
	spec, err := Apply(dir, Input{Mode: "user", Username: user, Password: password}, nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := Launch(dir, spec, "whoami", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer child.finishAttempt(false)
	if err := child.resume(); err != nil {
		child.Abort()
		t.Fatal(err)
	}
	go func() {
		defer child.request.Close()
		child.request.Write(child.payload)
	}()
	type result struct {
		text string
		code int
	}
	done := make(chan result, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(child.Output, 16384))
		child.Output.Close()
		done <- result{strings.ReplaceAll(string(b), password, "<redacted>"), child.Wait()}
	}()
	select {
	case got := <-done:
		if got.code != 0 || !strings.Contains(got.text, readyPrefix+child.nonce) || !strings.Contains(strings.ToLower(got.text), strings.ToLower(user)) {
			t.Fatalf("private desktop bootstrap exit=%d output=%q", got.code, got.text)
		}
	case <-time.After(45 * time.Second):
		child.Abort()
		t.Fatal("private desktop bootstrap timed out")
	}
}
