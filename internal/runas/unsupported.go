//go:build !windows && !linux

package runas

import (
	"errors"
	"os"
)

func Capability() (bool, string) {
	return false, "Account execution is supported on Windows and Linux."
}
func launch(*Spec, []byte) (*Child, error)          { return nil, errors.New("unsupported platform") }
func childInput() (*os.File, error)                 { return nil, errors.New("unsupported platform") }
func executeChild(Request) int                      { return 125 }
func seal(string, []byte, []byte) (string, error)   { return "", errors.New("unsupported platform") }
func unseal(string, string, []byte) ([]byte, error) { return nil, errors.New("unsupported platform") }
