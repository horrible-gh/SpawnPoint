//go:build windows

package access

import (
	"context"
	"errors"
	"os/user"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var authLogonUser = syscall.NewLazyDLL("advapi32.dll").NewProc("LogonUserW")

func osAvailable() (bool, string) {
	if e := authLogonUser.Find(); e != nil {
		return false, "Windows 인증 API를 사용할 수 없습니다"
	}
	return true, ""
}
func resolveOS(name string) (string, error) {
	if name == "" || len(name) > 256 || strings.ContainsAny(name, "\r\n\x00") {
		return "", errors.New("invalid OS username")
	}
	u, e := user.Lookup(name)
	if e != nil {
		return "", e
	}
	return "windows:sid:" + u.Uid, nil
}
func verifyOS(ctx context.Context, name, password string) (string, error) {
	if e := ctx.Err(); e != nil {
		return "", e
	}
	if name == "" || password == "" || len(password) > 1024 || strings.ContainsAny(name+password, "\x00") {
		return "", errors.New("invalid credential")
	}
	domain := "."
	if n := strings.Index(name, "\\"); n >= 0 {
		domain, name = name[:n], name[n+1:]
	} else if strings.Contains(name, "@") {
		domain = ""
	}
	u, e := syscall.UTF16FromString(name)
	if e != nil {
		return "", e
	}
	p, e := syscall.UTF16FromString(password)
	if e != nil {
		return "", e
	}
	defer func() {
		for i := range p {
			p[i] = 0
		}
	}()
	var d *uint16
	if domain != "" {
		d, e = syscall.UTF16PtrFromString(domain)
		if e != nil {
			return "", e
		}
	}
	var token syscall.Token
	// NETWORK validates now; NEW_CREDENTIALS must never be used for authentication.
	ok, _, _ := authLogonUser.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(&p[0])), 3, 0, uintptr(unsafe.Pointer(&token)))
	runtime.KeepAlive(u)
	runtime.KeepAlive(d)
	runtime.KeepAlive(p)
	if ok == 0 {
		return "", errors.New("OS authentication failed")
	}
	defer token.Close()
	tu, e := token.GetTokenUser()
	if e != nil {
		return "", e
	}
	sid, e := tu.User.Sid.String()
	if e != nil {
		return "", e
	}
	return "windows:sid:" + sid, nil
}
