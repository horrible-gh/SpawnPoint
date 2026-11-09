//go:build windows

package runas

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var user32 = syscall.NewLazyDLL("user32.dll")
var getProcessWindowStation = user32.NewProc("GetProcessWindowStation")
var getUserObjectInformation = user32.NewProc("GetUserObjectInformationW")
var createDesktop = user32.NewProc("CreateDesktopW")
var closeDesktop = user32.NewProc("CloseDesktop")
var convertSDDL = advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
var getSecurityInfo = advapi.NewProc("GetSecurityInfo")
var setSecurityInfo = advapi.NewProc("SetSecurityInfo")
var setEntriesInACL = advapi.NewProc("SetEntriesInAclW")
var desktopACLMutex sync.Mutex

type aclTrustee struct {
	Multiple  *aclTrustee
	Operation uint32
	Form      uint32
	Kind      uint32
	SID       *syscall.SID
}
type explicitAccess struct {
	Permissions uint32
	Mode        uint32
	Inheritance uint32
	Trustee     aclTrustee
}
type tokenLogonGroup struct {
	Count uint32
	Group syscall.SIDAndAttributes
}

func logonSID(token syscall.Token) (*syscall.SID, error) {
	var size uint32
	err := syscall.GetTokenInformation(token, syscall.TokenLogonSid, nil, 0, &size)
	if err != syscall.ERROR_INSUFFICIENT_BUFFER || size < uint32(unsafe.Sizeof(tokenLogonGroup{})) || size > 65536 {
		return nil, errors.New("cannot query authenticated logon session")
	}
	data := make([]byte, size)
	if err = syscall.GetTokenInformation(token, syscall.TokenLogonSid, &data[0], size, &size); err != nil {
		return nil, err
	}
	group := (*tokenLogonGroup)(unsafe.Pointer(&data[0]))
	if group.Count != 1 || group.Group.Sid == nil || group.Group.Attributes&0xc0000000 != 0xc0000000 {
		return nil, errors.New("authenticated token has no unique logon SID")
	}
	text, err := group.Group.Sid.String()
	runtime.KeepAlive(data)
	if err != nil {
		return nil, err
	}
	return syscall.StringToSid(text)
}

// Modify only our fresh logon SID's ACE, never restore a stale whole DACL.
// Concurrent launches keep each other's grants. No account-wide, Everyone, or
// Authenticated Users grant is installed. WRITE_DAC/WRITE_OWNER are NOT granted.
func stationAccess(station uintptr, sid *syscall.SID, grant bool) error {
	desktopACLMutex.Lock()
	defer desktopACLMutex.Unlock()
	var acl, descriptor uintptr
	code, _, _ := getSecurityInfo.Call(station, 7, 4, 0, 0, uintptr(unsafe.Pointer(&acl)), 0, uintptr(unsafe.Pointer(&descriptor)))
	if code != 0 {
		return syscall.Errno(code)
	}
	defer syscall.LocalFree(syscall.Handle(descriptor))
	// A NULL DACL is already unrestricted; do not accidentally replace it.
	if acl == 0 {
		return nil
	}
	mode := uint32(4) // REVOKE_ACCESS
	if grant {
		mode = 1
	} // GRANT_ACCESS
	access := explicitAccess{Permissions: 0x0002037f, Mode: mode, Trustee: aclTrustee{Kind: 2, SID: sid}}
	var updated uintptr
	code, _, _ = setEntriesInACL.Call(1, uintptr(unsafe.Pointer(&access)), acl, uintptr(unsafe.Pointer(&updated)))
	runtime.KeepAlive(sid)
	if code != 0 {
		return syscall.Errno(code)
	}
	defer syscall.LocalFree(syscall.Handle(updated))
	code, _, _ = setSecurityInfo.Call(station, 7, 4, 0, 0, updated, 0)
	if code != 0 {
		return syscall.Errno(code)
	}
	return nil
}

// Prepare objects in the caller BEFORE loading USER32 in the authenticated
// child. A Go child can start on an inaccessible service desktop, but loading
// USER32 there fails before it can create a replacement desktop itself.
// The existing server desktop is never opened to the target account. A private
// desktop grants only SYSTEM, the caller and this specific authenticated logon.
func prepareLaunchDesktop(token syscall.Token) (name []uint16, cleanup func(), err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := user32.Load(); err != nil {
		return nil, nil, err
	}
	sid, err := logonSID(token)
	if err != nil {
		return nil, nil, err
	}
	target, err := sid.String()
	if err != nil {
		return nil, nil, err
	}
	own, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return nil, nil, err
	}
	defer own.Close()
	u, err := own.GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	owner, err := u.User.Sid.String()
	if err != nil {
		return nil, nil, err
	}
	station, _, callErr := getProcessWindowStation.Call()
	if station == 0 {
		return nil, nil, fmt.Errorf("query window station: %w", callErr)
	}
	var size uint32
	getUserObjectInformation.Call(station, 2, 0, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 || size > 65536 {
		return nil, nil, errors.New("invalid window station name")
	}
	stationName := make([]uint16, (size+1)/2)
	r, _, callErr := getUserObjectInformation.Call(station, 2, ptr(stationName), uintptr(size), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return nil, nil, callErr
	}
	if err = stationAccess(station, sid, true); err != nil {
		return nil, nil, fmt.Errorf("grant logon-session station access: %w", err)
	}
	var desktop uintptr
	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			if desktop != 0 {
				closeDesktop.Call(desktop)
			}
			if err := stationAccess(station, sid, false); err != nil {
				// Contains only an OS error, never credentials. The SID belongs solely to
				// the terminating logon session, not to an account's other sessions.
				fmt.Printf("SpawnPoint: logon-session desktop cleanup failed: %v\n", err)
			}
		})
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	sddl, _ := wide(desktopSDDL(target, owner))
	var descriptor uintptr
	r, _, callErr = convertSDDL.Call(ptr(sddl), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	runtime.KeepAlive(sddl)
	if r == 0 {
		return nil, cleanup, callErr
	}
	defer syscall.LocalFree(syscall.Handle(descriptor))
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return nil, cleanup, err
	}
	shortName := "SpawnPoint-" + hex.EncodeToString(nonce)
	desktopName, _ := wide(shortName)
	desktop, _, callErr = createDesktop.Call(ptr(desktopName), 0, 0, 0, 0x000f01ff, uintptr(unsafe.Pointer(&sa)))
	runtime.KeepAlive(desktopName)
	runtime.KeepAlive(sa)
	if desktop == 0 {
		return nil, cleanup, callErr
	}
	name, err = wide(syscall.UTF16ToString(stationName) + "\\" + shortName)
	return name, cleanup, err
}

func desktopSDDL(logon, owner string) string {
	return "D:P(A;;GA;;;SY)(A;;GA;;;" + owner + ")(A;;GA;;;" + logon + ")"
}
