package agentd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

type workerStorageScrubber interface {
	ScrubTenantStorage(context.Context, executions.WorkerStorageScrub) error
}

func (m *WorkspaceMaterializer) ScrubTenantStorage(
	ctx context.Context,
	scrub executions.WorkerStorageScrub,
) error {
	if scrub.ExecutionTargetID == uuid.Nil || scrub.TenantID == uuid.Nil ||
		scrub.ExecutionTargetID != m.targetID {
		return errors.New("Worker storage scrub identity does not match this Workspace materializer")
	}
	workspaceRoot, cacheRoot, err := validateWorkspaceRoots(m.root, m.cacheRoot)
	if err != nil {
		return err
	}
	m.root = workspaceRoot
	m.cacheRoot = cacheRoot
	target := scrub.ExecutionTargetID.String()
	tenant := scrub.TenantID.String()
	if err := scrubManagedRoot(ctx, workspaceRoot, []string{
		filepath.Join("v2", target, tenant),
		filepath.Join("v3", target, tenant),
		filepath.Join(tenant),
		filepath.Join(".quarantine"),
	}); err != nil {
		return fmt.Errorf("scrub Tenant Workspace storage: %w", err)
	}
	if err := scrubManagedRoot(ctx, cacheRoot, []string{
		filepath.Join("v1", target, tenant),
	}); err != nil {
		return fmt.Errorf("scrub Tenant Git cache storage: %w", err)
	}
	return nil
}

func scrubWorkerPrivateTempStorage(
	ctx context.Context,
	targetKind platform.ExecutionTargetKind,
	privateTempRoot string,
) error {
	if targetKind != platform.TargetDocker && targetKind != platform.TargetKubernetes {
		return fmt.Errorf("shared Worker storage scrub is unsupported for %s Targets without a Worker-private temporary filesystem", targetKind)
	}
	privateTempRoot = filepath.Clean(privateTempRoot)
	if privateTempRoot == "." || !filepath.IsAbs(privateTempRoot) || dangerousManagedRoot(privateTempRoot) {
		return errors.New("shared Worker storage scrub requires an explicit Worker-private temporary root")
	}
	entries, err := os.ReadDir(privateTempRoot)
	if err != nil {
		return fmt.Errorf("read Worker-private temporary root: %w", err)
	}
	relatives := make([]string, 0, len(entries))
	for _, entry := range entries {
		relatives = append(relatives, entry.Name())
	}
	return scrubManagedRoot(ctx, privateTempRoot, relatives)
}

func scrubManagedRoot(ctx context.Context, rootPath string, relatives []string) error {
	root, err := openVerifiedWorkspaceRoot(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	for _, relative := range relatives {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative = filepath.Clean(relative)
		if relative == "." || filepath.IsAbs(relative) || relative == ".." ||
			len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
			return errors.New("Worker storage scrub path is unsafe")
		}
		if _, err := root.Lstat(relative); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		segments, err := cleanRootRelativeSegments(relative)
		if err != nil || len(segments) == 0 {
			return errors.New("Worker storage scrub path is unsafe")
		}
		parentRelative := "."
		if len(segments) > 1 {
			parentRelative = filepath.Join(segments[:len(segments)-1]...)
		}
		parent, err := root.OpenRoot(parentRelative)
		if err != nil {
			return err
		}
		name := segments[len(segments)-1]
		if err := reclaimStorageTree(ctx, parent, name); err != nil {
			_ = parent.Close()
			return err
		}
		if err := removeRootRelativeTreeEntry(ctx, parent, name, syncWorkspaceCleanupDirectory, false); err != nil {
			_ = parent.Close()
			return err
		}
		if err := syncWorkspaceCleanupDirectory(parent, "."); err != nil {
			_ = parent.Close()
			return err
		}
		if err := parent.Close(); err != nil {
			return err
		}
		if _, err := root.Lstat(relative); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("Worker storage scrub residue remained after deletion")
			}
			return err
		}
	}
	return nil
}

func reclaimStorageTree(ctx context.Context, root *os.Root, name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return errors.New("Worker storage scrub entry is unsafe")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	uid, gid := os.Geteuid(), os.Getegid()
	if err := root.Lchown(name, uid, gid); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info.IsDir() {
		mode = 0o700
	}
	if err := root.Chmod(name, mode); err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	entries, err := readRootDirectoryEntries(directory)
	if err != nil {
		_ = directory.Close()
		return err
	}
	for _, entry := range entries {
		if err := reclaimStorageTree(ctx, directory, entry.Name()); err != nil {
			_ = directory.Close()
			return err
		}
	}
	return directory.Close()
}
