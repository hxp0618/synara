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
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const modelSwitchSupportMode = "emulated"

func normalizeRequiredModel(value string) (string, error) {
	normalized, err := normalizeModel(&value)
	if err != nil {
		return "", err
	}
	if normalized == nil {
		return "", problem.New(400, "invalid_model", "Model is required.")
	}
	return *normalized, nil
}

func normalizeExpectedModel(input SwitchModelInput) (*string, error) {
	if !input.ExpectedModelProvided {
		return nil, problem.New(400, "expected_model_required", "expectedModel must be provided and may be null.")
	}
	if input.ExpectedModel == nil {
		return nil, nil
	}
	normalized, err := normalizeModel(input.ExpectedModel)
	if err != nil || normalized == nil {
		return nil, problem.New(400, "invalid_expected_model", "expectedModel must be null or a valid model name.")
	}
	return normalized, nil
}

func (s *Service) SwitchModel(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input SwitchModelInput,
	requestID, ipAddress string,
) (Session, error) {
	item, _, err := s.SwitchModelWithIdempotency(
		ctx, principal, sessionID, input, "", requestID, ipAddress,
	)
	return item, err
}

func (s *Service) SwitchModelWithIdempotency(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input SwitchModelInput,
	idempotencyKey, requestID, ipAddress string,
) (Session, bool, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Session{}, false, err
	}
	current, _, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.ExecutionCreate)
	if err != nil {
		return Session{}, false, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return Session{}, false, problem.New(404, "session_not_found", "Session not found.")
	}
	modelName, err := normalizeRequiredModel(input.Model)
	if err != nil {
		return Session{}, false, err
	}
	expectedModel, err := normalizeExpectedModel(input)
	if err != nil {
		return Session{}, false, err
	}

	var changedEvent persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "session.model.switch", SuccessStatus: 200,
		Request: map[string]any{
			"sessionId": sessionID, "model": modelName, "expectedModel": expectedModel,
		},
	}, func(tx *gorm.DB) (Session, error) {
		locked, err := lockActiveSession(ctx, tx, tenantID, sessionID)
		if err != nil {
			return Session{}, err
		}
		if locked.Visibility == "private" && locked.CreatedBy != principal.UserID {
			return Session{}, problem.New(404, "session_not_found", "Session not found.")
		}
		if locked.Model != nil && *locked.Model == modelName {
			return toSession(locked), nil
		}
		if !modelsEqual(locked.Model, expectedModel) {
			apiError := problem.New(
				409,
				"session_model_conflict",
				"The Session model changed after this model-switch request was prepared.",
			)
			apiError.Details = map[string]any{
				"expectedModel": nullableModelValue(expectedModel),
				"currentModel":  nullableModelValue(locked.Model),
			}
			return Session{}, apiError
		}

		var activeExecutions int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND session_id = ? AND status IN ?", tenantID, sessionID, activeSessionExecutionStatuses).
			Count(&activeExecutions).Error; err != nil {
			return Session{}, problem.Wrap(500, "session_execution_check_failed", "Failed to inspect the active Session execution.", err)
		}
		if activeExecutions > 0 {
			return Session{}, problem.New(409, "session_execution_active", "The Session already has an active Turn execution.")
		}

		var target persistence.ExecutionTarget
		targetErr := tx.WithContext(ctx).
			Where("id = ? AND status = ?", locked.ExecutionTargetID, "active").
			Where("(tenant_id IS NULL OR tenant_id = ?) AND (organization_id IS NULL OR organization_id = ?)", tenantID, locked.OrganizationID).
			Take(&target).Error
		if errors.Is(targetErr, gorm.ErrRecordNotFound) {
			return Session{}, problem.New(409, "execution_target_unavailable", "The Session Execution Target is no longer available.")
		}
		if targetErr != nil {
			return Session{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to reload the Session Execution Target.", targetErr)
		}
		if err := s.requireObservedTargetProviderCapabilities(
			ctx, tx, target, locked.Provider, "model-switch",
		); err != nil {
			return Session{}, err
		}

		now := s.now().UTC()
		if err := releaseActiveRuntimeBindings(ctx, tx, locked, now); err != nil {
			return Session{}, err
		}
		if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", tenantID, sessionID).
			Updates(map[string]any{
				"model":                                      modelName,
				"current_runtime_binding_id":                 nil,
				"provider_resume_cursor_encrypted":           nil,
				"provider_resume_cursor_state":               "absent",
				"provider_resume_cursor_source_execution_id": nil,
				"provider_resume_cursor_source_generation":   nil,
				"provider_resume_cursor_history_sequence":    nil,
				"updated_at":                                 now,
			}).Error; err != nil {
			return Session{}, problem.Wrap(500, "session_model_update_failed", "Failed to update the Session model.", err)
		}
		previousModel := nullableModelValue(locked.Model)
		locked.Model = &modelName
		locked.CurrentRuntimeBindingID = nil
		locked.ProviderResumeCursorEncrypted = nil
		locked.ProviderResumeCursorState = "absent"
		locked.ProviderResumeCursorSourceExecutionID = nil
		locked.ProviderResumeCursorSourceGeneration = nil
		locked.ProviderResumeCursorHistorySequence = nil
		locked.UpdatedAt = now
		if _, err := s.ensureRuntimeBinding(ctx, tx, &locked); err != nil {
			return Session{}, err
		}

		changedEvent, err = appendEvent(ctx, tx, &locked, eventInput{
			EventType: "session.model.changed", ActorType: "user", ActorID: &principal.UserID,
			Payload: map[string]any{
				"previousModel": previousModel, "model": modelName,
				"provider": locked.Provider, "supportMode": modelSwitchSupportMode,
			},
		})
		if err != nil {
			return Session{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "session.model.changed", ResourceType: "agent_session", ResourceID: &sessionID,
			OrganizationID: &locked.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"previousModel": previousModel, "model": modelName,
				"provider": locked.Provider, "supportMode": modelSwitchSupportMode,
			},
		}); err != nil {
			return Session{}, err
		}
		return toSession(locked), nil
	})
	if err != nil {
		return Session{}, false, err
	}
	if !result.Replayed && changedEvent.EventID != uuid.Nil {
		s.events.publish(toEvent(changedEvent))
	}
	return result.Value, result.Replayed, nil
}

func releaseActiveRuntimeBindings(
	ctx context.Context,
	tx *gorm.DB,
	session persistence.AgentSession,
	now time.Time,
) error {
	if err := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
		Where("tenant_id = ? AND session_id = ? AND status = ?", session.TenantID, session.ID, "active").
		Updates(map[string]any{"status": "released", "released_at": now, "updated_at": now}).Error; err != nil {
		return problem.Wrap(500, "runtime_binding_release_failed", "Failed to release the previous Provider runtime binding.", err)
	}
	return nil
}

func modelsEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return strings.TrimSpace(*left) == strings.TrimSpace(*right)
}

func nullableModelValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
