package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

// Enqueue is the write half of the transactional outbox: every domain event in
// the system reaches the dispatcher through it, and it is always called with
// the caller's transaction so the message commits or rolls back together with
// the state change it describes. Losing that — for instance by resolving the
// handle from a package-level connection during a refactor — would publish
// events for writes that never landed. It had no test.
func TestEnqueueCommitsAndRollsBackWithTheCallerTransaction(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	service := testService(t, &now, 3)

	committedID := uuid.New()
	if err := service.db.Transaction(func(tx *gorm.DB) error {
		return Enqueue(context.Background(), tx, EnqueueInput{
			ID: committedID, Topic: "execution.queued", MessageKey: "commit",
		})
	}); err != nil {
		t.Fatal(err)
	}

	rolledBackID := uuid.New()
	sentinel := errors.New("business write failed")
	err := service.db.Transaction(func(tx *gorm.DB) error {
		if err := Enqueue(context.Background(), tx, EnqueueInput{
			ID: rolledBackID, Topic: "execution.queued", MessageKey: "rollback",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("transaction returned %v, want the sentinel", err)
	}

	var committed int64
	if err := service.db.Model(&persistence.OutboxMessage{}).
		Where("id = ?", committedID).Count(&committed).Error; err != nil {
		t.Fatal(err)
	}
	if committed != 1 {
		t.Fatalf("committed message count = %d, want 1", committed)
	}
	var abandoned int64
	if err := service.db.Model(&persistence.OutboxMessage{}).
		Where("id = ?", rolledBackID).Count(&abandoned).Error; err != nil {
		t.Fatal(err)
	}
	if abandoned != 0 {
		t.Fatal("a rolled-back transaction still left an outbox message behind")
	}
}

// The repo rule is that every cross-process event is versioned, so an enqueue
// that supplies no headers must still carry a version rather than none.
func TestEnqueueDefaultsAndPreservesCallerValues(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	service := testService(t, &now, 3)

	if err := Enqueue(context.Background(), service.db, EnqueueInput{
		Topic: "turn.created", MessageKey: "defaults",
	}); err != nil {
		t.Fatal(err)
	}
	var defaulted persistence.OutboxMessage
	if err := service.db.Where("message_key = ?", "defaults").Take(&defaulted).Error; err != nil {
		t.Fatal(err)
	}
	if defaulted.ID == uuid.Nil {
		t.Fatal("Enqueue did not mint an identifier")
	}
	if defaulted.Headers["eventVersion"] == nil {
		t.Fatalf("defaulted headers carry no event version: %#v", defaulted.Headers)
	}
	if defaulted.Payload == nil {
		t.Fatal("defaulted payload is nil rather than an empty object")
	}
	if defaulted.AvailableAt.IsZero() || defaulted.CreatedAt.IsZero() {
		t.Fatalf("timestamps were not defaulted: %#v", defaulted)
	}

	// Delayed delivery depends on an explicit AvailableAt surviving untouched.
	deferredUntil := now.Add(time.Hour)
	explicitID := uuid.New()
	if err := Enqueue(context.Background(), service.db, EnqueueInput{
		ID: explicitID, Topic: "turn.created", MessageKey: "explicit",
		Headers:     map[string]any{"eventVersion": 7},
		AvailableAt: deferredUntil, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	var explicit persistence.OutboxMessage
	if err := service.db.Where("id = ?", explicitID).Take(&explicit).Error; err != nil {
		t.Fatal(err)
	}
	if !explicit.AvailableAt.UTC().Equal(deferredUntil) {
		t.Fatalf("AvailableAt was rewritten to %s, want %s", explicit.AvailableAt, deferredUntil)
	}
	if explicit.Headers["eventVersion"] == nil {
		t.Fatalf("caller headers were dropped: %#v", explicit.Headers)
	}
}
