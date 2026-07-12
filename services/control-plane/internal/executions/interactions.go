package executions

import (
	"context"
	"errors"
	"strings"

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

func (s *Service) ListInteractions(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
) ([]Interaction, error) {
	tenantID, _, err := s.authorizeInteraction(ctx, principal, executionID)
	if err != nil {
		return nil, err
	}
	models := make([]persistence.ExecutionInteraction, 0)
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ?", tenantID, executionID).
		Order("requested_at, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "interactions_load_failed", "Execution interactions could not be loaded.", err)
	}
	items := make([]Interaction, 0, len(models))
	for _, model := range models {
		items = append(items, toInteraction(model))
	}
	return items, nil
}

func (s *Service) ResolveApproval(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
	requestID string,
	input ResolveApprovalInput,
	idempotencyKey, auditRequestID, ipAddress string,
) (OperationResult[Interaction], error) {
	decision := strings.ToLower(strings.TrimSpace(input.Decision))
	if decision != "accept" && decision != "decline" {
		return OperationResult[Interaction]{}, problem.New(400, "invalid_approval_decision", "decision must be accept or decline.")
	}
	return s.resolveInteraction(
		ctx, principal, executionID, requestID, "approval", map[string]any{"decision": decision},
		idempotencyKey, auditRequestID, ipAddress,
	)
}

func (s *Service) ResolveUserInput(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
	requestID string,
	input ResolveUserInputInput,
	idempotencyKey, auditRequestID, ipAddress string,
) (OperationResult[Interaction], error) {
	if input.Answers == nil {
		return OperationResult[Interaction]{}, problem.New(400, "invalid_user_input_answers", "answers must be a JSON object.")
	}
	return s.resolveInteraction(
		ctx, principal, executionID, requestID, "user-input", map[string]any{"answers": input.Answers},
		idempotencyKey, auditRequestID, ipAddress,
	)
}

func (s *Service) resolveInteraction(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
	requestID, kind string,
	resolution map[string]any,
	idempotencyKey, auditRequestID, ipAddress string,
) (OperationResult[Interaction], error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 200 || strings.ContainsAny(requestID, "\r\n\t") {
		return OperationResult[Interaction]{}, problem.New(400, "invalid_interaction_request_id", "requestId is invalid.")
	}
	tenantID, session, err := s.authorizeInteraction(ctx, principal, executionID)
	if err != nil {
		return OperationResult[Interaction]{}, err
	}

	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "interaction." + kind + ".resolve", SuccessStatus: 200,
		Request: map[string]any{
			"executionId": executionID, "requestId": requestID, "kind": kind, "resolution": resolution,
		},
	}, func(tx *gorm.DB) (Interaction, error) {
		var lease persistence.WorkerLease
		leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ?", tenantID, executionID).Take(&lease).Error
		if errors.Is(leaseErr, gorm.ErrRecordNotFound) {
			return Interaction{}, problem.New(409, "interaction_lease_expired", "The execution lease is no longer active.")
		}
		if leaseErr != nil {
			return Interaction{}, problem.Wrap(500, "lease_lock_failed", "Failed to lock the execution lease.", leaseErr)
		}
		if !lease.ExpiresAt.After(s.now()) {
			return Interaction{}, problem.New(409, "interaction_lease_expired", "The execution lease expired before the interaction was resolved.")
		}

		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&execution).Error
		if executionErr != nil {
			return Interaction{}, problem.Wrap(409, "execution_state_conflict", "The execution is no longer available for interaction resolution.", executionErr)
		}
		var interaction persistence.ExecutionInteraction
		interactionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ? AND request_id = ? AND kind = ?", tenantID, executionID, requestID, kind).
			Take(&interaction).Error
		if errors.Is(interactionErr, gorm.ErrRecordNotFound) {
			return Interaction{}, problem.New(404, "interaction_not_found", "Execution interaction not found.")
		}
		if interactionErr != nil {
			return Interaction{}, problem.Wrap(500, "interaction_lock_failed", "The execution interaction could not be locked.", interactionErr)
		}
		if interaction.Status == "resolved" {
			if !sameJSON(interaction.Resolution, resolution) {
				return Interaction{}, problem.New(409, "interaction_resolution_conflict", "The execution interaction was already resolved differently.")
			}
			return toInteraction(interaction), nil
		}
		if interaction.Status != "pending" {
			return Interaction{}, problem.New(409, "interaction_not_pending", "The execution interaction is no longer pending.")
		}
		if interaction.WorkerID != lease.WorkerID || interaction.Generation != lease.Generation ||
			execution.WorkerID == nil || *execution.WorkerID != lease.WorkerID || execution.Generation != lease.Generation {
			return Interaction{}, problem.New(409, "interaction_generation_fenced", "The interaction belongs to an obsolete Worker generation.")
		}

		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
			Where("id = ? AND status = ?", interaction.ID, "pending").
			Select("status", "resolution", "resolved_at", "resolved_by").
			Updates(&persistence.ExecutionInteraction{
				Status: "resolved", Resolution: resolution, ResolvedAt: &now, ResolvedBy: &principal.UserID,
			})
		if err := expectOne(updated, 409, "interaction_resolve_conflict", "The execution interaction was resolved concurrently."); err != nil {
			return Interaction{}, err
		}
		interaction.Status = "resolved"
		interaction.Resolution = resolution
		interaction.ResolvedAt = &now
		interaction.ResolvedBy = &principal.UserID

		var pending int64
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
			Where("tenant_id = ? AND execution_id = ? AND status = ?", tenantID, executionID, "pending").
			Count(&pending).Error; err != nil {
			return Interaction{}, problem.Wrap(500, "interaction_pending_count_failed", "Pending interactions could not be checked.", err)
		}
		if pending == 0 && execution.Status == "waiting-for-approval" {
			resumed := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND id = ? AND status = ? AND worker_id = ? AND generation = ?",
					tenantID, executionID, "waiting-for-approval", lease.WorkerID, lease.Generation).
				Update("status", "running")
			if err := expectOne(resumed, 409, "execution_resume_conflict", "The execution could not resume after interaction resolution."); err != nil {
				return Interaction{}, err
			}
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, tenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: kind + ".resolved", ActorType: "user", ActorID: &principal.UserID,
			ExecutionID: &execution.ID, Payload: map[string]any{"requestId": requestID, "resolution": resolution},
		})
		if err != nil {
			return Interaction{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "execution." + kind + "_resolved", ResourceType: "execution_interaction", ResourceID: &interaction.ID,
			OrganizationID: &session.OrganizationID, RequestID: auditRequestID, IPAddress: ipAddress,
			Metadata: map[string]any{"executionId": executionID, "requestId": requestID},
		}); err != nil {
			return Interaction{}, err
		}
		return toInteraction(interaction), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return OperationResult[Interaction]{
		Value: result.Value, Replayed: result.Replayed, StatusCode: result.StatusCode,
	}, err
}

func (s *Service) authorizeInteraction(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
) (uuid.UUID, sessions.Session, error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return uuid.Nil, sessions.Session{}, err
	}
	var execution persistence.AgentExecution
	err = s.db.WithContext(ctx).Select("id", "session_id").
		Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, sessions.Session{}, problem.New(404, "execution_not_found", "Execution not found.")
	}
	if err != nil {
		return uuid.Nil, sessions.Session{}, problem.Wrap(500, "execution_load_failed", "Failed to load the execution.", err)
	}
	session, err := s.sessions.Get(ctx, principal, tenantID, execution.SessionID)
	if err != nil {
		return uuid.Nil, sessions.Session{}, err
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, session.OrganizationID, authorization.ExecutionApprove,
	); err != nil {
		return uuid.Nil, sessions.Session{}, err
	}
	return tenantID, session, nil
}
