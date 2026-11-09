//go:build linux

package access

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Membership of 'sudo'/'wheel', or permission for some unrelated sudo command,
// does not establish administrative authority. PAM authenticates the actual
// account; that account must be UID 0 or allowed by sudo policy to start a root shell. The policy query does not
// execute that shell. No supplied command, shell profile, password argv, or root web service.
// The second password is on a separate inherited FD so PAM cannot consume it.
func verifyOSAdmin(ctx context.Context, name, password string) (string, error) {
	if ok, _ := osAvailable(); !ok {
		return "", errors.New("OS authenticator unavailable")
	}
	expected, e := resolveOS(name)
	if e != nil {
		return "", e
	}
	if password == "" || len(password) > 1024 || strings.ContainsAny(password, "\x00\r\n") {
		return "", errors.New("invalid credential")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	extraRead, extraWrite, e := os.Pipe()
	if e != nil {
		return "", e
	}
	defer extraRead.Close()
	// Shell and id are fixed OS executables. A limited sudo rule for just id is
	// insufficient: root /bin/sh without arguments must be permitted by the OS policy.
	const probe = `u=$(/usr/bin/id -u) || exit 1; if [ "$u" != 0 ]; then /usr/bin/sudo -k -S -p '' -l -u root -- /bin/sh <&3 >/dev/null || exit 1; fi; printf 'SPADMIN:%s\n' "$u"`
	cmd := exec.CommandContext(ctx, "/usr/bin/su", "--shell", "/bin/sh", "--command", probe, "--", name)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	cmd.Stdin = strings.NewReader(password + "\n")
	cmd.ExtraFiles = []*os.File{extraRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var output boundedProbe
	cmd.Stdout = &output
	if e = cmd.Start(); e != nil {
		extraWrite.Close()
		return "", errors.New("cannot start OS authenticator")
	}
	extraRead.Close()
	secret := []byte(password + "\n")
	go func() { defer extraWrite.Close(); defer clear(secret); _, _ = extraWrite.Write(secret) }()
	if e = cmd.Wait(); e != nil {
		return "", errors.New("OS administrator authentication failed")
	}
	if strings.TrimSpace(output.String()) != "SPADMIN:"+strings.TrimPrefix(expected, "linux:uid:") {
		return "", errors.New("OS identity mismatch")
	}
	return expected, nil
}

type boundedProbe struct{ bytes.Buffer }

func (b *boundedProbe) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, errors.New("oversized authentication output")
	}
	return b.Buffer.Write(p)
}
