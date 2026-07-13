//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package agentd

import (
	"errors"
	"os"
)

func tryWorkspaceFileLock(*os.File) (bool, error) {
	return false, errors.New("cross-process Workspace locking is unsupported on this operating system")
}

func unlockWorkspaceFile(*os.File) error { return nil }
