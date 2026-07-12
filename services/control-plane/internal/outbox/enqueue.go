package outbox

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

type EnqueueInput struct {
	ID          uuid.UUID
	TenantID    *uuid.UUID
	Topic       string
	MessageKey  string
	Payload     map[string]any
	Headers     map[string]any
	AvailableAt time.Time
	CreatedAt   time.Time
}

func Enqueue(ctx context.Context, db *gorm.DB, input EnqueueInput) error {
	now := time.Now().UTC()
	if input.ID == uuid.Nil {
		input.ID = uuid.New()
	}
	if input.AvailableAt.IsZero() {
		input.AvailableAt = now
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = now
	}
	if input.Payload == nil {
		input.Payload = map[string]any{}
	}
	if input.Headers == nil {
		input.Headers = map[string]any{"eventVersion": 1}
	}
	return db.WithContext(ctx).Create(&persistence.OutboxMessage{
		ID: input.ID, TenantID: input.TenantID, Topic: input.Topic, MessageKey: input.MessageKey,
		Payload: input.Payload, Headers: input.Headers,
		AvailableAt: input.AvailableAt, CreatedAt: input.CreatedAt,
	}).Error
}
