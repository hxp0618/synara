package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func (s *Service) AppendRuntimeEvent(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input RuntimeEventInput,
	requestID string,
) (OperationResult[RuntimeEventResult], error) {
	input.EventType = strings.TrimSpace(input.EventType)
	if input.EventID == uuid.Nil {
		return OperationResult[RuntimeEventResult]{}, problem.New(400, "invalid_event_id", "eventId is required.")
	}
	if len(input.EventType) < 3 || len(input.EventType) > 160 || strings.ContainsAny(input.EventType, "\r\n\t") {
		return OperationResult[RuntimeEventResult]{}, problem.New(400, "invalid_event_type", "eventType must contain between 3 and 160 characters.")
	}
	if input.EventVersion <= 0 {
		input.EventVersion = 1
	}
	if input.Payload == nil {
		input.Payload = map[string]any{}
	}
	if input.OccurredAt.IsZero() {
		input.OccurredAt = s.now()
	}

	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker.ID, requestID, "execution.runtime_event", struct {
		ExecutionID uuid.UUID         `json:"executionId"`
		Input       RuntimeEventInput `json:"input"`
	}{executionID, input}, 201, func(tx *gorm.DB) (RuntimeEventResult, error) {
		var existing persistence.SessionEvent
		existingErr := tx.WithContext(ctx).
			Where("tenant_id = ? AND event_id = ?", input.TenantID, input.EventID).Take(&existing).Error
		if existingErr == nil {
			if existing.ExecutionID == nil || *existing.ExecutionID != executionID ||
				existing.WorkerID == nil || *existing.WorkerID != worker.ID ||
				existing.Generation == nil || *existing.Generation != input.Generation ||
				existing.EventType != input.EventType || existing.EventVersion != input.EventVersion ||
				!sameJSON(existing.Payload, input.Payload) {
				return RuntimeEventResult{}, problem.New(409, "event_id_reused", "eventId was already used for different event content.")
			}
			return RuntimeEventResult{
				EventID: existing.EventID, SessionID: existing.SessionID,
				Sequence: existing.Sequence, EventVersion: existing.EventVersion,
			}, nil
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return RuntimeEventResult{}, problem.Wrap(500, "runtime_event_lookup_failed", "Failed to inspect the runtime event.", existingErr)
		}
		_, execution, err := s.lockLease(ctx, tx, worker.ID, executionID, input.LeaseInput, true)
		if err != nil {
			return RuntimeEventResult{}, err
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventID: &input.EventID, EventVersion: input.EventVersion,
			EventType: input.EventType, ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: input.Payload, OccurredAt: &input.OccurredAt,
		})
		if err != nil {
			return RuntimeEventResult{}, err
		}
		return RuntimeEventResult{
			EventID: appended.EventID, SessionID: appended.SessionID,
			Sequence: appended.Sequence, EventVersion: appended.EventVersion,
		}, nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func sameJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
