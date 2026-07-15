package agentd

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestWorkspacePatchCaptureAndRestoreUsesBaseCommitAndPreservesFinalTree(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runGitTestCommand(t, root, "init", "--bare", "--initial-branch=main", remote)
	source := filepath.Join(root, "source")
	runGitTestCommand(t, root, "clone", remote, source)
	runGitTestCommand(t, source, "config", "user.email", "synara@example.com")
	runGitTestCommand(t, source, "config", "user.name", "Synara Test")
	if err := os.WriteFile(filepath.Join(source, ".gitignore"), []byte("*.ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "binary.bin"), []byte{0, 1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "delete.txt"), []byte("delete me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "run.sh"), []byte("#!/bin/sh\necho base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(source, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, source, "add", ".")
	runGitTestCommand(t, source, "commit", "-m", "base")
	runGitTestCommand(t, source, "push", "origin", "main")
	baseCommit := runGitTestCommand(t, source, "rev-parse", "HEAD")
	branch := "synara/session-patch"
	runGitTestCommand(t, source, "switch", "-c", branch)
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, source, "add", "tracked.txt")
	runGitTestCommand(t, source, "commit", "-m", "local commit not pushed")
	sourceHead := runGitTestCommand(t, source, "rev-parse", "HEAD")
	if sourceHead == baseCommit {
		t.Fatal("test did not create a local Commit after the recovery base")
	}
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("final tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "binary.bin"), []byte{0, 255, 7, 8, 9, 10}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "run.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("staged.txt", filepath.Join(source, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "staged.txt"), []byte("staged addition\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, source, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(source, "ignored.ignored"), []byte("ignored but durable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "untracked.sh"), []byte("#!/bin/sh\necho untracked\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	materialized := WorkspaceMaterialization{
		Directory: source, Managed: true, CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &baseCommit,
	}
	materializer := NewWorkspaceMaterializer(root)
	inspection, err := materializer.Inspect(context.Background(), materialized)
	if err != nil || !inspection.Dirty || inspection.HeadCommit == nil || *inspection.HeadCommit != sourceHead {
		t.Fatalf("unexpected dirty source inspection: %#v err=%v", inspection, err)
	}
	execution := executions.Execution{ID: uuid.New(), Generation: 7}
	first, err := captureWorkspaceCheckpoint(context.Background(), execution, materialized, inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cleanup()
	second, err := captureWorkspaceCheckpoint(context.Background(), execution, materialized, inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Cleanup()
	if first.Strategy != "patch" || first.Artifact == nil || first.Artifact.Kind != "checkpoint" ||
		first.BaseCommit == nil || *first.BaseCommit != baseCommit || first.HeadCommit == nil || *first.HeadCommit != sourceHead {
		t.Fatalf("unexpected Patch candidate: %#v", first)
	}
	firstArchive, err := os.ReadFile(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	secondArchive, err := os.ReadFile(second.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstArchive, secondArchive) {
		t.Fatal("identical Workspace state produced non-deterministic Patch Artifacts")
	}
	manifest, err := decodePatchManifest(executions.WorkspaceCheckpoint{
		Strategy: "patch", BaseCommit: first.BaseCommit, CurrentBranch: first.CurrentBranch,
		Manifest: first.Manifest, FileCount: &first.FileCount, TotalBytes: &first.TotalBytes,
	})
	if err != nil || manifest.Format != checkpointPatchFormat || manifest.TrackedPatch.SizeBytes == 0 {
		t.Fatalf("invalid captured Patch manifest: %#v err=%v", manifest, err)
	}
	ignoredFound := false
	for _, file := range manifest.Untracked {
		if file.Path == "ignored.ignored" {
			ignoredFound = true
		}
	}
	if !ignoredFound {
		t.Fatal("Patch capture silently omitted an ignored untracked file")
	}

	replacement := filepath.Join(root, "replacement")
	runGitTestCommand(t, root, "clone", remote, replacement)
	replacementBranch := "main"
	replacementMaterialized := WorkspaceMaterialization{
		Directory: replacement, Managed: true, CurrentBranch: &replacementBranch,
		BaseCommit: &baseCommit, HeadCommit: &baseCommit,
	}
	if err := os.WriteFile(filepath.Join(replacement, "sentinel.txt"), []byte("original workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidManifest := cloneManifestMap(t, first.Manifest)
	invalidManifest["trackedPatch"].(map[string]any)["sha256"] = strings.Repeat("0", 64)
	artifactID := uuid.New()
	artifactSHA := sha256.Sum256(firstArchive)
	artifactDigest := hex.EncodeToString(artifactSHA[:])
	invalidCheckpoint := executions.WorkspaceCheckpoint{
		ID: uuid.New(), Strategy: "patch", Status: "ready", ArtifactID: &artifactID, SHA256: &artifactDigest,
		BaseCommit: first.BaseCommit, HeadCommit: first.HeadCommit, CurrentBranch: first.CurrentBranch,
		Manifest: invalidManifest, FileCount: &first.FileCount, TotalBytes: &first.TotalBytes,
	}
	if _, err := materializer.Restore(context.Background(), replacementMaterialized, invalidCheckpoint, first.ArtifactPath); err == nil {
		t.Fatal("Patch restore accepted a manifest with the wrong tracked Patch digest")
	}
	if sentinel, err := os.ReadFile(filepath.Join(replacement, "sentinel.txt")); err != nil || string(sentinel) != "original workspace\n" {
		t.Fatalf("invalid Patch modified the active Workspace: %q err=%v", sentinel, err)
	}

	checkpoint := invalidCheckpoint
	checkpoint.Manifest = first.Manifest
	restored, err := materializer.Restore(context.Background(), replacementMaterialized, checkpoint, first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RestoredCheckpointID == nil || *restored.RestoredCheckpointID != checkpoint.ID {
		t.Fatalf("Patch restore did not report the Checkpoint: %#v", restored)
	}
	assertFileContent(t, replacement, "tracked.txt", []byte("final tracked\n"))
	assertFileContent(t, replacement, "binary.bin", []byte{0, 255, 7, 8, 9, 10})
	assertFileContent(t, replacement, "staged.txt", []byte("staged addition\n"))
	assertFileContent(t, replacement, "ignored.ignored", []byte("ignored but durable\n"))
	assertFileContent(t, replacement, "untracked.sh", []byte("#!/bin/sh\necho untracked\n"))
	if target, err := os.Readlink(filepath.Join(replacement, "tracked-link")); err != nil || target != "staged.txt" {
		t.Fatalf("Patch restore lost tracked symlink target: %q err=%v", target, err)
	}
	if _, err := os.Stat(filepath.Join(replacement, "delete.txt")); !os.IsNotExist(err) {
		t.Fatalf("Patch restore retained a tracked deletion: %v", err)
	}
	for _, executable := range []string{"run.sh", "untracked.sh"} {
		info, err := os.Stat(filepath.Join(replacement, executable))
		if err != nil {
			t.Fatalf("Stat restored executable %s: %v", executable, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("Patch restore lost executable mode for %s: mode=%v", executable, info.Mode())
		}
	}
	if head := runGitTestCommand(t, replacement, "rev-parse", "HEAD"); head != baseCommit {
		t.Fatalf("Patch restore claimed the unavailable source HEAD instead of the base Commit: %s", head)
	}
	if currentBranch := runGitTestCommand(t, replacement, "branch", "--show-current"); currentBranch != branch {
		t.Fatalf("Patch restore lost the Session branch: %s", currentBranch)
	}
	command := exec.Command("git", "cat-file", "-e", sourceHead+"^{commit}")
	command.Dir = replacement
	if err := command.Run(); err == nil {
		t.Fatal("replacement Workspace unexpectedly depended on the unpushed source Commit object")
	}
	staged := runGitTestCommand(t, replacement, "diff", "--cached", "--name-only", baseCommit, "--")
	for _, expected := range []string{"binary.bin", "delete.txt", "run.sh", "staged.txt", "tracked-link", "tracked.txt"} {
		if !strings.Contains(staged, expected) {
			t.Fatalf("Patch restore did not stage tracked delta %q: %s", expected, staged)
		}
	}
	untracked := runGitTestCommand(t, replacement, "ls-files", "--others", "--")
	if !strings.Contains(untracked, "ignored.ignored") || !strings.Contains(untracked, "untracked.sh") {
		t.Fatalf("Patch restore lost untracked classification: %s", untracked)
	}
	restoredInspection, err := materializer.Inspect(context.Background(), restored)
	if err != nil {
		t.Fatal(err)
	}
	recaptured, err := captureWorkspaceCheckpoint(context.Background(), execution, restored, restoredInspection)
	if err != nil {
		t.Fatal(err)
	}
	defer recaptured.Cleanup()
	if !checkpointMatchesRestored(recaptured, &checkpoint) {
		t.Fatalf("unchanged restored Patch produced a different content identity: %#v", recaptured)
	}
}

func TestWorkspacePatchIgnoredPolicyKeepsFilesAndExcludesDirectoryTrees(t *testing.T) {
	directory, baseCommit, branch := initializeCheckpointGitRepository(t, "*.ignored\nnode_modules/\ndurable-dir/\n")
	nodeModules := filepath.Join(directory, "node_modules")
	if err := os.Mkdir(nodeModules, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= checkpointSnapshotMaxFiles; index++ {
		name := filepath.Join(nodeModules, fmt.Sprintf("dependency-%04d.js", index))
		if err := os.WriteFile(name, []byte("cache"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Symlink("dependency-0000.js", filepath.Join(nodeModules, "dependency-link.js"))
	ignoredPath := filepath.Join(directory, "durable.ignored")
	if err := os.WriteFile(ignoredPath, []byte("durable ignored file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "durable-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "durable-dir", "state.json"), []byte("durable directory file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	materialized := WorkspaceMaterialization{
		Directory: directory, Managed: true, CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &baseCommit,
	}
	materializer := NewWorkspaceMaterializer(t.TempDir())
	inspection, err := materializer.Inspect(context.Background(), materialized)
	if err != nil || !inspection.Dirty {
		t.Fatalf("ignored file was not treated as durable Workspace content: %#v err=%v", inspection, err)
	}
	candidate, err := captureWorkspaceCheckpoint(
		context.Background(), executions.Execution{ID: uuid.New(), Generation: 1}, materialized, inspection,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	manifest, err := decodePatchManifest(executions.WorkspaceCheckpoint{
		Strategy: "patch", BaseCommit: candidate.BaseCommit, CurrentBranch: candidate.CurrentBranch,
		Manifest: candidate.Manifest, FileCount: &candidate.FileCount, TotalBytes: &candidate.TotalBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.FileCount != 2 || len(manifest.Untracked) != 2 || manifest.Untracked[0].Path != "durable-dir/state.json" ||
		manifest.Untracked[1].Path != "durable.ignored" {
		t.Fatalf("rebuildable ignored directory tree leaked into the Patch payload: %#v", manifest.Untracked)
	}
	if len(manifest.Excluded) != 2 || manifest.Excluded[0] != checkpointPatchExcludedGit ||
		manifest.Excluded[1] != checkpointPatchExcludedIgnored {
		t.Fatalf("Patch manifest did not declare the ignored-directory policy: %#v", manifest.Excluded)
	}
	if err := os.Remove(ignoredPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(directory, "durable-dir")); err != nil {
		t.Fatal(err)
	}
	excludedOnly, err := materializer.Inspect(context.Background(), materialized)
	if err != nil {
		t.Fatal(err)
	}
	if excludedOnly.Dirty {
		t.Fatalf("rebuildable ignored directory tree forced a Checkpoint: %#v", excludedOnly)
	}
}

func TestWorkspacePatchRestoresRawTrackedBytesAfterAttributesChange(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runGitTestCommand(t, root, "init", "--bare", "--initial-branch=main", remote)
	source := filepath.Join(root, "source")
	runGitTestCommand(t, root, "clone", remote, source)
	runGitTestCommand(t, source, "config", "user.email", "synara@example.com")
	runGitTestCommand(t, source, "config", "user.name", "Synara Test")
	runGitTestCommand(t, source, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(source, ".gitattributes"), []byte("*.txt text eol=lf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, source, "add", ".")
	runGitTestCommand(t, source, "commit", "-m", "base")
	runGitTestCommand(t, source, "push", "origin", "main")
	baseCommit := runGitTestCommand(t, source, "rev-parse", "HEAD")
	branch := "synara/session-attributes"
	runGitTestCommand(t, source, "switch", "-c", branch)
	if err := os.WriteFile(filepath.Join(source, ".gitattributes"), []byte("*.txt text eol=crlf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected := []byte("final\r\nbytes\r\n")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), expected, 0o600); err != nil {
		t.Fatal(err)
	}
	materialized := WorkspaceMaterialization{
		Directory: source, Managed: true, CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &baseCommit,
	}
	materializer := NewWorkspaceMaterializer(root)
	inspection, err := materializer.Inspect(context.Background(), materialized)
	if err != nil || !inspection.Dirty {
		t.Fatalf("attribute-changing Workspace was not dirty: %#v err=%v", inspection, err)
	}
	candidate, err := captureWorkspaceCheckpoint(
		context.Background(), executions.Execution{ID: uuid.New(), Generation: 2}, materialized, inspection,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	archive, err := os.ReadFile(candidate.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	artifactID := uuid.New()
	artifactSHA := hex.EncodeToString(digest[:])
	checkpoint := executions.WorkspaceCheckpoint{
		ID: uuid.New(), Strategy: "patch", Status: "ready", ArtifactID: &artifactID, SHA256: &artifactSHA,
		BaseCommit: candidate.BaseCommit, HeadCommit: candidate.HeadCommit, CurrentBranch: candidate.CurrentBranch,
		Manifest: candidate.Manifest, FileCount: &candidate.FileCount, TotalBytes: &candidate.TotalBytes,
	}
	replacement := filepath.Join(root, "replacement")
	runGitTestCommand(t, root, "clone", remote, replacement)
	runGitTestCommand(t, replacement, "config", "core.autocrlf", "false")
	mainBranch := "main"
	restored, err := materializer.Restore(context.Background(), WorkspaceMaterialization{
		Directory: replacement, Managed: true, CurrentBranch: &mainBranch, BaseCommit: &baseCommit, HeadCommit: &baseCommit,
	}, checkpoint, candidate.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, replacement, "tracked.txt", expected)
	runGitTestCommand(t, replacement, "diff", "--quiet", "--")
	if restored.HeadCommit == nil || *restored.HeadCommit != baseCommit {
		t.Fatalf("attribute Patch restore was not anchored to the base Commit: %#v", restored)
	}
}

func TestWorkspacePatchRejectsIndexFlagsAndNonReproducibleGitMetadata(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "assume unchanged",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "update-index", "--assume-unchanged", "tracked.txt")
			},
		},
		{
			name: "skip worktree",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "update-index", "--skip-worktree", "tracked.txt")
			},
		},
		{
			name: "autocrlf",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "config", "core.autocrlf", "true")
			},
		},
		{
			name: "filemode disabled",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "config", "core.filemode", "false")
			},
		},
		{
			name: "diff context",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "config", "diff.context", "1")
			},
		},
		{
			name: "forced color",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "config", "color.ui", "always")
			},
		},
		{
			name: "quoted path semantics",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "config", "core.quotePath", "false")
			},
		},
		{
			name: "binary threshold",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "config", "core.bigFileThreshold", "1")
			},
		},
		{
			name: "promisor remote",
			prepare: func(t *testing.T, directory string) {
				runGitTestCommand(t, directory, "config", "remote.origin.promisor", "true")
			},
		},
		{
			name: "info attributes",
			prepare: func(t *testing.T, directory string) {
				if err := os.WriteFile(filepath.Join(directory, ".git", "info", "attributes"), []byte("*.txt text eol=crlf\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "legacy grafts",
			prepare: func(t *testing.T, directory string) {
				if err := os.WriteFile(filepath.Join(directory, ".git", "info", "grafts"), []byte("unsupported\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory, baseCommit, branch := initializeCheckpointGitRepository(t, "")
			test.prepare(t, directory)
			if err := os.WriteFile(filepath.Join(directory, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := NewWorkspaceMaterializer(t.TempDir()).Inspect(context.Background(), WorkspaceMaterialization{
				Directory: directory, Managed: true, CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &baseCommit,
			})
			if err == nil {
				t.Fatal("Workspace inspection accepted Git state that Patch restore cannot reproduce")
			}
		})
	}
}

func TestWorkspacePatchIgnoresReplaceObjectRefs(t *testing.T) {
	directory, baseCommit, branch := initializeCheckpointGitRepository(t, "")
	if err := os.WriteFile(filepath.Join(directory, "tracked.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, directory, "add", "tracked.txt")
	runGitTestCommand(t, directory, "commit", "-m", "second")
	headCommit := runGitTestCommand(t, directory, "rev-parse", "HEAD")
	runGitTestCommand(t, directory, "replace", baseCommit, headCommit)
	materialized := WorkspaceMaterialization{
		Directory: directory, Managed: true, CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &baseCommit,
	}
	inspection, err := NewWorkspaceMaterializer(t.TempDir()).Inspect(context.Background(), materialized)
	if err != nil || !inspection.Dirty || inspection.HeadCommit == nil || *inspection.HeadCommit != headCommit {
		t.Fatalf("replace refs affected Workspace inspection: %#v err=%v", inspection, err)
	}
	candidate, err := captureWorkspaceCheckpoint(
		context.Background(), executions.Execution{ID: uuid.New(), Generation: 3}, materialized, inspection,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	manifest, err := decodePatchManifest(executions.WorkspaceCheckpoint{
		Strategy: "patch", BaseCommit: candidate.BaseCommit, CurrentBranch: candidate.CurrentBranch,
		Manifest: candidate.Manifest, FileCount: &candidate.FileCount, TotalBytes: &candidate.TotalBytes,
	})
	if err != nil || manifest.TrackedPatch.SizeBytes == 0 || len(manifest.TrackedFiles) == 0 {
		t.Fatalf("replace refs changed Patch content identity: %#v err=%v", manifest, err)
	}
}

func TestWorkspacePatchManifestUsesControlPlaneSizeLimit(t *testing.T) {
	manifest := checkpointPatchManifest{
		Format: checkpointPatchFormat, BaseCommit: strings.Repeat("a", 40), CurrentBranch: "main",
		TrackedPatch: checkpointPatchPayload{Path: checkpointPatchEntryName, SHA256: strings.Repeat("b", 64)},
		Excluded:     []string{strings.Repeat("x", executions.CheckpointManifestMaxBytes)},
		IndexPolicy:  checkpointPatchIndexPolicy,
	}
	if _, err := checkpointManifestMap(manifest); err == nil {
		t.Fatal("agentd accepted a manifest that the Control Plane would reject")
	}
}

func TestWorkspacePatchCaptureRejectsUntrackedSymlink(t *testing.T) {
	directory := t.TempDir()
	runGitTestCommand(t, directory, "init", "-b", "main")
	runGitTestCommand(t, directory, "config", "user.email", "synara@example.com")
	runGitTestCommand(t, directory, "config", "user.name", "Synara Test")
	if err := os.WriteFile(filepath.Join(directory, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, directory, "add", "tracked.txt")
	runGitTestCommand(t, directory, "commit", "-m", "base")
	base := runGitTestCommand(t, directory, "rev-parse", "HEAD")
	branch := "main"
	if err := os.Symlink("tracked.txt", filepath.Join(directory, "untracked-link")); err != nil {
		t.Fatal(err)
	}
	materialized := WorkspaceMaterialization{
		Directory: directory, Managed: true, CurrentBranch: &branch, BaseCommit: &base, HeadCommit: &base,
	}
	inspection, err := NewWorkspaceMaterializer(t.TempDir()).Inspect(context.Background(), materialized)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureWorkspaceCheckpoint(
		context.Background(), executions.Execution{ID: uuid.New(), Generation: 1}, materialized, inspection,
	); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("Patch capture accepted an untracked symlink: %v", err)
	}
}

func cloneManifestMap(t *testing.T, input map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func assertFileContent(t *testing.T, root, relative string, expected []byte) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil || !bytes.Equal(content, expected) {
		t.Fatalf("unexpected %s content: %v err=%v", relative, content, err)
	}
}

func TestWorkspaceSnapshotCaptureAndRestorePreservesVerifiedFiles(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "tenant", "project", "session", "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "note.txt"), []byte("durable content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "run.sh"), []byte("#!/bin/sh\necho restored\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	execution := executions.Execution{ID: uuid.New(), Generation: 3}
	materialized := WorkspaceMaterialization{Directory: workspace, Managed: true}
	candidate, err := captureWorkspaceCheckpoint(context.Background(), execution, materialized, WorkspaceInspection{Dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	if candidate.Strategy != "snapshot" || candidate.Artifact == nil || candidate.FileCount != 2 {
		t.Fatalf("unexpected Snapshot candidate: %#v", candidate)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "note.txt"), []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "extra.txt"), []byte("remove me"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactID := uuid.New()
	sha := strings.Repeat("a", 64)
	checkpoint := executions.WorkspaceCheckpoint{
		ID: uuid.New(), Strategy: "snapshot", Status: "ready", ArtifactID: &artifactID,
		SHA256: &sha, Manifest: candidate.Manifest, FileCount: &candidate.FileCount,
		TotalBytes: &candidate.TotalBytes,
	}
	restored, err := NewWorkspaceMaterializer(root).Restore(
		context.Background(), materialized, checkpoint, candidate.ArtifactPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RestoredCheckpointID == nil || *restored.RestoredCheckpointID != checkpoint.ID {
		t.Fatalf("restore did not report the applied Checkpoint: %#v", restored.RestoredCheckpointID)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "nested", "note.txt"))
	if err != nil || string(content) != "durable content\n" {
		t.Fatalf("Snapshot content was not restored: %q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("Snapshot restore retained an unexpected file: %v", err)
	}
	info, err := os.Stat(filepath.Join(workspace, "run.sh"))
	if err != nil {
		t.Fatalf("Stat restored Snapshot executable: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("Snapshot executable mode was not restored: mode=%v", info.Mode())
	}
}

func TestWorkspaceSnapshotCaptureRejectsCanceledContextForEmptyWorkspace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := captureWorkspaceSnapshot(
		ctx,
		executions.Execution{ID: uuid.New(), Generation: 1},
		WorkspaceMaterialization{Directory: t.TempDir(), Managed: true},
		WorkspaceInspection{Dirty: true},
		"cancelled-snapshot",
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled empty Workspace snapshot returned %v", err)
	}
}

func TestWorkspaceSnapshotRestoreRejectsTraversalArchive(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "tenant", "project", "session", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "malicious.tar")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(archive)
	payload := []byte("escape")
	if err := writer.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o600, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	artifactID := uuid.New()
	sha := strings.Repeat("b", 64)
	fileCount := 1
	totalBytes := int64(len(payload))
	checkpoint := executions.WorkspaceCheckpoint{
		ID: uuid.New(), Strategy: "snapshot", Status: "ready", ArtifactID: &artifactID,
		SHA256: &sha, FileCount: &fileCount, TotalBytes: &totalBytes,
		Manifest: map[string]any{
			"format": "synara-workspace-snapshot-v1",
			"files": []map[string]any{{
				"path": "safe.txt", "size": len(payload), "sha256": strings.Repeat("c", 64),
				"executable": false,
			}},
		},
	}
	if _, err := NewWorkspaceMaterializer(root).Restore(
		context.Background(), WorkspaceMaterialization{Directory: workspace, Managed: true}, checkpoint, archivePath,
	); err == nil {
		t.Fatal("Snapshot restore accepted a traversal archive")
	}
	if _, err := os.Stat(filepath.Join(root, "tenant", "project", "session", "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("Traversal archive wrote outside the Workspace: %v", err)
	}
}

func TestWorkspaceInspectionDetectsCommittedHeadChange(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, repository, "init", "-b", "main")
	runGitTestCommand(t, repository, "config", "user.email", "synara@example.com")
	runGitTestCommand(t, repository, "config", "user.name", "Synara Test")
	if err := os.WriteFile(filepath.Join(repository, "file.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, repository, "add", "file.txt")
	runGitTestCommand(t, repository, "commit", "-m", "first")
	initialHead := runGitTestCommand(t, repository, "rev-parse", "HEAD")
	branch := "main"
	if err := os.WriteFile(filepath.Join(repository, "file.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, repository, "add", "file.txt")
	runGitTestCommand(t, repository, "commit", "-m", "second")
	inspection, err := NewWorkspaceMaterializer(root).Inspect(context.Background(), WorkspaceMaterialization{
		Directory: repository, Managed: true, CurrentBranch: &branch, HeadCommit: &initialHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Dirty || inspection.HeadCommit == nil || *inspection.HeadCommit == initialHead {
		t.Fatalf("committed HEAD change was not detected: %#v", inspection)
	}
}

func TestUploadCheckpointArtifactRecoversCompletedArtifactAfterResponseLoss(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	checkpointID := uuid.New()
	artifactID := uuid.New()
	payload := []byte("durable checkpoint payload")
	artifactPath := filepath.Join(t.TempDir(), "workspace.tar")
	if err := os.WriteFile(artifactPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	sha := hex.EncodeToString(digest[:])
	var createRequestIDs []string
	createCalls, uploadCalls, completeCalls := 0, 0, 0
	ready := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch request.URL.Path {
		case base + "artifacts":
			createCalls++
			createRequestIDs = append(createRequestIDs, request.Header.Get("X-Request-ID"))
			var input artifacts.WorkerCreateInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.CheckpointID == nil || *input.CheckpointID != checkpointID {
				http.Error(response, "invalid Checkpoint Artifact create", http.StatusBadRequest)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			if ready {
				_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`","status":"ready","sizeBytes":`+
					fmt.Sprint(len(payload))+`,"sha256":"`+sha+`","contentType":"application/x-tar"},"uploadRequired":false}`)
				return
			}
			_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`","status":"pending"},"uploadRequired":true,"method":"PUT","url":"`+
				server.URL+`/checkpoint-upload","headers":{},"expiresAt":"2030-01-01T00:00:00Z"}`)
		case "/checkpoint-upload":
			uploadCalls++
			uploaded, _ := io.ReadAll(request.Body)
			if string(uploaded) != string(payload) {
				http.Error(response, "payload mismatch", http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		case base + "artifacts/" + artifactID.String() + "/complete":
			completeCalls++
			var input artifacts.WorkerCompleteInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.SHA256 != sha || input.SizeBytes != int64(len(payload)) {
				http.Error(response, "invalid Artifact complete", http.StatusBadRequest)
				return
			}
			ready = true
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"id":"`+artifactID.String()+`"`)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: controlPlaneURL, RequestTimeout: time.Second, ArtifactTimeout: time.Second})
	client.workerToken = "worker-token"
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 4, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	candidate := WorkspaceCheckpointCandidate{
		IdempotencyKey: "response-loss", ArtifactPath: artifactPath,
		Artifact: &RunnerArtifact{Kind: "workspace_snapshot", OriginalName: "workspace.tar", ContentType: "application/x-tar"},
	}
	completed, err := client.UploadCheckpointArtifact(
		context.Background(), executionID, lease, executions.WorkspaceCheckpoint{ID: checkpointID}, candidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ID != artifactID || completed.Status != "ready" {
		t.Fatalf("unexpected recovered Artifact: %#v", completed)
	}
	if createCalls != 2 || uploadCalls != 1 || completeCalls != 1 {
		t.Fatalf("unexpected recovery calls: create=%d upload=%d complete=%d", createCalls, uploadCalls, completeCalls)
	}
	if len(createRequestIDs) != 2 || createRequestIDs[0] == "" || createRequestIDs[0] != createRequestIDs[1] {
		t.Fatalf("Checkpoint Artifact create did not reuse a stable request ID: %#v", createRequestIDs)
	}
}

func TestUploadArtifactRecoversCompletedArtifactAfterResponseLoss(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	artifactID := uuid.New()
	payload := []byte("durable generated file\n")
	artifactPath := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(artifactPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	sha := hex.EncodeToString(digest[:])
	const normalizedContentType = "text/plain; charset=UTF-8"
	var createRequestIDs, createIdempotencyKeys []string
	createCalls, uploadCalls, completeCalls := 0, 0, 0
	ready := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch request.URL.Path {
		case base + "artifacts":
			createCalls++
			createRequestIDs = append(createRequestIDs, request.Header.Get("X-Request-ID"))
			var input struct {
				TenantID     uuid.UUID  `json:"tenantId"`
				Generation   int64      `json:"generation"`
				LeaseToken   string     `json:"leaseToken"`
				CheckpointID *uuid.UUID `json:"checkpointId,omitempty"`
				Kind         string     `json:"kind"`
				OriginalName *string    `json:"originalName"`
				ExpiresAt    *time.Time `json:"expiresAt"`
			}
			idempotencyKey := request.Header.Get(artifacts.WorkerIdempotencyKeyHeader)
			decoder := json.NewDecoder(request.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&input); err != nil || input.CheckpointID != nil ||
				!strings.HasPrefix(idempotencyKey, "artifact-") {
				http.Error(response, "invalid idempotent Artifact create", http.StatusBadRequest)
				return
			}
			createIdempotencyKeys = append(createIdempotencyKeys, idempotencyKey)
			response.Header().Set("Content-Type", "application/json")
			if ready {
				_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`","status":"ready","sizeBytes":`+
					fmt.Sprint(len(payload))+`,"sha256":"`+sha+`","contentType":"`+normalizedContentType+`"},"uploadRequired":false}`)
				return
			}
			_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`","status":"pending"},"uploadRequired":true,"method":"PUT","url":"`+
				server.URL+`/generated-file-upload","headers":{},"expiresAt":"2030-01-01T00:00:00Z"}`)
		case "/generated-file-upload":
			uploadCalls++
			if request.Header.Get("Content-Type") != normalizedContentType {
				http.Error(response, "Content-Type was not normalized", http.StatusBadRequest)
				return
			}
			uploaded, _ := io.ReadAll(request.Body)
			if string(uploaded) != string(payload) {
				http.Error(response, "payload mismatch", http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		case base + "artifacts/" + artifactID.String() + "/complete":
			completeCalls++
			var input artifacts.WorkerCompleteInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.SHA256 != sha ||
				input.SizeBytes != int64(len(payload)) || input.ContentType != normalizedContentType {
				http.Error(response, "invalid Artifact complete", http.StatusBadRequest)
				return
			}
			ready = true
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"id":"`+artifactID.String()+`"`)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: controlPlaneURL, RequestTimeout: time.Second, ArtifactTimeout: time.Second})
	client.workerToken = "worker-token"
	client.artifactIdempotencySupported = true
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 4, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	completed, err := client.UploadArtifact(context.Background(), executionID, lease, RunnerArtifact{
		Path: "generated/report.txt", Kind: "generated_file", OriginalName: "report.txt",
		ContentType: "TEXT/PLAIN; Charset=UTF-8",
	}, artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ID != artifactID || completed.Status != "ready" {
		t.Fatalf("unexpected recovered Artifact: %#v", completed)
	}
	if createCalls != 2 || uploadCalls != 1 || completeCalls != 1 {
		t.Fatalf("unexpected recovery calls: create=%d upload=%d complete=%d", createCalls, uploadCalls, completeCalls)
	}
	if len(createRequestIDs) != 2 || createRequestIDs[0] == "" || createRequestIDs[0] != createRequestIDs[1] {
		t.Fatalf("Artifact create did not reuse a stable request ID: %#v", createRequestIDs)
	}
	if len(createIdempotencyKeys) != 2 || createIdempotencyKeys[0] != createIdempotencyKeys[1] {
		t.Fatalf("Artifact create did not reuse a stable idempotency key: %#v", createIdempotencyKeys)
	}
}

func TestInspectArtifactUploadIdentityHonorsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := openRegularArtifactSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = inspectArtifactUploadIdentity(ctx, uuid.New(), executions.Lease{Generation: 1}, RunnerArtifact{
		Path: "large.log", Kind: "terminal_log", ContentType: "text/plain",
	}, source)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Artifact pre-hash ignored cancellation: %v", err)
	}
}

func TestVerifyReadyArtifactFileRejectsLocalDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace.tar")
	if err := os.WriteFile(path, []byte("new payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	size := int64(len("old payload"))
	sha := strings.Repeat("a", 64)
	contentType := "application/x-tar"
	err := verifyReadyArtifactFile(path, contentType, artifacts.Artifact{
		Status: "ready", SizeBytes: &size, SHA256: &sha, ContentType: &contentType,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ready Artifact local drift was not rejected: %v", err)
	}
}

func initializeCheckpointGitRepository(t *testing.T, ignoreRules string) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	runGitTestCommand(t, directory, "init", "-b", "main")
	runGitTestCommand(t, directory, "config", "user.email", "synara@example.com")
	runGitTestCommand(t, directory, "config", "user.name", "Synara Test")
	if err := os.WriteFile(filepath.Join(directory, ".gitignore"), []byte(ignoreRules), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, directory, "add", ".")
	runGitTestCommand(t, directory, "commit", "-m", "base")
	baseCommit := runGitTestCommand(t, directory, "rev-parse", "HEAD")
	return directory, baseCommit, "main"
}

func runGitTestCommand(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
