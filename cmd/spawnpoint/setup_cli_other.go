//go:build !windows && !linux

package main

import (
	"bufio"
	"errors"
	"io"
	"os"
)

func requireSetupAdmin() error                     { return errors.New("Windows/Linux 전용 기능입니다") }
func openLocalSetupToken(string) (*os.File, error) { return nil, requireSetupAdmin() }
func readSetupPassword(*os.File, *bufio.Reader, io.Writer, string) (string, error) {
	return "", requireSetupAdmin()
}
