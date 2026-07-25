package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	protectedProviderTraverseDirectoryMode = 0o711
	protectedSupervisorOnlyDirectoryMode   = 0o700
)

func prepareProtectedProviderExecutionFilesystem(
	cfg Config,
	materialized WorkspaceMaterialization,
	providerStateDirectory string,
	runtimeOutputDirectory string,
) error {
	if cfg.CgroupV2ProviderIdentity == nil || strings.TrimSpace(cfg.CgroupV2Root) == "" {
		return nil
	}
	if strings.TrimSpace(cfg.WorkspaceRoot) == "" || strings.TrimSpace(materialized.LogicalRoot) == "" {
		return fmt.Errorf("protected cgroup Provider filesystem handoff requires a materialized Workspace root")
	}
	if err := tightenProtectedSupervisorOnlyDirectory(cfg.GitCacheRoot); err != nil {
		return fmt.Errorf("tighten protected Git cache root: %w", err)
	}
	if err := exposeProtectedProviderAncestorPath(cfg.WorkspaceRoot, materialized.LogicalRoot); err != nil {
		return fmt.Errorf("prepare protected Workspace ancestor path: %w", err)
	}
	if providerStateDirectory != "" && !pathContainedBy(materialized.LogicalRoot, providerStateDirectory) {
		return fmt.Errorf("protected Workspace provider state directory escaped the materialized root")
	}
	if err := handoffProtectedProviderTree(materialized.LogicalRoot, *cfg.CgroupV2ProviderIdentity); err != nil {
		return fmt.Errorf("handoff protected Workspace root: %w", err)
	}
	if runtimeOutputDirectory != "" {
		if err := handoffProtectedProviderTree(runtimeOutputDirectory, *cfg.CgroupV2ProviderIdentity); err != nil {
			return fmt.Errorf("handoff protected Runtime Output root: %w", err)
		}
	}
	return nil
}

func exposeProtectedProviderAncestorPath(workspaceRoot, logicalRoot string) error {
	ancestors, err := protectedProviderAncestorDirectories(workspaceRoot, logicalRoot)
	if err != nil {
		return err
	}
	for _, directory := range ancestors {
		if err := reownProtectedDirectory(directory, protectedFilesystemSupervisorIdentity(), protectedProviderTraverseDirectoryMode); err != nil {
			return err
		}
	}
	return nil
}

func tightenProtectedSupervisorOnlyDirectory(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return reownProtectedDirectory(path, protectedFilesystemSupervisorIdentity(), protectedSupervisorOnlyDirectoryMode)
}

func handoffProtectedProviderTree(path string, providerIdentity ProtectedCgroupIdentity) error {
	return filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		return os.Chown(current, int(providerIdentity.UID), int(providerIdentity.GID))
	})
}

func reownProtectedDirectory(path string, owner ProtectedCgroupIdentity, mode os.FileMode) error {
	if err := validateExistingRealDirectory(path); err != nil {
		return err
	}
	if err := os.Chown(path, int(owner.UID), int(owner.GID)); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func protectedProviderAncestorDirectories(workspaceRoot, logicalRoot string) ([]string, error) {
	workspaceRoot, err := filepath.Abs(strings.TrimSpace(workspaceRoot))
	if err != nil || workspaceRoot == "" {
		return nil, fmt.Errorf("Workspace root is invalid")
	}
	logicalRoot, err = filepath.Abs(strings.TrimSpace(logicalRoot))
	if err != nil || logicalRoot == "" {
		return nil, fmt.Errorf("Workspace root is invalid")
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	logicalRoot = filepath.Clean(logicalRoot)
	if !pathContainedBy(workspaceRoot, logicalRoot) || workspaceRoot == logicalRoot {
		return nil, fmt.Errorf("Workspace root escaped the configured Workspace parent")
	}
	relative, err := filepath.Rel(workspaceRoot, logicalRoot)
	if err != nil {
		return nil, err
	}
	segments := strings.Split(relative, string(filepath.Separator))
	current := workspaceRoot
	ancestors := []string{workspaceRoot}
	for index, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, fmt.Errorf("Workspace root contains an invalid segment")
		}
		current = filepath.Join(current, segment)
		if index < len(segments)-1 {
			ancestors = append(ancestors, current)
		}
	}
	return ancestors, nil
}

func protectedFilesystemSupervisorIdentity() ProtectedCgroupIdentity {
	return ProtectedCgroupIdentity{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
}
