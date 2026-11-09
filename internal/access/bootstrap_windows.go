//go:build windows

package access

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
)

func adminToken(t windows.Token) bool {
	sid, e := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if e != nil {
		return false
	}
	gs, e := t.GetTokenGroups()
	if e != nil {
		return false
	}
	for _, g := range gs.AllGroups() {
		if g.Sid.Equals(sid) && g.Attributes&windows.SE_GROUP_ENABLED != 0 && g.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 {
			return true
		}
	}
	return false
}
func verifyOSAdmin(ctx context.Context, name, password string) (string, error) {
	token, e := authenticateWindows(ctx, name, password, 2)
	if e != nil {
		return "", e
	}
	defer token.Close()
	t := windows.Token(token)
	allowed := adminToken(t)
	// UAC can produce a limited token. Check the linked token supplied by Windows,
	// not group-name strings or client headers; do not impersonate/elevate/run it.
	if !allowed {
		if linked, e := t.GetLinkedToken(); e == nil {
			allowed = adminToken(linked)
			linked.Close()
		}
	}
	if !allowed {
		return "", errors.New("server Administrators membership required")
	}
	u, e := token.GetTokenUser()
	if e != nil {
		return "", e
	}
	sid, e := u.User.Sid.String()
	if e != nil {
		return "", e
	}
	return "windows:sid:" + sid, nil
}
