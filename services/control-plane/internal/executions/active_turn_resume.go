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
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func (s *Service) ResumeActiveTurnForSession(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[Execution], error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return OperationResult[Execution]{}, err
	}
	var execution persistence.AgentExecution
	err = s.db.WithContext(ctx).
		Joins("JOIN execution_suspend_attempts AS active_suspend ON active_suspend.tenant_id = agent_executions.tenant_id AND active_suspend.execution_id = agent_executions.id").
		Where("agent_executions.tenant_id = ? AND agent_executions.session_id = ? AND active_suspend.reason = ? AND active_suspend.status = ?",
			tenantID, sessionID, resourceSuspendReasonActiveIdleTimeout, "completed").
		Order("active_suspend.requested_at DESC, active_suspend.id DESC").Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OperationResult[Execution]{}, problem.New(409, "active_suspend_execution_not_found", "The Session does not have an active Turn waiting for resume.")
	}
	if err != nil {
		return OperationResult[Execution]{}, problem.Wrap(500, "active_suspend_execution_load_failed", "The active Turn could not be located for resume.", err)
	}
	return s.ResumeActiveTurn(ctx, principal, execution.ID, idempotencyKey, requestID, ipAddress)
}

// ResumeActiveTurn restarts an idle-suspended active Turn in a new Generation.
// Waiting interactions intentionally use their existing Resolve path instead;
// this endpoint cannot bypass a pending approval or user-input callback.
func (s *Service) ResumeActiveTurn(
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
	err = s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OperationResult[Execution]{}, problem.New(404, "execution_not_found", "Execution not found.")
	}
	if err != nil {
		return OperationResult[Execution]{}, problem.Wrap(500, "execution_load_failed", "Failed to load the suspended Execution.", err)
	}
	session, err := s.sessions.Get(ctx, principal, tenantID, current.SessionID)
	if err != nil {
		return OperationResult[Execution]{}, err
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, session.OrganizationID, authorization.ExecutionCreate,
	); err != nil {
		return OperationResult[Execution]{}, err
	}
	actorType, actorID := identity.ActorType(principal), identity.ActorID(principal)

	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: actorID, Key: idempotencyKey,
		Operation: "execution.active-turn.resume", SuccessStatus: 202,
		Request: map[string]any{"executionId": executionID},
	}, func(tx *gorm.DB) (Execution, error) {
		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&execution).Error
		if errors.Is(executionErr, gorm.ErrRecordNotFound) {
			return Execution{}, problem.New(404, "execution_not_found", "Execution not found.")
		}
		if executionErr != nil {
			return Execution{}, problem.Wrap(500, "execution_lock_failed", "The suspended Execution could not be locked.", executionErr)
		}
		alreadyRecovering := execution.Status == "recovering"
		if (execution.Status != "suspended" && !alreadyRecovering) || execution.WorkerID != nil ||
			execution.NextRecoveryReason == nil || *execution.NextRecoveryReason != "suspend-resume" {
			return Execution{}, problem.New(409, "active_suspend_resume_conflict", "The Execution is not an active Turn waiting for explicit resume.")
		}
		now := s.now()
		if err := s.requireExecutionSessionWithinAbsoluteLifetime(ctx, tx, execution, now); err != nil {
			return Execution{}, err
		}
		var attempt persistence.ExecutionSuspendAttempt
		attemptErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ? AND generation = ? AND reason = ? AND status = ?",
				execution.TenantID, execution.ID, execution.Generation,
				resourceSuspendReasonActiveIdleTimeout, "completed").
			Order("requested_at DESC, id DESC").Take(&attempt).Error
		if errors.Is(attemptErr, gorm.ErrRecordNotFound) {
			return Execution{}, problem.New(409, "active_suspend_receipt_missing", "The active-turn checkpoint receipt is unavailable for resume.")
		}
		if attemptErr != nil {
			return Execution{}, problem.Wrap(500, "active_suspend_receipt_load_failed", "The active-turn checkpoint receipt could not be locked.", attemptErr)
		}
		if _, err := activeTurnCheckpointProjection(attempt); err != nil {
			return Execution{}, problem.Wrap(409, "active_suspend_receipt_consumed", "The active-turn checkpoint receipt cannot be resumed again.", err)
		}
		cursorAvailable, err := s.activeTurnResumeCursorAvailable(ctx, tx, execution)
		if err != nil {
			return Execution{}, err
		}
		if !cursorAvailable {
			return Execution{}, problem.New(409, "active_suspend_resume_unavailable", "The exact active-turn checkpoint cursor is no longer available; automatic history replay is unsafe.")
		}
		if alreadyRecovering {
			return toExecution(execution), nil
		}
		appended, err = s.resumeSuspendedExecutionLocked(
			ctx, tx, &execution, actorType, &actorID, nil,
			"active_idle_explicit_resume", "active-idle-explicit-resume", now,
		)
		if err != nil {
			return Execution{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: actorType, ActorID: &actorID,
			Action: "execution.active_turn_resumed", ResourceType: "agent_execution", ResourceID: &execution.ID,
			OrganizationID: &session.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"sessionId": execution.SessionID, "turnId": execution.TurnID, "suspendAttemptId": attempt.ID},
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
