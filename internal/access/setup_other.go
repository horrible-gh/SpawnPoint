//go:build !windows && !linux

package access

import (
	"errors"
	"os"
)

func openSetupToken(path string, create bool) (*os.File, error) {
	return nil, errors.New("this platform requires SPAWNPOINT_SETUP_TOKEN for initial setup")
}
