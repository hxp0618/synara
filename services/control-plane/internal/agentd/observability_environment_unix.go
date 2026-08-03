//go:build !windows

package agentd

import (
	"errors"
	"os"
	"path/filepath"
)

func validateObservabilityEnvironmentPermissions(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("agentd observability environment must not be group- or world-writable")
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0o022 != 0 {
		return errors.New("agentd observability environment directory must not be group- or world-writable")
	}
	return nil
}
