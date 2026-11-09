//go:build windows

package runas

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var user32 = syscall.NewLazyDLL("user32.dll")
var createWindowStation = user32.NewProc("CreateWindowStationW")
var setProcessWindowStation = user32.NewProc("SetProcessWindowStation")
var createDesktop = user32.NewProc("CreateDesktopW")
var setThreadDesktop = user32.NewProc("SetThreadDesktop")
var convertSDDL = advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")

// prepareChildDesktop runs ONLY in the authenticated, disposable supervisor,
// on a locked OS thread. Never change the server's process-wide window station
// or widen the ACL of its service/interactive desktop.
//
// The station name must be NULL: only administrators may specify a name.
// Windows instead derives a name from this fresh authenticated logon session.
// CREATE_ONLY refuses an existing object rather than trusting its ACL.
// The new objects grant access only to the target account and SYSTEM.
//
// Handles live until the supervisor exits (OS cleanup). Closing an attached
// station/desktop would fail. Descendants inherit their own references.
func prepareChildDesktop(nonce string) ([]syscall.Handle, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("query execution account: %w", err)
	}
	defer token.Close()
	u, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("query execution account SID: %w", err)
	}
	sid, err := u.User.Sid.String()
	if err != nil {
		return nil, err
	}
	sddl, err := wide(desktopSDDL(sid))
	if err != nil {
		return nil, err
	}
	var descriptor uintptr
	r, _, callErr := convertSDDL.Call(ptr(sddl), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	runtime.KeepAlive(sddl)
	if r == 0 {
		return nil, fmt.Errorf("create private desktop security: %w", callErr)
	}
	defer syscall.LocalFree(syscall.Handle(descriptor))
	sa := syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
		InheritHandle:      1,
	}
	station, _, callErr := createWindowStation.Call(0, 1, 0x000f037f, uintptr(unsafe.Pointer(&sa)))
	if station == 0 {
		return nil, fmt.Errorf("create private window station: %w", callErr)
	}
	if r, _, callErr = setProcessWindowStation.Call(station); r == 0 {
		return nil, fmt.Errorf("select private window station: %w", callErr)
	}
	name, err := wide("SpawnPoint-" + nonce)
	if err != nil {
		return nil, err
	}
	desktop, _, callErr := createDesktop.Call(ptr(name), 0, 0, 0, 0x000f01ff, uintptr(unsafe.Pointer(&sa)))
	runtime.KeepAlive(name)
	runtime.KeepAlive(sa)
	if desktop == 0 {
		return nil, fmt.Errorf("create private desktop: %w", callErr)
	}
	if r, _, callErr = setThreadDesktop.Call(desktop); r == 0 {
		return nil, fmt.Errorf("select private desktop: %w", callErr)
	}
	return []syscall.Handle{syscall.Handle(station), syscall.Handle(desktop)}, nil
}

func desktopSDDL(sid string) string {
	return "D:P(A;;GA;;;SY)(A;;GA;;;" + sid + ")"
}
