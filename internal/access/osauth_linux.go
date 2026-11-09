//go:build linux

package access

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

func osAvailable() (bool, string) {
	if os.Geteuid() == 0 {
		return false, "OS 계정 인증은 non-root 서비스 계정에서 실행하세요"
	}
	if _, e := os.Stat("/usr/bin/su"); e != nil {
		return false, "util-linux su/PAM이 필요합니다"
	}
	return true, ""
}
func resolveOS(name string) (string, error) {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "/\\: \t\r\n\x00") {
		return "", errors.New("invalid OS username")
	}
	u, e := user.Lookup(name)
	if e != nil {
		return "", e
	}
	return "linux:uid:" + u.Uid, nil
}

// util-linux su invokes PAM authentication and account-policy checks. It runs
// only a fixed identity probe, never a user command or a shell startup file.
// No password is persisted, included in argv, or exported to the environment.
func verifyOS(ctx context.Context, name, password string) (string, error) {
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/su", "--shell", "/bin/sh", "--command", "/usr/bin/id -u", "--", name)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	cmd.Stdin = strings.NewReader(password + "\n")
	b, e := cmd.Output()
	if e != nil {
		return "", errors.New("OS authentication failed")
	}
	uid := strings.TrimSpace(string(b))
	if _, e = strconv.ParseUint(uid, 10, 32); e != nil || "linux:uid:"+uid != expected {
		return "", errors.New("OS identity mismatch")
	}
	return expected, nil
}
