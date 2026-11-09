//go:build !windows && !linux

package access

import (
	"context"
	"errors"
)

func verifyOSAdmin(context.Context, string, string) (string, error) {
	return "", errors.New("OS administrator authentication requires Windows or Linux")
}
