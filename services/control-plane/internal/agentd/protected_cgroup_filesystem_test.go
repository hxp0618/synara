package agentd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedProviderAncestorDirectoriesIncludeTraversePathOnly(t *testing.T) {
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	logicalRoot := filepath.Join(workspaceRoot, "v2", "target", "tenant", "project", "session", "workspace")

	ancestors, err := protectedProviderAncestorDirectories(workspaceRoot, logicalRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		workspaceRoot,
		filepath.Join(workspaceRoot, "v2"),
		filepath.Join(workspaceRoot, "v2", "target"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant", "project"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant", "project", "session"),
	}
	if len(ancestors) != len(want) {
		t.Fatalf("protectedProviderAncestorDirectories() returned %v, want %v", ancestors, want)
	}
	for index, path := range want {
		if ancestors[index] != path {
			t.Fatalf("ancestor[%d] = %q, want %q", index, ancestors[index], path)
		}
	}
}

func TestPrepareProtectedProviderExecutionFilesystemHandsOffWorkspaceButNotGitCache(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspaces")
	gitCacheRoot := filepath.Join(root, "git-cache")
	logicalRoot := filepath.Join(workspaceRoot, "v2", "target", "tenant", "project", "session", "workspace")
	checkout := filepath.Join(logicalRoot, "checkout")
	gitDir := filepath.Join(logicalRoot, "repo.git")
	providerState := filepath.Join(logicalRoot, workspaceProviderStateDir)
	runtimeOutput := filepath.Join(root, "runtime-output")
	for _, directory := range []string{
		workspaceRoot,
		gitCacheRoot,
		filepath.Join(workspaceRoot, "v2"),
		filepath.Join(workspaceRoot, "v2", "target"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant", "project"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant", "project", "session"),
		logicalRoot,
		checkout,
		gitDir,
		providerState,
		runtimeOutput,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manifestPath := filepath.Join(logicalRoot, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"format":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		WorkspaceRoot: workspaceRoot,
		GitCacheRoot:  gitCacheRoot,
		CgroupV2Root:  "/sys/fs/cgroup/system.slice/synara-agentd.service",
		CgroupV2ProviderIdentity: &ProtectedCgroupIdentity{
			UID: uint32(os.Getuid()),
			GID: uint32(os.Getgid()),
		},
	}
	materialized := WorkspaceMaterialization{
		Directory:   checkout,
		LogicalRoot: logicalRoot,
		GitDir:      gitDir,
	}
	if err := prepareProtectedProviderExecutionFilesystem(cfg, materialized, providerState, runtimeOutput); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		workspaceRoot,
		filepath.Join(workspaceRoot, "v2"),
		filepath.Join(workspaceRoot, "v2", "target"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant", "project"),
		filepath.Join(workspaceRoot, "v2", "target", "tenant", "project", "session"),
	} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != protectedProviderTraverseDirectoryMode {
			t.Fatalf("%s mode = %#o, want %#o", directory, info.Mode().Perm(), protectedProviderTraverseDirectoryMode)
		}
	}
	for _, directory := range []string{logicalRoot, checkout, gitDir, providerState, runtimeOutput} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %#o, want %#o", directory, info.Mode().Perm(), 0o700)
		}
	}
	gitCacheInfo, err := os.Stat(gitCacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if gitCacheInfo.Mode().Perm() != protectedSupervisorOnlyDirectoryMode {
		t.Fatalf("git cache mode = %#o, want %#o", gitCacheInfo.Mode().Perm(), protectedSupervisorOnlyDirectoryMode)
	}
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifestInfo.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %#o, want %#o", manifestInfo.Mode().Perm(), 0o600)
	}
}

func TestPrepareProtectedProviderExecutionFilesystemRejectsProviderStateEscape(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspaces")
	logicalRoot := filepath.Join(workspaceRoot, "v2", "target", "tenant", "project", "session", "workspace")
	if err := os.MkdirAll(filepath.Join(logicalRoot, "checkout"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		WorkspaceRoot: workspaceRoot,
		GitCacheRoot:  filepath.Join(root, "git-cache"),
		CgroupV2Root:  "/sys/fs/cgroup/system.slice/synara-agentd.service",
		CgroupV2ProviderIdentity: &ProtectedCgroupIdentity{
			UID: uint32(os.Getuid()),
			GID: uint32(os.Getgid()),
		},
	}
	if err := os.MkdirAll(cfg.GitCacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	err := prepareProtectedProviderExecutionFilesystem(
		cfg,
		WorkspaceMaterialization{LogicalRoot: logicalRoot, Directory: filepath.Join(logicalRoot, "checkout")},
		filepath.Join(root, "outside-provider-state"),
		"",
	)
	if err == nil {
		t.Fatal("provider-state escape was accepted")
	}
}
