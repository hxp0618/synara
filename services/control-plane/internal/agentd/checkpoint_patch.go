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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

const (
	checkpointPatchFormat          = "synara-workspace-patch-v1"
	checkpointPatchEntryName       = "tracked.patch"
	checkpointPatchTrackedPrefix   = "tracked/"
	checkpointPatchUntrackedPrefix = "untracked/"
	checkpointPatchIndexPolicy     = "all-tracked-deltas-staged"
	checkpointPatchExcludedGit     = ".git"
	checkpointPatchExcludedIgnored = "rebuildable-ignored-directory-segments-v1"
	checkpointGitOutputMaxBytes    = 16 << 20
	checkpointPathMaxBytes         = 4 << 10
)

var checkpointPatchRebuildableIgnoredSegments = map[string]struct{}{
	".astro": {}, ".bun": {}, ".gradle": {}, ".mypy_cache": {}, ".next": {}, ".nuxt": {},
	".pnpm-store": {}, ".pytest_cache": {}, ".ruff_cache": {}, ".turbo": {}, ".venv": {},
	".vite": {}, ".yarn": {}, "__pycache__": {}, "coverage": {}, "node_modules": {},
	"target": {}, "venv": {},
}

type checkpointPatchPayload struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type checkpointPatchTrackedFile struct {
	Path       string `json:"path"`
	Operation  string `json:"operation"`
	Kind       string `json:"kind,omitempty"`
	SizeBytes  int64  `json:"sizeBytes,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Executable bool   `json:"executable,omitempty"`
}

type checkpointPatchManifest struct {
	Format        string                       `json:"format"`
	BaseCommit    string                       `json:"baseCommit"`
	CurrentBranch string                       `json:"currentBranch"`
	TrackedPatch  checkpointPatchPayload       `json:"trackedPatch"`
	TrackedFiles  []checkpointPatchTrackedFile `json:"trackedFiles"`
	Untracked     []checkpointManifestFile     `json:"untrackedFiles"`
	Excluded      []string                     `json:"excluded"`
	IndexPolicy   string                       `json:"indexPolicy"`
}

type checkpointPatchState struct {
	tracked   []checkpointPatchTrackedFile
	untracked []checkpointManifestFile
	fileCount int
	total     int64
}

type checkpointGitChange struct {
	status string
	path   string
}

type checkpointCommandOutput struct {
	buffer  bytes.Buffer
	maximum int
	total   int
}

func (b *checkpointCommandOutput) Write(value []byte) (int, error) {
	b.total += len(value)
	if b.buffer.Len() < b.maximum {
		remaining := b.maximum - b.buffer.Len()
		_, _ = b.buffer.Write(value[:min(len(value), remaining)])
	}
	return len(value), nil
}

func (b *checkpointCommandOutput) exceeded() bool { return b.total > b.maximum }

type checkpointLimitedWriter struct {
	writer    io.Writer
	remaining int64
	exceeded  bool
}

func (w *checkpointLimitedWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > w.remaining {
		w.exceeded = true
		return 0, errors.New("Checkpoint payload exceeded the safe limit")
	}
	written, err := w.writer.Write(value)
	w.remaining -= int64(written)
	return written, err
}

func workspaceHasGitMetadata(directory string) bool {
	info, err := os.Lstat(filepath.Join(directory, ".git"))
	return err == nil && info.Mode()&os.ModeSymlink == 0 && (info.IsDir() || info.Mode().IsRegular())
}

func captureWorkspacePatch(
	ctx context.Context,
	execution executions.Execution,
	materialized WorkspaceMaterialization,
	inspection WorkspaceInspection,
	idempotencyKey string,
) (candidate WorkspaceCheckpointCandidate, resultErr error) {
	if materialized.BaseCommit == nil || inspection.CurrentBranch == nil || inspection.HeadCommit == nil ||
		!validGitObjectID(strings.TrimSpace(*materialized.BaseCommit)) || !validGitObjectID(strings.TrimSpace(*inspection.HeadCommit)) {
		return WorkspaceCheckpointCandidate{}, errors.New("Git Workspace Patch metadata is incomplete")
	}
	branch, err := gitpolicy.NormalizeBranch(strings.TrimSpace(*inspection.CurrentBranch), "")
	if err != nil {
		return WorkspaceCheckpointCandidate{}, errors.New("Git Workspace Patch branch is invalid")
	}
	baseCommit := strings.TrimSpace(*materialized.BaseCommit)
	if err := validatePatchGitRepository(ctx, materialized.Directory, baseCommit); err != nil {
		return WorkspaceCheckpointCandidate{}, err
	}
	state, err := capturePatchState(ctx, materialized.Directory, baseCommit)
	if err != nil {
		return WorkspaceCheckpointCandidate{}, err
	}
	patchFile, err := os.CreateTemp("", "synara-workspace-tracked-*.patch")
	if err != nil {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf("create tracked Workspace Patch: %w", err)
	}
	patchPath := patchFile.Name()
	defer os.Remove(patchPath)
	patchSize, patchSHA, err := streamTrackedPatch(ctx, materialized.Directory, baseCommit, patchFile)
	closeErr := patchFile.Close()
	if err != nil {
		return WorkspaceCheckpointCandidate{}, err
	}
	if closeErr != nil {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf("close tracked Workspace Patch: %w", closeErr)
	}
	if patchSize > checkpointSnapshotMaxBytes-state.total {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf("Workspace Patch exceeds %d bytes", checkpointSnapshotMaxBytes)
	}
	manifest := checkpointPatchManifest{
		Format: checkpointPatchFormat, BaseCommit: baseCommit, CurrentBranch: branch,
		TrackedPatch: checkpointPatchPayload{Path: checkpointPatchEntryName, SizeBytes: patchSize, SHA256: patchSHA},
		TrackedFiles: state.tracked, Untracked: state.untracked,
		Excluded:    []string{checkpointPatchExcludedGit, checkpointPatchExcludedIgnored},
		IndexPolicy: checkpointPatchIndexPolicy,
	}
	archivePath, cleanup, err := createPatchArchive(ctx, materialized.Directory, patchPath, manifest)
	if err != nil {
		return WorkspaceCheckpointCandidate{}, err
	}
	defer func() {
		if resultErr != nil {
			cleanup()
		}
	}()
	verifiedSize, verifiedSHA, err := streamTrackedPatch(ctx, materialized.Directory, baseCommit, io.Discard)
	if err != nil || verifiedSize != patchSize || verifiedSHA != patchSHA {
		return WorkspaceCheckpointCandidate{}, errors.New("Workspace changed while the tracked Patch was captured")
	}
	verifiedState, err := capturePatchState(ctx, materialized.Directory, baseCommit)
	if err != nil || !samePatchState(state, verifiedState) {
		return WorkspaceCheckpointCandidate{}, errors.New("Workspace changed while the Patch file manifest was captured")
	}
	manifestValue, err := checkpointManifestMap(manifest)
	if err != nil {
		return WorkspaceCheckpointCandidate{}, fmt.Errorf("encode Workspace Patch manifest: %w", err)
	}
	candidate = WorkspaceCheckpointCandidate{
		IdempotencyKey: idempotencyKey, Strategy: "patch",
		BaseCommit: &baseCommit, HeadCommit: inspection.HeadCommit, CurrentBranch: &branch,
		Manifest: manifestValue, FileCount: state.fileCount, TotalBytes: state.total,
		Artifact: &RunnerArtifact{
			Path: archivePath, Kind: "checkpoint",
			OriginalName: fmt.Sprintf("workspace-%s-generation-%d.patch.tar", execution.ID, execution.Generation),
			ContentType:  "application/x-tar",
		},
		ArtifactPath: archivePath, Cleanup: cleanup,
	}
	return candidate, nil
}

func validatePatchGitRepository(ctx context.Context, directory, baseCommit string) error {
	environment := gitEnvironment(nil)
	command := exec.CommandContext(ctx, "git", "cat-file", "-e", baseCommit+"^{commit}")
	command.Dir = directory
	command.Env = environment
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("Workspace Patch base Commit is unavailable")
	}
	config, err := runCheckpointGitOutput(ctx, directory, "config", "--local", "--no-includes", "--null", "--list")
	if err != nil {
		return errors.New("Workspace Patch could not inspect local Git configuration")
	}
	for _, entry := range splitNUL(config) {
		key, value := localGitConfigEntry(entry)
		if dangerousLocalGitConfigEntry(key, value) {
			return errors.New("Workspace Patch rejected unsafe local Git configuration")
		}
	}
	if err := rejectUnsupportedPatchGitMetadata(ctx, directory); err != nil {
		return err
	}
	if err := rejectUnsupportedPatchIndexFlags(ctx, directory); err != nil {
		return err
	}
	unmerged, err := runCheckpointGitOutput(ctx, directory, "ls-files", "--unmerged", "-z")
	if err != nil {
		return errors.New("Workspace Patch could not inspect unmerged files")
	}
	if len(unmerged) != 0 {
		return errors.New("Workspace Patch does not support an unmerged Git index")
	}
	staged, err := runCheckpointGitOutput(ctx, directory, "ls-files", "--stage", "-z")
	if err != nil {
		return errors.New("Workspace Patch could not inspect tracked file modes")
	}
	for _, entry := range splitNUL(staged) {
		metadata := entry
		if tab := strings.IndexByte(metadata, '\t'); tab >= 0 {
			metadata = metadata[:tab]
		}
		fields := strings.Fields(metadata)
		if len(fields) != 3 || fields[2] != "0" {
			return errors.New("Workspace Patch does not support an unmerged Git index")
		}
		if fields[0] == "160000" {
			return errors.New("Workspace Patch does not support Gitlinks or Submodules")
		}
	}
	return nil
}

func rejectUnsupportedPatchGitMetadata(ctx context.Context, directory string) error {
	paths := []struct {
		path       string
		allowEmpty bool
	}{
		{path: filepath.Join("info", "attributes"), allowEmpty: true},
		{path: filepath.Join("info", "grafts"), allowEmpty: true},
		{path: filepath.Join("info", "sparse-checkout")},
		{path: filepath.Join("objects", "info", "alternates")},
		{path: filepath.Join("objects", "info", "http-alternates")},
	}
	for _, candidate := range paths {
		output, err := runCheckpointGitOutput(ctx, directory, "rev-parse", "--git-path", filepath.ToSlash(candidate.path))
		resolved := strings.TrimSpace(string(output))
		if err != nil || resolved == "" {
			return errors.New("Workspace Patch could not resolve repository-local Git metadata")
		}
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(directory, resolved)
		}
		info, err := os.Lstat(filepath.Clean(resolved))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			(!candidate.allowEmpty || info.Size() > 0) {
			return errors.New("Workspace Patch rejected unsupported repository-local Git metadata")
		}
	}
	return nil
}

func rejectUnsupportedPatchIndexFlags(ctx context.Context, directory string) error {
	output, err := runCheckpointGitOutput(ctx, directory, "ls-files", "-v", "-z", "--")
	if err != nil {
		return errors.New("Workspace Patch could not inspect Git index flags")
	}
	for _, entry := range splitNUL(output) {
		if len(entry) < 3 || entry[1] != ' ' {
			return errors.New("Workspace Patch Git index flag output is invalid")
		}
		tag := entry[0]
		if tag == 'S' || (tag >= 'a' && tag <= 'z') {
			return errors.New("Workspace Patch does not support assume-unchanged, skip-worktree or sparse index entries")
		}
	}
	return nil
}

func capturePatchState(ctx context.Context, directory, baseCommit string) (checkpointPatchState, error) {
	changes, err := trackedPatchChanges(ctx, directory, baseCommit)
	if err != nil {
		return checkpointPatchState{}, err
	}
	untrackedPaths, err := untrackedPatchPaths(ctx, directory)
	if err != nil {
		return checkpointPatchState{}, err
	}
	if len(changes)+len(untrackedPaths) > checkpointSnapshotMaxFiles {
		return checkpointPatchState{}, fmt.Errorf("Workspace Patch exceeds %d files", checkpointSnapshotMaxFiles)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return checkpointPatchState{}, fmt.Errorf("open Workspace Patch root: %w", err)
	}
	defer root.Close()
	state := checkpointPatchState{
		tracked:   make([]checkpointPatchTrackedFile, 0, len(changes)),
		untracked: make([]checkpointManifestFile, 0, len(untrackedPaths)),
	}
	trackedOperations := make(map[string]string, len(changes))
	for _, change := range changes {
		entry := checkpointPatchTrackedFile{Path: change.path}
		switch change.status {
		case "D":
			entry.Operation = "delete"
		case "A", "M", "T":
			entry.Operation = "upsert"
			kind, size, digest, executable, inspectErr := inspectPatchPath(ctx, root, change.path, true)
			if inspectErr != nil {
				return checkpointPatchState{}, fmt.Errorf("inspect tracked Workspace Patch path %q: %w", change.path, inspectErr)
			}
			entry.Kind = kind
			entry.SizeBytes = size
			entry.SHA256 = digest
			entry.Executable = executable
			if size > checkpointSnapshotMaxBytes-state.total {
				return checkpointPatchState{}, fmt.Errorf("Workspace Patch exceeds %d bytes", checkpointSnapshotMaxBytes)
			}
			state.total += size
		default:
			return checkpointPatchState{}, fmt.Errorf("Workspace Patch does not support Git status %q", change.status)
		}
		trackedOperations[change.path] = entry.Operation
		state.tracked = append(state.tracked, entry)
	}
	for _, path := range untrackedPaths {
		if operation, exists := trackedOperations[path]; exists && operation != "delete" {
			return checkpointPatchState{}, fmt.Errorf("Workspace Patch path %q is both tracked and untracked", path)
		}
		kind, size, digest, executable, inspectErr := inspectPatchPath(ctx, root, path, false)
		if inspectErr != nil {
			return checkpointPatchState{}, fmt.Errorf("inspect untracked Workspace Patch path %q: %w", path, inspectErr)
		}
		if kind != "regular" {
			return checkpointPatchState{}, fmt.Errorf("Workspace Patch rejected non-regular untracked file %q", path)
		}
		if size > checkpointSnapshotMaxBytes-state.total {
			return checkpointPatchState{}, fmt.Errorf("Workspace Patch exceeds %d bytes", checkpointSnapshotMaxBytes)
		}
		state.total += size
		state.untracked = append(state.untracked, checkpointManifestFile{
			Path: path, Size: size, SHA256: digest, Executable: executable,
		})
	}
	state.fileCount = len(state.tracked) + len(state.untracked)
	return state, nil
}

func trackedPatchChanges(ctx context.Context, directory, baseCommit string) ([]checkpointGitChange, error) {
	output, err := runCheckpointGitOutput(ctx, directory, append([]string{"diff", "--name-status", "-z"}, trackedPatchDiffArguments(baseCommit)...)...)
	if err != nil {
		return nil, errors.New("Workspace Patch could not list tracked changes")
	}
	values := splitNUL(output)
	if len(values)%2 != 0 {
		return nil, errors.New("Workspace Patch tracked path output is invalid")
	}
	changes := make([]checkpointGitChange, 0, len(values)/2)
	seen := make(map[string]struct{}, len(values)/2)
	for index := 0; index < len(values); index += 2 {
		status := values[index]
		path, cleanErr := normalizeCheckpointPath(values[index+1])
		if cleanErr != nil || status == "" || len(status) > 1 {
			return nil, errors.New("Workspace Patch tracked path output is invalid")
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, errors.New("Workspace Patch contains a duplicate tracked path")
		}
		seen[path] = struct{}{}
		changes = append(changes, checkpointGitChange{status: status, path: path})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
	return changes, nil
}

func untrackedPatchPaths(ctx context.Context, directory string) ([]string, error) {
	output, err := runCheckpointGitOutput(ctx, directory, "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return nil, errors.New("Workspace Patch could not list untracked files")
	}
	ignored, err := ignoredPatchFilePaths(ctx, directory)
	if err != nil {
		return nil, err
	}
	values := append(splitNUL(output), ignored...)
	paths := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		path, cleanErr := normalizeCheckpointPath(value)
		if cleanErr != nil {
			return nil, errors.New("Workspace Patch untracked path output is invalid")
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, errors.New("Workspace Patch contains a duplicate untracked path")
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func ignoredPatchFilePaths(ctx context.Context, directory string) ([]string, error) {
	output, err := runCheckpointGitOutput(
		ctx, directory, "status", "--porcelain=v1", "-z", "--ignored=matching", "--untracked-files=all", "--",
	)
	if err != nil {
		return nil, errors.New("Workspace Patch could not list ignored files")
	}
	paths := make([]string, 0)
	seen := make(map[string]struct{})
	appendPath := func(value string) error {
		path, cleanErr := normalizeCheckpointPath(value)
		if cleanErr != nil || path != value {
			return errors.New("Workspace Patch ignored path output is invalid")
		}
		if _, duplicate := seen[path]; duplicate {
			return nil
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
		return nil
	}
	for _, entry := range splitNUL(output) {
		if !strings.HasPrefix(entry, "!! ") {
			continue
		}
		value := strings.TrimPrefix(entry, "!! ")
		if strings.HasSuffix(value, "/") {
			directoryPath := strings.TrimSuffix(value, "/")
			clean, cleanErr := normalizeCheckpointPath(directoryPath)
			if cleanErr != nil || clean != directoryPath {
				return nil, errors.New("Workspace Patch ignored directory output is invalid")
			}
			if patchIgnoredDirectoryRebuildable(clean) {
				continue
			}
			children, childErr := runCheckpointGitOutput(ctx, directory, "ls-files", "--others", "-z", "--", clean+"/")
			if childErr != nil {
				return nil, errors.New("Workspace Patch could not list durable ignored directory files")
			}
			for _, child := range splitNUL(children) {
				if patchIgnoredDirectoryRebuildable(filepath.ToSlash(child)) {
					continue
				}
				if err := appendPath(child); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := appendPath(value); err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func patchIgnoredDirectoryRebuildable(path string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if _, rebuildable := checkpointPatchRebuildableIgnoredSegments[segment]; rebuildable ||
			segment == ".synara" || strings.HasPrefix(segment, ".synara-") || strings.HasPrefix(segment, ".vitest-") {
			return true
		}
	}
	return false
}

func trackedPatchDiffArguments(baseCommit string) []string {
	return []string{
		"--binary", "--full-index", "--no-color", "--unified=3", "--inter-hunk-context=0",
		"--diff-algorithm=myers", "--no-indent-heuristic", "--no-renames", "--no-ext-diff", "--no-textconv",
		"--ita-visible-in-index", "--src-prefix=a/", "--dst-prefix=b/", baseCommit, "--",
	}
}

func runCheckpointGitOutput(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	command.Env = gitEnvironment(nil)
	stdout := &checkpointCommandOutput{maximum: checkpointGitOutputMaxBytes}
	stderr := &boundedBuffer{maximum: 32 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return nil, err
	}
	if stdout.exceeded() {
		return nil, errors.New("Git output exceeded the Workspace Checkpoint limit")
	}
	return append([]byte(nil), stdout.buffer.Bytes()...), nil
}

func splitNUL(value []byte) []string {
	if len(value) == 0 {
		return nil
	}
	parts := bytes.Split(value, []byte{0})
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, string(part))
	}
	return result
}

func normalizeCheckpointPath(value string) (string, error) {
	if len(value) == 0 || len(value) > checkpointPathMaxBytes || !utf8.ValidString(value) {
		return "", errors.New("invalid Checkpoint path length")
	}
	clean, err := cleanArchivePath(filepath.ToSlash(value))
	if err != nil || clean != filepath.ToSlash(value) {
		return "", errors.New("invalid Checkpoint path")
	}
	return clean, nil
}

func inspectPatchPath(
	ctx context.Context,
	root *os.Root,
	path string,
	allowSymlink bool,
) (string, int64, string, bool, error) {
	info, err := root.Lstat(filepath.FromSlash(path))
	if err != nil {
		return "", 0, "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if !allowSymlink {
			return "", 0, "", false, errors.New("symbolic links are not allowed")
		}
		target, err := root.Readlink(filepath.FromSlash(path))
		if err != nil {
			return "", 0, "", false, err
		}
		digest := sha256.Sum256([]byte(target))
		return "symlink", int64(len(target)), hex.EncodeToString(digest[:]), false, nil
	}
	if !info.Mode().IsRegular() {
		return "", 0, "", false, errors.New("non-regular files are not allowed")
	}
	file, err := root.Open(filepath.FromSlash(path))
	if err != nil {
		return "", 0, "", false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != info.Size() {
		return "", 0, "", false, errors.New("file changed while it was inspected")
	}
	hash := sha256.New()
	written, err := copyWithContext(ctx, hash, file)
	if err != nil || written != info.Size() {
		return "", 0, "", false, errors.New("file changed while it was hashed")
	}
	return "regular", info.Size(), hex.EncodeToString(hash.Sum(nil)), info.Mode().Perm()&0o111 != 0, nil
}

func streamTrackedPatch(
	ctx context.Context,
	directory, baseCommit string,
	destination io.Writer,
) (int64, string, error) {
	hash := sha256.New()
	limited := &checkpointLimitedWriter{
		writer: io.MultiWriter(destination, hash), remaining: checkpointSnapshotMaxBytes,
	}
	command := exec.CommandContext(ctx, "git", append([]string{"diff"}, trackedPatchDiffArguments(baseCommit)...)...)
	command.Dir = directory
	command.Env = gitEnvironment(nil)
	command.Stdout = limited
	stderr := &boundedBuffer{maximum: 32 << 10}
	command.Stderr = stderr
	err := command.Run()
	if limited.exceeded {
		return 0, "", fmt.Errorf("Workspace Patch exceeds %d bytes", checkpointSnapshotMaxBytes)
	}
	if err != nil {
		return 0, "", errors.New("Git could not create the tracked Workspace Patch")
	}
	size := checkpointSnapshotMaxBytes - limited.remaining
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func createPatchArchive(
	ctx context.Context,
	directory, patchPath string,
	manifest checkpointPatchManifest,
) (archivePath string, cleanup func(), resultErr error) {
	archive, err := os.CreateTemp("", "synara-workspace-patch-*.tar")
	if err != nil {
		return "", nil, fmt.Errorf("create Workspace Patch archive: %w", err)
	}
	archivePath = archive.Name()
	cleanup = func() { _ = os.Remove(archivePath) }
	defer func() {
		if resultErr != nil {
			_ = archive.Close()
			cleanup()
		}
	}()
	writer := tar.NewWriter(archive)
	if err := writePatchArchiveEntry(ctx, writer, patchPath, checkpointPatchEntryName, manifest.TrackedPatch.SizeBytes, manifest.TrackedPatch.SHA256, false); err != nil {
		return "", nil, fmt.Errorf("archive tracked Workspace Patch: %w", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", nil, fmt.Errorf("open Workspace Patch root: %w", err)
	}
	defer root.Close()
	for _, file := range manifest.TrackedFiles {
		if file.Operation == "delete" {
			continue
		}
		if err := writePatchTrackedEntry(ctx, writer, root, file); err != nil {
			return "", nil, fmt.Errorf("archive tracked Workspace Patch file %q: %w", file.Path, err)
		}
	}
	for _, file := range manifest.Untracked {
		if err := writePatchUntrackedEntry(ctx, writer, root, file); err != nil {
			return "", nil, fmt.Errorf("archive untracked Workspace Patch file %q: %w", file.Path, err)
		}
	}
	if err := writer.Close(); err != nil {
		return "", nil, fmt.Errorf("finalize Workspace Patch archive: %w", err)
	}
	if err := archive.Close(); err != nil {
		return "", nil, fmt.Errorf("close Workspace Patch archive: %w", err)
	}
	info, err := os.Stat(archivePath)
	if err != nil || info.Size() > checkpointSnapshotMaxBytes {
		return "", nil, fmt.Errorf("Workspace Patch archive exceeds %d bytes", checkpointSnapshotMaxBytes)
	}
	return archivePath, cleanup, nil
}

func writePatchTrackedEntry(
	ctx context.Context,
	writer *tar.Writer,
	root *os.Root,
	expected checkpointPatchTrackedFile,
) error {
	archiveName := checkpointPatchTrackedPrefix + expected.Path
	if expected.Kind == "regular" {
		return writePatchWorkspaceRegularEntry(
			ctx, writer, root, expected.Path, archiveName, expected.SizeBytes, expected.SHA256, expected.Executable,
		)
	}
	if expected.Kind != "symlink" {
		return errors.New("tracked Workspace Patch source kind is invalid")
	}
	target, err := root.Readlink(filepath.FromSlash(expected.Path))
	if err != nil {
		return err
	}
	value := []byte(target)
	if int64(len(value)) != expected.SizeBytes {
		return errors.New("tracked Workspace Patch symlink changed")
	}
	digest := sha256.Sum256(value)
	if hex.EncodeToString(digest[:]) != expected.SHA256 {
		return errors.New("tracked Workspace Patch symlink changed")
	}
	return writePatchBytesEntry(writer, archiveName, value, false)
}

func writePatchArchiveEntry(
	ctx context.Context,
	writer *tar.Writer,
	sourcePath, archiveName string,
	size int64,
	digest string,
	executable bool,
) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer file.Close()
	mode := int64(0o644)
	if executable {
		mode = 0o755
	}
	if err := writer.WriteHeader(&tar.Header{
		Name: archiveName, Size: size, Mode: mode, Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX,
	}); err != nil {
		return err
	}
	hash := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(writer, hash), file)
	if err != nil || written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("Workspace Patch archive source changed")
	}
	return nil
}

func writePatchUntrackedEntry(
	ctx context.Context,
	writer *tar.Writer,
	root *os.Root,
	expected checkpointManifestFile,
) error {
	return writePatchWorkspaceRegularEntry(
		ctx, writer, root, expected.Path, checkpointPatchUntrackedPrefix+expected.Path,
		expected.Size, expected.SHA256, expected.Executable,
	)
}

func writePatchWorkspaceRegularEntry(
	ctx context.Context,
	writer *tar.Writer,
	root *os.Root,
	sourcePath, archiveName string,
	size int64,
	digest string,
	executable bool,
) error {
	file, err := root.Open(filepath.FromSlash(sourcePath))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("Workspace Patch source changed")
	}
	mode := int64(0o644)
	if executable {
		mode = 0o755
	}
	if err := writer.WriteHeader(&tar.Header{
		Name: archiveName, Size: size, Mode: mode, Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX,
	}); err != nil {
		return err
	}
	hash := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(writer, hash), file)
	if err != nil || written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("Workspace Patch source changed")
	}
	return nil
}

func writePatchBytesEntry(writer *tar.Writer, archiveName string, value []byte, executable bool) error {
	mode := int64(0o644)
	if executable {
		mode = 0o755
	}
	if err := writer.WriteHeader(&tar.Header{
		Name: archiveName, Size: int64(len(value)), Mode: mode, Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX,
	}); err != nil {
		return err
	}
	_, err := writer.Write(value)
	return err
}

func checkpointManifestMap(value checkpointPatchManifest) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(encoded) > executions.CheckpointManifestMaxBytes {
		return nil, fmt.Errorf("Workspace Checkpoint manifest exceeds %d bytes", executions.CheckpointManifestMaxBytes)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func samePatchState(left, right checkpointPatchState) bool {
	leftJSON, leftErr := json.Marshal(struct {
		Tracked   []checkpointPatchTrackedFile `json:"tracked"`
		Untracked []checkpointManifestFile     `json:"untracked"`
	}{left.tracked, left.untracked})
	rightJSON, rightErr := json.Marshal(struct {
		Tracked   []checkpointPatchTrackedFile `json:"tracked"`
		Untracked []checkpointManifestFile     `json:"untracked"`
	}{right.tracked, right.untracked})
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON) &&
		left.fileCount == right.fileCount && left.total == right.total
}

func (m *WorkspaceMaterializer) restorePatch(
	ctx context.Context,
	materialized WorkspaceMaterialization,
	checkpoint executions.WorkspaceCheckpoint,
	artifactPath string,
) (WorkspaceMaterialization, error) {
	if strings.TrimSpace(artifactPath) == "" || checkpoint.ArtifactID == nil || checkpoint.SHA256 == nil ||
		!workspaceHasGitMetadata(materialized.Directory) {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint Artifact or Git Workspace is incomplete.", true, false,
		)
	}
	manifest, err := decodePatchManifest(checkpoint)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint manifest is invalid.", true, false,
		)
	}
	environment := gitEnvironment(nil)
	if err := m.rejectDangerousLocalGitConfig(ctx, materialized.Directory, environment); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git checkout contains unsafe local configuration.", true, false,
		)
	}
	if _, err := m.runGit(ctx, materialized.Directory, environment, "cat-file", "-e", manifest.BaseCommit+"^{commit}"); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint base Commit is unavailable.", true, true,
		)
	}
	originURL, err := m.runGit(ctx, materialized.Directory, environment, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(originURL) == "" {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git checkout origin is unavailable for Patch restore.", true, false,
		)
	}
	parent := filepath.Dir(materialized.Directory)
	if materialized.LogicalRoot != "" {
		parent = filepath.Dir(materialized.LogicalRoot)
	}
	payloadDirectory, err := os.MkdirTemp(parent, ".synara-patch-payload-*")
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint payload staging directory could not be created.", true, true,
		)
	}
	defer os.RemoveAll(payloadDirectory)
	patchPath, err := extractPatchArchive(artifactPath, payloadDirectory, manifest)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint failed archive verification.", true, false,
		)
	}
	if materialized.LogicalRoot != "" {
		if materialized.GitDir == "" || materialized.cache.RepoGit == "" || materialized.cache.LockPath == "" {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The private Git Workspace recovery metadata is incomplete.", true, false,
			)
		}
		stagingRoot, err := createWorkspaceStagingRoot(materialized.LogicalRoot)
		if err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Patch Checkpoint Workspace generation could not be staged.", false, true,
			)
		}
		defer os.RemoveAll(stagingRoot)
		if err := m.withCacheReadLock(ctx, materialized.cache, materialized.manifest.RepositoryURL, func(cacheRepository string) error {
			return m.buildPrivateGitGeneration(
				ctx, stagingRoot, materialized.manifest, cacheRepository, manifest.CurrentBranch, manifest.BaseCommit,
			)
		}); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Patch Checkpoint base Commit is unavailable from the validated cache.", true, true,
			)
		}
		stagingCheckout := filepath.Join(stagingRoot, "checkout")
		branch, head, err := m.applyPatchToCheckout(ctx, stagingCheckout, payloadDirectory, patchPath, manifest)
		if err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Patch Checkpoint failed isolated Workspace verification.", true, false,
			)
		}
		stagingLayout := workspaceLayout{
			Root: stagingRoot, Checkout: stagingCheckout, GitDir: filepath.Join(stagingRoot, "repo.git"),
			Manifest: filepath.Join(stagingRoot, "manifest.json"),
		}
		if err := m.validatePrivateGitGeneration(ctx, stagingLayout, materialized.manifest); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Patch Checkpoint private Git generation is invalid.", true, false,
			)
		}
		if err := replaceWorkspaceGeneration(materialized.LogicalRoot, stagingRoot); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The verified Patch Workspace generation could not replace the active generation.", false, true,
			)
		}
		materialized.CurrentBranch = &branch
		materialized.BaseCommit = &head
		materialized.HeadCommit = &head
		materialized.RestoredCheckpointID = &checkpoint.ID
		return materialized, nil
	}
	staging, err := os.MkdirTemp(parent, ".synara-patch-restore-*")
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint Workspace staging directory could not be created.", true, true,
		)
	}
	defer os.RemoveAll(staging)
	if _, err := m.runGit(ctx, parent, environment,
		"-c", "protocol.file.allow=always", "clone", "--no-local", "--no-hardlinks", "--no-checkout", "--", materialized.Directory, staging,
	); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint staging repository could not be created.", true, true,
		)
	}
	if _, err := m.runGit(ctx, staging, environment, "remote", "set-url", "origin", strings.TrimSpace(originURL)); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint staging origin could not be secured.", true, true,
		)
	}
	if err := m.rejectDangerousLocalGitConfig(ctx, staging, environment); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint staging repository contains unsafe configuration.", true, false,
		)
	}
	branch, head, err := m.applyPatchToCheckout(ctx, staging, payloadDirectory, patchPath, manifest)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Patch Checkpoint failed isolated Workspace verification.", true, false,
		)
	}
	if err := replaceWorkspaceDirectory(materialized.Directory, staging); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The verified Patch Workspace could not replace the active checkout.", true, true,
		)
	}
	materialized.CurrentBranch = &branch
	materialized.BaseCommit = &head
	materialized.HeadCommit = &head
	materialized.RestoredCheckpointID = &checkpoint.ID
	return materialized, nil
}

func (m *WorkspaceMaterializer) applyPatchToCheckout(
	ctx context.Context,
	staging, payloadDirectory, patchPath string,
	manifest checkpointPatchManifest,
) (string, string, error) {
	environment := gitEnvironment(nil)
	if _, err := m.runGit(ctx, staging, environment, "switch", "-C", manifest.CurrentBranch, manifest.BaseCommit); err != nil {
		return "", "", errors.New("The Patch Checkpoint base branch could not be prepared")
	}
	if manifest.TrackedPatch.SizeBytes > 0 {
		applyArguments := []string{"apply", "--index", "--binary", "--whitespace=nowarn", "--", patchPath}
		checkArguments := append([]string{"apply", "--check", "--index", "--binary", "--whitespace=nowarn", "--"}, patchPath)
		if _, err := m.runGit(ctx, staging, environment, checkArguments...); err != nil {
			return "", "", errors.New("The tracked Patch cannot be applied to its base Commit")
		}
		if _, err := m.runGit(ctx, staging, environment, applyArguments...); err != nil {
			return "", "", errors.New("The tracked Patch could not be applied in staging")
		}
	}
	if err := installPatchTrackedFiles(payloadDirectory, staging, manifest.TrackedFiles); err != nil {
		return "", "", errors.New("The Patch Checkpoint tracked files could not be installed in staging")
	}
	if _, err := m.runGit(ctx, staging, environment, "diff", "--quiet", "--"); err != nil {
		return "", "", errors.New("The Patch Checkpoint tracked file bytes do not match the staged Patch")
	}
	if err := installPatchUntrackedFiles(payloadDirectory, staging, manifest.Untracked); err != nil {
		return "", "", errors.New("The Patch Checkpoint untracked files could not be installed in staging")
	}
	stagedState, err := capturePatchState(ctx, staging, manifest.BaseCommit)
	if err != nil {
		return "", "", errors.New("The restored Patch Workspace manifest could not be verified")
	}
	expectedState := checkpointPatchState{
		tracked: manifest.TrackedFiles, untracked: manifest.Untracked,
		fileCount: len(manifest.TrackedFiles) + len(manifest.Untracked),
	}
	for _, file := range manifest.TrackedFiles {
		if file.Operation != "delete" {
			expectedState.total += file.SizeBytes
		}
	}
	for _, file := range manifest.Untracked {
		expectedState.total += file.Size
	}
	if !samePatchState(stagedState, expectedState) {
		return "", "", errors.New("The restored Patch Workspace does not match its file manifest")
	}
	patchSize, patchSHA, err := streamTrackedPatch(ctx, staging, manifest.BaseCommit, io.Discard)
	if err != nil || patchSize != manifest.TrackedPatch.SizeBytes || patchSHA != manifest.TrackedPatch.SHA256 {
		return "", "", errors.New("The restored tracked Patch does not match its content identity")
	}
	branch, err := m.runGit(ctx, staging, environment, "branch", "--show-current")
	if err != nil || branch != manifest.CurrentBranch {
		return "", "", errors.New("The restored Patch Workspace branch is invalid")
	}
	head, err := m.runGit(ctx, staging, environment, "rev-parse", "HEAD")
	if err != nil || head != manifest.BaseCommit {
		return "", "", errors.New("The restored Patch Workspace is not anchored to its base Commit")
	}
	return branch, head, nil
}

func installPatchTrackedFiles(
	payloadDirectory, staging string,
	files []checkpointPatchTrackedFile,
) error {
	sourceRoot, err := os.OpenRoot(payloadDirectory)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	destinationRoot, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	defer destinationRoot.Close()
	for _, expected := range files {
		if expected.Operation == "delete" {
			continue
		}
		if expected.Kind == "regular" {
			if err := installPatchRegularPayload(
				sourceRoot, destinationRoot, staging,
				checkpointPatchTrackedPrefix+expected.Path, expected.Path,
				expected.SizeBytes, expected.SHA256, expected.Executable, true,
			); err != nil {
				return err
			}
			continue
		}
		if expected.Kind != "symlink" {
			return errors.New("tracked Patch payload kind is invalid")
		}
		source, err := sourceRoot.Open(filepath.FromSlash(checkpointPatchTrackedPrefix + expected.Path))
		if err != nil {
			return err
		}
		value, readErr := io.ReadAll(io.LimitReader(source, expected.SizeBytes+1))
		closeErr := source.Close()
		if readErr != nil || closeErr != nil || int64(len(value)) != expected.SizeBytes {
			return errors.New("tracked Patch symlink payload changed")
		}
		digest := sha256.Sum256(value)
		if hex.EncodeToString(digest[:]) != expected.SHA256 {
			return errors.New("tracked Patch symlink payload changed")
		}
		targetPath := filepath.Join(staging, filepath.FromSlash(expected.Path))
		if err := preparePatchInstallTarget(staging, targetPath, true); err != nil {
			return err
		}
		if err := os.Symlink(string(value), targetPath); err != nil {
			return err
		}
	}
	return nil
}

func decodePatchManifest(checkpoint executions.WorkspaceCheckpoint) (checkpointPatchManifest, error) {
	if checkpoint.BaseCommit == nil || checkpoint.CurrentBranch == nil || checkpoint.FileCount == nil || checkpoint.TotalBytes == nil {
		return checkpointPatchManifest{}, errors.New("incomplete Patch Checkpoint metadata")
	}
	encoded, err := json.Marshal(checkpoint.Manifest)
	if err != nil {
		return checkpointPatchManifest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest checkpointPatchManifest
	if err := decoder.Decode(&manifest); err != nil {
		return checkpointPatchManifest{}, err
	}
	baseCommit := strings.TrimSpace(*checkpoint.BaseCommit)
	branch, err := gitpolicy.NormalizeBranch(strings.TrimSpace(*checkpoint.CurrentBranch), "")
	if err != nil || manifest.Format != checkpointPatchFormat || manifest.BaseCommit != baseCommit ||
		manifest.CurrentBranch != branch || !validGitObjectID(baseCommit) ||
		manifest.TrackedPatch.Path != checkpointPatchEntryName || manifest.TrackedPatch.SizeBytes < 0 ||
		manifest.TrackedPatch.SizeBytes > checkpointSnapshotMaxBytes || !validSHA256(manifest.TrackedPatch.SHA256) ||
		manifest.IndexPolicy != checkpointPatchIndexPolicy || len(manifest.Excluded) != 2 ||
		manifest.Excluded[0] != checkpointPatchExcludedGit || manifest.Excluded[1] != checkpointPatchExcludedIgnored {
		return checkpointPatchManifest{}, errors.New("invalid Patch Checkpoint manifest metadata")
	}
	if len(manifest.TrackedFiles)+len(manifest.Untracked) != *checkpoint.FileCount ||
		*checkpoint.FileCount > checkpointSnapshotMaxFiles {
		return checkpointPatchManifest{}, errors.New("invalid Patch Checkpoint file count")
	}
	seenTracked := make(map[string]string, len(manifest.TrackedFiles))
	var totalBytes int64
	previous := ""
	for _, file := range manifest.TrackedFiles {
		path, pathErr := normalizeCheckpointPath(file.Path)
		if pathErr != nil || path != file.Path || (previous != "" && previous >= path) {
			return checkpointPatchManifest{}, errors.New("invalid or unsorted tracked Patch path")
		}
		previous = path
		if file.Operation == "delete" {
			if file.Kind != "" || file.SizeBytes != 0 || file.SHA256 != "" || file.Executable {
				return checkpointPatchManifest{}, errors.New("invalid deleted tracked Patch entry")
			}
		} else if file.Operation == "upsert" {
			if (file.Kind != "regular" && file.Kind != "symlink") || file.SizeBytes < 0 ||
				!validSHA256(file.SHA256) || (file.Kind == "symlink" && file.Executable) ||
				file.SizeBytes > checkpointSnapshotMaxBytes-totalBytes {
				return checkpointPatchManifest{}, errors.New("invalid tracked Patch entry")
			}
			totalBytes += file.SizeBytes
		} else {
			return checkpointPatchManifest{}, errors.New("invalid tracked Patch operation")
		}
		seenTracked[path] = file.Operation
	}
	previous = ""
	for _, file := range manifest.Untracked {
		path, pathErr := normalizeCheckpointPath(file.Path)
		if pathErr != nil || path != file.Path || (previous != "" && previous >= path) || file.Size < 0 ||
			!validSHA256(file.SHA256) || file.Size > checkpointSnapshotMaxBytes-totalBytes {
			return checkpointPatchManifest{}, errors.New("invalid or unsorted untracked Patch entry")
		}
		previous = path
		if operation, collision := seenTracked[path]; collision && operation != "delete" {
			return checkpointPatchManifest{}, errors.New("tracked and untracked Patch path collision")
		}
		totalBytes += file.Size
	}
	if totalBytes != *checkpoint.TotalBytes || manifest.TrackedPatch.SizeBytes > checkpointSnapshotMaxBytes-totalBytes {
		return checkpointPatchManifest{}, errors.New("invalid Patch Checkpoint byte count")
	}
	return manifest, nil
}

func validSHA256(value string) bool {
	return len(value) == 64 && validGitObjectID(value)
}

func extractPatchArchive(
	archivePath, destination string,
	manifest checkpointPatchManifest,
) (string, error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return "", err
	}
	defer root.Close()
	expectedTracked := make(map[string]checkpointPatchTrackedFile)
	for _, file := range manifest.TrackedFiles {
		if file.Operation == "upsert" {
			expectedTracked[file.Path] = file
		}
	}
	expectedUntracked := make(map[string]checkpointManifestFile, len(manifest.Untracked))
	for _, file := range manifest.Untracked {
		expectedUntracked[file.Path] = file
	}
	seenTracked := make(map[string]struct{}, len(expectedTracked))
	seenUntracked := make(map[string]struct{}, len(expectedUntracked))
	patchSeen := false
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) {
			return "", errors.New("Patch archive contains an invalid entry")
		}
		if header.Name == checkpointPatchEntryName {
			if patchSeen || header.Size != manifest.TrackedPatch.SizeBytes {
				return "", errors.New("Patch archive tracked Patch entry is invalid")
			}
			patchSeen = true
			if err := writeVerifiedPatchArchiveFile(
				root, destination, checkpointPatchEntryName, reader,
				manifest.TrackedPatch.SizeBytes, manifest.TrackedPatch.SHA256, false,
			); err != nil {
				return "", err
			}
			continue
		}
		if strings.HasPrefix(header.Name, checkpointPatchTrackedPrefix) {
			path := strings.TrimPrefix(header.Name, checkpointPatchTrackedPrefix)
			clean, cleanErr := normalizeCheckpointPath(path)
			expected, found := expectedTracked[clean]
			if cleanErr != nil || clean != path || !found || header.Size != expected.SizeBytes {
				return "", errors.New("Patch archive tracked file entry is invalid")
			}
			if _, duplicate := seenTracked[path]; duplicate {
				return "", errors.New("Patch archive contains a duplicate tracked file entry")
			}
			seenTracked[path] = struct{}{}
			if err := writeVerifiedPatchArchiveFile(
				root, destination, checkpointPatchTrackedPrefix+path, reader,
				expected.SizeBytes, expected.SHA256, expected.Executable && expected.Kind == "regular",
			); err != nil {
				return "", err
			}
			continue
		}
		if !strings.HasPrefix(header.Name, checkpointPatchUntrackedPrefix) {
			return "", errors.New("Patch archive contains an unexpected entry")
		}
		path := strings.TrimPrefix(header.Name, checkpointPatchUntrackedPrefix)
		clean, cleanErr := normalizeCheckpointPath(path)
		expected, found := expectedUntracked[clean]
		if cleanErr != nil || clean != path || !found || header.Size != expected.Size {
			return "", errors.New("Patch archive untracked entry is invalid")
		}
		if _, duplicate := seenUntracked[path]; duplicate {
			return "", errors.New("Patch archive contains a duplicate untracked entry")
		}
		seenUntracked[path] = struct{}{}
		if err := writeVerifiedPatchArchiveFile(
			root, destination, checkpointPatchUntrackedPrefix+path, reader,
			expected.Size, expected.SHA256, expected.Executable,
		); err != nil {
			return "", err
		}
	}
	if !patchSeen || len(seenTracked) != len(expectedTracked) || len(seenUntracked) != len(expectedUntracked) {
		return "", errors.New("Patch archive omitted a manifest entry")
	}
	return filepath.Join(destination, checkpointPatchEntryName), nil
}

func writeVerifiedPatchArchiveFile(
	root *os.Root,
	rootPath, relative string,
	reader io.Reader,
	size int64,
	digest string,
	executable bool,
) error {
	target := filepath.Join(rootPath, filepath.FromSlash(relative))
	if !pathContainedBy(rootPath, target) {
		return errors.New("Patch archive escaped the payload staging root")
	}
	if err := ensureSnapshotParent(rootPath, filepath.Dir(target)); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if executable {
		mode = 0o700
	}
	file, err := root.OpenFile(filepath.FromSlash(relative), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), reader)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("Patch archive entry failed size or SHA-256 verification")
	}
	return nil
}

func installPatchUntrackedFiles(payloadDirectory, staging string, files []checkpointManifestFile) error {
	sourceRoot, err := os.OpenRoot(payloadDirectory)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	destinationRoot, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	defer destinationRoot.Close()
	for _, expected := range files {
		if err := installPatchRegularPayload(
			sourceRoot, destinationRoot, staging,
			checkpointPatchUntrackedPrefix+expected.Path, expected.Path,
			expected.Size, expected.SHA256, expected.Executable, false,
		); err != nil {
			return err
		}
	}
	return nil
}

func installPatchRegularPayload(
	sourceRoot, destinationRoot *os.Root,
	staging, sourcePath, targetRelative string,
	size int64,
	digest string,
	executable, replace bool,
) error {
	source, err := sourceRoot.Open(filepath.FromSlash(sourcePath))
	if err != nil {
		return err
	}
	targetPath := filepath.Join(staging, filepath.FromSlash(targetRelative))
	if err := preparePatchInstallTarget(staging, targetPath, replace); err != nil {
		_ = source.Close()
		return err
	}
	mode := os.FileMode(0o600)
	if executable {
		mode = 0o700
	}
	target, err := destinationRoot.OpenFile(filepath.FromSlash(targetRelative), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		_ = source.Close()
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(target, hash), source)
	sourceCloseErr := source.Close()
	targetCloseErr := target.Close()
	if copyErr != nil || sourceCloseErr != nil || targetCloseErr != nil || written != size ||
		hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("Patch file failed final verification")
	}
	return nil
}

func preparePatchInstallTarget(staging, targetPath string, replace bool) error {
	if !pathContainedBy(staging, targetPath) {
		return errors.New("Patch file escaped the staging root")
	}
	if err := ensureSnapshotParent(staging, filepath.Dir(targetPath)); err != nil {
		return err
	}
	info, err := os.Lstat(targetPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !replace || (info.IsDir() && info.Mode()&os.ModeSymlink == 0) {
		return errors.New("Patch file target already exists")
	}
	return os.Remove(targetPath)
}

func replaceWorkspaceDirectory(workspace, staging string) error {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	staging, err = filepath.Abs(staging)
	if err != nil {
		return err
	}
	parent := filepath.Dir(workspace)
	if filepath.Dir(staging) != parent || workspace == parent || staging == parent {
		return errors.New("Workspace Patch staging is not a safe sibling")
	}
	backup, err := os.MkdirTemp(parent, ".synara-workspace-backup-*")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(workspace, backup); err != nil {
		return err
	}
	if err := os.Rename(staging, workspace); err != nil {
		rollbackErr := os.Rename(backup, workspace)
		return errors.Join(err, rollbackErr)
	}
	_ = os.RemoveAll(backup)
	return nil
}
