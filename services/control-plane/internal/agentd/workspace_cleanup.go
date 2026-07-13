package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	workspaceCleanupMarkerFormat = "synara-workspace-cleanup-v1"
	workspaceCleanupMarkerName   = "cleanup.json"
	workspaceCleanupPendingName  = ".cleanup.json.pending"
	workspaceCleanupPayloadName  = "generation"
	workspaceCleanupMaxSize      = 32 << 10
)

var errWorkspaceCleanupDurability = errors.New("Workspace cleanup durability failed")

type WorkspaceCleanupStatus string

const (
	WorkspaceCleanupDeleted       WorkspaceCleanupStatus = "deleted"
	WorkspaceCleanupAlreadyAbsent WorkspaceCleanupStatus = "already_absent"
)

// WorkspaceCleanupRequest is deliberately path-free. The Worker derives every
// filesystem location from configured roots and immutable UUID identity.
type WorkspaceCleanupRequest struct {
	CleanupID          uuid.UUID
	TenantID           uuid.UUID
	OrganizationID     uuid.UUID
	ProjectID          uuid.UUID
	SessionID          uuid.UUID
	LogicalWorkspaceID uuid.UUID
	MaterializationID  uuid.UUID
	IncarnationID      uuid.UUID
	ExecutionTargetID  uuid.UUID
	TargetKind         string
	StorageScope       string
	LayoutVersion      int
	DispatchGeneration int64
}

type WorkspaceCleanupResult struct {
	Status WorkspaceCleanupStatus
}

// WorkspaceCleanupError preserves the retry boundary for the daemon without
// exposing a host path to the Control Plane.
type WorkspaceCleanupError struct {
	Code      string
	Message   string
	Retryable bool
	cause     error
}

func (e *WorkspaceCleanupError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *WorkspaceCleanupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type workspaceCleanupLayout struct {
	Root                  string
	SourceRelative        string
	LockPath              string
	ClaimRelative         string
	MarkerRelative        string
	PendingMarkerRelative string
	PayloadRelative       string
}

type workspaceCleanupDirectorySync func(*os.Root, string) error

type workspaceCleanupMarker struct {
	Format             string `json:"format"`
	State              string `json:"state"`
	CleanupID          string `json:"cleanupId"`
	TenantID           string `json:"tenantId"`
	OrganizationID     string `json:"organizationId"`
	ProjectID          string `json:"projectId"`
	SessionID          string `json:"sessionId"`
	LogicalWorkspaceID string `json:"logicalWorkspaceId"`
	MaterializationID  string `json:"materializationId"`
	IncarnationID      string `json:"incarnationId"`
	ExecutionTargetID  string `json:"executionTargetId"`
	TargetKind         string `json:"targetKind"`
	StorageScope       string `json:"storageScope"`
	LayoutVersion      int    `json:"layoutVersion"`
}

const (
	workspaceCleanupPrepared    = "prepared"
	workspaceCleanupQuarantined = "quarantined"
	workspaceCleanupDeleting    = "deleting"
)

type workspaceCleanupManifestMatch int

const (
	workspaceCleanupManifestMismatch workspaceCleanupManifestMatch = iota
	workspaceCleanupManifestExact
	workspaceCleanupManifestLegacy
	workspaceCleanupManifestStaleIncarnation
)

func (m *WorkspaceMaterializer) persistWorkspaceCleanupDirectory(root *os.Root, relative string) error {
	persist := syncWorkspaceCleanupDirectory
	if m.cleanupDirectorySync != nil {
		persist = m.cleanupDirectorySync
	}
	if err := persist(root, relative); err != nil {
		return fmt.Errorf("%w: %w", errWorkspaceCleanupDurability, err)
	}
	return nil
}

func (m *WorkspaceMaterializer) CleanupWorkspace(
	ctx context.Context,
	request WorkspaceCleanupRequest,
) (WorkspaceCleanupResult, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
			"workspace_cleanup_cancelled", "Workspace cleanup was cancelled before it started.", err,
		)
	}
	if err := requireWorkspaceCleanupDurability(); err != nil {
		return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
			"workspace_cleanup_unsupported_platform",
			"This operating system cannot durably delete managed Workspaces.",
			err,
		)
	}
	layout, err := m.resolveWorkspaceCleanupLayout(request)
	if err != nil {
		return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
			"workspace_cleanup_invalid", "Workspace cleanup identity is invalid.", err,
		)
	}
	if info, statErr := os.Lstat(layout.Root); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_unsafe_path", "The configured Workspace root is unsafe.", nil,
			)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
			"workspace_cleanup_unsafe_path", "The configured Workspace root is unavailable.", statErr,
		)
	}
	workspaceLock, err := acquireWorkspaceFileLock(ctx, layout.Root, layout.LockPath)
	if err != nil {
		return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
			"workspace_cleanup_lock_busy", "The logical Workspace is still in use or could not be locked.", err,
		)
	}
	result, cleanupErr := m.cleanupWorkspaceLocked(ctx, request, layout)
	releaseErr := workspaceLock.Release()
	if cleanupErr != nil {
		return WorkspaceCleanupResult{}, cleanupErr
	}
	if releaseErr != nil {
		return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
			"workspace_cleanup_lock_release_failed", "The Workspace cleanup lock could not be released.", releaseErr,
		)
	}
	return result, nil
}

func (m *WorkspaceMaterializer) resolveWorkspaceCleanupLayout(
	request WorkspaceCleanupRequest,
) (workspaceCleanupLayout, error) {
	if err := validateWorkspaceCleanupRequest(request); err != nil {
		return workspaceCleanupLayout{}, err
	}
	workspaceRoot, cacheRoot, err := validateWorkspaceRoots(m.root, m.cacheRoot)
	if err != nil {
		return workspaceCleanupLayout{}, err
	}
	m.root = workspaceRoot
	m.cacheRoot = cacheRoot
	if m.targetID != uuid.Nil && m.targetID != request.ExecutionTargetID {
		return workspaceCleanupLayout{}, errors.New("cleanup Execution Target does not match this Worker")
	}
	logicalSegments := []string{
		request.ExecutionTargetID.String(), request.TenantID.String(), request.ProjectID.String(),
		request.SessionID.String(), request.LogicalWorkspaceID.String(),
	}
	pathSegments := append([]string(nil), logicalSegments...)
	lockNamespace := "workspace-v2"
	switch request.LayoutVersion {
	case workspaceLayoutV2:
	case workspaceLayoutV3:
		pathSegments = append(pathSegments, request.IncarnationID.String())
		lockNamespace = "workspace-v3"
	default:
		return workspaceCleanupLayout{}, errors.New("cleanup Workspace layout version is unsupported")
	}
	sourceRelative := filepath.Join(append([]string{fmt.Sprintf("v%d", request.LayoutVersion)}, pathSegments...)...)
	claimRelative := filepath.Join(".quarantine", "workspace-v1", request.CleanupID.String())
	lockPath, err := lockPathFor(workspaceRoot, lockNamespace, logicalSegments...)
	if err != nil {
		return workspaceCleanupLayout{}, err
	}
	for _, relative := range []string{sourceRelative, claimRelative} {
		absolute := filepath.Join(workspaceRoot, relative)
		if !pathContainedBy(workspaceRoot, absolute) || filepath.Clean(absolute) == workspaceRoot {
			return workspaceCleanupLayout{}, errors.New("cleanup Workspace path escapes its configured root")
		}
	}
	return workspaceCleanupLayout{
		Root: workspaceRoot, SourceRelative: sourceRelative, LockPath: lockPath,
		ClaimRelative:         claimRelative,
		MarkerRelative:        filepath.Join(claimRelative, workspaceCleanupMarkerName),
		PendingMarkerRelative: filepath.Join(claimRelative, workspaceCleanupPendingName),
		PayloadRelative:       filepath.Join(claimRelative, workspaceCleanupPayloadName),
	}, nil
}

func validateWorkspaceCleanupRequest(request WorkspaceCleanupRequest) error {
	for name, value := range map[string]uuid.UUID{
		"cleanup": request.CleanupID, "tenant": request.TenantID, "organization": request.OrganizationID,
		"project": request.ProjectID, "session": request.SessionID, "logical Workspace": request.LogicalWorkspaceID,
		"materialization": request.MaterializationID, "incarnation": request.IncarnationID,
		"Execution Target": request.ExecutionTargetID,
	} {
		if value == uuid.Nil {
			return fmt.Errorf("%s ID is missing", name)
		}
	}
	if request.LayoutVersion != workspaceLayoutV2 && request.LayoutVersion != workspaceLayoutV3 {
		return errors.New("Workspace layout version is unsupported")
	}
	if request.DispatchGeneration <= 0 {
		return errors.New("cleanup dispatch generation is invalid")
	}
	switch strings.TrimSpace(request.TargetKind) {
	case "local", "ssh", "docker", "kubernetes":
	default:
		return errors.New("cleanup Target kind is unsupported")
	}
	if strings.TrimSpace(request.StorageScope) != "target" {
		return errors.New("only target-scoped Workspace storage can be deleted by agentd")
	}
	return nil
}

func (m *WorkspaceMaterializer) cleanupWorkspaceLocked(
	ctx context.Context,
	request WorkspaceCleanupRequest,
	layout workspaceCleanupLayout,
) (WorkspaceCleanupResult, error) {
	root, err := openVerifiedWorkspaceRoot(layout.Root)
	if err != nil {
		return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
			"workspace_cleanup_unsafe_path", "The configured Workspace root is unsafe.", err,
		)
	}
	defer root.Close()
	if err := m.persistWorkspaceCleanupDirectory(root, "."); err != nil {
		if workspaceCleanupDurabilityUnavailable(err) {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_unsupported_platform",
				"This Workspace filesystem cannot provide durable cleanup ordering.",
				err,
			)
		}
		return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
			"workspace_cleanup_durability_failed", "The Workspace root could not provide durable cleanup ordering.", err,
		)
	}

	claimExists, err := inspectRootRelativeDirectory(root, layout.ClaimRelative)
	if err != nil {
		return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
			"workspace_cleanup_unsafe_quarantine", "The Workspace cleanup quarantine is unsafe.", err,
		)
	}
	marker, markerExists, err := loadWorkspaceCleanupMarker(
		root, layout, request, claimExists, m.persistWorkspaceCleanupDirectory,
	)
	if err != nil {
		return WorkspaceCleanupResult{}, err
	}
	payloadExists, err := inspectRootRelativeDirectory(root, layout.PayloadRelative)
	if err != nil {
		return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
			"workspace_cleanup_unsafe_quarantine", "The quarantined Workspace generation is unsafe.", err,
		)
	}
	if payloadExists && !markerExists {
		return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
			"workspace_cleanup_identity_mismatch", "A quarantined Workspace has no matching cleanup marker.", nil,
		)
	}
	if claimExists {
		if err := validateWorkspaceCleanupClaimEntries(root, layout, markerExists, payloadExists); err != nil {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_unsafe_quarantine", "The Workspace cleanup quarantine contains unexpected state.", err,
			)
		}
	}

	sourceExists, err := inspectRootRelativeDirectory(root, layout.SourceRelative)
	if err != nil {
		return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
			"workspace_cleanup_unsafe_path", "The Workspace generation path is unsafe.", err,
		)
	}
	var sourceManifest workspaceGenerationManifest
	sourceMatch := workspaceCleanupManifestMismatch
	if sourceExists {
		sourceManifest, err = readWorkspaceManifestAt(root, filepath.Join(layout.SourceRelative, "manifest.json"))
		if err != nil {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_identity_mismatch", "The Workspace generation Manifest is unavailable or invalid.", err,
			)
		}
		sourceMatch = compareWorkspaceCleanupManifest(sourceManifest, request)
		if sourceMatch == workspaceCleanupManifestMismatch {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_identity_mismatch", "The Workspace generation does not match the cleanup command.", nil,
			)
		}
	}

	if payloadExists {
		if marker.State != workspaceCleanupDeleting {
			payloadManifest, manifestErr := readWorkspaceManifestAt(root, filepath.Join(layout.PayloadRelative, "manifest.json"))
			if manifestErr != nil || compareWorkspaceCleanupManifest(payloadManifest, request) != workspaceCleanupManifestExact {
				return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
					"workspace_cleanup_identity_mismatch", "The quarantined Workspace does not match the cleanup command.", manifestErr,
				)
			}
		} else if manifestExists, manifestErr := rootRelativeEntryExists(root, filepath.Join(layout.PayloadRelative, "manifest.json")); manifestErr != nil {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_unsafe_quarantine", "The quarantined Workspace Manifest path is unsafe.", manifestErr,
			)
		} else if manifestExists {
			payloadManifest, manifestErr := readWorkspaceManifestAt(root, filepath.Join(layout.PayloadRelative, "manifest.json"))
			if manifestErr != nil || compareWorkspaceCleanupManifest(payloadManifest, request) != workspaceCleanupManifestExact {
				return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
					"workspace_cleanup_identity_mismatch", "The partially deleted Workspace does not match the cleanup command.", manifestErr,
				)
			}
		}
		if sourceExists && sourceMatch != workspaceCleanupManifestStaleIncarnation {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_identity_mismatch", "Both active and quarantined copies claim the same Workspace incarnation.", nil,
			)
		}
	}

	if !sourceExists && !payloadExists {
		if err := persistRootRelativeAbsence(
			root, layout.SourceRelative, m.persistWorkspaceCleanupDirectory,
		); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_durability_failed", "The absent Workspace source was not durably confirmed.", err,
			)
		}
		if claimExists {
			if err := finishWorkspaceCleanupClaim(ctx, root, layout, m.persistWorkspaceCleanupDirectory); err != nil {
				return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
					"workspace_cleanup_retryable", "Workspace cleanup finalization must be retried.", err,
				)
			}
		} else if err := persistRootRelativeAbsence(
			root, layout.ClaimRelative, m.persistWorkspaceCleanupDirectory,
		); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_durability_failed", "The absent Workspace quarantine was not durably confirmed.", err,
			)
		}
		return WorkspaceCleanupResult{Status: WorkspaceCleanupAlreadyAbsent}, nil
	}
	if sourceExists && sourceMatch == workspaceCleanupManifestStaleIncarnation && !payloadExists {
		if claimExists {
			if err := finishWorkspaceCleanupClaim(ctx, root, layout, m.persistWorkspaceCleanupDirectory); err != nil {
				return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
					"workspace_cleanup_retryable", "Workspace cleanup finalization must be retried.", err,
				)
			}
		} else if err := persistRootRelativeAbsence(
			root, layout.PayloadRelative, m.persistWorkspaceCleanupDirectory,
		); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_durability_failed", "The old Workspace quarantine was not durably absent.", err,
			)
		}
		return WorkspaceCleanupResult{Status: WorkspaceCleanupAlreadyAbsent}, nil
	}

	if sourceExists && !payloadExists {
		if sourceMatch == workspaceCleanupManifestLegacy {
			sourceManifest = adoptLegacyWorkspaceManifest(sourceManifest, request)
			if err := writeWorkspaceManifestAt(
				root, layout.SourceRelative, sourceManifest, m.persistWorkspaceCleanupDirectory,
			); err != nil {
				return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
					"workspace_cleanup_retryable", "The legacy Workspace incarnation marker could not be adopted.", err,
				)
			}
			adopted, readErr := readWorkspaceManifestAt(root, filepath.Join(layout.SourceRelative, "manifest.json"))
			if readErr != nil || compareWorkspaceCleanupManifest(adopted, request) != workspaceCleanupManifestExact {
				return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
					"workspace_cleanup_identity_mismatch", "The adopted legacy Workspace identity could not be verified.", readErr,
				)
			}
		}
		if err := ctx.Err(); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_cancelled", "Workspace cleanup was cancelled before quarantine.", err,
			)
		}
		if err := ensureRootRelativeDirectory(root, layout.ClaimRelative, m.persistWorkspaceCleanupDirectory); err != nil {
			if errors.Is(err, errWorkspaceCleanupDurability) {
				return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
					"workspace_cleanup_durability_failed", "The Workspace cleanup quarantine was not durably prepared.", err,
				)
			}
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_unsafe_quarantine", "The Workspace cleanup quarantine could not be prepared safely.", err,
			)
		}
		if !markerExists {
			marker = workspaceCleanupMarkerForRequest(request, workspaceCleanupPrepared)
			if err := writeWorkspaceCleanupMarker(
				root, layout, marker, m.persistWorkspaceCleanupDirectory,
			); err != nil {
				return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
					"workspace_cleanup_retryable", "The Workspace cleanup marker could not be persisted.", err,
				)
			}
			markerExists = true
		}
		sourceInfo, infoErr := root.Lstat(layout.SourceRelative)
		if infoErr != nil || sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.IsDir() {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_unsafe_path", "The Workspace generation changed before quarantine.", infoErr,
			)
		}
		if err := root.Rename(layout.SourceRelative, layout.PayloadRelative); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_retryable", "The Workspace generation could not be quarantined atomically.", err,
			)
		}
		if err := m.persistWorkspaceCleanupDirectory(root, layout.ClaimRelative); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_durability_failed", "The quarantined Workspace destination was not durable.", err,
			)
		}
		if err := m.persistWorkspaceCleanupDirectory(root, filepath.Dir(layout.SourceRelative)); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_durability_failed", "The Workspace source removal was not durable.", err,
			)
		}
		payloadInfo, infoErr := root.Lstat(layout.PayloadRelative)
		if infoErr != nil || payloadInfo.Mode()&os.ModeSymlink != 0 || !payloadInfo.IsDir() || !os.SameFile(sourceInfo, payloadInfo) {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_identity_mismatch", "The quarantined Workspace is not the generation that was locked.", infoErr,
			)
		}
		payloadManifest, manifestErr := readWorkspaceManifestAt(root, filepath.Join(layout.PayloadRelative, "manifest.json"))
		if manifestErr != nil || compareWorkspaceCleanupManifest(payloadManifest, request) != workspaceCleanupManifestExact {
			return WorkspaceCleanupResult{}, permanentWorkspaceCleanupError(
				"workspace_cleanup_identity_mismatch", "The quarantined Workspace Manifest changed during rename.", manifestErr,
			)
		}
		marker.State = workspaceCleanupQuarantined
		if err := writeWorkspaceCleanupMarker(
			root, layout, marker, m.persistWorkspaceCleanupDirectory,
		); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_retryable", "The quarantined Workspace marker could not be persisted.", err,
			)
		}
		payloadExists = true
	}

	if !payloadExists {
		return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
			"workspace_cleanup_retryable", "The Workspace cleanup state changed and must be retried.", nil,
		)
	}
	if marker.State != workspaceCleanupDeleting {
		marker.State = workspaceCleanupDeleting
		if err := writeWorkspaceCleanupMarker(
			root, layout, marker, m.persistWorkspaceCleanupDirectory,
		); err != nil {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_retryable", "The Workspace deletion marker could not be persisted.", err,
			)
		}
	}
	if err := removeQuarantinedWorkspace(
		ctx, root, layout, m.persistWorkspaceCleanupDirectory,
	); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
				"workspace_cleanup_cancelled", "Workspace cleanup was cancelled during deletion.", err,
			)
		}
		return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
			"workspace_cleanup_retryable", "The quarantined Workspace could not be fully deleted.", err,
		)
	}
	if err := verifyRequestedWorkspaceAbsent(
		root, layout, request, m.persistWorkspaceCleanupDirectory,
	); err != nil {
		return WorkspaceCleanupResult{}, err
	}
	if err := finishWorkspaceCleanupClaim(ctx, root, layout, m.persistWorkspaceCleanupDirectory); err != nil {
		return WorkspaceCleanupResult{}, retryableWorkspaceCleanupError(
			"workspace_cleanup_retryable", "Workspace cleanup finalization must be retried.", err,
		)
	}
	return WorkspaceCleanupResult{Status: WorkspaceCleanupDeleted}, nil
}

func openVerifiedWorkspaceRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, errors.New("Workspace root is not a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, openedErr := root.Stat(".")
	after, afterErr := os.Lstat(path)
	if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = root.Close()
		return nil, errors.New("Workspace root changed while it was opened")
	}
	return root, nil
}

func openWorkspaceCleanupDirectoryForSync(root *os.Root, relative string) (*os.File, error) {
	relative = filepath.Clean(strings.TrimSpace(relative))
	if relative != "." {
		if _, err := cleanRootRelativeSegments(relative); err != nil {
			return nil, err
		}
		exists, err := inspectRootRelativeDirectory(root, relative)
		if err != nil || !exists {
			if err == nil {
				err = os.ErrNotExist
			}
			return nil, err
		}
	}
	before, err := root.Lstat(relative)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, errors.New("cleanup durability directory is unavailable")
	}
	directory, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	opened, openedErr := directory.Stat()
	after, afterErr := root.Lstat(relative)
	if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = directory.Close()
		return nil, errors.New("cleanup durability directory changed while it was opened")
	}
	return directory, nil
}

func inspectRootRelativeDirectory(root *os.Root, relative string) (bool, error) {
	segments, err := cleanRootRelativeSegments(relative)
	if err != nil {
		return false, err
	}
	current := "."
	for _, segment := range segments {
		current = filepath.Join(current, segment)
		info, statErr := root.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		if statErr != nil {
			return false, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, errors.New("path contains a symlink or non-directory component")
		}
	}
	return true, nil
}

func persistRootRelativeAbsence(
	root *os.Root,
	relative string,
	persist workspaceCleanupDirectorySync,
) error {
	segments, err := cleanRootRelativeSegments(relative)
	if err != nil {
		return err
	}
	current := "."
	for _, segment := range segments {
		next := filepath.Join(current, segment)
		info, statErr := root.Lstat(next)
		if errors.Is(statErr, os.ErrNotExist) {
			return persist(root, current)
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("absent cleanup path contains a symlink or non-directory component")
		}
		current = next
	}
	return errors.New("cleanup path still exists")
}

func ensureRootRelativeDirectory(
	root *os.Root,
	relative string,
	persist workspaceCleanupDirectorySync,
) error {
	segments, err := cleanRootRelativeSegments(relative)
	if err != nil {
		return err
	}
	current := "."
	for _, segment := range segments {
		current = filepath.Join(current, segment)
		info, statErr := root.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := root.Mkdir(current, 0o700); err != nil {
				return err
			}
			info, statErr = root.Lstat(current)
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("path contains a symlink or non-directory component")
		}
		if err := persist(root, filepath.Dir(current)); err != nil {
			return err
		}
	}
	return nil
}

func cleanRootRelativeSegments(relative string) ([]string, error) {
	relative = filepath.Clean(strings.TrimSpace(relative))
	if relative == "" || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("root-relative path is invalid")
	}
	segments := strings.Split(relative, string(filepath.Separator))
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, errors.New("root-relative path contains an invalid segment")
		}
	}
	return segments, nil
}

func loadWorkspaceCleanupMarker(
	root *os.Root,
	layout workspaceCleanupLayout,
	request WorkspaceCleanupRequest,
	claimExists bool,
	persist workspaceCleanupDirectorySync,
) (workspaceCleanupMarker, bool, error) {
	if !claimExists {
		return workspaceCleanupMarker{}, false, nil
	}
	marker, markerExists, markerErr := readWorkspaceCleanupMarker(root, layout.MarkerRelative)
	if markerErr != nil {
		return workspaceCleanupMarker{}, false, permanentWorkspaceCleanupError(
			"workspace_cleanup_identity_mismatch", "The Workspace cleanup marker is invalid.", markerErr,
		)
	}
	pending, pendingExists, pendingErr := readWorkspaceCleanupMarker(root, layout.PendingMarkerRelative)
	if pendingErr != nil && markerExists {
		// An interrupted rewrite cannot override an already verified marker.
		if removeErr := root.Remove(layout.PendingMarkerRelative); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return workspaceCleanupMarker{}, false, retryableWorkspaceCleanupError(
				"workspace_cleanup_retryable", "The incomplete cleanup marker could not be discarded.", removeErr,
			)
		}
		if err := persist(root, layout.ClaimRelative); err != nil {
			return workspaceCleanupMarker{}, false, retryableWorkspaceCleanupError(
				"workspace_cleanup_durability_failed", "The incomplete cleanup marker removal was not durable.", err,
			)
		}
		pendingExists = false
	} else if pendingErr != nil {
		return workspaceCleanupMarker{}, false, permanentWorkspaceCleanupError(
			"workspace_cleanup_identity_mismatch", "The pending Workspace cleanup marker is invalid.", pendingErr,
		)
	}
	if markerExists && !workspaceCleanupMarkerMatchesRequest(marker, request) {
		return workspaceCleanupMarker{}, false, permanentWorkspaceCleanupError(
			"workspace_cleanup_identity_mismatch", "The Workspace cleanup marker belongs to another command.", nil,
		)
	}
	if pendingExists && !workspaceCleanupMarkerMatchesRequest(pending, request) {
		return workspaceCleanupMarker{}, false, permanentWorkspaceCleanupError(
			"workspace_cleanup_identity_mismatch", "The pending Workspace cleanup marker belongs to another command.", nil,
		)
	}
	if pendingExists {
		if !markerExists || workspaceCleanupStateRank(pending.State) >= workspaceCleanupStateRank(marker.State) {
			if err := root.Rename(layout.PendingMarkerRelative, layout.MarkerRelative); err != nil {
				return workspaceCleanupMarker{}, false, retryableWorkspaceCleanupError(
					"workspace_cleanup_retryable", "The pending Workspace cleanup marker could not be recovered.", err,
				)
			}
			if err := persist(root, layout.ClaimRelative); err != nil {
				return workspaceCleanupMarker{}, false, retryableWorkspaceCleanupError(
					"workspace_cleanup_durability_failed", "The recovered Workspace cleanup marker was not durable.", err,
				)
			}
			marker = pending
			markerExists = true
		} else if err := root.Remove(layout.PendingMarkerRelative); err != nil && !errors.Is(err, os.ErrNotExist) {
			return workspaceCleanupMarker{}, false, retryableWorkspaceCleanupError(
				"workspace_cleanup_retryable", "The obsolete pending cleanup marker could not be removed.", err,
			)
		} else if err := persist(root, layout.ClaimRelative); err != nil {
			return workspaceCleanupMarker{}, false, retryableWorkspaceCleanupError(
				"workspace_cleanup_durability_failed", "The obsolete cleanup marker removal was not durable.", err,
			)
		}
	}
	return marker, markerExists, nil
}

func readWorkspaceCleanupMarker(root *os.Root, relative string) (workspaceCleanupMarker, bool, error) {
	info, err := root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return workspaceCleanupMarker{}, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > workspaceCleanupMaxSize {
		return workspaceCleanupMarker{}, false, errors.New("cleanup marker is unavailable")
	}
	file, err := root.Open(relative)
	if err != nil {
		return workspaceCleanupMarker{}, false, err
	}
	defer file.Close()
	opened, openedErr := file.Stat()
	current, currentErr := root.Lstat(relative)
	if openedErr != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		!os.SameFile(info, current) || !os.SameFile(current, opened) {
		return workspaceCleanupMarker{}, false, errors.New("cleanup marker changed while it was opened")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, workspaceCleanupMaxSize+1))
	if err != nil || len(encoded) > workspaceCleanupMaxSize {
		return workspaceCleanupMarker{}, false, errors.New("cleanup marker exceeds its safe limit")
	}
	var marker workspaceCleanupMarker
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return workspaceCleanupMarker{}, false, errors.New("cleanup marker is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return workspaceCleanupMarker{}, false, errors.New("cleanup marker contains trailing data")
	}
	if marker.Format != workspaceCleanupMarkerFormat || workspaceCleanupStateRank(marker.State) == 0 {
		return workspaceCleanupMarker{}, false, errors.New("cleanup marker format or state is unsupported")
	}
	return marker, true, nil
}

func writeWorkspaceCleanupMarker(
	root *os.Root,
	layout workspaceCleanupLayout,
	marker workspaceCleanupMarker,
	persist workspaceCleanupDirectorySync,
) error {
	if marker.Format != workspaceCleanupMarkerFormat || workspaceCleanupStateRank(marker.State) == 0 {
		return errors.New("cleanup marker is invalid")
	}
	encoded, err := json.Marshal(marker)
	if err != nil || len(encoded) > workspaceCleanupMaxSize {
		return errors.New("cleanup marker is too large")
	}
	if info, statErr := root.Lstat(layout.PendingMarkerRelative); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("pending cleanup marker path is unsafe")
		}
		if err := root.Remove(layout.PendingMarkerRelative); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	file, err := root.OpenFile(layout.PendingMarkerRelative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(layout.PendingMarkerRelative, layout.MarkerRelative); err != nil {
		return err
	}
	return persist(root, layout.ClaimRelative)
}

func workspaceCleanupMarkerForRequest(request WorkspaceCleanupRequest, state string) workspaceCleanupMarker {
	return workspaceCleanupMarker{
		Format: workspaceCleanupMarkerFormat, State: state, CleanupID: request.CleanupID.String(),
		TenantID: request.TenantID.String(), OrganizationID: request.OrganizationID.String(),
		ProjectID: request.ProjectID.String(), SessionID: request.SessionID.String(),
		LogicalWorkspaceID: request.LogicalWorkspaceID.String(), MaterializationID: request.MaterializationID.String(),
		IncarnationID: request.IncarnationID.String(), ExecutionTargetID: request.ExecutionTargetID.String(),
		TargetKind: strings.TrimSpace(request.TargetKind), StorageScope: strings.TrimSpace(request.StorageScope),
		LayoutVersion: request.LayoutVersion,
	}
}

func workspaceCleanupMarkerMatchesRequest(marker workspaceCleanupMarker, request WorkspaceCleanupRequest) bool {
	expected := workspaceCleanupMarkerForRequest(request, marker.State)
	return marker == expected
}

func workspaceCleanupStateRank(state string) int {
	switch state {
	case workspaceCleanupPrepared:
		return 1
	case workspaceCleanupQuarantined:
		return 2
	case workspaceCleanupDeleting:
		return 3
	default:
		return 0
	}
}

func validateWorkspaceCleanupClaimEntries(
	root *os.Root,
	layout workspaceCleanupLayout,
	markerExists, payloadExists bool,
) error {
	file, err := openVerifiedRootRelativeDirectory(root, layout.ClaimRelative)
	if err != nil {
		return err
	}
	defer file.Close()
	entries, err := readRootDirectoryEntries(file)
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		workspaceCleanupMarkerName:  markerExists,
		workspaceCleanupPayloadName: payloadExists,
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("unexpected quarantine entry %q", entry.Name())
		}
	}
	return nil
}

func readWorkspaceManifestAt(root *os.Root, relative string) (workspaceGenerationManifest, error) {
	info, err := root.Lstat(relative)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > workspaceManifestMaxSize {
		return workspaceGenerationManifest{}, errors.New("Workspace manifest is unavailable")
	}
	file, err := root.Open(relative)
	if err != nil {
		return workspaceGenerationManifest{}, err
	}
	defer file.Close()
	opened, openedErr := file.Stat()
	current, currentErr := root.Lstat(relative)
	if openedErr != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		!os.SameFile(info, current) || !os.SameFile(current, opened) {
		return workspaceGenerationManifest{}, errors.New("Workspace manifest changed while it was opened")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, workspaceManifestMaxSize+1))
	if err != nil || len(encoded) > workspaceManifestMaxSize {
		return workspaceGenerationManifest{}, errors.New("Workspace manifest exceeds its safe limit")
	}
	return decodeWorkspaceManifest(encoded)
}

func writeWorkspaceManifestAt(
	root *os.Root,
	directoryRelative string,
	manifest workspaceGenerationManifest,
	persist workspaceCleanupDirectorySync,
) error {
	if err := validateWorkspaceManifestFormat(manifest); err != nil {
		return err
	}
	directory, err := openVerifiedRootRelativeDirectory(root, directoryRelative)
	if err != nil {
		return err
	}
	defer directory.Close()
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > workspaceManifestMaxSize {
		return errors.New("Workspace manifest is invalid")
	}
	temporaryName := ".manifest-adoption-" + uuid.NewString() + ".json"
	temporary, err := directory.OpenFile(temporaryName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer directory.Remove(temporaryName)
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
	if err := directory.Rename(temporaryName, "manifest.json"); err != nil {
		return err
	}
	return persist(root, directoryRelative)
}

func compareWorkspaceCleanupManifest(
	manifest workspaceGenerationManifest,
	request WorkspaceCleanupRequest,
) workspaceCleanupManifestMatch {
	if !manifest.Managed || manifest.ExecutionTargetID != request.ExecutionTargetID.String() ||
		manifest.TenantID != request.TenantID.String() || manifest.ProjectID != request.ProjectID.String() ||
		manifest.SessionID != request.SessionID.String() || manifest.LogicalWorkspaceID != request.LogicalWorkspaceID.String() {
		return workspaceCleanupManifestMismatch
	}
	if manifest.Format == workspaceLayoutVersion {
		if request.LayoutVersion == workspaceLayoutV2 && manifest.LayoutVersion == 0 &&
			manifest.MaterializationID == "" && manifest.IncarnationID == "" {
			return workspaceCleanupManifestLegacy
		}
		return workspaceCleanupManifestMismatch
	}
	if manifest.Format != workspaceLayoutV3Format || manifest.LayoutVersion != request.LayoutVersion {
		return workspaceCleanupManifestMismatch
	}
	if manifest.MaterializationID == request.MaterializationID.String() && manifest.IncarnationID == request.IncarnationID.String() {
		return workspaceCleanupManifestExact
	}
	if request.LayoutVersion == workspaceLayoutV2 && manifest.IncarnationID != request.IncarnationID.String() {
		return workspaceCleanupManifestStaleIncarnation
	}
	return workspaceCleanupManifestMismatch
}

func adoptLegacyWorkspaceManifest(
	manifest workspaceGenerationManifest,
	request WorkspaceCleanupRequest,
) workspaceGenerationManifest {
	manifest.Format = workspaceLayoutV3Format
	manifest.LayoutVersion = workspaceLayoutV2
	manifest.MaterializationID = request.MaterializationID.String()
	manifest.IncarnationID = request.IncarnationID.String()
	return manifest
}

func rootRelativeEntryExists(root *os.Root, relative string) (bool, error) {
	info, err := root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("entry is a symlink")
	}
	return true, nil
}

func openVerifiedRootRelativeDirectory(root *os.Root, relative string) (*os.Root, error) {
	exists, err := inspectRootRelativeDirectory(root, relative)
	if err != nil || !exists {
		if err == nil {
			err = os.ErrNotExist
		}
		return nil, err
	}
	before, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	directory, err := root.OpenRoot(relative)
	if err != nil {
		return nil, err
	}
	opened, openedErr := directory.Stat(".")
	after, afterErr := root.Lstat(relative)
	if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = directory.Close()
		return nil, errors.New("directory changed while it was opened")
	}
	return directory, nil
}

func readRootDirectoryEntries(root *os.Root) ([]fs.DirEntry, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.ReadDir(-1)
}

func removeQuarantinedWorkspace(
	ctx context.Context,
	root *os.Root,
	layout workspaceCleanupLayout,
	persist workspaceCleanupDirectorySync,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	claimRoot, err := openVerifiedRootRelativeDirectory(root, layout.ClaimRelative)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer claimRoot.Close()
	return removeRootRelativeTree(ctx, claimRoot, workspaceCleanupPayloadName, persist)
}

func removeRootRelativeTree(
	ctx context.Context,
	parent *os.Root,
	name string,
	persist workspaceCleanupDirectorySync,
) error {
	return removeRootRelativeTreeEntry(ctx, parent, name, persist, true)
}

func removeRootRelativeTreeEntry(
	ctx context.Context,
	parent *os.Root,
	name string,
	persist workspaceCleanupDirectorySync,
	persistParent bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return errors.New("cleanup entry name is invalid")
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		current, currentErr := parent.Lstat(name)
		if currentErr != nil || !os.SameFile(info, current) {
			return errors.New("cleanup entry changed before removal")
		}
		if err := parent.Remove(name); err != nil {
			return err
		}
		if persistParent {
			return persist(parent, ".")
		}
		return nil
	}
	directory, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	opened, openedErr := directory.Stat(".")
	current, currentErr := parent.Lstat(name)
	if openedErr != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(info, current) || !os.SameFile(current, opened) {
		_ = directory.Close()
		return errors.New("cleanup directory changed while it was opened")
	}
	entries, err := readRootDirectoryEntries(directory)
	if err != nil {
		_ = directory.Close()
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			_ = directory.Close()
			return err
		}
		if err := removeRootRelativeTreeEntry(ctx, directory, entry.Name(), persist, false); err != nil {
			_ = directory.Close()
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		_ = directory.Close()
		return err
	}
	if err := persist(directory, "."); err != nil {
		_ = directory.Close()
		return err
	}
	current, currentErr = parent.Lstat(name)
	if currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(opened, current) {
		_ = directory.Close()
		return errors.New("cleanup directory changed before final removal")
	}
	if err := directory.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, currentErr = parent.Lstat(name)
	if currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(opened, current) {
		return errors.New("cleanup directory changed after it was closed")
	}
	if err := parent.Remove(name); err != nil {
		return err
	}
	if persistParent {
		return persist(parent, ".")
	}
	return nil
}

func verifyRequestedWorkspaceAbsent(
	root *os.Root,
	layout workspaceCleanupLayout,
	request WorkspaceCleanupRequest,
	persist workspaceCleanupDirectorySync,
) error {
	payloadExists, err := inspectRootRelativeDirectory(root, layout.PayloadRelative)
	if err != nil {
		return permanentWorkspaceCleanupError(
			"workspace_cleanup_unsafe_quarantine", "The Workspace quarantine became unsafe after deletion.", err,
		)
	}
	if payloadExists {
		return retryableWorkspaceCleanupError(
			"workspace_cleanup_retryable", "The quarantined Workspace still exists.", nil,
		)
	}
	if err := persistRootRelativeAbsence(root, layout.PayloadRelative, persist); err != nil {
		return retryableWorkspaceCleanupError(
			"workspace_cleanup_durability_failed", "The Workspace quarantine deletion was not durable.", err,
		)
	}
	sourceExists, err := inspectRootRelativeDirectory(root, layout.SourceRelative)
	if err != nil {
		return permanentWorkspaceCleanupError(
			"workspace_cleanup_unsafe_path", "The Workspace path became unsafe after deletion.", err,
		)
	}
	if !sourceExists {
		if err := persistRootRelativeAbsence(root, layout.SourceRelative, persist); err != nil {
			return retryableWorkspaceCleanupError(
				"workspace_cleanup_durability_failed", "The Workspace source removal was not durable.", err,
			)
		}
		return nil
	}
	manifest, err := readWorkspaceManifestAt(root, filepath.Join(layout.SourceRelative, "manifest.json"))
	if err != nil {
		return permanentWorkspaceCleanupError(
			"workspace_cleanup_identity_mismatch", "A Workspace reappeared without a verifiable incarnation.", err,
		)
	}
	if compareWorkspaceCleanupManifest(manifest, request) == workspaceCleanupManifestStaleIncarnation {
		return nil
	}
	return permanentWorkspaceCleanupError(
		"workspace_cleanup_identity_mismatch", "The requested Workspace incarnation still exists after cleanup.", nil,
	)
}

func finishWorkspaceCleanupClaim(
	ctx context.Context,
	root *os.Root,
	layout workspaceCleanupLayout,
	persist workspaceCleanupDirectorySync,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	claimExists, err := inspectRootRelativeDirectory(root, layout.ClaimRelative)
	if err != nil {
		return err
	}
	if !claimExists {
		return persistRootRelativeAbsence(root, layout.ClaimRelative, persist)
	}
	if payloadExists, err := inspectRootRelativeDirectory(root, layout.PayloadRelative); err != nil {
		return err
	} else if payloadExists {
		return errors.New("quarantined Workspace still exists")
	}
	if err := persistRootRelativeAbsence(root, layout.PayloadRelative, persist); err != nil {
		return err
	}
	for _, relative := range []string{layout.PendingMarkerRelative, layout.MarkerRelative} {
		if err := root.Remove(relative); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := persist(root, layout.ClaimRelative); err != nil {
		return err
	}
	if err := root.Remove(layout.ClaimRelative); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := persistRootRelativeAbsence(root, layout.ClaimRelative, persist); err != nil {
		return err
	}
	claimExists, err = inspectRootRelativeDirectory(root, layout.ClaimRelative)
	if err != nil {
		return err
	}
	if claimExists {
		return errors.New("cleanup quarantine claim still exists")
	}
	return nil
}

func permanentWorkspaceCleanupError(code, message string, cause error) *WorkspaceCleanupError {
	return &WorkspaceCleanupError{Code: code, Message: message, Retryable: false, cause: cause}
}

func retryableWorkspaceCleanupError(code, message string, cause error) *WorkspaceCleanupError {
	return &WorkspaceCleanupError{Code: code, Message: message, Retryable: true, cause: cause}
}
