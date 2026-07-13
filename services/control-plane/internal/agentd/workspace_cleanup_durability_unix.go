//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package agentd

import (
	"errors"
	"os"
)

func requireWorkspaceCleanupDurability() error {
	return nil
}

func workspaceCleanupDurabilityUnavailable(_ error) bool {
	return false
}

func syncWorkspaceCleanupDirectory(root *os.Root, relative string) error {
	directory, err := openWorkspaceCleanupDirectoryForSync(root, relative)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
