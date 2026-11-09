//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"strings"
)

func requireSetupAdmin() error {
	if os.Geteuid() != 0 {
		return errors.New("서버 로컬에서 sudo ./spawnpoint admin init (토큰 확인: sudo ./spawnpoint setup token)을 실행하세요")
	}
	return nil
}
func openLocalSetupToken(path string) (*os.File, error) {
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("초기 설정 토큰은 접근이 제한된 일반 파일이어야 합니다")
	}
	return f, nil
}
func readSetupPassword(in *os.File, r *bufio.Reader, out io.Writer, prompt string) (string, error) {
	term, e := unix.IoctlGetTermios(int(in.Fd()), unix.TCGETS)
	if e != nil {
		return "", errors.New("대화형 터미널이 필요합니다. 자동화는 --password-stdin을 명시하세요")
	}
	old := *term
	term.Lflag &^= unix.ECHO
	if e = unix.IoctlSetTermios(int(in.Fd()), unix.TCSETS, term); e != nil {
		return "", e
	}
	defer unix.IoctlSetTermios(int(in.Fd()), unix.TCSETS, &old)
	fmt.Fprint(out, prompt)
	s, e := r.ReadString('\n')
	fmt.Fprintln(out)
	return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r"), e
}
