//go:build windows

package access

import (
	"context"
	"errors"
	"os"
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
	// Account linking needs only the stable SID. os/user.Lookup also queries
	// profile and primary-group information, which may be unavailable to a
	// LocalService process even when the account itself exists.
	// Match authenticateWindows: an unqualified name or .\name is local to
	// this server, not a same-named account in a trusted domain.
	lookup := name
	if strings.HasPrefix(name, `.\`) || (!strings.Contains(name, `\`) && !strings.Contains(name, "@")) {
		host, e := os.Hostname()
		if e != nil || host == "" {
			return "", errors.New("cannot determine local Windows account domain")
		}
		if strings.HasPrefix(name, `.\`) {
			lookup = host + name[1:]
		} else {
			lookup = host + `\` + name
		}
	}
	sid, _, kind, e := syscall.LookupSID("", lookup)
	if e != nil {
		return "", e
	}
	if kind != syscall.SidTypeUser {
		return "", errors.New("OS account is not a user")
	}
	value, e := sid.String()
	if e != nil {
		return "", e
	}
	return "windows:sid:" + value, nil
}
func authenticateWindows(ctx context.Context, name, password string, logonType uint32) (syscall.Token, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	if name == "" || password == "" || len(password) > 1024 || strings.ContainsAny(name+password, "\x00") {
		return 0, errors.New("invalid credential")
	}
	domain := "."
	if n := strings.Index(name, "\\"); n >= 0 {
		domain, name = name[:n], name[n+1:]
	} else if strings.Contains(name, "@") {
		domain = ""
	}
	u, e := syscall.UTF16FromString(name)
	if e != nil {
		return 0, e
	}
	p, e := syscall.UTF16FromString(password)
	if e != nil {
		return 0, e
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
			return 0, e
		}
	}
	var token syscall.Token
	// NETWORK validates now; NEW_CREDENTIALS must never be used for authentication.
	ok, _, _ := authLogonUser.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(&p[0])), uintptr(logonType), 0, uintptr(unsafe.Pointer(&token)))
	runtime.KeepAlive(u)
	runtime.KeepAlive(d)
	runtime.KeepAlive(p)
	if ok == 0 {
		return 0, errors.New("OS authentication failed")
	}
	return token, nil
}

func verifyOS(ctx context.Context, name, password string) (string, error) {
	token, e := authenticateWindows(ctx, name, password, 3)
	if e != nil {
		return "", e
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