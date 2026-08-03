//go:build windows

package agentd

import "os"

func validateObservabilityEnvironmentPermissions(_ string, _ os.FileInfo) error {
	return nil
}
