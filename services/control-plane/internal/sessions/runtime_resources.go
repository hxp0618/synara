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
	RestoreCheckpointID *uuid.UUID
}

func (s *Service) ensureRuntimeResources(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
) (sessionRuntimeResources, error) {
	binding, err := s.ensureRuntimeBinding(ctx, tx, session)
	if err != nil {
		return sessionRuntimeResources{}, err
	}
	workspace, err := s.ensureRemoteWorkspace(ctx, tx, session)
	if err != nil {
		return sessionRuntimeResources{}, err
	}
	return sessionRuntimeResources{
		BindingID: binding.ID, WorkspaceID: workspace.ID,
		RestoreCheckpointID: workspace.CurrentCheckpointID,
	}, nil
}

func (s *Service) ensureRuntimeBinding(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
) (persistence.ProviderRuntimeBinding, error) {
	var binding persistence.ProviderRuntimeBinding
	query := tx.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND provider = ? AND status = ?",
			session.TenantID, session.ID, session.Provider, "active").
		Order("revision DESC, id DESC").Take(&binding)
	if query.Error != nil && !errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return persistence.ProviderRuntimeBinding{}, problem.Wrap(500, "runtime_binding_load_failed", "Failed to load the Provider runtime binding.", query.Error)
	}
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
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

func (s *Service) ensureRemoteWorkspace(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
) (persistence.RemoteWorkspace, error) {
	var workspace persistence.RemoteWorkspace
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND session_id = ?", session.TenantID, session.ID).
		Take(&workspace).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.RemoteWorkspace{}, problem.Wrap(500, "remote_workspace_load_failed", "Failed to load the Session Workspace.", err)
	}
	var project persistence.Project
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "organization_id", "repository_url", "default_branch").
		Where("tenant_id = ? AND id = ?", session.TenantID, session.ProjectID).Take(&project).Error; err != nil {
		return persistence.RemoteWorkspace{}, problem.Wrap(500, "remote_workspace_project_load_failed", "Failed to load the Workspace Project.", err)
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
			return persistence.RemoteWorkspace{}, problem.Wrap(500, "remote_workspace_create_failed", "Failed to create the Session Workspace.", err)
		}
		return workspace, nil
	}
	updates := map[string]any{}
	if workspace.ExecutionTargetID != session.ExecutionTargetID {
		updates["execution_target_id"] = session.ExecutionTargetID
		updates["state"] = "recovering"
		workspace.ExecutionTargetID = session.ExecutionTargetID
		workspace.State = "recovering"
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
		updates["updated_at"] = time.Now().UTC()
		if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", workspace.TenantID, workspace.ID).
			Updates(updates).Error; err != nil {
			return persistence.RemoteWorkspace{}, problem.Wrap(500, "remote_workspace_update_failed", "Failed to update the Session Workspace binding.", err)
		}
	}
	return workspace, nil
}
