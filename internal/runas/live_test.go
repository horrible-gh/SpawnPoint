package runas

import (
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestLiveAuthenticatedAccount(t *testing.T) {
	user, pw := os.Getenv("SPAWNPOINT_RUNAS_TEST_USER"), os.Getenv("SPAWNPOINT_RUNAS_TEST_PASSWORD")
	if user == "" || pw == "" {
		t.Skip("requires an isolated OS test account")
	}
	dir := keyDir(t)
	s, e := Apply(dir, Input{Mode: "user", Username: user, Password: pw}, nil)
	if e != nil {
		t.Fatal(e)
	}
	command := `id -un; printf '%s\n' "$HOME" "$SPAWNPOINT_TEST_VALUE"; printf stderr-ok >&2`
	if runtime.GOOS == "windows" {
		command = `whoami & echo %USERPROFILE% & echo %SPAWNPOINT_TEST_VALUE% & echo stderr-ok 1>&2`
	}
	c, e := Launch(dir, s, command, "", map[string]string{"SPAWNPOINT_TEST_VALUE": "value,with=punctuation"})
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Ready(); e != nil {
		c.Abort()
		t.Fatal(e)
	}
	b, e := io.ReadAll(c.Output)
	c.Output.Close()
	code := c.Wait()
	if e != nil || code != 0 {
		t.Fatalf("read/exit: %v/%d", e, code)
	}
	out := strings.ToLower(string(b))
	if !strings.Contains(out, strings.ToLower(user)) || !strings.Contains(out, "value,with=punctuation") || !strings.Contains(out, "stderr-ok") {
		t.Fatalf("unexpected output: %q", out)
	}
	if strings.Contains(out, pw) {
		t.Fatal("password leaked to child output")
	}
	// A wrong password must not produce the command's sentinel under any account.
	bad, e := Apply(dir, Input{Mode: "user", Username: user, Password: pw + "-wrong"}, s)
	if e != nil {
		t.Fatal(e)
	}
	c, e = Launch(dir, bad, "echo AUTH-BYPASS", "", nil)
	if e == nil {
		e = c.Ready()
		if e == nil {
			c.Abort()
			t.Fatal("bad password authenticated")
		}
		c.Abort()
	}
}
