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
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

type checkpointSnapshotManifest struct {
	Format string                   `json:"format"`
	Files  []checkpointManifestFile `json:"files"`
}

func (m *WorkspaceMaterializer) Restore(
	ctx context.Context,
	materialized WorkspaceMaterialization,
	checkpoint executions.WorkspaceCheckpoint,
	artifactPath string,
) (WorkspaceMaterialization, error) {
	if !materialized.Managed {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "An unmanaged Workspace cannot consume a remote Checkpoint.", true, false,
		)
	}
	switch checkpoint.Strategy {
	case "git-reference":
		return m.restoreGitReference(ctx, materialized, checkpoint)
	case "snapshot":
		return m.restoreSnapshot(ctx, materialized, checkpoint, artifactPath)
	case "patch":
		return m.restorePatch(ctx, materialized, checkpoint, artifactPath)
	default:
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Workspace Checkpoint strategy is unsupported.", true, false,
		)
	}
}

func (m *WorkspaceMaterializer) restoreGitReference(
	ctx context.Context,
	materialized WorkspaceMaterialization,
	checkpoint executions.WorkspaceCheckpoint,
) (WorkspaceMaterialization, error) {
	if checkpoint.HeadCommit == nil || checkpoint.CurrentBranch == nil || checkpoint.ArtifactID != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git-reference Checkpoint is incomplete.", true, false,
		)
	}
	head := strings.TrimSpace(*checkpoint.HeadCommit)
	branch, err := gitpolicy.NormalizeBranch(strings.TrimSpace(*checkpoint.CurrentBranch), "")
	if err != nil || !validGitObjectID(head) {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git-reference Checkpoint metadata is invalid.", true, false,
		)
	}
	environment := gitEnvironment(nil)
	if err := m.rejectDangerousLocalGitConfig(ctx, materialized.Directory, environment); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git checkout contains unsafe local configuration.", true, false,
		)
	}
	if _, err := m.runGit(ctx, materialized.Directory, environment, "cat-file", "-e", head+"^{commit}"); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git-reference Checkpoint commit is not available from the persisted repository.", true, true,
		)
	}
	if _, err := m.runGit(ctx, materialized.Directory, environment, "switch", "-C", branch, head); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git-reference Checkpoint branch could not be restored.", true, true,
		)
	}
	if _, err := m.runGit(ctx, materialized.Directory, environment, "reset", "--hard", head); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git-reference Checkpoint could not reset the checkout.", true, true,
		)
	}
	if _, err := m.runGit(ctx, materialized.Directory, environment, "clean", "-fdx"); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git-reference Checkpoint could not clean the checkout.", true, true,
		)
	}
	inspection, err := m.Inspect(ctx, materialized)
	if err != nil || inspection.Dirty || inspection.HeadCommit == nil || *inspection.HeadCommit != head {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git-reference Checkpoint restore did not reproduce the expected checkout.", true, true,
		)
	}
	materialized.CurrentBranch = inspection.CurrentBranch
	materialized.HeadCommit = inspection.HeadCommit
	materialized.RestoredCheckpointID = &checkpoint.ID
	if checkpoint.BaseCommit != nil {
		materialized.BaseCommit = checkpoint.BaseCommit
	}
	return materialized, nil
}

func (m *WorkspaceMaterializer) restoreSnapshot(
	ctx context.Context,
	materialized WorkspaceMaterialization,
	checkpoint executions.WorkspaceCheckpoint,
	artifactPath string,
) (WorkspaceMaterialization, error) {
	if strings.TrimSpace(artifactPath) == "" || checkpoint.ArtifactID == nil || checkpoint.SHA256 == nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Snapshot Checkpoint Artifact is incomplete.", true, false,
		)
	}
	manifest, err := decodeSnapshotManifest(checkpoint.Manifest)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Snapshot Checkpoint manifest is invalid.", true, false,
		)
	}
	staging, err := os.MkdirTemp(filepath.Dir(materialized.Directory), ".synara-restore-*")
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Snapshot Checkpoint staging directory could not be created.", true, true,
		)
	}
	defer os.RemoveAll(staging)
	if err := extractSnapshotArchive(artifactPath, staging, manifest); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Snapshot Checkpoint failed archive verification.", true, false,
		)
	}
	if err := clearWorkspaceContent(materialized.Directory); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Workspace could not be cleared for Snapshot restore.", true, true,
		)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Snapshot Checkpoint staging directory is unavailable.", true, true,
		)
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(staging, entry.Name()), filepath.Join(materialized.Directory, entry.Name())); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Snapshot Checkpoint could not be installed into the Workspace.", true, true,
			)
		}
	}
	inspection, err := m.Inspect(ctx, materialized)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The restored Snapshot Workspace could not be inspected.", true, true,
		)
	}
	materialized.CurrentBranch = inspection.CurrentBranch
	materialized.HeadCommit = inspection.HeadCommit
	materialized.RestoredCheckpointID = &checkpoint.ID
	if checkpoint.BaseCommit != nil {
		materialized.BaseCommit = checkpoint.BaseCommit
	}
	return materialized, nil
}

func decodeSnapshotManifest(value map[string]any) (checkpointSnapshotManifest, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return checkpointSnapshotManifest{}, err
	}
	var manifest checkpointSnapshotManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return checkpointSnapshotManifest{}, err
	}
	if manifest.Format != "synara-workspace-snapshot-v1" || len(manifest.Files) > checkpointSnapshotMaxFiles {
		return checkpointSnapshotManifest{}, errors.New("unsupported Snapshot manifest")
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	var total int64
	for _, file := range manifest.Files {
		clean, err := cleanArchivePath(file.Path)
		if err != nil || clean != file.Path || file.Size < 0 || file.Size > checkpointSnapshotMaxBytes-total ||
			len(file.SHA256) != 64 || !validGitObjectID(file.SHA256) {
			return checkpointSnapshotManifest{}, errors.New("invalid Snapshot manifest entry")
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return checkpointSnapshotManifest{}, errors.New("duplicate Snapshot manifest entry")
		}
		seen[file.Path] = struct{}{}
		total += file.Size
	}
	return manifest, nil
}

func extractSnapshotArchive(
	archivePath, destination string,
	manifest checkpointSnapshotManifest,
) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	expected := make(map[string]checkpointManifestFile, len(manifest.Files))
	for _, file := range manifest.Files {
		expected[file.Path] = file
	}
	seen := make(map[string]struct{}, len(expected))
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return errors.New("Snapshot archive contains a non-regular entry")
		}
		clean, err := cleanArchivePath(header.Name)
		if err != nil || clean != header.Name {
			return errors.New("Snapshot archive path is invalid")
		}
		expectedFile, found := expected[clean]
		if !found || header.Size != expectedFile.Size {
			return errors.New("Snapshot archive does not match its manifest")
		}
		if _, duplicate := seen[clean]; duplicate {
			return errors.New("Snapshot archive contains a duplicate entry")
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if !pathContainedBy(destination, target) {
			return errors.New("Snapshot archive escaped the staging root")
		}
		if err := ensureSnapshotParent(destination, filepath.Dir(target)); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if expectedFile.Executable {
			mode = 0o700
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(file, hash), reader)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || written != expectedFile.Size ||
			hex.EncodeToString(hash.Sum(nil)) != expectedFile.SHA256 {
			return errors.New("Snapshot archive entry failed size or SHA-256 verification")
		}
		seen[clean] = struct{}{}
	}
	if len(seen) != len(expected) {
		return errors.New("Snapshot archive omitted a manifest entry")
	}
	return nil
}

func cleanArchivePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", errors.New("invalid archive path")
	}
	clean := pathpkg.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("invalid archive path")
	}
	for _, segment := range strings.Split(clean, "/") {
		if strings.EqualFold(segment, ".git") {
			return "", errors.New("invalid archive path")
		}
	}
	return clean, nil
}

func ensureSnapshotParent(root, parent string) error {
	root = filepath.Clean(root)
	parent = filepath.Clean(parent)
	if parent == root {
		return nil
	}
	relative, err := filepath.Rel(root, parent)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Snapshot parent escaped the staging root")
	}
	current := root
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("Snapshot parent contains an invalid segment")
		}
		current = filepath.Join(current, segment)
		if err := ensureRealDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func clearWorkspaceContent(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(directory, entry.Name())); err != nil {
			return fmt.Errorf("remove Workspace entry %q: %w", entry.Name(), err)
		}
	}
	return nil
}
