package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestReadWorkspaceManifestRejectsSymlinkAndNonRegularFile(t *testing.T) {
	manifestRoot := t.TempDir()
	if err := writeWorkspaceManifest(manifestRoot, workspaceGenerationManifest{Format: workspaceLayoutVersion}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(manifestRoot, "manifest.json")

	t.Run("symlink", func(t *testing.T) {
		linkPath := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.Symlink(manifestPath, linkPath); err != nil {
			t.Fatal(err)
		}
		if _, err := readWorkspaceManifest(linkPath); err == nil {
			t.Fatal("Workspace manifest reader followed a symlink")
		}
	})

	t.Run("directory", func(t *testing.T) {
		directoryPath := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.Mkdir(directoryPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := readWorkspaceManifest(directoryPath); err == nil {
			t.Fatal("Workspace manifest reader accepted a non-regular file")
		}
	})
}

func TestReplaceWorkspaceGenerationTreatsBackupCleanupAsPostInstallCleanup(t *testing.T) {
	parent := t.TempDir()
	active := filepath.Join(parent, "active")
	staging := filepath.Join(parent, "staging")
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(active, "old.txt"), "old generation\n")
	writeWorkspaceTestFile(t, filepath.Join(staging, "new.txt"), "new generation\n")
	cleanupErr := errors.New("injected backup cleanup failure")
	err := replaceWorkspaceGenerationWithFS(active, staging, workspaceGenerationFS{
		rename: os.Rename,
		removeAll: func(string) error {
			return cleanupErr
		},
	})
	if err != nil {
		t.Fatalf("installed generation was reported as failed after backup cleanup error: %v", err)
	}
	assertWorkspaceTestFile(t, filepath.Join(active, "new.txt"), "new generation\n")
	if _, err := os.Lstat(filepath.Join(active, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("installed generation retained old content: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(parent, ".active.backup-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("failed backup cleanup was not left for later cleanup: backups=%v err=%v", backups, err)
	}
}

func TestReplaceWorkspaceGenerationRollsBackFailedInstall(t *testing.T) {
	parent := t.TempDir()
	active := filepath.Join(parent, "active")
	staging := filepath.Join(parent, "staging")
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(active, "old.txt"), "old generation\n")
	writeWorkspaceTestFile(t, filepath.Join(staging, "new.txt"), "new generation\n")
	installErr := errors.New("injected staging install failure")
	renameCalls := 0
	err := replaceWorkspaceGenerationWithFS(active, staging, workspaceGenerationFS{
		rename: func(from, to string) error {
			renameCalls++
			if renameCalls == 2 {
				return installErr
			}
			return os.Rename(from, to)
		},
		removeAll: os.RemoveAll,
	})
	if !errors.Is(err, installErr) {
		t.Fatalf("failed generation install returned %v", err)
	}
	assertWorkspaceTestFile(t, filepath.Join(active, "old.txt"), "old generation\n")
	assertWorkspaceTestFile(t, filepath.Join(staging, "new.txt"), "new generation\n")
	backups, globErr := filepath.Glob(filepath.Join(parent, ".active.backup-*"))
	if globErr != nil || len(backups) != 0 {
		t.Fatalf("successful rollback left backup state: backups=%v err=%v", backups, globErr)
	}
}

func TestReplaceWorkspaceGenerationReportsRollbackFailure(t *testing.T) {
	parent := t.TempDir()
	active := filepath.Join(parent, "active")
	staging := filepath.Join(parent, "staging")
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	installErr := errors.New("injected staging install failure")
	rollbackErr := errors.New("injected rollback failure")
	renameCalls := 0
	err := replaceWorkspaceGenerationWithFS(active, staging, workspaceGenerationFS{
		rename: func(from, to string) error {
			renameCalls++
			switch renameCalls {
			case 2:
				return installErr
			case 3:
				return rollbackErr
			default:
				return os.Rename(from, to)
			}
		},
		removeAll: os.RemoveAll,
	})
	if !errors.Is(err, installErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("rollback failure did not preserve both causes: %v", err)
	}
	if _, err := os.Lstat(active); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catastrophic rollback failure unexpectedly created active generation: %v", err)
	}
	backups, globErr := filepath.Glob(filepath.Join(parent, ".active.backup-*"))
	if globErr != nil || len(backups) != 1 {
		t.Fatalf("rollback failure did not preserve the previous generation backup: backups=%v err=%v", backups, globErr)
	}
}

func TestReconcileWorkspaceGenerationRestoresSingleBackupAndRemovesStaging(t *testing.T) {
	parent := t.TempDir()
	active := filepath.Join(parent, "active")
	backup := filepath.Join(parent, ".active.backup-"+uuid.NewString())
	staging := filepath.Join(parent, ".active.staging-"+uuid.NewString())
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(backup, "preserved.txt"), "previous generation\n")
	writeWorkspaceTestFile(t, filepath.Join(staging, "partial.txt"), "partial replacement\n")
	if err := reconcileWorkspaceGeneration(active, func(root string) error {
		value, err := os.ReadFile(filepath.Join(root, "preserved.txt"))
		if err != nil || string(value) != "previous generation\n" {
			return errors.New("generation marker is invalid")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceTestFile(t, filepath.Join(active, "preserved.txt"), "previous generation\n")
	for _, path := range []string{backup, staging} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Workspace recovery residue survived at %s: %v", path, err)
		}
	}
}

func TestReconcileWorkspaceGenerationKeepsValidActiveAndCleansResidue(t *testing.T) {
	parent := t.TempDir()
	active := filepath.Join(parent, "active")
	backup := filepath.Join(parent, ".active.backup-"+uuid.NewString())
	staging := filepath.Join(parent, ".active.staging-"+uuid.NewString())
	for _, path := range []string{active, backup, staging} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeWorkspaceTestFile(t, filepath.Join(active, "authoritative.txt"), "active generation\n")
	writeWorkspaceTestFile(t, filepath.Join(backup, "authoritative.txt"), "stale backup\n")
	if err := reconcileWorkspaceGeneration(active, func(root string) error {
		value, err := os.ReadFile(filepath.Join(root, "authoritative.txt"))
		if err != nil || string(value) != "active generation\n" {
			return errors.New("active generation is invalid")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceTestFile(t, filepath.Join(active, "authoritative.txt"), "active generation\n")
	for _, path := range []string{backup, staging} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale Workspace residue survived at %s: %v", path, err)
		}
	}
}

func TestReconcileWorkspaceGenerationRejectsAmbiguousBackups(t *testing.T) {
	parent := t.TempDir()
	active := filepath.Join(parent, "active")
	backups := []string{
		filepath.Join(parent, ".active.backup-"+uuid.NewString()),
		filepath.Join(parent, ".active.backup-"+uuid.NewString()),
	}
	for _, backup := range backups {
		if err := os.Mkdir(backup, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := reconcileWorkspaceGeneration(active, func(string) error { return nil }); err == nil {
		t.Fatal("Workspace reconciliation selected one of multiple ambiguous backups")
	}
	for _, backup := range backups {
		if _, err := os.Lstat(backup); err != nil {
			t.Fatalf("ambiguous Workspace backup was not preserved: %v", err)
		}
	}
}

func TestReconcileWorkspaceGenerationPreservesInvalidActiveAndResidue(t *testing.T) {
	parent := t.TempDir()
	active := filepath.Join(parent, "active")
	backup := filepath.Join(parent, ".active.backup-"+uuid.NewString())
	staging := filepath.Join(parent, ".active.staging-"+uuid.NewString())
	for _, path := range []string{active, backup, staging} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeWorkspaceTestFile(t, filepath.Join(active, "invalid.txt"), "uncheckpointed state\n")
	writeWorkspaceTestFile(t, filepath.Join(backup, "valid.txt"), "previous state\n")
	if err := reconcileWorkspaceGeneration(active, func(root string) error {
		if _, err := os.Stat(filepath.Join(root, "valid.txt")); err != nil {
			return errors.New("generation is invalid")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceTestFile(t, filepath.Join(active, "invalid.txt"), "uncheckpointed state\n")
	for _, path := range []string{backup, staging} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("invalid active reconciliation discarded recovery evidence at %s: %v", path, err)
		}
	}
}

func TestWorkspaceMaterializerRecoversInterruptedNonGitGenerationInstall(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	workload.RepositoryURL = nil
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, targetID)
	first, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(first.Directory, "preserved.txt"), "durable workspace\n")
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(
		filepath.Dir(first.LogicalRoot), "."+filepath.Base(first.LogicalRoot)+".backup-"+uuid.NewString(),
	)
	if err := os.Rename(first.LogicalRoot, backup); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(
		filepath.Dir(first.LogicalRoot), "."+filepath.Base(first.LogicalRoot)+".staging-"+uuid.NewString(),
	)
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(staging, "partial.txt"), "partial install\n")

	recovered, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Release()
	assertWorkspaceTestFile(t, filepath.Join(recovered.Directory, "preserved.txt"), "durable workspace\n")
	for _, path := range []string{backup, staging} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("interrupted Workspace install residue survived at %s: %v", path, err)
		}
	}
}

func TestPrivateWorkspaceRejectsGitFileOutsideItsGeneration(t *testing.T) {
	materializer, _, materialized := materializeV2GitWorkspace(t)
	gitFile := filepath.Join(materialized.Directory, ".git")
	original, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatal(err)
	}
	layout := workspaceLayout{
		Root: materialized.LogicalRoot, Checkout: materialized.Directory, GitDir: materialized.GitDir,
		Manifest: filepath.Join(materialized.LogicalRoot, "manifest.json"),
	}
	siblingMetadata := filepath.Join(
		filepath.Dir(materialized.LogicalRoot), "sibling-generation", "repo.git", "worktrees", "checkout",
	)
	externalMetadata := filepath.Join(t.TempDir(), "external.git", "worktrees", "checkout")
	cacheRelative, err := filepath.Rel(materialized.Directory, materialized.cache.RepoGit)
	if err != nil {
		t.Fatal(err)
	}
	siblingRelative, err := filepath.Rel(materialized.Directory, siblingMetadata)
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name   string
		gitDir string
	}{
		{name: "cache", gitDir: cacheRelative},
		{name: "sibling-generation", gitDir: siblingRelative},
		{name: "external-absolute", gitDir: externalMetadata},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := os.WriteFile(gitFile, []byte("gitdir: "+testCase.gitDir+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.WriteFile(gitFile, original, 0o600); err != nil {
					t.Errorf("restore original Git file: %v", err)
				}
			}()
			if err := validatePrivateWorktreeFilesystem(layout, materialized.manifest); err == nil {
				t.Fatalf("Workspace accepted .git pointer %q", testCase.gitDir)
			}
		})
	}

	if err := materializer.validatePrivateGitGeneration(context.Background(), layout, materialized.manifest); err != nil {
		t.Fatalf("test did not restore the valid private generation: %v", err)
	}
}

func TestPrivateWorkspaceRejectsCommonDirAndGitDirAliases(t *testing.T) {
	materializer, _, materialized := materializeV2GitWorkspace(t)
	gitFile := filepath.Join(materialized.Directory, ".git")
	gitFileValue, err := readSmallRegularFile(gitFile, 4096)
	if err != nil {
		t.Fatal(err)
	}
	worktreeGitDir, err := resolveRelativeMetadataPath(
		materialized.Directory,
		strings.TrimSpace(strings.TrimPrefix(gitFileValue, "gitdir: ")),
	)
	if err != nil {
		t.Fatal(err)
	}
	layout := workspaceLayout{
		Root: materialized.LogicalRoot, Checkout: materialized.Directory, GitDir: materialized.GitDir,
		Manifest: filepath.Join(materialized.LogicalRoot, "manifest.json"),
	}

	t.Run("commondir symlink alias", func(t *testing.T) {
		commonPath := filepath.Join(worktreeGitDir, "commondir")
		original, err := os.ReadFile(commonPath)
		if err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(materialized.LogicalRoot, "repo-alias")
		if err := os.Symlink(materialized.GitDir, alias); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(alias)
		relative, err := filepath.Rel(worktreeGitDir, alias)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(commonPath, []byte(relative+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.WriteFile(commonPath, original, 0o600); err != nil {
				t.Errorf("restore commondir: %v", err)
			}
		}()
		if err := validatePrivateWorktreeFilesystem(layout, materialized.manifest); err == nil {
			t.Fatal("Workspace accepted a symlink alias for its private common Git directory")
		}
	})

	t.Run("gitdir symlink alias", func(t *testing.T) {
		checkoutPointerPath := filepath.Join(worktreeGitDir, "gitdir")
		original, err := os.ReadFile(checkoutPointerPath)
		if err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(materialized.LogicalRoot, "checkout-git-alias")
		if err := os.Symlink(gitFile, alias); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(alias)
		relative, err := filepath.Rel(worktreeGitDir, alias)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(checkoutPointerPath, []byte(relative+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.WriteFile(checkoutPointerPath, original, 0o600); err != nil {
				t.Errorf("restore gitdir pointer: %v", err)
			}
		}()
		if err := validatePrivateWorktreeFilesystem(layout, materialized.manifest); err == nil {
			t.Fatal("Workspace accepted a symlink alias for its checkout Git pointer")
		}
	})

	if err := materializer.validatePrivateGitGeneration(context.Background(), layout, materialized.manifest); err != nil {
		t.Fatalf("test did not restore the valid private generation: %v", err)
	}
}

func TestWorkspaceInspectionRejectsSymlinkAncestorBelowConfiguredRoot(t *testing.T) {
	materializer, _, materialized := materializeV2GitWorkspace(t)
	sessionRoot := filepath.Dir(materialized.LogicalRoot)
	relocatedRoot := sessionRoot + "-relocated"
	if err := os.Rename(sessionRoot, relocatedRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relocatedRoot, sessionRoot); err != nil {
		_ = os.Rename(relocatedRoot, sessionRoot)
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(sessionRoot)
		if err := os.Rename(relocatedRoot, sessionRoot); err != nil {
			t.Errorf("restore Session Workspace ancestor: %v", err)
		}
	}()
	if _, err := materializer.Inspect(context.Background(), materialized); err == nil {
		t.Fatal("Workspace inspection followed a symlink ancestor below the configured root")
	}
}

func TestV2GitReferenceRestoreValidatesBeforeReplacingWholeGeneration(t *testing.T) {
	materializer, _, materialized := materializeV2GitWorkspace(t)
	head := strings.TrimSpace(runTestGitOutput(t, materialized.Directory, "rev-parse", "HEAD"))
	branch := "synara/restored-reference"
	checkpoint := executions.WorkspaceCheckpoint{
		ID: uuid.New(), Strategy: "git-reference", Status: "ready",
		BaseCommit: materialized.BaseCommit, HeadCommit: &head, CurrentBranch: &branch,
		Manifest: map[string]any{
			"format": "synara-git-reference-v1", "headCommit": head, "currentBranch": branch,
		},
	}
	checkoutMarker := filepath.Join(materialized.Directory, "old-generation.txt")
	repositoryMarker := filepath.Join(materialized.GitDir, "old-generation-marker")
	writeWorkspaceTestFile(t, checkoutMarker, "preserve until install\n")
	writeWorkspaceTestFile(t, repositoryMarker, "old private repository\n")
	oldRepositoryInfo, err := os.Stat(materialized.GitDir)
	if err != nil {
		t.Fatal(err)
	}

	realRun := materializer.runGit
	materializer.runGit = func(
		ctx context.Context,
		directory string,
		environment []string,
		arguments ...string,
	) (string, error) {
		command := gitCommandArguments(arguments)
		if directory != materialized.Directory && len(command) == 2 && command[0] == "branch" && command[1] == "--show-current" {
			return "", errors.New("injected staged inspection failure")
		}
		return realRun(ctx, directory, environment, arguments...)
	}
	if _, err := materializer.Restore(context.Background(), materialized, checkpoint, ""); err == nil {
		t.Fatal("Git-reference restore ignored a staged generation validation failure")
	}
	materializer.runGit = realRun
	assertWorkspaceTestFile(t, checkoutMarker, "preserve until install\n")
	assertWorkspaceTestFile(t, repositoryMarker, "old private repository\n")
	assertValidPrivateGeneration(t, materializer, materialized)
	failedRepositoryInfo, err := os.Stat(materialized.GitDir)
	if err != nil || !os.SameFile(oldRepositoryInfo, failedRepositoryInfo) {
		t.Fatalf("failed Git-reference restore replaced the old private repository: info=%v err=%v", failedRepositoryInfo, err)
	}

	restored, err := materializer.Restore(context.Background(), materialized, checkpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	if restored.RestoredCheckpointID == nil || *restored.RestoredCheckpointID != checkpoint.ID {
		t.Fatalf("Git-reference restore did not report the installed Checkpoint: %#v", restored.RestoredCheckpointID)
	}
	for _, stalePath := range []string{checkoutMarker, repositoryMarker} {
		if _, err := os.Lstat(stalePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("whole-generation Git-reference restore retained %s: %v", stalePath, err)
		}
	}
	if restored.CurrentBranch == nil || *restored.CurrentBranch != branch || restored.HeadCommit == nil || *restored.HeadCommit != head {
		t.Fatalf("Git-reference restore returned the wrong Git state: %#v", restored)
	}
	newRepositoryInfo, err := os.Stat(restored.GitDir)
	if err != nil || os.SameFile(oldRepositoryInfo, newRepositoryInfo) {
		t.Fatalf("Git-reference restore did not replace the complete private repository: info=%v err=%v", newRepositoryInfo, err)
	}
	assertValidPrivateGeneration(t, materializer, restored)
}

func TestV2PatchRestoreValidatesBeforeReplacingWholeGeneration(t *testing.T) {
	materializer, execution, materialized := materializeV2GitWorkspace(t)
	execution.Generation = 3
	writeWorkspaceTestFile(t, filepath.Join(materialized.Directory, "README.md"), "captured tracked state\n")
	writeWorkspaceTestFile(t, filepath.Join(materialized.Directory, "captured-untracked.txt"), "captured untracked state\n")
	inspection, err := materializer.Inspect(context.Background(), materialized)
	if err != nil || !inspection.Dirty {
		t.Fatalf("failed to prepare dirty v2 Workspace: %#v err=%v", inspection, err)
	}
	candidate, err := captureWorkspaceCheckpoint(context.Background(), execution, materialized, inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	if candidate.Strategy != "patch" {
		t.Fatalf("dirty Git Workspace produced %q instead of a Patch", candidate.Strategy)
	}
	checkpoint := workspaceCheckpointFromCandidate(t, candidate)
	checkoutMarker := filepath.Join(materialized.Directory, "old-generation-only.txt")
	repositoryMarker := filepath.Join(materialized.GitDir, "old-generation-marker")
	writeWorkspaceTestFile(t, checkoutMarker, "must survive failed restore\n")
	writeWorkspaceTestFile(t, repositoryMarker, "must survive failed restore\n")
	oldRepositoryInfo, err := os.Stat(materialized.GitDir)
	if err != nil {
		t.Fatal(err)
	}

	realRun := materializer.runGit
	materializer.runGit = func(
		ctx context.Context,
		directory string,
		environment []string,
		arguments ...string,
	) (string, error) {
		command := gitCommandArguments(arguments)
		if directory != materialized.Directory && len(command) > 0 && command[0] == "apply" {
			return "", errors.New("injected staged Patch failure")
		}
		return realRun(ctx, directory, environment, arguments...)
	}
	if _, err := materializer.Restore(context.Background(), materialized, checkpoint, candidate.ArtifactPath); err == nil {
		t.Fatal("Patch restore ignored a staged Patch failure")
	}
	materializer.runGit = realRun
	assertWorkspaceTestFile(t, checkoutMarker, "must survive failed restore\n")
	assertWorkspaceTestFile(t, repositoryMarker, "must survive failed restore\n")
	assertValidPrivateGeneration(t, materializer, materialized)
	failedRepositoryInfo, err := os.Stat(materialized.GitDir)
	if err != nil || !os.SameFile(oldRepositoryInfo, failedRepositoryInfo) {
		t.Fatalf("failed Patch restore replaced the old private repository: info=%v err=%v", failedRepositoryInfo, err)
	}

	restored, err := materializer.Restore(context.Background(), materialized, checkpoint, candidate.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RestoredCheckpointID == nil || *restored.RestoredCheckpointID != checkpoint.ID {
		t.Fatalf("Patch restore did not report the installed Checkpoint: %#v", restored.RestoredCheckpointID)
	}
	assertWorkspaceTestFile(t, filepath.Join(restored.Directory, "README.md"), "captured tracked state\n")
	assertWorkspaceTestFile(t, filepath.Join(restored.Directory, "captured-untracked.txt"), "captured untracked state\n")
	for _, stalePath := range []string{checkoutMarker, repositoryMarker} {
		if _, err := os.Lstat(stalePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("whole-generation Patch restore retained %s: %v", stalePath, err)
		}
	}
	newRepositoryInfo, err := os.Stat(restored.GitDir)
	if err != nil || os.SameFile(oldRepositoryInfo, newRepositoryInfo) {
		t.Fatalf("Patch restore did not replace the complete private repository: info=%v err=%v", newRepositoryInfo, err)
	}
	assertValidPrivateGeneration(t, materializer, restored)
}

func TestV2SnapshotRestoreInspectsBeforeReplacingWholeGeneration(t *testing.T) {
	materializer, execution, materialized := materializeV2NonGitWorkspace(t)
	execution.Generation = 4
	capturedPath := filepath.Join(materialized.Directory, "captured.txt")
	writeWorkspaceTestFile(t, capturedPath, "captured Snapshot state\n")
	inspection, err := materializer.Inspect(context.Background(), materialized)
	if err != nil || !inspection.Dirty {
		t.Fatalf("failed to prepare non-Git v2 Workspace: %#v err=%v", inspection, err)
	}
	candidate, err := captureWorkspaceCheckpoint(context.Background(), execution, materialized, inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	if candidate.Strategy != "snapshot" {
		t.Fatalf("non-Git Workspace produced %q instead of a Snapshot", candidate.Strategy)
	}
	checkpoint := workspaceCheckpointFromCandidate(t, candidate)
	writeWorkspaceTestFile(t, capturedPath, "old generation changed after capture\n")
	stalePath := filepath.Join(materialized.Directory, "old-generation-only.txt")
	writeWorkspaceTestFile(t, stalePath, "must survive failed restore\n")
	oldRootInfo, err := os.Stat(materialized.LogicalRoot)
	if err != nil {
		t.Fatal(err)
	}

	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := materializer.Restore(canceledContext, materialized, checkpoint, candidate.ArtifactPath); err == nil {
		t.Fatal("Snapshot restore ignored a staged inspection failure")
	}
	assertWorkspaceTestFile(t, capturedPath, "old generation changed after capture\n")
	assertWorkspaceTestFile(t, stalePath, "must survive failed restore\n")
	assertValidNonGitGeneration(t, materialized)
	failedRootInfo, err := os.Stat(materialized.LogicalRoot)
	if err != nil || !os.SameFile(oldRootInfo, failedRootInfo) {
		t.Fatalf("failed Snapshot restore replaced the old generation root: info=%v err=%v", failedRootInfo, err)
	}

	restored, err := materializer.Restore(context.Background(), materialized, checkpoint, candidate.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RestoredCheckpointID == nil || *restored.RestoredCheckpointID != checkpoint.ID {
		t.Fatalf("Snapshot restore did not report the installed Checkpoint: %#v", restored.RestoredCheckpointID)
	}
	assertWorkspaceTestFile(t, capturedPath, "captured Snapshot state\n")
	if _, err := os.Lstat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("whole-generation Snapshot restore retained %s: %v", stalePath, err)
	}
	newRootInfo, err := os.Stat(restored.LogicalRoot)
	if err != nil || os.SameFile(oldRootInfo, newRootInfo) {
		t.Fatalf("Snapshot restore did not replace the complete generation: info=%v err=%v", newRootInfo, err)
	}
	assertValidNonGitGeneration(t, restored)
}

func materializeV2GitWorkspace(
	t *testing.T,
) (*WorkspaceMaterializer, executions.Execution, WorkspaceMaterialization) {
	t.Helper()
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, targetID)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	configureWorkspaceTestNetwork(t, materializer, createWorkspaceTestSource(t), nil)
	materialized, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if materialized.LogicalRoot == "" || materialized.GitDir == "" ||
		materialized.Directory != filepath.Join(materialized.LogicalRoot, "checkout") ||
		materialized.GitDir != filepath.Join(materialized.LogicalRoot, "repo.git") ||
		materialized.cache.RepoGit == "" || materialized.cache.LockPath == "" {
		t.Fatalf("test Workspace did not use the v2 private generation path: %#v", materialized)
	}
	t.Cleanup(func() {
		if err := materialized.Release(); err != nil {
			t.Errorf("release v2 Workspace: %v", err)
		}
	})
	return materializer, execution, materialized
}

func materializeV2NonGitWorkspace(
	t *testing.T,
) (*WorkspaceMaterializer, executions.Execution, WorkspaceMaterialization) {
	t.Helper()
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	workload.RepositoryURL = nil
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, targetID)
	materialized, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if materialized.LogicalRoot == "" || materialized.GitDir != "" ||
		materialized.Directory != filepath.Join(materialized.LogicalRoot, "checkout") {
		t.Fatalf("test Workspace did not use the v2 non-Git generation path: %#v", materialized)
	}
	t.Cleanup(func() {
		if err := materialized.Release(); err != nil {
			t.Errorf("release v2 non-Git Workspace: %v", err)
		}
	})
	return materializer, execution, materialized
}

func workspaceCheckpointFromCandidate(t *testing.T, candidate WorkspaceCheckpointCandidate) executions.WorkspaceCheckpoint {
	t.Helper()
	fileCount := candidate.FileCount
	totalBytes := candidate.TotalBytes
	checkpoint := executions.WorkspaceCheckpoint{
		ID: uuid.New(), Strategy: candidate.Strategy, Status: "ready",
		BaseCommit: candidate.BaseCommit, HeadCommit: candidate.HeadCommit, CurrentBranch: candidate.CurrentBranch,
		Manifest: candidate.Manifest, FileCount: &fileCount, TotalBytes: &totalBytes,
	}
	if candidate.Artifact != nil {
		artifact, err := os.ReadFile(candidate.ArtifactPath)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(artifact)
		encodedDigest := hex.EncodeToString(digest[:])
		artifactID := uuid.New()
		checkpoint.ArtifactID = &artifactID
		checkpoint.SHA256 = &encodedDigest
	}
	return checkpoint
}

func assertValidPrivateGeneration(
	t *testing.T,
	materializer *WorkspaceMaterializer,
	materialized WorkspaceMaterialization,
) {
	t.Helper()
	layout := workspaceLayout{
		Root: materialized.LogicalRoot, Checkout: materialized.Directory, GitDir: materialized.GitDir,
		Manifest: filepath.Join(materialized.LogicalRoot, "manifest.json"),
	}
	if err := materializer.validatePrivateGitGeneration(context.Background(), layout, materialized.manifest); err != nil {
		t.Fatalf("restored private generation is invalid: %v", err)
	}
}

func assertValidNonGitGeneration(t *testing.T, materialized WorkspaceMaterialization) {
	t.Helper()
	layout := workspaceLayout{
		Root: materialized.LogicalRoot, Checkout: materialized.Directory,
		GitDir:   filepath.Join(materialized.LogicalRoot, "repo.git"),
		Manifest: filepath.Join(materialized.LogicalRoot, "manifest.json"),
	}
	if err := validateNonGitGeneration(layout, materialized.manifest); err != nil {
		t.Fatalf("restored non-Git generation is invalid: %v", err)
	}
}

func writeWorkspaceTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertWorkspaceTestFile(t *testing.T, path, expected string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil || string(content) != expected {
		t.Fatalf("unexpected Workspace file %s: %q err=%v", path, content, err)
	}
}
