package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

const (
	// workspaceLayoutVersion is retained as the legacy v2 Manifest format so
	// existing on-disk generations and focused tests remain explicit. New
	// materializations use workspaceLayoutV3Format.
	workspaceLayoutVersion    = "synara-workspace-layout-v2"
	workspaceLayoutV3Format   = "synara-workspace-layout-v3"
	workspaceLayoutV2         = 2
	workspaceLayoutV3         = 3
	workspaceManifestMaxSize  = 32 << 10
	workspaceProviderStateDir = ".provider-state"
)

type workspaceLayout struct {
	Root                string
	Checkout            string
	GitDir              string
	Manifest            string
	LockPath            string
	LegacyRoot          string
	TargetID            uuid.UUID
	LogicalID           uuid.UUID
	MaterializationID   uuid.UUID
	IncarnationID       uuid.UUID
	LayoutVersion       int
	AllowLegacyAdoption bool
}

type workspaceGenerationManifest struct {
	Format                string `json:"format"`
	ExecutionTargetID     string `json:"executionTargetId"`
	TenantID              string `json:"tenantId"`
	ProjectID             string `json:"projectId"`
	SessionID             string `json:"sessionId"`
	LogicalWorkspaceID    string `json:"logicalWorkspaceId"`
	MaterializationID     string `json:"materializationId,omitempty"`
	IncarnationID         string `json:"incarnationId,omitempty"`
	LayoutVersion         int    `json:"layoutVersion,omitempty"`
	Managed               bool   `json:"managed"`
	RepositoryFingerprint string `json:"repositoryFingerprint,omitempty"`
	RepositoryURL         string `json:"repositoryUrl,omitempty"`
	DefaultBranch         string `json:"defaultBranch,omitempty"`
}

func (m *WorkspaceMaterializer) resolveWorkspaceLayout(
	execution executions.Execution,
	workload executions.Workload,
) (workspaceLayout, error) {
	workspaceRoot, cacheRoot, err := validateWorkspaceRoots(m.root, m.cacheRoot)
	if err != nil {
		return workspaceLayout{}, err
	}
	m.root = workspaceRoot
	m.cacheRoot = cacheRoot
	targetID := m.targetID
	if targetID == uuid.Nil {
		targetID = execution.ExecutionTargetID
	}
	if m.targetID != uuid.Nil && execution.ExecutionTargetID != uuid.Nil && m.targetID != execution.ExecutionTargetID {
		return workspaceLayout{}, errors.New("Execution Target does not match this Worker")
	}
	for name, value := range map[string]uuid.UUID{
		"Tenant": workload.TenantID, "Project": workload.ProjectID, "Session": workload.SessionID,
	} {
		if value == uuid.Nil {
			return workspaceLayout{}, fmt.Errorf("%s ID is missing from the Workspace workload", name)
		}
	}
	logicalID := execution.ID
	if workload.RemoteWorkspaceID != nil {
		logicalID = *workload.RemoteWorkspaceID
	}
	if logicalID == uuid.Nil {
		return workspaceLayout{}, errors.New("logical Workspace ID is missing")
	}
	logicalSegments := []string{
		targetID.String(), workload.TenantID.String(), workload.ProjectID.String(), workload.SessionID.String(), logicalID.String(),
	}
	layoutVersion := workload.WorkspaceLayoutVersion
	materializationID := uuid.Nil
	incarnationID := uuid.Nil
	legacyCompatibility := layoutVersion == 0 && workload.WorkspaceMaterializationID == nil &&
		workload.WorkspaceMaterializationIncarnationID == nil
	if legacyCompatibility {
		layoutVersion = workspaceLayoutV2
	} else {
		if workload.WorkspaceMaterializationID == nil || *workload.WorkspaceMaterializationID == uuid.Nil {
			return workspaceLayout{}, errors.New("Workspace materialization ID is missing")
		}
		if workload.WorkspaceMaterializationIncarnationID == nil || *workload.WorkspaceMaterializationIncarnationID == uuid.Nil {
			return workspaceLayout{}, errors.New("Workspace materialization incarnation ID is missing")
		}
		materializationID = *workload.WorkspaceMaterializationID
		incarnationID = *workload.WorkspaceMaterializationIncarnationID
	}
	pathSegments := append([]string(nil), logicalSegments...)
	lockNamespace := "workspace-v2"
	switch layoutVersion {
	case workspaceLayoutV2:
		// Explicit layout v2 claims are existing generations being adopted.
	case workspaceLayoutV3:
		if legacyCompatibility {
			return workspaceLayout{}, errors.New("Workspace layout v3 requires a materialization incarnation")
		}
		pathSegments = append(pathSegments, incarnationID.String())
		lockNamespace = "workspace-v3"
	default:
		return workspaceLayout{}, errors.New("Workspace layout version is unsupported")
	}
	root := filepath.Join(append([]string{workspaceRoot, fmt.Sprintf("v%d", layoutVersion)}, pathSegments...)...)
	if !pathContainedBy(workspaceRoot, root) || root == workspaceRoot {
		return workspaceLayout{}, errors.New("Workspace path escapes the configured root")
	}
	if err := ensureContainedDirectory(workspaceRoot, filepath.Dir(root)); err != nil {
		return workspaceLayout{}, err
	}
	lockPath, err := lockPathFor(workspaceRoot, lockNamespace, logicalSegments...)
	if err != nil {
		return workspaceLayout{}, err
	}
	legacyID := execution.ID
	if workload.RemoteWorkspaceID != nil {
		legacyID = *workload.RemoteWorkspaceID
	}
	legacyRoot := filepath.Join(
		workspaceRoot, workload.TenantID.String(), workload.ProjectID.String(), workload.SessionID.String(), legacyID.String(),
	)
	return workspaceLayout{
		Root: root, Checkout: filepath.Join(root, "checkout"), GitDir: filepath.Join(root, "repo.git"),
		Manifest: filepath.Join(root, "manifest.json"), LockPath: lockPath, LegacyRoot: legacyRoot,
		TargetID: targetID, LogicalID: logicalID, MaterializationID: materializationID,
		IncarnationID: incarnationID, LayoutVersion: layoutVersion,
		AllowLegacyAdoption: !legacyCompatibility && layoutVersion == workspaceLayoutV2,
	}, nil
}

func validateWorkspaceRoots(workspaceRoot, cacheRoot string) (string, string, error) {
	workspaceRoot, err := filepath.Abs(strings.TrimSpace(workspaceRoot))
	if err != nil || strings.TrimSpace(workspaceRoot) == "" {
		return "", "", errors.New("Workspace root is invalid")
	}
	cacheRoot, err = filepath.Abs(strings.TrimSpace(cacheRoot))
	if err != nil || strings.TrimSpace(cacheRoot) == "" {
		return "", "", errors.New("Git cache root is invalid")
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	cacheRoot = filepath.Clean(cacheRoot)
	if dangerousManagedRoot(workspaceRoot) || dangerousManagedRoot(cacheRoot) {
		return "", "", errors.New("Workspace or Git cache root is unsafe")
	}
	if pathContainedBy(workspaceRoot, cacheRoot) || pathContainedBy(cacheRoot, workspaceRoot) {
		return "", "", errors.New("Workspace and Git cache roots must be separate")
	}
	return workspaceRoot, cacheRoot, nil
}

func dangerousManagedRoot(root string) bool {
	volumeRoot := filepath.VolumeName(root) + string(filepath.Separator)
	if filepath.Clean(root) == filepath.Clean(volumeRoot) {
		return true
	}
	home, err := os.UserHomeDir()
	return err == nil && strings.TrimSpace(home) != "" && filepath.Clean(root) == filepath.Clean(home)
}

func expectedWorkspaceManifest(
	layout workspaceLayout,
	workload executions.Workload,
	managed bool,
	repositoryFingerprint, repositoryURL, defaultBranch string,
) workspaceGenerationManifest {
	manifest := workspaceGenerationManifest{
		Format: workspaceLayoutVersion, ExecutionTargetID: layout.TargetID.String(),
		TenantID: workload.TenantID.String(), ProjectID: workload.ProjectID.String(), SessionID: workload.SessionID.String(),
		LogicalWorkspaceID: layout.LogicalID.String(), Managed: managed,
		RepositoryFingerprint: repositoryFingerprint, RepositoryURL: repositoryURL, DefaultBranch: defaultBranch,
	}
	if layout.MaterializationID != uuid.Nil || layout.IncarnationID != uuid.Nil || layout.LayoutVersion == workspaceLayoutV3 {
		manifest.Format = workspaceLayoutV3Format
		manifest.MaterializationID = layout.MaterializationID.String()
		manifest.IncarnationID = layout.IncarnationID.String()
		manifest.LayoutVersion = layout.LayoutVersion
	}
	return manifest
}

func writeWorkspaceManifest(root string, manifest workspaceGenerationManifest) error {
	if err := validateWorkspaceManifestFormat(manifest); err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > workspaceManifestMaxSize {
		return errors.New("Workspace manifest is invalid")
	}
	temporary, err := os.CreateTemp(root, ".manifest-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(root, "manifest.json"))
}

func readWorkspaceManifest(path string) (workspaceGenerationManifest, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || pathInfo.Size() > workspaceManifestMaxSize {
		return workspaceGenerationManifest{}, errors.New("Workspace manifest is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return workspaceGenerationManifest{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	currentPathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || currentPathInfo.Mode()&os.ModeSymlink != 0 || !currentPathInfo.Mode().IsRegular() ||
		!info.Mode().IsRegular() || info.Size() > workspaceManifestMaxSize ||
		!os.SameFile(pathInfo, currentPathInfo) || !os.SameFile(currentPathInfo, info) {
		return workspaceGenerationManifest{}, errors.New("Workspace manifest is unavailable")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, workspaceManifestMaxSize+1))
	if err != nil || len(encoded) > workspaceManifestMaxSize {
		return workspaceGenerationManifest{}, errors.New("Workspace manifest is too large")
	}
	return decodeWorkspaceManifest(encoded)
}

func decodeWorkspaceManifest(encoded []byte) (workspaceGenerationManifest, error) {
	var manifest workspaceGenerationManifest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return workspaceGenerationManifest{}, errors.New("Workspace manifest is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return workspaceGenerationManifest{}, errors.New("Workspace manifest contains trailing data")
	}
	if err := validateWorkspaceManifestFormat(manifest); err != nil {
		return workspaceGenerationManifest{}, err
	}
	return manifest, nil
}

func validateWorkspaceManifestFormat(manifest workspaceGenerationManifest) error {
	switch manifest.Format {
	case workspaceLayoutVersion:
		if manifest.LayoutVersion != 0 || manifest.MaterializationID != "" || manifest.IncarnationID != "" {
			return errors.New("legacy Workspace manifest contains v3 identity fields")
		}
	case workspaceLayoutV3Format:
		if manifest.LayoutVersion != workspaceLayoutV2 && manifest.LayoutVersion != workspaceLayoutV3 {
			return errors.New("Workspace manifest layout version is unsupported")
		}
		materializationID, materializationErr := uuid.Parse(manifest.MaterializationID)
		incarnationID, incarnationErr := uuid.Parse(manifest.IncarnationID)
		if materializationErr != nil || incarnationErr != nil || materializationID == uuid.Nil || incarnationID == uuid.Nil {
			return errors.New("Workspace manifest materialization identity is invalid")
		}
	default:
		return errors.New("Workspace manifest format is unsupported")
	}
	return nil
}

func validateWorkspaceManifest(path string, expected workspaceGenerationManifest) error {
	actual, err := readWorkspaceManifest(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("Workspace manifest does not match the claimed workload")
	}
	return nil
}

func validateWorkspaceManifestForLayout(layout workspaceLayout, expected workspaceGenerationManifest) error {
	actual, err := readWorkspaceManifest(layout.Manifest)
	if err != nil {
		return err
	}
	if actual == expected {
		return nil
	}
	if !layout.AllowLegacyAdoption || !legacyWorkspaceManifestEquivalent(actual, expected) {
		return errors.New("Workspace manifest does not match the claimed workload")
	}
	if err := writeWorkspaceManifest(layout.Root, expected); err != nil {
		return errors.New("legacy Workspace manifest could not be adopted")
	}
	return validateWorkspaceManifest(layout.Manifest, expected)
}

func legacyWorkspaceManifestEquivalent(actual, expected workspaceGenerationManifest) bool {
	if actual.Format != workspaceLayoutVersion || expected.Format != workspaceLayoutV3Format ||
		expected.LayoutVersion != workspaceLayoutV2 {
		return false
	}
	legacyExpected := expected
	legacyExpected.Format = workspaceLayoutVersion
	legacyExpected.MaterializationID = ""
	legacyExpected.IncarnationID = ""
	legacyExpected.LayoutVersion = 0
	return actual == legacyExpected
}

func validateNonGitGeneration(layout workspaceLayout, expected workspaceGenerationManifest) error {
	if err := validateExistingRealDirectory(layout.Root); err != nil {
		return err
	}
	if err := validateExistingRealDirectory(layout.Checkout); err != nil {
		return err
	}
	if _, err := os.Lstat(layout.GitDir); !errors.Is(err, os.ErrNotExist) {
		return errors.New("non-Git Workspace unexpectedly contains private Git metadata")
	}
	return validateWorkspaceManifestForLayout(layout, expected)
}

func buildNonGitWorkspaceGeneration(root string, manifest workspaceGenerationManifest) error {
	if err := validateExistingRealDirectory(root); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(root, "checkout"), 0o700); err != nil {
		return err
	}
	return writeWorkspaceManifest(root, manifest)
}

func workspaceProviderStateDirectory(root string) string {
	return filepath.Join(root, workspaceProviderStateDir)
}

func workspaceProviderStateDirectoryStatus(root string) (string, bool, error) {
	path := workspaceProviderStateDirectory(root)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return path, false, nil
	case err != nil:
		return path, false, err
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return path, false, errors.New("Workspace provider state directory is unsafe")
	case info.Mode().Perm() != 0o700:
		return path, false, errors.New("Workspace provider state directory permissions are unsafe")
	default:
		return path, true, nil
	}
}

func ensureWorkspaceProviderStateDirectory(root string) (string, error) {
	if err := validateExistingRealDirectory(root); err != nil {
		return "", err
	}
	path, exists, err := workspaceProviderStateDirectoryStatus(root)
	if err != nil {
		return "", err
	}
	if exists {
		return path, nil
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, validateExistingRealDirectory(path)
}

func validatePrivateWorktreeFilesystem(layout workspaceLayout, expected workspaceGenerationManifest) error {
	for _, directory := range []string{layout.Root, layout.GitDir, layout.Checkout} {
		if err := validateExistingRealDirectory(directory); err != nil {
			return err
		}
	}
	if err := validateWorkspaceManifestForLayout(layout, expected); err != nil {
		return err
	}
	gitFile := filepath.Join(layout.Checkout, ".git")
	gitFileValue, err := readSmallRegularFile(gitFile, 4096)
	if err != nil {
		return errors.New("Workspace Git file is unavailable")
	}
	prefix := "gitdir: "
	if !strings.HasPrefix(gitFileValue, prefix) {
		return errors.New("Workspace Git file is invalid")
	}
	worktreeGitDir, err := resolveRelativeMetadataPath(layout.Checkout, strings.TrimSpace(strings.TrimPrefix(gitFileValue, prefix)))
	if err != nil || !pathContainedBy(layout.GitDir, worktreeGitDir) || filepath.Clean(worktreeGitDir) == filepath.Clean(layout.GitDir) {
		return errors.New("Workspace Git file escapes the private repository")
	}
	if err := validateExistingContainedDirectory(layout.GitDir, worktreeGitDir); err != nil {
		return err
	}
	commonValue, err := readSmallRegularFile(filepath.Join(worktreeGitDir, "commondir"), 4096)
	if err != nil {
		return errors.New("Workspace common Git directory is unavailable")
	}
	commonDir, err := resolveRelativeMetadataPath(worktreeGitDir, strings.TrimSpace(commonValue))
	if err != nil || filepath.Clean(commonDir) != filepath.Clean(layout.GitDir) || !sameExistingPath(commonDir, layout.GitDir) {
		return errors.New("Workspace common Git directory is not private")
	}
	checkoutPointer, err := readSmallRegularFile(filepath.Join(worktreeGitDir, "gitdir"), 4096)
	if err != nil {
		return errors.New("Workspace checkout Git pointer is unavailable")
	}
	checkoutGitFile, err := resolveRelativeMetadataPath(worktreeGitDir, strings.TrimSpace(checkoutPointer))
	if err != nil || filepath.Clean(checkoutGitFile) != filepath.Clean(gitFile) || !sameExistingPath(checkoutGitFile, gitFile) {
		return errors.New("Workspace checkout Git pointer is invalid")
	}
	for _, relative := range []string{
		filepath.Join("objects", "info", "alternates"), filepath.Join("objects", "info", "http-alternates"),
	} {
		if _, err := os.Lstat(filepath.Join(layout.GitDir, relative)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("Workspace private repository uses shared object alternates")
		}
	}
	if _, err := readSmallRegularFile(filepath.Join(layout.GitDir, "config"), workspaceManifestMaxSize); err != nil {
		return errors.New("Workspace private Git configuration is unavailable")
	}
	return nil
}

func validateExistingRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Workspace generation contains a symlink or non-directory component")
	}
	return nil
}

func validateExistingContainedDirectory(root, directory string) error {
	root = filepath.Clean(root)
	directory = filepath.Clean(directory)
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Workspace Git metadata escaped its private repository")
	}
	if err := validateExistingRealDirectory(root); err != nil {
		return err
	}
	current := root
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("Workspace Git metadata path is invalid")
		}
		current = filepath.Join(current, segment)
		if err := validateExistingRealDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkspaceGenerationPath(workspaceRoot, generationRoot string) error {
	workspaceRoot, err := filepath.Abs(strings.TrimSpace(workspaceRoot))
	if err != nil || strings.TrimSpace(workspaceRoot) == "" {
		return errors.New("Workspace root is invalid")
	}
	generationRoot, err = filepath.Abs(strings.TrimSpace(generationRoot))
	if err != nil || generationRoot == workspaceRoot || !pathContainedBy(workspaceRoot, generationRoot) {
		return errors.New("Workspace generation escaped the configured root")
	}
	return validateExistingContainedDirectory(workspaceRoot, generationRoot)
}

func readSmallRegularFile(path string, maximum int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maximum {
		return "", errors.New("file is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	fileInfo, statErr := file.Stat()
	currentPathInfo, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || currentPathInfo.Mode()&os.ModeSymlink != 0 || !currentPathInfo.Mode().IsRegular() ||
		!fileInfo.Mode().IsRegular() || !os.SameFile(info, currentPathInfo) || !os.SameFile(currentPathInfo, fileInfo) {
		return "", errors.New("file changed while it was opened")
	}
	value, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(value)) > maximum {
		return "", errors.New("file exceeds its safe limit")
	}
	return strings.TrimSpace(string(value)), nil
}

func resolveRelativeMetadataPath(base, value string) (string, error) {
	if value == "" || filepath.IsAbs(value) {
		return "", errors.New("Git metadata path must be relative")
	}
	resolved := filepath.Clean(filepath.Join(base, value))
	if resolved == filepath.Clean(base) {
		return "", errors.New("Git metadata path is empty")
	}
	return resolved, nil
}

func sameExistingPath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func replaceWorkspaceGeneration(active, staging string) error {
	return replaceWorkspaceGenerationWithFS(active, staging, workspaceGenerationFS{
		rename: os.Rename, removeAll: os.RemoveAll,
	})
}

type workspaceGenerationFS struct {
	rename    func(string, string) error
	removeAll func(string) error
}

func replaceWorkspaceGenerationWithFS(active, staging string, fs workspaceGenerationFS) error {
	active = filepath.Clean(active)
	staging = filepath.Clean(staging)
	if filepath.Dir(active) != filepath.Dir(staging) || active == staging {
		return errors.New("Workspace generation staging must be a distinct sibling")
	}
	if err := validateExistingRealDirectory(staging); err != nil {
		return err
	}
	backup := filepath.Join(filepath.Dir(active), "."+filepath.Base(active)+".backup-"+uuid.NewString())
	hadActive := false
	if info, err := os.Lstat(active); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("active Workspace generation is unsafe")
		}
		if err := fs.rename(active, backup); err != nil {
			return err
		}
		hadActive = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	providerStateMoved := false
	if hadActive {
		moved, err := transplantWorkspaceProviderStateDirectory(backup, staging)
		if err != nil {
			if rollbackErr := fs.rename(backup, active); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous Workspace generation: %w", rollbackErr))
			}
			return err
		}
		providerStateMoved = moved
	}
	if err := fs.rename(staging, active); err != nil {
		rollbackErrors := make([]error, 0, 2)
		if hadActive {
			if providerStateMoved {
				if _, restoreStateErr := transplantWorkspaceProviderStateDirectory(staging, backup); restoreStateErr != nil {
					rollbackErrors = append(rollbackErrors, fmt.Errorf("restore previous Workspace provider state: %w", restoreStateErr))
				}
			}
			if rollbackErr := fs.rename(backup, active); rollbackErr != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore previous Workspace generation: %w", rollbackErr))
			}
		}
		if len(rollbackErrors) != 0 {
			joined := []error{err}
			joined = append(joined, rollbackErrors...)
			return errors.Join(joined...)
		}
		return err
	}
	if hadActive {
		// The new generation is already authoritative. A stale backup is safe
		// to retry during later physical cleanup and must not turn a successful
		// atomic install into a false restore failure.
		_ = fs.removeAll(backup)
	}
	return nil
}

func transplantWorkspaceProviderStateDirectory(sourceRoot, destinationRoot string) (bool, error) {
	if err := validateExistingRealDirectory(sourceRoot); err != nil {
		return false, err
	}
	if err := validateExistingRealDirectory(destinationRoot); err != nil {
		return false, err
	}
	sourcePath, sourceExists, err := workspaceProviderStateDirectoryStatus(sourceRoot)
	if err != nil {
		return false, err
	}
	destinationPath, destinationExists, err := workspaceProviderStateDirectoryStatus(destinationRoot)
	if err != nil {
		return false, err
	}
	if !sourceExists {
		return false, nil
	}
	if destinationExists {
		return false, errors.New("destination Workspace provider state directory already exists")
	}
	if err := os.Rename(sourcePath, destinationPath); err != nil {
		return false, err
	}
	return true, nil
}

func reconcileWorkspaceGeneration(
	active string,
	validate func(string) error,
) error {
	active = filepath.Clean(active)
	parent := filepath.Dir(active)
	if err := validateExistingRealDirectory(parent); err != nil {
		return err
	}
	staging, backups, err := workspaceGenerationResidue(active)
	if err != nil {
		return err
	}
	activeInfo, activeErr := os.Lstat(active)
	switch {
	case activeErr == nil:
		if activeInfo.Mode()&os.ModeSymlink != 0 || !activeInfo.IsDir() {
			return errors.New("active Workspace generation is unsafe")
		}
		if err := validate(active); err != nil {
			// The active path remains authoritative until the normal materialization
			// policy decides whether a bound Ready Checkpoint may replace it. Keep
			// every residue so an invalid generation is never silently discarded.
			return nil
		}
		removeWorkspaceGenerationResidue(staging)
		removeWorkspaceGenerationResidue(backups)
		return nil
	case !errors.Is(activeErr, os.ErrNotExist):
		return activeErr
	}

	if len(backups) > 1 {
		return errors.New("Workspace generation recovery found multiple authoritative backups")
	}
	if len(backups) == 0 {
		removeWorkspaceGenerationResidue(staging)
		return nil
	}

	backup := backups[0]
	if err := validate(backup); err != nil {
		return fmt.Errorf("Workspace generation backup is invalid: %w", err)
	}
	if err := os.Rename(backup, active); err != nil {
		return fmt.Errorf("restore Workspace generation backup: %w", err)
	}
	if err := validate(active); err != nil {
		rollbackErr := os.Rename(active, backup)
		if rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("validate restored Workspace generation: %w", err),
				fmt.Errorf("preserve invalid restored Workspace generation: %w", rollbackErr),
			)
		}
		return fmt.Errorf("validate restored Workspace generation: %w", err)
	}
	removeWorkspaceGenerationResidue(staging)
	return nil
}

func workspaceLayoutAtRoot(layout workspaceLayout, root string) workspaceLayout {
	layout.Root = root
	layout.Checkout = filepath.Join(root, "checkout")
	layout.GitDir = filepath.Join(root, "repo.git")
	layout.Manifest = filepath.Join(root, "manifest.json")
	return layout
}

func workspaceGenerationResidue(active string) ([]string, []string, error) {
	parent := filepath.Dir(active)
	base := filepath.Base(active)
	stagingPrefix := "." + base + ".staging-"
	backupPrefix := "." + base + ".backup-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, nil, err
	}
	staging := make([]string, 0)
	backups := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		kind := ""
		switch {
		case strings.HasPrefix(name, stagingPrefix) && len(name) > len(stagingPrefix):
			kind = "staging"
		case strings.HasPrefix(name, backupPrefix) && len(name) > len(backupPrefix):
			kind = "backup"
		default:
			continue
		}
		path := filepath.Join(parent, name)
		if !pathContainedBy(parent, path) || filepath.Dir(path) != parent {
			return nil, nil, errors.New("Workspace generation residue escaped its parent")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, nil, errors.New("Workspace generation residue is unsafe")
		}
		if kind == "staging" {
			staging = append(staging, path)
		} else {
			backups = append(backups, path)
		}
	}
	return staging, backups, nil
}

func removeWorkspaceGenerationResidue(paths []string) {
	for _, path := range paths {
		_ = os.RemoveAll(path)
	}
}

func legacyWorkspaceContainsData(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return true, nil
	}
	entries, err := os.ReadDir(path)
	return len(entries) > 0, err
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
