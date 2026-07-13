package executions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

var terminalExecutionStatuses = []string{"completed", "failed", "cancelled", "interrupted"}

type WorkspaceCheckpointRetentionResult struct {
	RestoreReferencesCleared int
	CheckpointsExpired       int
}

func (s *Service) ApplyWorkspaceCheckpointRetention(
	ctx context.Context,
	tenantID uuid.UUID,
	cutoff, now time.Time,
	limit int,
) (WorkspaceCheckpointRetentionResult, error) {
	if limit <= 0 {
		limit = 200
	}
	result := WorkspaceCheckpointRetentionResult{}
	var terminalExecutionIDs []uuid.UUID
	if err := s.db.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Select("id").
		Where("tenant_id = ? AND restore_checkpoint_id IS NOT NULL AND status IN ?", tenantID, terminalExecutionStatuses).
		Order("COALESCE(finished_at, queued_at), id").Limit(limit).Scan(&terminalExecutionIDs).Error; err != nil {
		return result, problem.Wrap(500, "checkpoint_restore_references_load_failed", "Checkpoint retention could not load terminal restore references.", err)
	}
	if len(terminalExecutionIDs) > 0 {
		cleared := s.db.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id IN ? AND restore_checkpoint_id IS NOT NULL AND status IN ?",
				tenantID, terminalExecutionIDs, terminalExecutionStatuses).
			Update("restore_checkpoint_id", nil)
		if cleared.Error != nil {
			return result, problem.Wrap(500, "checkpoint_restore_references_clear_failed", "Checkpoint retention could not release terminal restore references.", cleared.Error)
		}
		result.RestoreReferencesCleared = int(cleared.RowsAffected)
	}

	var checkpointIDs []uuid.UUID
	if err := s.db.WithContext(ctx).Table("workspace_checkpoints AS checkpoint").
		Select("checkpoint.id").
		Joins("JOIN agent_sessions AS session ON session.tenant_id = checkpoint.tenant_id AND session.id = checkpoint.session_id").
		Where("checkpoint.tenant_id = ? AND checkpoint.status IN ?", tenantID, []string{"ready", "failed", "superseded"}).
		Where(`(
			(checkpoint.expires_at IS NOT NULL AND checkpoint.expires_at <= ?)
			OR (session.status = ? AND COALESCE(checkpoint.ready_at, checkpoint.failed_at, checkpoint.created_at) <= ?)
		)`, now, "archived", cutoff).
		Where(`NOT EXISTS (
			SELECT 1 FROM remote_workspaces workspace
			WHERE workspace.tenant_id = checkpoint.tenant_id
			  AND workspace.current_checkpoint_id = checkpoint.id
		)`).
		Where(`NOT EXISTS (
			SELECT 1 FROM agent_executions execution
			WHERE execution.tenant_id = checkpoint.tenant_id
			  AND execution.restore_checkpoint_id = checkpoint.id
		)`).
		Order("COALESCE(checkpoint.expires_at, checkpoint.ready_at, checkpoint.failed_at, checkpoint.created_at), checkpoint.id").
		Limit(limit).Scan(&checkpointIDs).Error; err != nil {
		return result, problem.Wrap(500, "checkpoint_retention_load_failed", "Checkpoint retention could not load eligible Checkpoints.", err)
	}
	for _, checkpointID := range checkpointIDs {
		updated := s.db.WithContext(ctx).Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ? AND status IN ?", tenantID, checkpointID, []string{"ready", "failed", "superseded"}).
			Where(`NOT EXISTS (
				SELECT 1 FROM remote_workspaces workspace
				WHERE workspace.tenant_id = workspace_checkpoints.tenant_id
				  AND workspace.current_checkpoint_id = workspace_checkpoints.id
			)`).
			Where(`NOT EXISTS (
				SELECT 1 FROM agent_executions execution
				WHERE execution.tenant_id = workspace_checkpoints.tenant_id
				  AND execution.restore_checkpoint_id = workspace_checkpoints.id
			)`).
			Update("status", "expired")
		if updated.Error != nil {
			return result, problem.Wrap(409, "checkpoint_retention_conflict", "Checkpoint retention conflicted with a new recovery reference.", updated.Error)
		}
		result.CheckpointsExpired += int(updated.RowsAffected)
	}
	return result, nil
}

func (s *Service) FailAbandonedWorkspaceCheckpoints(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	var candidates []persistence.WorkspaceCheckpoint
	if err := s.db.WithContext(ctx).Table("workspace_checkpoints AS checkpoint").
		Select("checkpoint.*").
		Where("checkpoint.status IN ?", []string{"pending", "uploading"}).
		Where(`NOT EXISTS (
			SELECT 1
			FROM worker_leases lease
			WHERE lease.tenant_id = checkpoint.tenant_id
			  AND lease.execution_id = checkpoint.execution_id
			  AND lease.generation = checkpoint.generation
			  AND lease.expires_at > ?
		)`, now).
		Order("checkpoint.created_at, checkpoint.id").Limit(limit).Scan(&candidates).Error; err != nil {
		return 0, problem.Wrap(500, "abandoned_checkpoints_load_failed", "Abandoned Workspace Checkpoints could not be loaded.", err)
	}
	failed := 0
	for _, candidate := range candidates {
		var appended persistence.SessionEvent
		transitioned := false
		err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
			var lease persistence.WorkerLease
			leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("tenant_id = ? AND execution_id = ? AND generation = ?",
					candidate.TenantID, candidate.ExecutionID, candidate.Generation).
				Take(&lease).Error
			if leaseErr != nil && !errors.Is(leaseErr, gorm.ErrRecordNotFound) {
				return leaseErr
			}
			var execution persistence.AgentExecution
			if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Select("id").Where("tenant_id = ? AND id = ?", candidate.TenantID, candidate.ExecutionID).
				Take(&execution).Error; err != nil {
				return err
			}
			var activeLeaseCount int64
			if err := tx.WithContext(ctx).Model(&persistence.WorkerLease{}).
				Where("tenant_id = ? AND execution_id = ? AND generation = ? AND expires_at > ?",
					candidate.TenantID, candidate.ExecutionID, candidate.Generation, now).
				Count(&activeLeaseCount).Error; err != nil {
				return err
			}
			if activeLeaseCount > 0 {
				return nil
			}
			var workspace persistence.RemoteWorkspace
			if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("tenant_id = ? AND id = ?", candidate.TenantID, candidate.WorkspaceID).
				Take(&workspace).Error; err != nil {
				return err
			}
			var checkpoint persistence.WorkspaceCheckpoint
			if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("tenant_id = ? AND id = ? AND status IN ?", candidate.TenantID, candidate.ID, []string{"pending", "uploading"}).
				Take(&checkpoint).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}
			const failureCode = "checkpoint_lease_inactive"
			const failureMessage = "The Worker lease ended before the Workspace Checkpoint became ready."
			updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCheckpoint{}).
				Where("tenant_id = ? AND id = ? AND status IN ?", checkpoint.TenantID, checkpoint.ID, []string{"pending", "uploading"}).
				Updates(map[string]any{
					"status": "failed", "failure_code": failureCode, "failure_message": failureMessage,
					"failed_at": now, "ready_at": nil,
				})
			if updated.Error != nil || updated.RowsAffected != 1 {
				return problem.Wrap(409, "abandoned_checkpoint_conflict", "The abandoned Workspace Checkpoint changed concurrently.", updated.Error)
			}
			workspaceState := "ready"
			if checkpoint.Strategy != "git-reference" {
				workspaceState = "dirty"
			}
			workspaceUpdate := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
				Where("tenant_id = ? AND id = ? AND state = ?", checkpoint.TenantID, checkpoint.WorkspaceID, "checkpointing").
				Updates(map[string]any{"state": workspaceState, "last_used_at": now, "updated_at": now})
			if workspaceUpdate.Error != nil {
				return workspaceUpdate.Error
			}
			eventInput := sessions.InternalEventInput{
				EventType: "checkpoint.failed", ActorType: "system", ExecutionID: &checkpoint.ExecutionID,
				Payload: map[string]any{
					"turnId": checkpoint.TurnID, "workspaceId": checkpoint.WorkspaceID,
					"checkpointId": checkpoint.ID, "strategy": checkpoint.Strategy,
					"failureCode": failureCode, "failureMessage": failureMessage,
				},
			}
			if workspace.LastWorkerID != nil {
				eventInput.WorkerID = workspace.LastWorkerID
				eventInput.Generation = &checkpoint.Generation
			}
			event, appendErr := s.sessions.AppendInternalEvent(ctx, tx, checkpoint.TenantID, checkpoint.SessionID, eventInput)
			appended = event
			transitioned = appendErr == nil
			return appendErr
		})
		if err != nil {
			return failed, err
		}
		if appended.EventID != uuid.Nil {
			s.sessions.PublishInternalEvent(appended)
		}
		if transitioned {
			failed++
		}
	}
	return failed, nil
}
