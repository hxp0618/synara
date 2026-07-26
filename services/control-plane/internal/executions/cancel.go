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
		case "queued", "recovering", "leased", "running", "waiting-for-approval", "suspended":
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
	const interactionCancellationReason = "The Execution was cancelled before the interaction lifecycle completed."
	wasRecovering := execution.Status == "recovering"
	if err := supersedeResourceSuspendAttempts(
		ctx, tx, *execution, now, "execution_cancelled",
		"The Execution was cancelled while a resource suspension attempt was in progress.",
	); err != nil {
		return persistence.SessionEvent{}, err
	}
	interactionOutcomeUnknown := false
	if lease != nil {
		var err error
		interactionOutcomeUnknown, err = s.reconcileInteractionGenerationForRecovery(
			ctx, tx, *execution, *lease, false, now, interactionCancellationReason,
		)
		if err != nil {
			return persistence.SessionEvent{}, err
		}
		leaseDelete := tx.WithContext(ctx).Delete(lease)
		if leaseDelete.Error != nil {
			return persistence.SessionEvent{}, problem.Wrap(500, "lease_release_failed", "Failed to release the cancelled Execution lease.", leaseDelete.Error)
		}
		if leaseDelete.RowsAffected > 0 {
			releaseReason := workerClaimReleaseUserCancelled
			releasedAt := now
			switch reason {
			case "tenant-delete":
				releaseReason = workerClaimReleaseTenantDeleted
			case sessionAbsoluteExpiryAction:
				releaseReason = workerClaimReleaseSessionAbsoluteExpired
				var session persistence.AgentSession
				if err := tx.WithContext(ctx).Select("absolute_expires_at").
					Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
					Take(&session).Error; err != nil {
					return persistence.SessionEvent{}, problem.Wrap(500, "session_lifetime_load_failed", "The Session absolute release time could not be loaded.", err)
				}
				if session.AbsoluteExpiresAt == nil {
					return persistence.SessionEvent{}, problem.New(500, "session_lifetime_missing", "The Session absolute release time is missing.")
				}
				releasedAt = *session.AbsoluteExpiresAt
			}
			authorityKind := workerClaimReleaseAuthorityControlPlane
			if actorType == "worker" {
				authorityKind = workerClaimReleaseAuthorityWorker
			} else if actorType == "user" {
				authorityKind = workerClaimReleaseAuthorityUser
			}
			authorityID := ""
			if actorID != nil {
				authorityID = actorID.String()
			}
			if err := recordWorkerClaimReleaseFact(ctx, tx, executionClaimReleaseInput(
				*lease, releasedAt, now, releaseReason, authorityKind, authorityID, "",
			)); err != nil {
				return persistence.SessionEvent{}, err
			}
		}
		if err := transitionWorkerAfterLeaseReleasedLocked(ctx, tx, *lease, now); err != nil {
			return persistence.SessionEvent{}, err
		}
	} else {
		var err error
		interactionOutcomeUnknown, err = s.terminalizeLeaseFreeInteractionsForCancellation(
			ctx, tx, *execution, interactionCancellationReason,
		)
		if err != nil {
			return persistence.SessionEvent{}, err
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
			[]string{"queued", "recovering", "leased", "running", "waiting-for-approval", "suspended"}).
		Updates(map[string]any{
			"status": "cancelled", "worker_id": nil, "next_recovery_reason": nil, "finished_at": now,
		})
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
	if interactionOutcomeUnknown {
		payload["interactionOutcomeUnknown"] = true
	}
	event, err := s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
		EventType: "execution.cancelled", ActorType: actorType, ActorID: actorID,
		ExecutionID: &execution.ID, WorkerID: previousWorkerID, Generation: eventGeneration,
		Payload: payload,
	})
	if err != nil {
		return persistence.SessionEvent{}, err
	}
	if err := s.markExecutionGenerationTerminalOutcomeLocked(
		ctx, tx, *execution, now, generationTerminalOutcomeCancelled,
	); err != nil {
		return persistence.SessionEvent{}, err
	}
	if wasRecovering {
		if err := s.markExecutionGenerationTerminalOutcomeAtGenerationLocked(
			ctx, tx, *execution, execution.Generation+1, now, generationTerminalOutcomeCancelled,
		); err != nil {
			return persistence.SessionEvent{}, err
		}
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

// terminalizeLeaseFreeInteractionsForCancellation handles queued, recovering,
// and suspended executions after Worker ownership has already been fenced.
// A command written across the Provider boundary or consumed by one immutable
// Recovery Bundle cannot be labelled superseded: its application outcome is
// unknown and replay must remain forbidden. An unbound resume-recorded answer
// remains as durable audit history but a terminal Execution can no longer
// include it in a future Bundle.
func (s *Service) terminalizeLeaseFreeInteractionsForCancellation(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	reason string,
) (bool, error) {
	deliveredUnknown := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where(
			"tenant_id = ? AND execution_id = ? AND status = ? AND delivery_status = ?",
			execution.TenantID, execution.ID, "resolved", "delivered",
		).
		Updates(map[string]any{"delivery_status": "outcome-unknown", "delivery_error": reason})
	if deliveredUnknown.Error != nil {
		return false, problem.Wrap(
			500,
			"interaction_cancellation_outcome_unknown_update_failed",
			"The delivered interaction resolution could not be fenced during cancellation.",
			deliveredUnknown.Error,
		)
	}
	boundUnknown := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where(
			"tenant_id = ? AND execution_id = ? AND status = ? AND delivery_status = ?",
			execution.TenantID, execution.ID, "resolved", "resume-bound",
		).
		Updates(map[string]any{"delivery_status": "outcome-unknown", "delivery_error": reason})
	if boundUnknown.Error != nil {
		return false, problem.Wrap(
			500,
			"interaction_cancellation_resume_outcome_unknown_update_failed",
			"The recovery-bound interaction resolution could not be fenced during cancellation.",
			boundUnknown.Error,
		)
	}
	outcomeUnknown := deliveredUnknown.RowsAffected > 0 || boundUnknown.RowsAffected > 0
	if outcomeUnknown {
		if err := s.terminalizeInteractionsAfterOutcomeUnknown(ctx, tx, execution, reason); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND status = ?", execution.TenantID, execution.ID, "pending").
		Updates(map[string]any{
			"status": "expired", "delivery_status": "superseded", "delivery_error": reason,
		}).Error; err != nil {
		return false, problem.Wrap(
			500,
			"interaction_cancellation_pending_terminalize_failed",
			"Pending interactions could not be terminalized during cancellation.",
			err,
		)
	}
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where(
			"tenant_id = ? AND execution_id = ? AND status = ? AND delivery_status IN ?",
			execution.TenantID, execution.ID, "resolved", []string{"pending", "failed"},
		).
		Updates(map[string]any{"delivery_status": "superseded", "delivery_error": reason}).Error; err != nil {
		return false, problem.Wrap(
			500,
			"interaction_cancellation_delivery_terminalize_failed",
			"Unacknowledged interaction resolutions could not be terminalized during cancellation.",
			err,
		)
	}
	return false, nil
}
