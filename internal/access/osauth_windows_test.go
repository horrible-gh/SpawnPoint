//go:build windows

package access

import (
	"context"
	"os"
	"strings"
	"testing"
)

// These checks also run with SpawnPoint's own isolated Windows account on
// hosted CI. Registration must resolve the same SID as actual OS logon.
func TestLiveOSLoginWindowsLocalAliases(t *testing.T) {
	username, password := os.Getenv("SPAWNPOINT_AUTH_TEST_USER"), os.Getenv("SPAWNPOINT_AUTH_TEST_PASSWORD")
	if username == "" || password == "" {
		t.Skip("requires isolated Windows account")
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Fatal("no computer name", err)
	}
	var expected string
	for _, name := range []string{username, `.\` + username, host + `\` + username} {
		got, err := resolveOS(name)
		if err != nil || !strings.HasPrefix(got, "windows:sid:") {
			t.Fatalf("resolve %q: %q, %v", name, got, err)
		}
		if expected != "" && got != expected {
			t.Fatalf("local aliases differ: %q: %s / %s", name, expected, got)
		}
		expected = got
	}
	authenticated, err := verifyOS(context.Background(), `.\`+username, password)
	if err != nil || expected != authenticated {
		t.Fatalf("validated logon SID differs from stored account: %s / %s / %v", expected, authenticated, err)
	}
	if _, err := resolveOS(`.\does-not-exist-spawnpoint-test`); err == nil {
		t.Fatal("unknown local user accepted")
	}
}
