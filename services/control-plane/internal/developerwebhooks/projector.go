package developerwebhooks

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

type webhookPayload struct {
	SchemaVersion  string     `json:"schemaVersion"`
	DeliveryID     uuid.UUID  `json:"deliveryId"`
	EventID        uuid.UUID  `json:"eventId"`
	Type           string     `json:"type"`
	Sequence       int64      `json:"sequence"`
	TenantID       uuid.UUID  `json:"tenantId"`
	OrganizationID uuid.UUID  `json:"organizationId"`
	ProjectID      uuid.UUID  `json:"projectId"`
	SessionID      uuid.UUID  `json:"sessionId"`
	ExecutionID    *uuid.UUID `json:"executionId"`
	OccurredAt     time.Time  `json:"occurredAt"`
}

func (s *Service) ProjectSessionEvent(
	ctx context.Context,
	tx *gorm.DB,
	event persistence.SessionEvent,
) error {
	if s == nil || tx == nil {
		return nil
	}
	if _, supported := supportedEventTypes[event.EventType]; !supported {
		return nil
	}
	endpoints := make([]persistence.DeveloperWebhookEndpoint, 0)
	if err := tx.WithContext(ctx).Where(
		"tenant_id = ? AND status = ?", event.TenantID, "active",
	).Order("id").Find(&endpoints).Error; err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		if !containsEventType(endpoint.EventTypes, event.EventType) {
			continue
		}
		deliveryID := uuid.NewSHA1(endpoint.ID, event.EventID[:])
		payload := webhookPayload{
			SchemaVersion: "polaris.webhook.v1", DeliveryID: deliveryID,
			EventID: event.EventID, Type: event.EventType, Sequence: event.Sequence,
			TenantID: event.TenantID, OrganizationID: event.OrganizationID,
			ProjectID: event.ProjectID, SessionID: event.SessionID,
			ExecutionID: event.ExecutionID, OccurredAt: event.OccurredAt.UTC(),
		}
		payloadMap := map[string]any{
			"schemaVersion": payload.SchemaVersion, "deliveryId": payload.DeliveryID,
			"eventId": payload.EventID, "type": payload.Type, "sequence": payload.Sequence,
			"tenantId": payload.TenantID, "organizationId": payload.OrganizationID,
			"projectId": payload.ProjectID, "sessionId": payload.SessionID,
			"executionId": payload.ExecutionID, "occurredAt": payload.OccurredAt,
		}
		outboxMessage := persistence.OutboxMessage{
			ID: deliveryID, TenantID: &event.TenantID, Topic: OutboxTopic,
			MessageKey: endpoint.ID.String(), Payload: payloadMap,
			Headers:     map[string]any{"eventVersion": 1, "webhookEndpointId": endpoint.ID.String()},
			AvailableAt: event.OccurredAt.UTC(), CreatedAt: event.OccurredAt.UTC(),
		}
		inserted := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&outboxMessage)
		if inserted.Error != nil {
			return inserted.Error
		}
		if inserted.RowsAffected == 0 {
			continue
		}
		if err := tx.WithContext(ctx).Create(&persistence.DeveloperWebhookDelivery{
			ID: deliveryID, TenantID: event.TenantID, EndpointID: endpoint.ID,
			SessionEventID: event.EventID, EventType: event.EventType,
			SessionID: event.SessionID, ExecutionID: event.ExecutionID, Sequence: event.Sequence,
			OutboxMessageID: deliveryID, CreatedAt: event.OccurredAt.UTC(),
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func containsEventType(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
