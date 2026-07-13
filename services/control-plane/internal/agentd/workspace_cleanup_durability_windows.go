//go:build windows

package agentd

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func requireWorkspaceCleanupDurability() error {
	return nil
}

func workspaceCleanupDurabilityUnavailable(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_INVALID_HANDLE) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}

func syncWorkspaceCleanupDirectory(root *os.Root, relative string) error {
	verified, err := openWorkspaceCleanupDirectoryForSync(root, relative)
	if err != nil {
		return err
	}
	defer verified.Close()
	verifiedInfo, err := verified.Stat()
	if err != nil {
		return err
	}

	absolute := filepath.Join(root.Name(), relative)
	path, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	writable := os.NewFile(uintptr(handle), absolute)
	if writable == nil {
		_ = windows.CloseHandle(handle)
		return errors.New("open writable cleanup durability directory")
	}
	writableInfo, statErr := writable.Stat()
	if statErr != nil || !writableInfo.IsDir() || !os.SameFile(verifiedInfo, writableInfo) {
		_ = writable.Close()
		return errors.New("writable cleanup durability handle does not match the verified directory")
	}
	flushErr := windows.FlushFileBuffers(handle)
	closeErr := writable.Close()
	return errors.Join(flushErr, closeErr)
}
