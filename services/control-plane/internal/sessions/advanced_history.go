package sessions

import (
	"context"
	"errors"
	"strings"
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
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

func (s *Service) Rollback(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input RollbackSessionInput,
	idempotencyKey, requestID, ipAddress string,
) (RollbackSessionResult, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return RollbackSessionResult{}, false, problem.New(400, "idempotency_key_required", "Idempotency-Key is required for Session operations.")
	}
	expected, err := validateExpectedSessionSequence(input.ExpectedLastEventSequence)
	if err != nil {
		return RollbackSessionResult{}, false, err
	}
	if input.FromTurnID == uuid.Nil {
		return RollbackSessionResult{}, false, problem.New(400, "invalid_rollback_turn", "fromTurnId is required.")
	}
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return RollbackSessionResult{}, false, err
	}
	current, _, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.ExecutionCreate)
	if err != nil {
		return RollbackSessionResult{}, false, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return RollbackSessionResult{}, false, problem.New(404, "session_not_found", "Session not found.")
	}
	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "session.rollback", SuccessStatus: 200,
		Request: map[string]any{
			"sessionId": sessionID, "expectedLastEventSequence": expected, "fromTurnId": input.FromTurnID,
		},
	}, func(tx *gorm.DB) (RollbackSessionResult, error) {
		session, err := lockSessionForHistoryMutation(ctx, tx, tenantID, sessionID, expected)
		if err != nil {
			return RollbackSessionResult{}, err
		}
		if err := requireIdleSession(ctx, tx, tenantID, sessionID); err != nil {
			return RollbackSessionResult{}, err
		}
		logical, err := LoadLogicalEvents(ctx, tx, tenantID, sessionID, session.LastEventSequence)
		if err != nil {
			return RollbackSessionResult{}, err
		}
		effective, err := EffectiveLogicalEvents(logical)
		if err != nil {
			return RollbackSessionResult{}, err
		}
		fromIndex := -1
		fromSessionID := uuid.Nil
		fromSequence := int64(0)
		for index, item := range effective {
			if item.Event.EventType != "turn.created" {
				continue
			}
			turnID, parseErr := logicalPayloadUUID(item.Event.Payload, "turnId")
			if parseErr == nil && turnID == input.FromTurnID {
				fromIndex = index
				fromSessionID = item.OriginSessionID
				fromSequence = item.Event.Sequence
				break
			}
		}
		if fromIndex < 0 {
			return RollbackSessionResult{}, problem.New(409, "rollback_target_stale", "The rollback Turn is not part of the current logical Session history.")
		}
		removedTurns := map[uuid.UUID]struct{}{}
		for _, item := range effective[fromIndex:] {
			if item.Event.EventType != "turn.created" {
				continue
			}
			turnID, parseErr := logicalPayloadUUID(item.Event.Payload, "turnId")
			if parseErr == nil {
				removedTurns[turnID] = struct{}{}
			}
		}
		if err := clearProviderCursorForHistoryMutation(ctx, tx, &session, s.now()); err != nil {
			return RollbackSessionResult{}, err
		}
		appended, err = appendEvent(ctx, tx, &session, eventInput{
			EventType: "session.history.rolled-back", ActorType: "user", ActorID: &principal.UserID,
			Payload: map[string]any{
				"fromSessionId": fromSessionID, "fromTurnId": input.FromTurnID,
				"fromSequence": fromSequence, "removedTurnCount": len(removedTurns),
				"supportMode": "emulated", "workspaceDisposition": "unchanged",
				"externalSideEffectsReverted": false,
			},
		})
		if err != nil {
			return RollbackSessionResult{}, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: appended.EventType, MessageKey: appended.EventID.String(),
			Payload: map[string]any{
				"tenantId": tenantID, "sessionId": sessionID, "eventId": appended.EventID,
				"sequence": appended.Sequence, "fromSessionId": fromSessionID,
				"fromTurnId": input.FromTurnID, "fromSequence": fromSequence,
				"removedTurnCount": len(removedTurns), "supportMode": "emulated",
				"workspaceDisposition": "unchanged", "externalSideEffectsReverted": false,
			},
		}); err != nil {
			return RollbackSessionResult{}, problem.Wrap(500, "session_rollback_outbox_failed", "The Session rollback event could not be queued.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "session.history_rolled_back", ResourceType: "agent_session", ResourceID: &session.ID,
			OrganizationID: &session.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"fromSessionId": fromSessionID, "fromTurnId": input.FromTurnID,
				"fromSequence": fromSequence, "removedTurnCount": len(removedTurns),
				"workspaceDisposition": "unchanged", "externalSideEffectsReverted": false,
			},
		}); err != nil {
			return RollbackSessionResult{}, err
		}
		return RollbackSessionResult{
			SessionID: session.ID, EventID: appended.EventID, EventSequence: appended.Sequence,
			FromSessionID: fromSessionID, FromTurnID: input.FromTurnID, FromSequence: fromSequence,
			RemovedTurnCount: len(removedTurns), SupportMode: "emulated",
			WorkspaceDisposition: "unchanged", ExternalSideEffectsReverted: false,
		}, nil
	})
	if err != nil {
		return RollbackSessionResult{}, false, err
	}
	if !result.Replayed {
		s.events.publish(toEvent(appended))
	}
	return result.Value, result.Replayed, nil
}

func (s *Service) Fork(
	ctx context.Context,
	principal identity.Principal,
	sourceSessionID uuid.UUID,
	input ForkSessionInput,
	idempotencyKey, requestID, ipAddress string,
) (ForkSessionResult, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return ForkSessionResult{}, false, problem.New(400, "idempotency_key_required", "Idempotency-Key is required for Session operations.")
	}
	expected, err := validateExpectedSessionSequence(input.ExpectedLastEventSequence)
	if err != nil {
		return ForkSessionResult{}, false, err
	}
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return ForkSessionResult{}, false, err
	}
	source, _, err := s.authorizedModel(ctx, principal, tenantID, sourceSessionID, authorization.SessionRead)
	if err != nil {
		return ForkSessionResult{}, false, err
	}
	if source.Visibility == "private" && source.CreatedBy != principal.UserID {
		return ForkSessionResult{}, false, problem.New(404, "session_not_found", "Session not found.")
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, source.OrganizationID, authorization.SessionCreate,
	); err != nil {
		return ForkSessionResult{}, false, err
	}
	requestedCredentialID := normalizeRequestedCredentialID(input.ProviderCredentialID)
	if requestedCredentialID != nil {
		if _, err := s.authorizer.RequireOrganization(
			ctx, principal.UserID, tenantID, source.OrganizationID, authorization.CredentialsUse,
		); err != nil {
			return ForkSessionResult{}, false, err
		}
	}
	titleValue := strings.TrimSpace(input.Title)
	if titleValue == "" {
		titleValue = source.Title + " Fork"
	}
	title, err := validation.Name(titleValue, "invalid_session_title", "Session title", 300)
	if err != nil {
		return ForkSessionResult{}, false, err
	}
	visibility := input.Visibility
	if strings.TrimSpace(visibility) == "" {
		visibility = source.Visibility
	}
	visibility, err = normalizeSessionVisibility(visibility)
	if err != nil {
		return ForkSessionResult{}, false, err
	}
	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "session.fork", SuccessStatus: 201,
		Request: map[string]any{
			"sourceSessionId": sourceSessionID, "expectedLastEventSequence": expected,
			"title": title, "visibility": visibility, "providerCredentialId": requestedCredentialID,
			"executionTargetId": input.ExecutionTargetID,
		},
	}, func(tx *gorm.DB) (ForkSessionResult, error) {
		locked, err := lockSessionForHistoryMutation(ctx, tx, tenantID, sourceSessionID, expected)
		if err != nil {
			return ForkSessionResult{}, err
		}
		if err := requireIdleSession(ctx, tx, tenantID, sourceSessionID); err != nil {
			return ForkSessionResult{}, err
		}
		if err := requireForkLineageExtendable(ctx, tx, tenantID, sourceSessionID); err != nil {
			return ForkSessionResult{}, err
		}
		targetID := locked.ExecutionTargetID
		if input.ExecutionTargetID != nil && *input.ExecutionTargetID != uuid.Nil {
			targetID = *input.ExecutionTargetID
		}
		var target persistence.ExecutionTarget
		if err := tx.WithContext(ctx).
			Where("id = ? AND status = ?", targetID, "active").
			Where("(tenant_id IS NULL OR tenant_id = ?) AND (organization_id IS NULL OR organization_id = ?)", tenantID, locked.OrganizationID).
			Take(&target).Error; err != nil {
			return ForkSessionResult{}, problem.Wrap(409, "execution_target_unavailable", "The Fork Execution Target is unavailable.", err)
		}
		if err := s.requireTargetProviderCapabilities(
			ctx, tx, target, locked.Provider, "start-session", "send-turn",
		); err != nil {
			return ForkSessionResult{}, err
		}
		credentialID, err := s.resolveProviderCredentialSelection(
			ctx,
			tx,
			tenantID,
			locked.OrganizationID,
			principal.UserID,
			locked.Provider,
			locked.Model,
			requestedCredentialID,
		)
		if err != nil {
			return ForkSessionResult{}, err
		}
		logical, err := LoadLogicalEvents(ctx, tx, tenantID, sourceSessionID, locked.LastEventSequence)
		if err != nil {
			return ForkSessionResult{}, err
		}
		effective, err := EffectiveLogicalEvents(logical)
		if err != nil {
			return ForkSessionResult{}, err
		}
		var sourceTurnID *uuid.UUID
		for index := len(effective) - 1; index >= 0; index-- {
			if effective[index].Event.EventType != "turn.created" {
				continue
			}
			turnID, parseErr := logicalPayloadUUID(effective[index].Event.Payload, "turnId")
			if parseErr == nil {
				sourceTurnID = &turnID
				break
			}
		}
		now := s.now()
		strategy := "emulated"
		forkSequence := locked.LastEventSequence
		created := persistence.AgentSession{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: locked.OrganizationID,
			ProjectID: locked.ProjectID, CreatedBy: principal.UserID, Title: title,
			Status: "active", Visibility: visibility, Provider: locked.Provider, Model: locked.Model,
			ProviderCredentialID: credentialID, ExecutionTargetID: target.ID,
			ProviderResumeCursorState: "absent", ForkSourceSessionID: &sourceSessionID,
			ForkSourceTurnID: sourceTurnID, ForkSourceEventSequence: &forkSequence, ForkStrategy: &strategy,
			LastEventSequence: forkSequence, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&created).Error; err != nil {
			return ForkSessionResult{}, problem.Wrap(409, "session_fork_rejected", "The Fork Session could not be created.", err)
		}
		appended, err = appendEvent(ctx, tx, &created, eventInput{
			EventType: "session.forked", ActorType: "user", ActorID: &principal.UserID,
			Payload: map[string]any{
				"sourceSessionId": sourceSessionID, "sourceEventSequence": forkSequence,
				"sourceTurnId": sourceTurnID, "supportMode": "emulated",
			},
		})
		if err != nil {
			return ForkSessionResult{}, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: appended.EventType, MessageKey: created.ID.String(),
			Payload: map[string]any{
				"tenantId": tenantID, "sourceSessionId": sourceSessionID,
				"sessionId": created.ID, "sourceEventSequence": forkSequence,
				"sourceTurnId": sourceTurnID, "eventId": appended.EventID,
				"sequence": appended.Sequence, "supportMode": "emulated",
			},
		}); err != nil {
			return ForkSessionResult{}, problem.Wrap(500, "session_fork_outbox_failed", "The Session Fork event could not be queued.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "session.forked", ResourceType: "agent_session", ResourceID: &created.ID,
			OrganizationID: &created.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"sourceSessionId": sourceSessionID, "sourceEventSequence": forkSequence,
				"sourceTurnId": sourceTurnID, "supportMode": "emulated",
			},
		}); err != nil {
			return ForkSessionResult{}, err
		}
		return ForkSessionResult{
			Session: toSession(created), SourceSessionID: sourceSessionID,
			SourceEventSequence: forkSequence, SupportMode: "emulated",
		}, nil
	})
	if err != nil {
		return ForkSessionResult{}, false, err
	}
	if !result.Replayed {
		s.events.publish(toEvent(appended))
	}
	return result.Value, result.Replayed, nil
}

func validateExpectedSessionSequence(value *int64) (int64, error) {
	if value == nil || *value < 0 {
		return 0, problem.New(400, "expected_session_sequence_required", "expectedLastEventSequence must be provided and cannot be negative.")
	}
	return *value, nil
}

func lockSessionForHistoryMutation(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	expected int64,
) (persistence.AgentSession, error) {
	var session persistence.AgentSession
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ? AND status = ? AND archived_at IS NULL", tenantID, sessionID, "active").
		Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.AgentSession{}, problem.New(409, "session_not_active", "Session is not active.")
	}
	if err != nil {
		return persistence.AgentSession{}, problem.Wrap(500, "session_lock_failed", "Failed to lock the Session.", err)
	}
	if session.LastEventSequence != expected {
		apiError := problem.New(409, "stale_session_sequence", "The Session changed after it was loaded.")
		apiError.Details = map[string]any{"expectedLastEventSequence": expected, "actualLastEventSequence": session.LastEventSequence}
		return persistence.AgentSession{}, apiError
	}
	return session, nil
}

func requireIdleSession(ctx context.Context, tx *gorm.DB, tenantID, sessionID uuid.UUID) error {
	var active int64
	if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND session_id = ? AND status IN ?", tenantID, sessionID, activeSessionExecutionStatuses).
		Count(&active).Error; err != nil {
		return problem.Wrap(500, "session_execution_check_failed", "Failed to inspect the active Session execution.", err)
	}
	if active != 0 {
		return problem.New(409, "session_execution_active", "The Session already has an active Turn execution.")
	}
	return nil
}

func clearProviderCursorForHistoryMutation(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
	now time.Time,
) error {
	updates := map[string]any{
		"provider_resume_cursor_encrypted":           nil,
		"provider_resume_cursor_state":               "absent",
		"provider_resume_cursor_source_execution_id": nil,
		"provider_resume_cursor_source_generation":   nil,
		"provider_resume_cursor_history_sequence":    nil,
	}
	if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", session.TenantID, session.ID).Updates(updates).Error; err != nil {
		return problem.Wrap(500, "session_cursor_clear_failed", "The stale Provider Cursor could not be cleared.", err)
	}
	session.ProviderResumeCursorEncrypted = nil
	session.ProviderResumeCursorState = "absent"
	session.ProviderResumeCursorSourceExecutionID = nil
	session.ProviderResumeCursorSourceGeneration = nil
	session.ProviderResumeCursorHistorySequence = nil
	if session.CurrentRuntimeBindingID != nil {
		if err := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
			Where("tenant_id = ? AND id = ? AND session_id = ?", session.TenantID, *session.CurrentRuntimeBindingID, session.ID).
			Updates(map[string]any{
				"resume_strategy": "authoritative-history", "cursor_compatibility_key": nil,
				"cursor_updated_at": nil, "updated_at": now.UTC(),
			}).Error; err != nil {
			return problem.Wrap(500, "runtime_binding_history_reset_failed", "The Provider runtime binding could not be reset to authoritative history.", err)
		}
	}
	return nil
}
