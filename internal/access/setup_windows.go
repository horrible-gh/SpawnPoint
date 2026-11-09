//go:build windows

package access

import (
	"golang.org/x/sys/windows"
	"os"
	"runtime"
	"unsafe"
)

// CREATE_NEW applies the protected DACL at creation, before the secret is
// written. Only the service identity, SYSTEM and local Administrators can read.
func openSetupToken(path string, create bool) (*os.File, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + u.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ | windows.WRITE_DAC)
	disposition := uint32(windows.OPEN_EXISTING)
	if create {
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
		disposition = windows.CREATE_NEW
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ, &sa, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	if !create {
		acl, _, e := sd.DACL()
		if e != nil {
			f.Close()
			return nil, e
		}
		if e = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); e != nil {
			f.Close()
			return nil, e
		}
	}
	return f, nil
}
