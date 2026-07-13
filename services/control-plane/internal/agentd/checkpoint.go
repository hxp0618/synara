package agentd

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

const (
	checkpointSnapshotMaxFiles = 2_000
	checkpointSnapshotMaxBytes = int64(2 << 30)
)

type checkpointManifestFile struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable"`
}

func captureWorkspaceCheckpoint(
	ctx context.Context,
	execution executions.Execution,
	materialized WorkspaceMaterialization,
	inspection WorkspaceInspection,
) (WorkspaceCheckpointCandidate, error) {
	idempotencyKey := fmt.Sprintf("execution:%s:generation:%d:terminal", execution.ID, execution.Generation)
	if !inspection.Dirty && inspection.CurrentBranch != nil && inspection.HeadCommit != nil {
		return WorkspaceCheckpointCandidate{
			IdempotencyKey: idempotencyKey, Strategy: "git-reference",
			BaseCommit: materialized.BaseCommit, HeadCommit: inspection.HeadCommit,
			CurrentBranch: inspection.CurrentBranch,
			Manifest: map[string]any{
				"format": "synara-git-reference-v1", "headCommit": *inspection.HeadCommit,
				"currentBranch": *inspection.CurrentBranch,
			},
		}, nil
	}
	if workspaceHasGitMetadata(materialized.Directory) {
		return captureWorkspacePatch(ctx, execution, materialized, inspection, idempotencyKey)
	}
	return captureWorkspaceSnapshot(execution, materialized, inspection, idempotencyKey)
}

func captureWorkspaceSnapshot(
	execution executions.Execution,
	materialized WorkspaceMaterialization,
	inspection WorkspaceInspection,
	idempotencyKey string,
) (candidate WorkspaceCheckpointCandidate, resultErr error) {
	archive, err := os.CreateTemp("", "synara-workspace-snapshot-*.tar")
	if err != nil {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf("create Workspace snapshot: %w", err)
	}
	archivePath := archive.Name()
	cleanup := func() { _ = os.Remove(archivePath) }
	defer func() {
		if resultErr != nil {
			_ = archive.Close()
			cleanup()
		}
	}()

	writer := tar.NewWriter(archive)
	files := make([]checkpointManifestFile, 0)
	var totalBytes int64
	err = filepath.WalkDir(materialized.Directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(materialized.Directory, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		relative = filepath.Clean(relative)
		if relative == ".git" || strings.HasPrefix(relative, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("Workspace snapshot path escaped the Workspace root")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("Workspace snapshot rejected non-regular file %q", filepath.ToSlash(relative))
		}
		if len(files) >= checkpointSnapshotMaxFiles {
			return fmt.Errorf("Workspace snapshot exceeds %d files", checkpointSnapshotMaxFiles)
		}
		if info.Size() > checkpointSnapshotMaxBytes-totalBytes {
			return fmt.Errorf("Workspace snapshot exceeds %d bytes", checkpointSnapshotMaxBytes)
		}
		header := &tar.Header{
			Name: filepath.ToSlash(relative), Size: info.Size(), ModTime: info.ModTime(),
			Mode: int64(0o644), Typeflag: tar.TypeReg,
		}
		executable := info.Mode().Perm()&0o111 != 0
		if executable {
			header.Mode = 0o755
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(writer, hash), file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		files = append(files, checkpointManifestFile{
			Path: filepath.ToSlash(relative), Size: info.Size(),
			SHA256: hex.EncodeToString(hash.Sum(nil)), Executable: executable,
		})
		totalBytes += info.Size()
		return nil
	})
	if err != nil {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf("capture Workspace snapshot: %w", err)
	}
	if err := writer.Close(); err != nil {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf("finalize Workspace snapshot: %w", err)
	}
	if err := archive.Close(); err != nil {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf("close Workspace snapshot: %w", err)
	}
	fileCount := len(files)
	manifest := map[string]any{
		"format": "synara-workspace-snapshot-v1", "files": files,
		"excluded": []string{".git"},
	}
	encodedManifest, err := json.Marshal(manifest)
	if err != nil || len(encodedManifest) > executions.CheckpointManifestMaxBytes {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf(
			"Workspace snapshot manifest exceeds %d bytes", executions.CheckpointManifestMaxBytes,
		)
	}
	candidate = WorkspaceCheckpointCandidate{
		IdempotencyKey: idempotencyKey, Strategy: "snapshot",
		BaseCommit: materialized.BaseCommit, HeadCommit: inspection.HeadCommit,
		CurrentBranch: inspection.CurrentBranch,
		Manifest:      manifest,
		FileCount:     fileCount, TotalBytes: totalBytes,
		Artifact: &RunnerArtifact{
			Path: archivePath, Kind: "workspace_snapshot",
			OriginalName: fmt.Sprintf("workspace-%s-generation-%d.tar", execution.ID, execution.Generation),
			ContentType:  "application/x-tar",
		},
		ArtifactPath: archivePath, Cleanup: cleanup,
	}
	return candidate, nil
}

func validateCheckpointRequest(payload map[string]any) error {
	for key := range payload {
		if key != "idempotencyKey" && key != "reason" && key != "strategyHint" {
			return protocolFailure("Provider Host Checkpoint payload contains an unsupported field")
		}
	}
	if value, found := payload["idempotencyKey"]; found {
		key, ok := value.(string)
		if !ok || strings.TrimSpace(key) == "" || len(strings.TrimSpace(key)) > 200 {
			return protocolFailure("Provider Host Checkpoint idempotencyKey is invalid")
		}
	}
	if value, found := payload["reason"]; found {
		reason, ok := value.(string)
		if !ok || len(strings.TrimSpace(reason)) > 500 {
			return protocolFailure("Provider Host Checkpoint reason is invalid")
		}
	}
	if value, found := payload["strategyHint"]; found {
		strategy, ok := value.(string)
		if !ok || (strategy != "auto" && strategy != "git-reference" && strategy != "patch" && strategy != "snapshot") {
			return protocolFailure("Provider Host Checkpoint strategyHint is invalid")
		}
	}
	return nil
}

func (d *Daemon) persistWorkspaceCheckpoint(
	ctx context.Context,
	execution executions.Execution,
	lease executions.Lease,
	candidate WorkspaceCheckpointCandidate,
) error {
	if candidate.Cleanup != nil {
		defer candidate.Cleanup()
	}
	checkpoint, err := d.client.CreateWorkspaceCheckpoint(ctx, execution.ID, lease, candidate)
	if err != nil {
		return fmt.Errorf("create Workspace Checkpoint: %w", err)
	}
	if checkpoint.Status == "ready" {
		return nil
	}
	if checkpoint.Status == "failed" {
		return workspaceFailure("workspace_invalid", "The Workspace Checkpoint is already failed.", true, true)
	}
	var completedArtifact *artifacts.Artifact
	if candidate.Artifact != nil {
		artifact, uploadErr := d.client.UploadCheckpointArtifact(ctx, execution.ID, lease, checkpoint, candidate)
		if uploadErr != nil {
			failedErr := d.client.MarkWorkspaceCheckpointFailed(ctx, execution.ID, lease, candidate, checkpoint, uploadErr)
			return errors.Join(fmt.Errorf("upload Workspace Checkpoint Artifact: %w", uploadErr), failedErr)
		}
		completedArtifact = &artifact
	}
	if err := d.client.MarkWorkspaceCheckpointReady(
		ctx, execution.ID, lease, candidate, checkpoint, completedArtifact,
	); err != nil {
		failedErr := d.client.MarkWorkspaceCheckpointFailed(ctx, execution.ID, lease, candidate, checkpoint, err)
		return errors.Join(fmt.Errorf("mark Workspace Checkpoint ready: %w", err), failedErr)
	}
	return nil
}

func checkpointMatchesRestored(
	candidate WorkspaceCheckpointCandidate,
	restored *executions.WorkspaceCheckpoint,
) bool {
	if restored == nil || restored.Status != "ready" || candidate.Strategy != restored.Strategy ||
		!sameCheckpointString(candidate.BaseCommit, restored.BaseCommit) ||
		!sameCheckpointString(candidate.CurrentBranch, restored.CurrentBranch) ||
		candidate.FileCount != checkpointInt(restored.FileCount) || candidate.TotalBytes != checkpointInt64(restored.TotalBytes) {
		return false
	}
	if candidate.Strategy != "patch" && !sameCheckpointString(candidate.HeadCommit, restored.HeadCommit) {
		return false
	}
	left, leftErr := json.Marshal(candidate.Manifest)
	right, rightErr := json.Marshal(restored.Manifest)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func sameCheckpointString(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func checkpointInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func checkpointInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
