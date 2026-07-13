package executions

import (
	"context"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

var workspaceHashPattern = regexp.MustCompile(`^[0-9a-f]{7,128}$`)

func (s *Service) MarkWorkspaceReady(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input WorkspaceReadyInput,
	requestID string,
) (OperationResult[WorkspaceState], error) {
	if err := validateWorkspaceReadyInput(input); err != nil {
		return OperationResult[WorkspaceState]{}, err
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "workspace.ready", map[string]any{
		"executionId": executionID, "tenantId": input.TenantID, "generation": input.Generation,
		"repositoryFingerprint": input.RepositoryFingerprint, "currentBranch": input.CurrentBranch,
		"baseCommit": input.BaseCommit, "headCommit": input.HeadCommit,
		"restoredCheckpointId": input.RestoredCheckpointID,
	}, 200, func(tx *gorm.DB) (WorkspaceState, error) {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return WorkspaceState{}, err
		}
		workspace, err := lockExecutionWorkspace(ctx, tx, execution)
		if err != nil {
			return WorkspaceState{}, err
		}
		if input.RestoredCheckpointID != nil {
			if execution.RestoreCheckpointID == nil || *execution.RestoreCheckpointID != *input.RestoredCheckpointID {
				return WorkspaceState{}, problem.New(409, "restore_checkpoint_not_bound", "The restored Checkpoint is not bound to the current Execution.")
			}
			var checkpoint persistence.WorkspaceCheckpoint
			if err := tx.WithContext(ctx).
				Where("tenant_id = ? AND workspace_id = ? AND id = ? AND status = ?",
					execution.TenantID, workspace.ID, *input.RestoredCheckpointID, "ready").
				Take(&checkpoint).Error; err != nil {
				return WorkspaceState{}, problem.Wrap(409, "restore_checkpoint_unavailable", "The restored Checkpoint is not ready.", err)
			}
		}
		now := s.now()
		updates := map[string]any{
			"state": "ready", "repository_fingerprint": input.RepositoryFingerprint,
			"current_branch": input.CurrentBranch, "base_commit": input.BaseCommit,
			"head_commit": input.HeadCommit, "last_used_at": now, "updated_at": now,
		}
		updated := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND last_worker_id = ? AND last_execution_id = ? AND last_generation = ? AND state IN ?",
				execution.TenantID, workspace.ID, worker.ID, execution.ID, execution.Generation,
				[]string{"preparing", "recovering", "ready"}).Updates(updates)
		if err := expectOne(updated, 409, "workspace_preparation_conflict", "The Workspace preparation belongs to an obsolete Worker Generation."); err != nil {
			return WorkspaceState{}, err
		}
		workspace.State = "ready"
		workspace.RepositoryFingerprint = input.RepositoryFingerprint
		workspace.CurrentBranch = input.CurrentBranch
		workspace.BaseCommit = input.BaseCommit
		workspace.HeadCommit = input.HeadCommit
		workspace.LastUsedAt = &now
		workspace.UpdatedAt = now
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "workspace.ready", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "workspaceId": workspace.ID,
				"repositoryFingerprint": input.RepositoryFingerprint, "currentBranch": input.CurrentBranch,
				"baseCommit": input.BaseCommit, "headCommit": input.HeadCommit,
				"restoredCheckpointId": input.RestoredCheckpointID,
			},
		})
		if err != nil {
			return WorkspaceState{}, err
		}
		return toWorkspaceState(workspace), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) MarkWorkspaceFailed(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input WorkspaceFailedInput,
	requestID string,
) (OperationResult[WorkspaceState], error) {
	input.FailureCode = strings.TrimSpace(input.FailureCode)
	input.FailureMessage = strings.TrimSpace(input.FailureMessage)
	if input.FailureCode == "" || len(input.FailureCode) > 160 || strings.ContainsAny(input.FailureCode, "\r\n\t") {
		return OperationResult[WorkspaceState]{}, problem.New(400, "invalid_workspace_failure", "failureCode is invalid.")
	}
	if len(input.FailureMessage) > 10_000 {
		return OperationResult[WorkspaceState]{}, problem.New(400, "invalid_workspace_failure", "failureMessage must not exceed 10000 characters.")
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "workspace.failed", map[string]any{
		"executionId": executionID, "tenantId": input.TenantID, "generation": input.Generation,
		"failureCode": input.FailureCode, "failureMessage": input.FailureMessage,
	}, 200, func(tx *gorm.DB) (WorkspaceState, error) {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return WorkspaceState{}, err
		}
		workspace, err := lockExecutionWorkspace(ctx, tx, execution)
		if err != nil {
			return WorkspaceState{}, err
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND last_worker_id = ? AND last_execution_id = ? AND last_generation = ? AND state IN ?",
				execution.TenantID, workspace.ID, worker.ID, execution.ID, execution.Generation,
				[]string{"preparing", "recovering", "failed"}).
			Updates(map[string]any{"state": "failed", "last_used_at": now, "updated_at": now})
		if err := expectOne(updated, 409, "workspace_preparation_conflict", "The Workspace failure belongs to an obsolete Worker Generation."); err != nil {
			return WorkspaceState{}, err
		}
		workspace.State = "failed"
		workspace.LastUsedAt = &now
		workspace.UpdatedAt = now
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "workspace.failed", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "workspaceId": workspace.ID,
				"failureCode": input.FailureCode, "failureMessage": input.FailureMessage,
			},
		})
		if err != nil {
			return WorkspaceState{}, err
		}
		return toWorkspaceState(workspace), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) MarkWorkspaceDirty(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input WorkspaceDirtyInput,
	requestID string,
) (OperationResult[WorkspaceState], error) {
	if err := validateWorkspaceDirtyInput(input); err != nil {
		return OperationResult[WorkspaceState]{}, err
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "workspace.dirty", map[string]any{
		"executionId": executionID, "tenantId": input.TenantID, "generation": input.Generation,
		"currentBranch": input.CurrentBranch, "headCommit": input.HeadCommit,
	}, 200, func(tx *gorm.DB) (WorkspaceState, error) {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return WorkspaceState{}, err
		}
		workspace, err := lockExecutionWorkspace(ctx, tx, execution)
		if err != nil {
			return WorkspaceState{}, err
		}
		now := s.now()
		updates := map[string]any{"state": "dirty", "last_used_at": now, "updated_at": now}
		if input.CurrentBranch != nil {
			updates["current_branch"] = input.CurrentBranch
		}
		if input.HeadCommit != nil {
			updates["head_commit"] = input.HeadCommit
		}
		updated := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND last_worker_id = ? AND last_execution_id = ? AND last_generation = ? AND state IN ?",
				execution.TenantID, workspace.ID, worker.ID, execution.ID, execution.Generation,
				[]string{"ready", "dirty"}).Updates(updates)
		if err := expectOne(updated, 409, "workspace_dirty_conflict", "The Workspace change belongs to an obsolete Worker Generation."); err != nil {
			return WorkspaceState{}, err
		}
		workspace.State = "dirty"
		if input.CurrentBranch != nil {
			workspace.CurrentBranch = input.CurrentBranch
		}
		if input.HeadCommit != nil {
			workspace.HeadCommit = input.HeadCommit
		}
		workspace.LastUsedAt = &now
		workspace.UpdatedAt = now
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "workspace.dirty", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "workspaceId": workspace.ID,
				"currentBranch": input.CurrentBranch, "headCommit": input.HeadCommit,
			},
		})
		if err != nil {
			return WorkspaceState{}, err
		}
		return toWorkspaceState(workspace), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func validateWorkspaceReadyInput(input WorkspaceReadyInput) error {
	for name, value := range map[string]*string{
		"repositoryFingerprint": input.RepositoryFingerprint,
		"baseCommit":            input.BaseCommit,
		"headCommit":            input.HeadCommit,
	} {
		if value != nil && !workspaceHashPattern.MatchString(strings.TrimSpace(*value)) {
			return problem.New(400, "invalid_workspace_metadata", name+" is invalid.")
		}
	}
	if input.RepositoryFingerprint != nil && len(strings.TrimSpace(*input.RepositoryFingerprint)) != 64 {
		return problem.New(400, "invalid_workspace_metadata", "repositoryFingerprint is invalid.")
	}
	if input.CurrentBranch != nil {
		branch := strings.TrimSpace(*input.CurrentBranch)
		if _, err := gitpolicy.NormalizeBranch(branch, ""); err != nil {
			return problem.New(400, "invalid_workspace_metadata", "currentBranch is invalid.")
		}
	}
	return nil
}

func validateWorkspaceDirtyInput(input WorkspaceDirtyInput) error {
	if input.HeadCommit != nil && !workspaceHashPattern.MatchString(strings.TrimSpace(*input.HeadCommit)) {
		return problem.New(400, "invalid_workspace_metadata", "headCommit is invalid.")
	}
	if input.CurrentBranch != nil {
		if _, err := gitpolicy.NormalizeBranch(strings.TrimSpace(*input.CurrentBranch), ""); err != nil {
			return problem.New(400, "invalid_workspace_metadata", "currentBranch is invalid.")
		}
	}
	return nil
}

func lockExecutionWorkspace(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (persistence.RemoteWorkspace, error) {
	if execution.RemoteWorkspaceID == nil {
		return persistence.RemoteWorkspace{}, problem.New(409, "workspace_not_bound", "The Execution does not have a logical Workspace binding.")
	}
	var workspace persistence.RemoteWorkspace
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ? AND session_id = ? AND execution_target_id = ?",
			execution.TenantID, *execution.RemoteWorkspaceID, execution.SessionID, execution.ExecutionTargetID).
		Take(&workspace).Error; err != nil {
		return persistence.RemoteWorkspace{}, problem.Wrap(409, "workspace_not_bound", "The logical Workspace is unavailable for this Execution.", err)
	}
	return workspace, nil
}

func toWorkspaceState(model persistence.RemoteWorkspace) WorkspaceState {
	return WorkspaceState{
		ID: model.ID, State: model.State, RepositoryFingerprint: model.RepositoryFingerprint,
		CurrentBranch: model.CurrentBranch, BaseCommit: model.BaseCommit, HeadCommit: model.HeadCommit,
		LastWorkerID: model.LastWorkerID, LastExecutionID: model.LastExecutionID,
		LastGeneration: model.LastGeneration, UpdatedAt: model.UpdatedAt,
	}
}
