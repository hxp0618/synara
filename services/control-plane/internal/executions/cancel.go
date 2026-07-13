package executions

import (
	"context"
	"errors"
	"time"

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
		case "completed", "failed", "interrupted":
			return Execution{}, problem.New(409, "execution_terminal", "The execution already reached a terminal state.")
		case "queued", "recovering", "leased", "running", "waiting-for-approval":
		default:
			return Execution{}, problem.New(409, "execution_state_conflict", "The execution cannot be cancelled from its current state.")
		}

		now := s.now()
		var lockedLease *persistence.WorkerLease
		if leaseErr == nil {
			lockedLease = &lease
		}
		appended, err = s.cancelExecutionLocked(
			ctx, tx, &execution, lockedLease, "user", &principal.UserID, now, "user-requested",
		)
		if err != nil {
			return Execution{}, err
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

func (s *Service) cancelExecutionLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution *persistence.AgentExecution,
	lease *persistence.WorkerLease,
	actorType string,
	actorID *uuid.UUID,
	now time.Time,
	reason string,
) (persistence.SessionEvent, error) {
	if lease != nil {
		if err := s.supersedeInteractionGenerationWithReason(ctx, tx, *execution, *lease,
			"The Execution was cancelled before the interaction lifecycle completed."); err != nil {
			return persistence.SessionEvent{}, err
		}
		if err := tx.WithContext(ctx).Delete(lease).Error; err != nil {
			return persistence.SessionEvent{}, problem.Wrap(500, "lease_release_failed", "Failed to release the cancelled Execution lease.", err)
		}
	}

	previousWorkerID := execution.WorkerID
	previousGeneration := execution.Generation
	var eventGeneration *int64
	if previousGeneration > 0 {
		eventGeneration = &previousGeneration
	}
	finishedAt := now
	cancelled := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ? AND status IN ?", execution.TenantID, execution.ID,
			[]string{"queued", "recovering", "leased", "running", "waiting-for-approval"}).
		Updates(map[string]any{"status": "cancelled", "worker_id": nil, "finished_at": now})
	if err := expectOne(cancelled, 409, "execution_cancel_conflict", "The Execution could not be cancelled from its current state."); err != nil {
		return persistence.SessionEvent{}, err
	}
	turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
		Where("tenant_id = ? AND session_id = ? AND id = ?", execution.TenantID, execution.SessionID, execution.TurnID).
		Updates(map[string]any{"status": "cancelled", "completed_at": now})
	if err := expectOne(turnUpdate, 500, "turn_cancel_failed", "Failed to cancel the Turn."); err != nil {
		return persistence.SessionEvent{}, err
	}
	if err := supersedeControlCommands(ctx, tx, *execution, uuid.Nil,
		"The Execution was cancelled before the Control command was acknowledged."); err != nil {
		return persistence.SessionEvent{}, err
	}
	payload := map[string]any{"turnId": execution.TurnID, "finishedAt": now, "reason": reason}
	event, err := s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
		EventType: "execution.cancelled", ActorType: actorType, ActorID: actorID,
		ExecutionID: &execution.ID, WorkerID: previousWorkerID, Generation: eventGeneration,
		Payload: payload,
	})
	if err != nil {
		return persistence.SessionEvent{}, err
	}
	if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
		TenantID: &execution.TenantID, Topic: "execution.cancelled", MessageKey: execution.ID.String(),
		Payload: map[string]any{
			"executionId": execution.ID, "tenantId": execution.TenantID, "sessionId": execution.SessionID,
			"turnId": execution.TurnID, "finishedAt": now, "reason": reason,
		},
	}); err != nil {
		return persistence.SessionEvent{}, problem.Wrap(500, "execution_cancel_outbox_failed", "The cancelled Execution event could not be queued.", err)
	}
	execution.Status = "cancelled"
	execution.WorkerID = nil
	execution.FinishedAt = &finishedAt
	return event, nil
}
