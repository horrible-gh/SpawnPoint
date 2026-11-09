//go:build linux

package runas

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"sort"
	"strings"
	"syscall"
	"time"
)

func Capability() (bool, string) {
	if os.Geteuid() == 0 {
		return false, "Run SpawnPoint as a non-root service user to enable password-authenticated execution."
	}
	if _, err := os.Stat("/usr/bin/su"); err != nil {
		return false, "Install util-linux su to enable account execution."
	}
	return true, ""
}
func launch(s *Spec, password []byte) (*Child, error) {
	if ok, why := Capability(); !ok {
		return nil, errors.New(why)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		requestRead.Close()
		requestWrite.Close()
		return nil, err
	}
	cmd := exec.Command("/usr/bin/su", "--session-command", "exec "+shellQuote(exe)+" "+childFlag, "--shell", "/bin/sh", "--", s.Username)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	cmd.ExtraFiles = []*os.File{requestRead}
	// su/PAM reads this private pipe. No terminal echo, argv or environment copy.
	secret := append(append([]byte(nil), password...), '\n')
	pwRead, pwWrite, err := os.Pipe()
	if err != nil {
		requestRead.Close()
		requestWrite.Close()
		outRead.Close()
		outWrite.Close()
		wipe(secret)
		return nil, err
	}
	cmd.Stdin = pwRead
	cmd.Stdout = outWrite
	cmd.Stderr = outWrite
	err = cmd.Start()
	requestRead.Close()
	outWrite.Close()
	pwRead.Close()
	if err != nil {
		requestWrite.Close()
		outRead.Close()
		pwWrite.Close()
		wipe(secret)
		return nil, errors.New("cannot start the system account authenticator")
	}
	go func() { defer pwWrite.Close(); defer wipe(secret); pwWrite.Write(secret) }()
	c := &Child{PID: cmd.Process.Pid, Output: outRead, request: requestWrite, resume: func() error { return nil }}
	done := make(chan struct{})
	c.wait = func() int {
		cmd.Wait()
		close(done)
		if cmd.ProcessState == nil {
			return -1
		}
		return cmd.ProcessState.ExitCode()
	}
	c.terminate = func() {
		// su forwards TERM to the authenticated supervisor, which stops the target
		// process group. SIGKILL is only the bounded last resort.
		cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
			}
		}()
	}
	return c, nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func childInput() (*os.File, error) {
	f := os.NewFile(3, "execution-request")
	if f == nil {
		return nil, errors.New("no request pipe")
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		return nil, errors.New("invalid request pipe")
	}
	return f, nil
}
func executeChild(r Request) int {
	u, err := user.Current()
	if err != nil {
		return 125
	}
	// A fresh target-account environment, not the web service's environment.
	base := map[string]string{"HOME": u.HomeDir, "USER": u.Username, "LOGNAME": u.Username, "SHELL": "/bin/sh", "PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8", "PYTHONUTF8": "1", "PYTHONIOENCODING": "utf-8"}
	for k, v := range r.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return 125
		}
		base[k] = v
	}
	cmd := exec.Command("/bin/sh", "-c", r.Command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if r.Cwd != "" {
		cmd.Dir = r.Cwd
	} else {
		cmd.Dir = u.HomeDir
	}
	for k, v := range base {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	sort.Strings(cmd.Env)
	// Buffer output in a separate pipe until the parent has seen the handshake.
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		return 125
	}
	defer outputRead.Close()
	cmd.Stdout = outputWrite
	cmd.Stderr = outputWrite
	sig := make(chan os.Signal, 8)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err = cmd.Start(); err != nil {
		outputWrite.Close()
		return 125
	}
	outputWrite.Close()
	if err = ready(r.Nonce); err != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		return 125
	}
	copied := make(chan struct{})
	go func() { io.Copy(os.Stdout, outputRead); close(copied) }()
	waited := make(chan int, 1)
	go func() {
		cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		waited <- code
	}()
	var code int
	select {
	case code = <-waited:
	case <-sig:
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case code = <-waited:
		case <-time.After(time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			code = <-waited
		}
	}
	// Descendants must not survive their supervised shell, even when it exits
	// before them. All members have the authenticated user's UID.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-copied:
	case <-time.After(time.Second):
	}
	if code < 0 {
		return 1
	}
	return code
}
