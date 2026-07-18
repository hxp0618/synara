package sessions

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type sessionRuntimeResources struct {
	BindingID           uuid.UUID
	WorkspaceID         uuid.UUID
	MaterializationID   uuid.UUID
	IncarnationID       uuid.UUID
	LayoutVersion       int
	RestoreCheckpointID *uuid.UUID
}

type RuntimeResources = sessionRuntimeResources

func (s *Service) EnsureRuntimeResources(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
) (RuntimeResources, error) {
	return s.ensureRuntimeResources(ctx, tx, session)
}

const currentWorkspaceLayoutVersion = 3

func (s *Service) ensureRuntimeResources(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
) (sessionRuntimeResources, error) {
	binding, err := s.ensureRuntimeBinding(ctx, tx, session)
	if err != nil {
		return sessionRuntimeResources{}, err
	}
	workspace, materialization, err := s.ensureRemoteWorkspace(ctx, tx, session)
	if err != nil {
		return sessionRuntimeResources{}, err
	}
	restoreCheckpointID, err := readyWorkspaceCheckpointID(ctx, tx, workspace)
	if err != nil {
		return sessionRuntimeResources{}, err
	}
	return sessionRuntimeResources{
		BindingID: binding.ID, WorkspaceID: workspace.ID,
		MaterializationID: materialization.ID, IncarnationID: materialization.IncarnationID,
		LayoutVersion: materialization.LayoutVersion, RestoreCheckpointID: restoreCheckpointID,
	}, nil
}

func (s *Service) ensureRuntimeBinding(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
) (persistence.ProviderRuntimeBinding, error) {
	binding, found, err := s.LoadActiveRuntimeBinding(
		ctx, tx, session.TenantID, session.ID, session.Provider,
	)
	if err != nil {
		return persistence.ProviderRuntimeBinding{}, err
	}
	if !found {
		var maximum int
		if err := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
			Where("tenant_id = ? AND session_id = ?", session.TenantID, session.ID).
			Select("COALESCE(MAX(revision), 0)").Scan(&maximum).Error; err != nil {
			return persistence.ProviderRuntimeBinding{}, problem.Wrap(500, "runtime_binding_revision_failed", "Failed to allocate a Provider runtime binding revision.", err)
		}
		now := time.Now().UTC()
		binding = persistence.ProviderRuntimeBinding{
			ID: uuid.New(), TenantID: session.TenantID, SessionID: session.ID, Provider: session.Provider,
			Revision: maximum + 1, Status: "active", ResumeStrategy: "authoritative-history",
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&binding).Error; err != nil {
			return persistence.ProviderRuntimeBinding{}, problem.Wrap(500, "runtime_binding_create_failed", "Failed to create the Provider runtime binding.", err)
		}
	}
	if session.CurrentRuntimeBindingID == nil || *session.CurrentRuntimeBindingID != binding.ID {
		if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", session.TenantID, session.ID).
			Update("current_runtime_binding_id", binding.ID).Error; err != nil {
			return persistence.ProviderRuntimeBinding{}, problem.Wrap(500, "runtime_binding_attach_failed", "Failed to attach the Provider runtime binding to the Session.", err)
		}
		session.CurrentRuntimeBindingID = &binding.ID
	}
	return binding, nil
}

func (s *Service) LoadActiveRuntimeBinding(
	ctx context.Context,
	db *gorm.DB,
	tenantID, sessionID uuid.UUID,
	provider string,
) (persistence.ProviderRuntimeBinding, bool, error) {
	var binding persistence.ProviderRuntimeBinding
	err := db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND provider = ? AND status = ?",
			tenantID, sessionID, provider, "active").
		Order("revision DESC, id DESC").Take(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ProviderRuntimeBinding{}, false, nil
	}
	if err != nil {
		return persistence.ProviderRuntimeBinding{}, false,
			problem.Wrap(500, "runtime_binding_load_failed", "Failed to load the Provider runtime binding.", err)
	}
	return binding, true, nil
}

func (s *Service) ensureRemoteWorkspace(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
) (persistence.RemoteWorkspace, persistence.WorkspaceMaterialization, error) {
	var workspace persistence.RemoteWorkspace
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND session_id = ?", session.TenantID, session.ID).
		Take(&workspace).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, problem.Wrap(500, "remote_workspace_load_failed", "Failed to load the Session Workspace.", err)
	}
	var project persistence.Project
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "organization_id", "repository_url", "default_branch").
		Where("tenant_id = ? AND id = ?", session.TenantID, session.ProjectID).Take(&project).Error; err != nil {
		return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, problem.Wrap(500, "remote_workspace_project_load_failed", "Failed to load the Workspace Project.", err)
	}
	var target persistence.ExecutionTarget
	if err := tx.WithContext(ctx).Select("id", "kind").
		Where("id = ?", session.ExecutionTargetID).Take(&target).Error; err != nil {
		return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, problem.Wrap(500, "remote_workspace_target_load_failed", "Failed to load the Workspace Execution Target.", err)
	}
	mode := "empty"
	if project.RepositoryURL != nil && strings.TrimSpace(*project.RepositoryURL) != "" {
		mode = "clone"
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		now := time.Now().UTC()
		workspace = persistence.RemoteWorkspace{
			ID: uuid.New(), TenantID: session.TenantID, OrganizationID: session.OrganizationID,
			ProjectID: session.ProjectID, SessionID: session.ID, ExecutionTargetID: session.ExecutionTargetID,
			WorkspaceMode: mode, State: "pending", DefaultBranch: project.DefaultBranch,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&workspace).Error; err != nil {
			return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, problem.Wrap(500, "remote_workspace_create_failed", "Failed to create the Session Workspace.", err)
		}
		materialization, err := createWorkspaceMaterialization(ctx, tx, workspace, target, now)
		if err != nil {
			return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, err
		}
		if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", workspace.TenantID, workspace.ID).
			Update("current_materialization_id", materialization.ID).Error; err != nil {
			return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, problem.Wrap(500, "workspace_materialization_attach_failed", "Failed to attach the physical Workspace incarnation.", err)
		}
		workspace.CurrentMaterializationID = &materialization.ID
		return workspace, materialization, nil
	}
	now := time.Now().UTC()
	materialization, hasMaterialization, err := loadCurrentWorkspaceMaterialization(ctx, tx, workspace)
	if err != nil {
		return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, err
	}
	updates := map[string]any{}
	targetMoved := workspace.ExecutionTargetID != session.ExecutionTargetID
	replaceMaterialization := false
	retireReason := ""
	switch {
	case targetMoved:
		replaceMaterialization = true
		if hasMaterialization {
			retireReason = "target-move"
		}
	case !hasMaterialization || materialization.State != "active":
		replaceMaterialization = true
	case workspacePodPlacementRequiresReplacement(materialization):
		replaceMaterialization = true
		retireReason = "worker-instance-replaced"
	}
	if replaceMaterialization {
		if err := requireRecoverableDirtyWorkspace(ctx, tx, workspace); err != nil {
			return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, err
		}
		if retireReason != "" {
			if err := retireWorkspaceMaterialization(ctx, tx, &materialization, retireReason, now); err != nil {
				return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, err
			}
		}
		materialization, err = createWorkspaceMaterialization(ctx, tx, workspace, target, now)
		if err != nil {
			return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, err
		}
		updates["state"] = "recovering"
		updates["current_materialization_id"] = materialization.ID
		updates["cleaned_at"] = nil
		workspace.State = "recovering"
		workspace.CurrentMaterializationID = &materialization.ID
		workspace.CleanedAt = nil
		if targetMoved {
			updates["execution_target_id"] = session.ExecutionTargetID
			workspace.ExecutionTargetID = session.ExecutionTargetID
		}
	}
	if workspace.WorkspaceMode != mode {
		updates["workspace_mode"] = mode
		workspace.WorkspaceMode = mode
	}
	if workspace.DefaultBranch != project.DefaultBranch {
		updates["default_branch"] = project.DefaultBranch
		workspace.DefaultBranch = project.DefaultBranch
	}
	if len(updates) > 0 {
		updates["updated_at"] = now
		if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", workspace.TenantID, workspace.ID).
			Updates(updates).Error; err != nil {
			return persistence.RemoteWorkspace{}, persistence.WorkspaceMaterialization{}, problem.Wrap(500, "remote_workspace_update_failed", "Failed to update the Session Workspace binding.", err)
		}
		workspace.UpdatedAt = now
	}
	return workspace, materialization, nil
}

func createWorkspaceMaterialization(
	ctx context.Context,
	tx *gorm.DB,
	workspace persistence.RemoteWorkspace,
	target persistence.ExecutionTarget,
	now time.Time,
) (persistence.WorkspaceMaterialization, error) {
	storageScope := "target"
	if target.Kind == "kubernetes" {
		storageScope = "pod"
	}
	materialization := persistence.WorkspaceMaterialization{
		ID: uuid.New(), TenantID: workspace.TenantID, WorkspaceID: workspace.ID,
		OrganizationID: workspace.OrganizationID, ProjectID: workspace.ProjectID, SessionID: workspace.SessionID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, StorageScope: storageScope,
		LayoutVersion: currentWorkspaceLayoutVersion, IncarnationID: uuid.New(), State: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.WithContext(ctx).Create(&materialization).Error; err != nil {
		return persistence.WorkspaceMaterialization{}, problem.Wrap(500, "workspace_materialization_create_failed", "Failed to create the physical Workspace incarnation.", err)
	}
	return materialization, nil
}

func loadCurrentWorkspaceMaterialization(
	ctx context.Context,
	tx *gorm.DB,
	workspace persistence.RemoteWorkspace,
) (persistence.WorkspaceMaterialization, bool, error) {
	if workspace.CurrentMaterializationID == nil {
		return persistence.WorkspaceMaterialization{}, false, nil
	}
	var materialization persistence.WorkspaceMaterialization
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND workspace_id = ? AND id = ?", workspace.TenantID, workspace.ID, *workspace.CurrentMaterializationID).
		Take(&materialization).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkspaceMaterialization{}, false, problem.New(500, "workspace_materialization_missing", "The current physical Workspace incarnation is missing.")
	}
	if err != nil {
		return persistence.WorkspaceMaterialization{}, false, problem.Wrap(500, "workspace_materialization_load_failed", "Failed to load the physical Workspace incarnation.", err)
	}
	if materialization.ExecutionTargetID != workspace.ExecutionTargetID {
		return persistence.WorkspaceMaterialization{}, false, problem.New(500, "workspace_materialization_target_mismatch", "The current physical Workspace incarnation belongs to another Execution Target.")
	}
	return materialization, true, nil
}

func workspacePodPlacementRequiresReplacement(materialization persistence.WorkspaceMaterialization) bool {
	if materialization.StorageScope != "pod" || materialization.WorkerInstanceUID != nil {
		return false
	}
	return materialization.LayoutVersion != currentWorkspaceLayoutVersion ||
		materialization.WorkerID != nil || materialization.WorkerIncarnation != nil ||
		materialization.LastExecutionID != nil || materialization.LastGeneration != nil
}

func retireWorkspaceMaterialization(
	ctx context.Context,
	tx *gorm.DB,
	materialization *persistence.WorkspaceMaterialization,
	reason string,
	requestedAt time.Time,
) error {
	if materialization.State == "cleaned" || materialization.State == "cleanup-pending" || materialization.State == "cleaning" {
		return nil
	}
	updates := map[string]any{
		"cleanup_reason": reason, "cleanup_requested_at": requestedAt, "updated_at": requestedAt,
	}
	if materialization.State == "active" || materialization.State == "retired" {
		updates["state"] = "retired"
	}
	if err := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
		Where("tenant_id = ? AND id = ? AND incarnation_id = ?", materialization.TenantID, materialization.ID, materialization.IncarnationID).
		Updates(updates).Error; err != nil {
		return problem.Wrap(500, "workspace_materialization_retire_failed", "Failed to retire the previous physical Workspace incarnation.", err)
	}
	if _, ok := updates["state"]; ok {
		materialization.State = "retired"
	}
	materialization.CleanupReason = &reason
	materialization.CleanupRequestedAt = &requestedAt
	materialization.UpdatedAt = requestedAt
	return nil
}

func readyWorkspaceCheckpointID(
	ctx context.Context,
	tx *gorm.DB,
	workspace persistence.RemoteWorkspace,
) (*uuid.UUID, error) {
	if workspace.CurrentCheckpointID == nil {
		return nil, nil
	}
	var checkpoint persistence.WorkspaceCheckpoint
	err := tx.WithContext(ctx).Select("id").
		Where("tenant_id = ? AND workspace_id = ? AND session_id = ? AND id = ? AND status = ?",
			workspace.TenantID, workspace.ID, workspace.SessionID, *workspace.CurrentCheckpointID, "ready").
		Take(&checkpoint).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, problem.Wrap(500, "workspace_checkpoint_load_failed", "Failed to validate the current Workspace Checkpoint.", err)
	}
	return &checkpoint.ID, nil
}

func requireRecoverableDirtyWorkspace(
	ctx context.Context,
	tx *gorm.DB,
	workspace persistence.RemoteWorkspace,
) error {
	if workspace.State != "dirty" {
		return nil
	}
	if workspace.CurrentCheckpointID == nil || workspace.LastExecutionID == nil || workspace.LastGeneration == nil {
		return problem.New(
			409,
			"dirty_workspace_checkpoint_required",
			"The dirty Workspace requires a ready Checkpoint covering its latest Execution before changing physical placement.",
		)
	}
	var checkpoint persistence.WorkspaceCheckpoint
	err := tx.WithContext(ctx).Select("id").
		Where("tenant_id = ? AND workspace_id = ? AND session_id = ? AND id = ? AND status = ? AND execution_id = ? AND generation = ?",
			workspace.TenantID, workspace.ID, workspace.SessionID, *workspace.CurrentCheckpointID, "ready",
			*workspace.LastExecutionID, *workspace.LastGeneration,
		).Take(&checkpoint).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(
			409,
			"dirty_workspace_checkpoint_required",
			"The dirty Workspace requires a ready Checkpoint covering its latest Execution before changing physical placement.",
		)
	}
	if err != nil {
		return problem.Wrap(500, "workspace_checkpoint_load_failed", "Failed to validate the latest dirty Workspace Checkpoint.", err)
	}
	return nil
}

func scheduleArchivedWorkspaceCleanup(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	archivedAt time.Time,
	cleanupAfterDays *int,
	reason string,
) error {
	var workspace persistence.RemoteWorkspace
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND session_id = ?", tenantID, sessionID).
		Take(&workspace).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return problem.Wrap(500, "archived_workspace_load_failed", "Failed to load the archived Session Workspace.", err)
	}
	if cleanupAfterDays == nil {
		if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", tenantID, workspace.ID).
			Update("retention_until", nil).Error; err != nil {
			return problem.Wrap(500, "workspace_retention_update_failed", "Failed to clear the Workspace retention deadline.", err)
		}
		return nil
	}
	retentionUntil := archivedAt.AddDate(0, 0, *cleanupAfterDays)
	if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", tenantID, workspace.ID).
		Updates(map[string]any{"retention_until": retentionUntil, "updated_at": archivedAt}).Error; err != nil {
		return problem.Wrap(500, "workspace_retention_update_failed", "Failed to set the Workspace retention deadline.", err)
	}
	if workspace.CurrentMaterializationID == nil {
		return nil
	}
	result := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
		Where("tenant_id = ? AND workspace_id = ? AND id = ?", tenantID, workspace.ID, *workspace.CurrentMaterializationID).
		Where("state IN ?", []string{"active", "retired", "failed"}).
		Where("cleanup_requested_at IS NULL OR cleanup_requested_at > ?", retentionUntil).
		Updates(map[string]any{
			"cleanup_reason": reason, "cleanup_requested_at": retentionUntil, "updated_at": archivedAt,
		})
	if result.Error != nil {
		return problem.Wrap(500, "workspace_cleanup_intent_failed", "Failed to schedule physical Workspace cleanup.", result.Error)
	}
	return nil
}

func loadWorkspaceCleanupAfterDays(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
) (*int, error) {
	var policy persistence.TenantRetentionPolicy
	err := tx.WithContext(ctx).Select("tenant_id", "workspace_cleanup_after_days").
		Where("tenant_id = ?", tenantID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, problem.Wrap(500, "retention_policy_load_failed", "Failed to load the Workspace retention policy.", err)
	}
	return policy.WorkspaceCleanupAfterDays, nil
}
