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

// prepareChildDesktop runs ONLY in the already-authenticated, disposable
// supervisor, on a locked OS thread. Never change the server's process-wide
// window station or widen the ACL of its service/interactive desktop.
//
// CreateProcessWithLogonW does not grant the target user access to an inherited
// service desktop on modern Windows. Pure-Go bootstrap code can run there, but
// USER32-importing descendants then fail with STATUS_DLL_INIT_FAILED (0xc0000142).
// Window-station/desktop functions can establish a private connection before
// other USER32/GDI calls. The private objects grant access only to this target
// account and SYSTEM, not Everyone or Authenticated Users.
//
// These handles deliberately live until the supervisor exits (OS cleanup).
// Closing an attached desktop/station would fail, and restoring the inherited
// inaccessible service desktop would reintroduce the original bug. Descendants
// retain their own inherited references for their lifetime.
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
	name, err := wide("SpawnPoint-" + nonce)
	if err != nil {
		return nil, err
	}
	// CWF_CREATE_ONLY refuses an existing object rather than trusting its ACL.
	// WINSTA_ALL_ACCESS / DESKTOP_ALL_ACCESS apply only to these private objects.
	station, _, callErr := createWindowStation.Call(ptr(name), 1, 0x000f037f, uintptr(unsafe.Pointer(&sa)))
	runtime.KeepAlive(name)
	if station == 0 {
		return nil, fmt.Errorf("create private window station: %w", callErr)
	}
	if r, _, callErr = setProcessWindowStation.Call(station); r == 0 {
		return nil, fmt.Errorf("select private window station: %w", callErr)
	}
	name, _ = wide("Default")
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
