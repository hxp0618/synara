package executions

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func (s *Service) resumeSuspendedExecutionLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution *persistence.AgentExecution,
	actorType string,
	actorID, workerID *uuid.UUID,
	recoveryReason, eventReason string,
	now time.Time,
) (persistence.SessionEvent, error) {
	if execution.Status != "suspended" || execution.WorkerID != nil ||
		execution.NextRecoveryReason == nil || *execution.NextRecoveryReason != "suspend-resume" {
		return persistence.SessionEvent{}, problem.New(409, "execution_resume_conflict", "The suspended Execution cannot be resumed from its current state.")
	}
	var pendingInteractions int64
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND status = ?", execution.TenantID, execution.ID, "pending").
		Count(&pendingInteractions).Error; err != nil {
		return persistence.SessionEvent{}, problem.Wrap(500, "interaction_pending_count_failed", "Pending interactions could not be checked before resuming the suspended Execution.", err)
	}
	if pendingInteractions > 0 {
		return persistence.SessionEvent{}, problem.New(409, "execution_resume_interactions_pending", "Resolve pending interactions before resuming the suspended Execution.")
	}
	// A suspended Execution deliberately releases tenant concurrent-execution
	// capacity. Reacquire that admission before making recovery visible; all
	// explicit, interaction-driven, and boundary-crossing resume paths converge
	// here, so none can bypass the quota.
	var session persistence.AgentSession
	if err := tx.WithContext(ctx).Select("id", "project_id").
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
		Take(&session).Error; err != nil {
		return persistence.SessionEvent{}, problem.Wrap(
			500, "execution_resume_session_load_failed", "The suspended Execution Session could not be loaded for quota admission.", err,
		)
	}
	if err := s.sessions.RequireExecutionQuotaAvailableFor(ctx, tx, execution.TenantID, sessions.ExecutionQuotaAdmission{
		ProjectID: session.ProjectID, SessionID: execution.SessionID,
		AutomationID: execution.AutomationID, QuotaUnits: execution.QuotaUnits,
	}); err != nil {
		return persistence.SessionEvent{}, err
	}
	resumed := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where(
			"tenant_id = ? AND id = ? AND status = ? AND worker_id IS NULL AND generation = ? AND next_recovery_reason = ?",
			execution.TenantID, execution.ID, "suspended", execution.Generation, "suspend-resume",
		).
		Update("status", "recovering")
	if err := expectOne(resumed, 409, "execution_resume_conflict", "The suspended Execution could not enter recovery."); err != nil {
		return persistence.SessionEvent{}, err
	}
	execution.Status = "recovering"
	turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
		Where("tenant_id = ? AND session_id = ? AND id = ?", execution.TenantID, execution.SessionID, execution.TurnID).
		Update("status", "queued")
	if err := expectOne(turnUpdate, 500, "turn_recovery_failed", "Failed to return the suspended Turn to the recovery queue."); err != nil {
		return persistence.SessionEvent{}, err
	}
	if execution.RemoteWorkspaceID != nil {
		if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", execution.TenantID, *execution.RemoteWorkspaceID).
			Updates(map[string]any{"state": "recovering", "updated_at": now}).Error; err != nil {
			return persistence.SessionEvent{}, problem.Wrap(500, "workspace_recovery_update_failed", "Failed to mark the suspended Workspace for recovery.", err)
		}
	}
	if err := s.enqueueRecovery(ctx, tx, *execution, recoveryReason, ""); err != nil {
		return persistence.SessionEvent{}, err
	}
	return s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
		EventType: "execution.recovering", ActorType: actorType, ActorID: actorID,
		ExecutionID: &execution.ID, WorkerID: workerID, Generation: &execution.Generation,
		Payload: map[string]any{"turnId": execution.TurnID, "reason": eventReason},
	})
}
