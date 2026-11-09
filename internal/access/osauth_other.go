//go:build !linux && !windows

package access

import (
	"context"
	"errors"
)

func osAvailable() (bool, string) {
	return false, "OS 계정 인증은 Windows/Linux에서만 지원합니다"
}
func resolveOS(string) (string, error) { return "", errors.New("unsupported OS") }
func verifyOS(context.Context, string, string) (string, error) {
	return "", errors.New("unsupported OS")
}
