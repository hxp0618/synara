package executions

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Service) loadWorkload(ctx context.Context, tx *gorm.DB, execution persistence.AgentExecution) (Workload, error) {
	var row struct {
		TenantID                              uuid.UUID  `gorm:"column:tenant_id"`
		OrganizationID                        uuid.UUID  `gorm:"column:organization_id"`
		ProjectID                             uuid.UUID  `gorm:"column:project_id"`
		SessionID                             uuid.UUID  `gorm:"column:session_id"`
		TurnID                                uuid.UUID  `gorm:"column:turn_id"`
		SessionTitle                          string     `gorm:"column:session_title"`
		Provider                              string     `gorm:"column:provider"`
		ProviderRuntimeBindingID              *uuid.UUID `gorm:"column:provider_runtime_binding_id"`
		RemoteWorkspaceID                     *uuid.UUID `gorm:"column:remote_workspace_id"`
		WorkspaceMaterializationID            *uuid.UUID `gorm:"column:workspace_materialization_id"`
		WorkspaceMaterializationIncarnationID *uuid.UUID `gorm:"column:workspace_materialization_incarnation_id"`
		WorkspaceLayoutVersion                int        `gorm:"column:workspace_layout_version"`
		RestoreCheckpointID                   *uuid.UUID `gorm:"column:restore_checkpoint_id"`
		WorkspaceCurrentCheckpointID          *uuid.UUID `gorm:"column:workspace_current_checkpoint_id"`
		WorkspaceRepositoryFingerprint        *string    `gorm:"column:workspace_repository_fingerprint"`
		WorkspaceCurrentBranch                *string    `gorm:"column:workspace_current_branch"`
		WorkspaceBaseCommit                   *string    `gorm:"column:workspace_base_commit"`
		WorkspaceHeadCommit                   *string    `gorm:"column:workspace_head_commit"`
		WorkerManifestID                      *uuid.UUID `gorm:"column:worker_manifest_id"`
		Model                                 *string    `gorm:"column:model"`
		ProviderCredentialID                  *uuid.UUID `gorm:"column:provider_credential_id"`
		GitCredentialID                       *uuid.UUID `gorm:"column:git_credential_id"`
		InputText                             string     `gorm:"column:input_text"`
		RuntimeMode                           string     `gorm:"column:runtime_mode"`
		InteractionMode                       string     `gorm:"column:interaction_mode"`
		RepositoryURL                         *string    `gorm:"column:repository_url"`
		DefaultBranch                         string     `gorm:"column:default_branch"`
	}
	err := tx.WithContext(ctx).Table("agent_executions AS e").
		Select(`e.tenant_id, s.organization_id, s.project_id, e.session_id, e.turn_id,
			s.title AS session_title, COALESCE(e.provider, s.provider) AS provider,
			e.provider_runtime_binding_id, e.remote_workspace_id, e.workspace_materialization_id,
			materialization.incarnation_id AS workspace_materialization_incarnation_id,
			COALESCE(materialization.layout_version, 0) AS workspace_layout_version,
			e.restore_checkpoint_id, w.current_checkpoint_id AS workspace_current_checkpoint_id,
			w.repository_fingerprint AS workspace_repository_fingerprint,
			w.current_branch AS workspace_current_branch, w.base_commit AS workspace_base_commit,
			w.head_commit AS workspace_head_commit, e.worker_manifest_id,
				s.model, s.provider_credential_id, p.git_credential_id,
				t.input_text, t.runtime_mode, t.interaction_mode, p.repository_url, p.default_branch`).
		Joins("JOIN agent_sessions AS s ON s.tenant_id = e.tenant_id AND s.id = e.session_id").
		Joins("JOIN agent_turns AS t ON t.tenant_id = e.tenant_id AND t.session_id = e.session_id AND t.id = e.turn_id").
		Joins("JOIN projects AS p ON p.tenant_id = s.tenant_id AND p.id = s.project_id").
		Joins("LEFT JOIN remote_workspaces AS w ON w.tenant_id = e.tenant_id AND w.id = e.remote_workspace_id").
		Joins("LEFT JOIN workspace_materializations AS materialization ON materialization.tenant_id = e.tenant_id AND materialization.id = e.workspace_materialization_id").
		Where("e.tenant_id = ? AND e.id = ?", execution.TenantID, execution.ID).
		Take(&row).Error
	if err != nil {
		return Workload{}, problem.Wrap(500, "execution_workload_load_failed", "Failed to load the execution workload.", err)
	}
	var restoreCheckpoint *WorkspaceCheckpoint
	if row.RestoreCheckpointID != nil {
		restoreCheckpoint, err = loadReadyResumeCheckpoint(
			ctx, tx, execution, row.RemoteWorkspaceID, *row.RestoreCheckpointID,
		)
		if err != nil {
			return Workload{}, err
		}
	}
	snapshotCheckpoint := restoreCheckpoint
	if snapshotCheckpoint == nil && row.WorkspaceCurrentCheckpointID != nil {
		snapshotCheckpoint, err = loadReadyResumeCheckpoint(
			ctx, tx, execution, row.RemoteWorkspaceID, *row.WorkspaceCurrentCheckpointID,
		)
		if err != nil {
			return Workload{}, err
		}
	}
	resumeSnapshot, err := s.loadResumeSnapshot(ctx, tx, execution, resumeSnapshotContext{
		Provider:                              row.Provider,
		Model:                                 row.Model,
		RuntimeMode:                           row.RuntimeMode,
		InteractionMode:                       row.InteractionMode,
		RemoteWorkspaceID:                     row.RemoteWorkspaceID,
		WorkspaceMaterializationID:            row.WorkspaceMaterializationID,
		WorkspaceMaterializationIncarnationID: row.WorkspaceMaterializationIncarnationID,
		WorkspaceLayoutVersion:                row.WorkspaceLayoutVersion,
		WorkspaceRepositoryFingerprint:        row.WorkspaceRepositoryFingerprint,
		WorkspaceDefaultBranch:                row.DefaultBranch,
		WorkspaceCurrentBranch:                row.WorkspaceCurrentBranch,
		WorkspaceBaseCommit:                   row.WorkspaceBaseCommit,
		WorkspaceHeadCommit:                   row.WorkspaceHeadCommit,
		Checkpoint:                            snapshotCheckpoint,
	})
	if err != nil {
		return Workload{}, err
	}
	return Workload{
		TenantID: row.TenantID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID,
		SessionID: row.SessionID, TurnID: row.TurnID, SessionTitle: row.SessionTitle,
		Provider: row.Provider, ProviderRuntimeBindingID: row.ProviderRuntimeBindingID,
		RemoteWorkspaceID: row.RemoteWorkspaceID, RestoreCheckpointID: row.RestoreCheckpointID,
		WorkspaceMaterializationID:            row.WorkspaceMaterializationID,
		WorkspaceMaterializationIncarnationID: row.WorkspaceMaterializationIncarnationID,
		WorkspaceLayoutVersion:                row.WorkspaceLayoutVersion,
		RestoreCheckpoint:                     restoreCheckpoint,
		WorkspaceRepositoryFingerprint:        row.WorkspaceRepositoryFingerprint,
		WorkerManifestID:                      row.WorkerManifestID,
		Model:                                 row.Model, ProviderCredentialID: row.ProviderCredentialID,
		GitCredentialID: row.GitCredentialID, InputText: row.InputText,
		RuntimeMode: row.RuntimeMode, InteractionMode: row.InteractionMode,
		RepositoryURL: row.RepositoryURL, DefaultBranch: row.DefaultBranch,
		ConversationHistory: conversationHistoryFromResumeSnapshot(resumeSnapshot),
		ResumeSnapshot:      &resumeSnapshot,
	}, nil
}

func loadReadyResumeCheckpoint(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workspaceID *uuid.UUID,
	checkpointID uuid.UUID,
) (*WorkspaceCheckpoint, error) {
	if workspaceID == nil {
		return nil, problem.New(409, "restore_checkpoint_unavailable", "The Execution restore Checkpoint does not have a logical Workspace.")
	}
	var checkpoint persistence.WorkspaceCheckpoint
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND id = ? AND workspace_id = ? AND session_id = ? AND status = ?",
			execution.TenantID, checkpointID, *workspaceID, execution.SessionID, "ready").
		Take(&checkpoint).Error; err != nil {
		return nil, problem.Wrap(409, "restore_checkpoint_unavailable", "The authoritative Workspace Checkpoint is not ready or no longer available.", err)
	}
	converted := toWorkspaceCheckpoint(checkpoint)
	return &converted, nil
}
