//go:build windows

package runas

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var logonUser = advapi.NewProc("LogonUserW")
var createWithToken = advapi.NewProc("CreateProcessWithTokenW")
var lookupPrivilege = advapi.NewProc("LookupPrivilegeValueW")
var checkPrivilege = advapi.NewProc("PrivilegeCheck")

type privilegeLUID struct {
	Low  uint32
	High int32
}
type singlePrivilegeSet struct {
	Count      uint32
	Control    uint32
	LUID       privilegeLUID
	Attributes uint32
}

// Inspect existing rights only; never enable or install privileges.
func tokenLaunchAvailable() (bool, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return false, err
	}
	defer token.Close()
	name, _ := wide("SeImpersonatePrivilege")
	privileges := singlePrivilegeSet{Count: 1, Control: 1, Attributes: 2}
	r, _, callErr := lookupPrivilege.Call(0, ptr(name), uintptr(unsafe.Pointer(&privileges.LUID)))
	runtime.KeepAlive(name)
	if r == 0 {
		return false, callErr
	}
	var available uint32
	r, _, callErr = checkPrivilege.Call(uintptr(token), uintptr(unsafe.Pointer(&privileges)), uintptr(unsafe.Pointer(&available)))
	if r == 0 {
		return false, callErr
	}
	return available != 0, nil
}

// Select before authentication. Never retry a rejected credential or fall back
// to the service identity. The returned cleanup owns the private desktop and
// the unique logon-session ACL grant until the supervised process exits.
func startWithCredentials(user, domain, password, app, line []uint16, si *syscall.StartupInfo, pi *syscall.ProcessInformation) (uintptr, func(), error) {
	noop := func() {}
	useToken, err := tokenLaunchAvailable()
	if err != nil {
		return 0, noop, fmt.Errorf("query process-launch privilege: %w", err)
	}
	const flags = 0x00000004 | 0x08000000 | 0x00000400
	if !useToken {
		r, _, callErr := createWithLogon.Call(ptr(user), ptr(domain), ptr(password), 1, ptr(app), ptr(line), flags, 0, 0, uintptr(unsafe.Pointer(si)), uintptr(unsafe.Pointer(pi)))
		return r, noop, callErr
	}
	var token syscall.Token
	r, _, callErr := logonUser.Call(ptr(user), ptr(domain), ptr(password), 2, 0, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return 0, noop, callErr
	}
	defer token.Close()
	desktop, cleanup, err := prepareLaunchDesktop(token)
	if err != nil {
		return 0, noop, fmt.Errorf("prepare authenticated desktop: %w", err)
	}
	si.Desktop = &desktop[0]
	r, _, callErr = createWithToken.Call(uintptr(token), 1, ptr(app), ptr(line), flags, 0, 0, uintptr(unsafe.Pointer(si)), uintptr(unsafe.Pointer(pi)))
	runtime.KeepAlive(desktop)
	if r == 0 {
		cleanup()
		return 0, noop, callErr
	}
	return r, cleanup, nil
}
