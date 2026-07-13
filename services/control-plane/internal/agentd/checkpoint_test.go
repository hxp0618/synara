package agentd

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

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
	candidate, err := captureWorkspaceCheckpoint(execution, materialized, WorkspaceInspection{Dirty: true})
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
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("Snapshot executable mode was not restored: mode=%v err=%v", info.Mode(), err)
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
