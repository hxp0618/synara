package outbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestDispatcherPublishesAndAcknowledgesBatch(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	service := testService(t, &now, 3)
	first := seedMessage(t, service.db, now, "execution.queued", "one")
	second := seedMessage(t, service.db, now, "artifact.ready", "two")
	publisher := &memoryPublisher{}
	dispatcher, err := NewDispatcher(service, publisher, time.Second, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	count, err := dispatcher.DispatchOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || len(publisher.messages) != 2 {
		t.Fatalf("unexpected dispatch count: claimed=%d published=%d", count, len(publisher.messages))
	}
	for _, id := range []uuid.UUID{first, second} {
		var model persistence.OutboxMessage
		if err := service.db.Where("id = ?", id).Take(&model).Error; err != nil {
			t.Fatal(err)
		}
		if model.PublishedAt == nil || model.ClaimedBy != nil || model.LastError != nil {
			t.Fatalf("message was not acknowledged cleanly: %#v", model)
		}
	}
}

func TestDispatcherRetriesThenDeadLettersAndReplays(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	service := testService(t, &now, 2)
	tenantID := uuid.New()
	messageID := seedTenantMessage(t, service.db, tenantID, now, "execution.queued", "retry")
	dispatcher, err := NewDispatcher(service, PublisherFunc(func(context.Context, Message) error {
		return errors.New("provider token=secret\nrequest failed")
	}), time.Second, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.DispatchOnce(context.Background()); err == nil {
		t.Fatal("expected the first publish to fail")
	}
	var retry persistence.OutboxMessage
	if err := service.db.Where("id = ?", messageID).Take(&retry).Error; err != nil {
		t.Fatal(err)
	}
	if retry.Attempts != 1 || retry.DeadLetteredAt != nil || retry.LastError == nil || strings.Contains(*retry.LastError, "\n") {
		t.Fatalf("unexpected retry state: %#v", retry)
	}
	if strings.Contains(*retry.LastError, "secret") || !strings.Contains(*retry.LastError, "[REDACTED]") {
		t.Fatalf("publisher error was not safely redacted: %q", *retry.LastError)
	}
	now = retry.AvailableAt.Add(time.Millisecond)
	if _, err := dispatcher.DispatchOnce(context.Background()); err == nil {
		t.Fatal("expected the second publish to fail")
	}
	var dead persistence.OutboxMessage
	if err := service.db.Where("id = ?", messageID).Take(&dead).Error; err != nil {
		t.Fatal(err)
	}
	if dead.Attempts != 2 || dead.DeadLetteredAt == nil || dead.ClaimedBy != nil {
		t.Fatalf("message was not dead-lettered: %#v", dead)
	}
	replayed, err := service.Replay(context.Background(), tenantID, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Attempts != 0 {
		t.Fatalf("replay did not reset attempts: %#v", replayed)
	}
}

func TestExpiredClaimCanBeRecoveredByAnotherInstance(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	first := testServiceWithInstance(t, testDB(t), &now, "first", 3)
	messageID := seedMessage(t, first.db, now, "execution.queued", "recovery")
	claimed, err := first.Claim(context.Background())
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim failed: messages=%d err=%v", len(claimed), err)
	}
	second := testServiceWithInstance(t, first.db, &now, "second", 3)
	if messages, err := second.Claim(context.Background()); err != nil || len(messages) != 0 {
		t.Fatalf("active claim was stolen: messages=%d err=%v", len(messages), err)
	}
	now = now.Add(31 * time.Second)
	recovered, err := second.Claim(context.Background())
	if err != nil || len(recovered) != 1 || recovered[0].ID != messageID {
		t.Fatalf("expired claim was not recovered: %#v err=%v", recovered, err)
	}
}

func TestStatsReportPendingRetryDeadLetterAndAge(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	service := testService(t, &now, 3)
	seedMessage(t, service.db, now.Add(-time.Minute), "execution.queued", "pending")
	retryingID := seedMessage(t, service.db, now, "execution.queued", "retrying")
	deadID := seedMessage(t, service.db, now, "execution.queued", "dead")
	deadAt := now
	if err := service.db.Model(&persistence.OutboxMessage{}).Where("id = ?", retryingID).Update("attempts", 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&persistence.OutboxMessage{}).Where("id = ?", deadID).Updates(map[string]any{"attempts": 3, "dead_lettered_at": deadAt}).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := service.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 2 || stats.Retrying != 1 || stats.DeadLettered != 1 || stats.OldestPending < time.Minute {
		t.Fatalf("unexpected outbox stats: %#v", stats)
	}
}

func TestTenantOperatorsCanInspectAndAuditReplayWithoutPayloadExposure(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	db := adminTestDB(t)
	service := testServiceWithInstance(t, db, &now, "admin", 3)
	userID := uuid.New()
	tenantID := uuid.New()
	deadAt := now
	messageID := uuid.New()
	models := []any{
		&persistence.User{ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Outbox auditor", Status: "active"},
		&persistence.Tenant{ID: tenantID, Slug: "outbox-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10], Name: "Outbox tenant", Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "auditor", Status: "active", JoinedAt: &now},
		&persistence.OutboxMessage{ID: messageID, TenantID: &tenantID, Topic: "artifact.ready", MessageKey: uuid.NewString(), Payload: map[string]any{"secret": "must-not-be-listed"}, Headers: map[string]any{}, Attempts: 3, AvailableAt: now, CreatedAt: now, DeadLetteredAt: &deadAt},
		&persistence.OutboxMessage{ID: uuid.New(), TenantID: &tenantID, Topic: "worker.release.promoted", MessageKey: uuid.NewString(), Payload: map[string]any{}, Headers: map[string]any{}, Attempts: 3, AvailableAt: now, CreatedAt: now.Add(time.Second), DeadLetteredAt: &deadAt},
	}
	for _, model := range models {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	principal := identity.Principal{UserID: userID, ActiveTenantID: &tenantID}
	items, err := service.ListForTenant(context.Background(), principal, tenantID, ListQuery{
		Status: "dead-letter", TopicPrefix: "artifact.", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != messageID || items[0].Status != "dead-letter" {
		t.Fatalf("unexpected operational list: %#v", items)
	}
	_, err = service.ListForTenant(context.Background(), principal, tenantID, ListQuery{
		Status: "all", TopicPrefix: "worker.release.%", Limit: 10,
	})
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "invalid_outbox_topic_prefix" {
		t.Fatalf("invalid topic prefix error = %v", err)
	}
	if _, err := service.ReplayAuthorized(context.Background(), principal, tenantID, messageID, "request-auditor", "127.0.0.1"); err == nil {
		t.Fatal("auditor was allowed to replay a dead-letter message")
	}
	if err := db.Model(&persistence.TenantMembership{}).Where("tenant_id = ? AND user_id = ?", tenantID, userID).Update("role", "admin").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReplayAuthorized(context.Background(), principal, tenantID, messageID, "request-admin", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var auditCount int64
	if err := db.Model(&persistence.AuditLog{}).Where("tenant_id = ? AND action = ? AND resource_id = ?", tenantID, "outbox.replayed", messageID).Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("replay audit record count = %d", auditCount)
	}
}

type memoryPublisher struct {
	mu       sync.Mutex
	messages []Message
}

func (p *memoryPublisher) Publish(_ context.Context, message Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, message)
	return nil
}

func testService(t *testing.T, now *time.Time, maxAttempts int) *Service {
	t.Helper()
	return testServiceWithInstance(t, testDB(t), now, "test-instance", maxAttempts)
}

func testServiceWithInstance(t *testing.T, db *gorm.DB, now *time.Time, instance string, maxAttempts int) *Service {
	t.Helper()
	service, err := NewService(db, Config{
		InstanceID: instance, BatchSize: 20, ClaimTTL: 30 * time.Second,
		MaxAttempts: maxAttempts, BaseBackoff: time.Second, MaxBackoff: time.Minute,
		Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.OutboxMessage{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func adminTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.User{}, &persistence.Tenant{}, &persistence.TenantMembership{},
		&persistence.AuditLog{}, &persistence.OutboxMessage{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedMessage(t *testing.T, db *gorm.DB, now time.Time, topic, key string) uuid.UUID {
	t.Helper()
	return seedTenantMessage(t, db, uuid.Nil, now, topic, key)
}

func seedTenantMessage(t *testing.T, db *gorm.DB, tenantID uuid.UUID, now time.Time, topic, key string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var tenant *uuid.UUID
	if tenantID != uuid.Nil {
		tenant = &tenantID
	}
	model := persistence.OutboxMessage{
		ID: id, TenantID: tenant, Topic: topic, MessageKey: key,
		Payload: map[string]any{"id": id}, Headers: map[string]any{"eventVersion": 1},
		AvailableAt: now, CreatedAt: now,
	}
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	return id
}
