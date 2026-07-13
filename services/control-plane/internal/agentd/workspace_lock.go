package agentd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const workspaceLockRetryInterval = 25 * time.Millisecond

type workspaceFileLock struct {
	file    *os.File
	release func() error
}

func acquireWorkspaceFileLock(ctx context.Context, root, lockPath string) (*workspaceFileLock, error) {
	root = filepath.Clean(root)
	lockPath = filepath.Clean(lockPath)
	if !pathContainedBy(root, lockPath) || lockPath == root {
		return nil, errors.New("lock path escapes its configured root")
	}
	if err := ensureContainedDirectory(root, filepath.Dir(lockPath)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(lockPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("lock path is a symlink or non-regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("lock file is unavailable")
	}
	for {
		locked, lockErr := tryWorkspaceFileLock(file)
		if lockErr != nil {
			_ = file.Close()
			return nil, lockErr
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(workspaceLockRetryInterval):
		}
	}
	pathInfo, err := os.Lstat(lockPath)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		_ = unlockWorkspaceFile(file)
		_ = file.Close()
		return nil, errors.New("lock file changed while it was acquired")
	}
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			releaseErr = errors.Join(unlockWorkspaceFile(file), file.Close())
		})
		return releaseErr
	}
	return &workspaceFileLock{file: file, release: release}, nil
}

func (l *workspaceFileLock) Release() error {
	if l == nil || l.release == nil {
		return nil
	}
	return l.release()
}

func lockPathFor(root, namespace string, segments ...string) (string, error) {
	if strings.TrimSpace(namespace) == "" {
		return "", errors.New("lock namespace is empty")
	}
	parts := []string{root, ".locks", namespace}
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, `/\\`) {
			return "", errors.New("lock path segment is invalid")
		}
		parts = append(parts, segment)
	}
	if len(parts) == 3 {
		return "", errors.New("lock path has no identity segments")
	}
	parts[len(parts)-1] += ".lock"
	return filepath.Join(parts...), nil
}
