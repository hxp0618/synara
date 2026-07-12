package executions

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	apiidempotency "github.com/synara-ai/synara/services/control-plane/internal/idempotency"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func (s *Service) Cancel(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[Execution], error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return OperationResult[Execution]{}, err
	}
	var current persistence.AgentExecution
	err = s.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OperationResult[Execution]{}, problem.New(404, "execution_not_found", "Execution not found.")
	}
	if err != nil {
		return OperationResult[Execution]{}, problem.Wrap(500, "execution_load_failed", "Failed to load the execution.", err)
	}
	session, err := s.sessions.Get(ctx, principal, tenantID, current.SessionID)
	if err != nil {
		return OperationResult[Execution]{}, err
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, session.OrganizationID, authorization.ExecutionCancel,
	); err != nil {
		return OperationResult[Execution]{}, err
	}

	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "execution.cancel", SuccessStatus: 200,
		Request: map[string]any{"executionId": executionID},
	}, func(tx *gorm.DB) (Execution, error) {
		var lease persistence.WorkerLease
		leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("execution_id = ? AND tenant_id = ?", executionID, tenantID).Take(&lease).Error
		if leaseErr != nil && !errors.Is(leaseErr, gorm.ErrRecordNotFound) {
			return Execution{}, problem.Wrap(500, "lease_lock_failed", "Failed to lock the execution lease.", leaseErr)
		}

		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&execution).Error
		if errors.Is(executionErr, gorm.ErrRecordNotFound) {
			return Execution{}, problem.New(404, "execution_not_found", "Execution not found.")
		}
		if executionErr != nil {
			return Execution{}, problem.Wrap(500, "execution_lock_failed", "Failed to lock the execution.", executionErr)
		}
		switch execution.Status {
		case "cancelled":
			return toExecution(execution), nil
		case "completed", "failed":
			return Execution{}, problem.New(409, "execution_terminal", "The execution already reached a terminal state.")
		case "queued", "recovering", "leased", "running", "waiting-for-approval":
		default:
			return Execution{}, problem.New(409, "execution_state_conflict", "The execution cannot be cancelled from its current state.")
		}

		if leaseErr == nil {
			if err := tx.WithContext(ctx).Delete(&lease).Error; err != nil {
				return Execution{}, problem.Wrap(500, "lease_release_failed", "Failed to release the cancelled execution lease.", err)
			}
		}
		now := s.now()
		execution.Status = "cancelled"
		execution.WorkerID = nil
		execution.FinishedAt = &now
		cancelled := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ? AND status IN ?", tenantID, executionID,
				[]string{"queued", "recovering", "leased", "running", "waiting-for-approval"}).
			Updates(map[string]any{"status": "cancelled", "worker_id": nil, "finished_at": now})
		if err := expectOne(cancelled, 409, "execution_cancel_conflict", "The execution could not be cancelled from its current state."); err != nil {
			return Execution{}, err
		}
		turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND session_id = ? AND id = ?", tenantID, execution.SessionID, execution.TurnID).
			Updates(map[string]any{"status": "cancelled", "completed_at": now})
		if err := expectOne(turnUpdate, 500, "turn_cancel_failed", "Failed to cancel the Turn."); err != nil {
			return Execution{}, err
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, tenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.cancelled", ActorType: "user", ActorID: &principal.UserID,
			ExecutionID: &execution.ID, Payload: map[string]any{"turnId": execution.TurnID, "finishedAt": now},
		})
		if err != nil {
			return Execution{}, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "execution.cancelled", MessageKey: execution.ID.String(),
			Payload: map[string]any{
				"executionId": execution.ID, "tenantId": tenantID, "sessionId": execution.SessionID,
				"turnId": execution.TurnID, "finishedAt": now,
			},
		}); err != nil {
			return Execution{}, problem.Wrap(500, "execution_cancel_outbox_failed", "The cancelled Execution event could not be queued.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "execution.cancelled", ResourceType: "agent_execution", ResourceID: &execution.ID,
			OrganizationID: &session.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"sessionId": execution.SessionID, "turnId": execution.TurnID},
		}); err != nil {
			return Execution{}, err
		}
		return toExecution(execution), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return OperationResult[Execution]{
		Value: result.Value, Replayed: result.Replayed, StatusCode: result.StatusCode,
	}, err
}
