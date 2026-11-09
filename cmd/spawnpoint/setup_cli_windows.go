//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"strings"
)

func requireSetupAdmin() error {
	t := windows.GetCurrentProcessToken()
	if !t.IsElevated() {
		return errors.New("관리자 권한 PowerShell에서 .\\spawnpoint.exe admin init 또는 setup token을 실행하세요")
	}
	u, e := t.GetTokenUser()
	if e != nil {
		return e
	}
	if u.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return nil
	}
	sid, e := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if e != nil {
		return e
	}
	gs, e := t.GetTokenGroups()
	if e != nil {
		return e
	}
	for _, g := range gs.AllGroups() {
		if g.Sid.Equals(sid) && g.Attributes&windows.SE_GROUP_ENABLED != 0 && g.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 {
			return nil
		}
	}
	return errors.New("서버 로컬 관리자 권한이 필요합니다")
}
func openLocalSetupToken(path string) (*os.File, error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(h), path)
	var info windows.ByHandleFileInformation
	if e = windows.GetFileInformationByHandle(h, &info); e != nil {
		f.Close()
		return nil, e
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		f.Close()
		return nil, errors.New("토큰 파일 링크/디렉터리는 허용되지 않습니다")
	}
	return f, nil
}
func readSetupPassword(in *os.File, r *bufio.Reader, out io.Writer, prompt string) (string, error) {
	h := windows.Handle(in.Fd())
	var mode uint32
	if e := windows.GetConsoleMode(h, &mode); e != nil {
		return "", errors.New("대화형 터미널이 필요합니다. 자동화는 --password-stdin을 명시하세요")
	}
	if e := windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT); e != nil {
		return "", e
	}
	defer windows.SetConsoleMode(h, mode)
	fmt.Fprint(out, prompt)
	s, e := r.ReadString('\n')
	fmt.Fprintln(out)
	return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r"), e
}
