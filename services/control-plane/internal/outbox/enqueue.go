package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
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
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := enforceOutboxAdmission(ctx, tx, input.Topic); err != nil {
			return err
		}
		return tx.WithContext(ctx).Create(&persistence.OutboxMessage{
			ID: input.ID, TenantID: input.TenantID, Topic: input.Topic, MessageKey: input.MessageKey,
			Payload: input.Payload, Headers: input.Headers,
			AvailableAt: input.AvailableAt, CreatedAt: input.CreatedAt,
		}).Error
	})
}

func enforceOutboxAdmission(ctx context.Context, tx *gorm.DB, topic string) error {
	// Recovery, cancellation, terminal, credential, and cleanup events must
	// always be able to drain the system. Only a new Execution admission can be
	// rejected when the durable delivery boundary is already saturated.
	if strings.TrimSpace(topic) != "execution.queued" {
		return nil
	}
	if tx.Dialector.Name() == "sqlite" && !tx.Migrator().HasTable(&persistence.OutboxPressureState{}) {
		return nil
	}
	if tx.Dialector.Name() == "postgres" {
		if err := tx.WithContext(ctx).
			Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "synara:outbox-admission").Error; err != nil {
			return fmt.Errorf("acquire Outbox admission lock: %w", err)
		}
	}
	var state persistence.OutboxPressureState
	err := tx.WithContext(ctx).Where("singleton_key = ?", "global").Take(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load Outbox pressure policy: %w", err)
	}
	var pending int64
	if err := tx.WithContext(ctx).Model(&persistence.OutboxMessage{}).
		Where("published_at IS NULL AND dead_lettered_at IS NULL").Count(&pending).Error; err != nil {
		return fmt.Errorf("count Outbox pressure for admission: %w", err)
	}
	if pending >= state.ThrottleDepth {
		return problem.New(
			503,
			"outbox_backpressure_throttled",
			"New Execution admission is temporarily throttled while the durable Outbox backlog drains.",
		)
	}
	return nil
}
