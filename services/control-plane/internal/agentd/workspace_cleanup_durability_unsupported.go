//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package agentd

import (
	"errors"
	"os"
)

var errUnsupportedWorkspaceCleanupDurability = errors.New(
	"durable Workspace cleanup is unsupported on this operating system",
)

func requireWorkspaceCleanupDurability() error {
	return errUnsupportedWorkspaceCleanupDurability
}

func workspaceCleanupDurabilityUnavailable(_ error) bool {
	return true
}

func syncWorkspaceCleanupDirectory(*os.Root, string) error {
	return errUnsupportedWorkspaceCleanupDurability
}
