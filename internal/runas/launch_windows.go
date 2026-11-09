//go:build windows

package runas

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

var advapi = syscall.NewLazyDLL("advapi32.dll")
var createWithLogon = advapi.NewProc("CreateProcessWithLogonW")
var kernel = syscall.NewLazyDLL("kernel32.dll")
var resumeThread = kernel.NewProc("ResumeThread")

func Capability() (bool, string)      { return true, "" }
func wide(s string) ([]uint16, error) { return syscall.UTF16FromString(s) }
func ptr(b []uint16) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&b[0]))
}
func launch(s *Spec, password []byte) (*Child, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	user, err := wide(s.Username)
	if err != nil {
		return nil, err
	}
	var domain []uint16
	if s.Domain != "" {
		domain, err = wide(s.Domain)
		if err != nil {
			return nil, err
		}
	}
	pw, err := wide(string(password))
	if err != nil {
		return nil, err
	}
	defer func() {
		for i := range pw {
			pw[i] = 0
		}
	}()
	app, err := wide(exe)
	if err != nil {
		return nil, err
	}
	line, err := wide(syscall.EscapeArg(exe) + " " + childFlag)
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
	if err = syscall.SetHandleInformation(syscall.Handle(requestRead.Fd()), syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
		requestRead.Close()
		requestWrite.Close()
		outRead.Close()
		outWrite.Close()
		return nil, err
	}
	if err = syscall.SetHandleInformation(syscall.Handle(outWrite.Fd()), syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
		requestRead.Close()
		requestWrite.Close()
		outRead.Close()
		outWrite.Close()
		return nil, err
	}
	// CreateProcessWithLogonW duplicates these explicit standard handles into
	// the authenticated process; no password ever enters the request pipe.
	si := syscall.StartupInfo{Cb: uint32(unsafe.Sizeof(syscall.StartupInfo{})), Flags: syscall.STARTF_USESTDHANDLES | syscall.STARTF_USESHOWWINDOW, ShowWindow: syscall.SW_HIDE, StdInput: syscall.Handle(requestRead.Fd()), StdOutput: syscall.Handle(outWrite.Fd()), StdErr: syscall.Handle(outWrite.Fd())}
	var pi syscall.ProcessInformation
	r, _, callErr := createWithLogon.Call(ptr(user), ptr(domain), ptr(pw), 1, ptr(app), ptr(line), 0x00000004|0x08000000|0x00000400, 0, 0, uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
	runtime.KeepAlive(user)
	runtime.KeepAlive(domain)
	runtime.KeepAlive(pw)
	runtime.KeepAlive(app)
	runtime.KeepAlive(line)
	requestRead.Close()
	outWrite.Close()
	if r == 0 {
		requestWrite.Close()
		outRead.Close()
		return nil, fmt.Errorf("Windows account logon failed: %w", callErr)
	}
	c := &Child{PID: int(pi.ProcessId), Handle: uintptr(pi.Process), Output: outRead, request: requestWrite}
	thread := pi.Thread
	c.resume = func() error {
		r, _, e := resumeThread.Call(uintptr(thread))
		syscall.CloseHandle(thread)
		thread = 0
		if r == 0xffffffff {
			return e
		}
		return nil
	}
	c.terminate = func() {
		syscall.TerminateProcess(pi.Process, 1)
		if thread != 0 {
			syscall.CloseHandle(thread)
			thread = 0
		}
	}
	c.wait = func() int {
		defer syscall.CloseHandle(pi.Process)
		syscall.WaitForSingleObject(pi.Process, syscall.INFINITE)
		var code uint32
		if syscall.GetExitCodeProcess(pi.Process, &code) != nil {
			return -1
		}
		return int(code)
	}
	return c, nil
}
func childInput() (*os.File, error) {
	if os.Stdin == nil {
		return nil, errors.New("no request pipe")
	}
	return os.Stdin, nil
}
func executeChild(r Request) int {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	shell := systemRoot + `\System32\cmd.exe`
	cmd := &exec.Cmd{Path: shell, SysProcAttr: &syscall.SysProcAttr{CmdLine: syscall.EscapeArg(shell) + ` /d /s /c "chcp 65001>nul & ` + r.Command + `"`, HideWindow: true}}
	if r.Cwd != "" {
		cmd.Dir = r.Cwd
	} else {
		cmd.Dir = os.Getenv("USERPROFILE")
	}
	env := map[string]string{}
	names := map[string]string{}
	for _, v := range os.Environ() {
		i := strings.Index(v[1:], "=") + 1
		if i <= 0 {
			continue
		}
		k := v[:i]
		env[strings.ToUpper(k)] = v[i+1:]
		names[strings.ToUpper(k)] = k
	}
	env["PYTHONUTF8"] = "1"
	names["PYTHONUTF8"] = "PYTHONUTF8"
	env["PYTHONIOENCODING"] = "utf-8"
	names["PYTHONIOENCODING"] = "PYTHONIOENCODING"
	for k, v := range r.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return 125
		}
		env[strings.ToUpper(k)] = v
		names[strings.ToUpper(k)] = k
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, names[k]+"="+v)
	}
	sort.Strings(cmd.Env)
	// The supervisor was assigned to the parent-owned job before resuming; the
	// actual shell and its descendants inherit that job at creation.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Ready is written before creating the command. Command-start failure is
	// reported as nonzero exit with a short error, never a fallback identity.
	// Check the directory as the target account before declaring readiness.
	if cmd.Dir != "" {
		st, e := os.Stat(cmd.Dir)
		if e != nil || !st.IsDir() {
			return 125
		}
	}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		return 125
	}
	defer outputRead.Close()
	cmd.Stdout = outputWrite
	cmd.Stderr = outputWrite
	if err = cmd.Start(); err != nil {
		outputWrite.Close()
		return 125
	}
	outputWrite.Close()
	if err = ready(r.Nonce); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 125
	}
	done := make(chan struct{})
	go func() { defer close(done); copyOutput(outputRead) }()
	cmd.Wait()
	<-done
	if cmd.ProcessState == nil {
		return 1
	}
	code := cmd.ProcessState.ExitCode()
	if code < 0 {
		return 1
	}
	return code
}
