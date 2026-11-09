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

// No rights are enabled or installed here. LocalService already has the
// impersonation privilege, while an ordinary console account need not have it.
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

// Services need CreateProcessWithTokenW's desktop-access initialization.
// CreateProcessWithLogonW alone leaves USER32-dependent programs unable to
// initialize on a service desktop. Select the API BEFORE authenticating; never
// retry a rejected password, enable privileges, or fall back to another user.
func startWithCredentials(user, domain, password, app, line []uint16, si *syscall.StartupInfo, pi *syscall.ProcessInformation) (uintptr, error) {
	useToken, err := tokenLaunchAvailable()
	if err != nil {
		return 0, fmt.Errorf("query process-launch privilege: %w", err)
	}
	const flags = 0x00000004 | 0x08000000 | 0x00000400 // suspended, no console, Unicode environment
	if !useToken {
		r, _, callErr := createWithLogon.Call(ptr(user), ptr(domain), ptr(password), 1, ptr(app), ptr(line), flags, 0, 0, uintptr(unsafe.Pointer(si)), uintptr(unsafe.Pointer(pi)))
		return r, callErr
	}
	var token syscall.Token
	// INTERACTIVE (2), not NEW_CREDENTIALS: credentials must be checked now.
	r, _, callErr := logonUser.Call(ptr(user), ptr(domain), ptr(password), 2, 0, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return 0, callErr
	}
	defer token.Close()
	// LOGON_WITH_PROFILE. With a nil Desktop Windows grants this authenticated
	// identity access to the inherited bootstrap desktop. The supervisor then
	// switches to target-owned private objects before executing user commands.
	r, _, callErr = createWithToken.Call(uintptr(token), 1, ptr(app), ptr(line), flags, 0, 0, uintptr(unsafe.Pointer(si)), uintptr(unsafe.Pointer(pi)))
	return r, callErr
}
