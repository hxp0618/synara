package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestDeveloperWebhookMigrationPostgresConstraintsAndJoins(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	ctx := context.Background()
	db, err := Open(ctx, postgresisolation.URL(t, databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "developer-webhook-migration-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	endpoint := persistence.DeveloperWebhookEndpoint{
		ID: uuid.New(), TenantID: domain.TenantID, Name: "postgres-delivery",
		URL: "https://example.com/hooks/polaris", Status: "active",
		EventTypes: []string{"execution.failed"}, SecretVersion: 1,
		EncryptedSecret: []byte{1}, EncryptedDataKey: []byte{2},
		KMSProvider: "local", KMSKeyID: "migration-test", CreatedBy: domain.UserID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&endpoint).Error; err != nil {
		t.Fatalf("insert valid Webhook endpoint: %v", err)
	}
	duplicate := endpoint
	duplicate.ID = uuid.New()
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("Webhook endpoint uniqueness did not reject a duplicate Tenant/name")
	}
	invalid := endpoint
	invalid.ID, invalid.Name, invalid.Status = uuid.New(), "invalid-status", "pending"
	if err := db.Create(&invalid).Error; err == nil {
		t.Fatal("Webhook endpoint status constraint accepted an unknown status")
	}

	deliveryID := uuid.New()
	if err := db.Create(&persistence.OutboxMessage{
		ID: deliveryID, TenantID: &domain.TenantID, Topic: "developer.webhook.delivery",
		MessageKey: endpoint.ID.String(), Payload: map[string]any{"deliveryId": deliveryID.String()},
		Headers:     map[string]any{"webhookEndpointId": endpoint.ID.String()},
		AvailableAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("insert Webhook Outbox message: %v", err)
	}
	delivery := persistence.DeveloperWebhookDelivery{
		ID: deliveryID, TenantID: domain.TenantID, EndpointID: endpoint.ID,
		SessionEventID: uuid.New(), EventType: "execution.failed", SessionID: uuid.New(),
		Sequence: 1, OutboxMessageID: deliveryID, CreatedAt: now,
	}
	if err := db.Create(&delivery).Error; err != nil {
		t.Fatalf("insert valid Webhook delivery: %v", err)
	}
	duplicateDelivery := delivery
	duplicateDelivery.ID, duplicateDelivery.OutboxMessageID = uuid.New(), deliveryID
	if err := db.Create(&duplicateDelivery).Error; err == nil {
		t.Fatal("Webhook delivery Outbox uniqueness did not reject duplicate binding")
	}
	var joined int64
	if err := db.Table("developer_webhook_deliveries AS delivery").
		Joins("JOIN outbox_messages AS message ON message.id = delivery.outbox_message_id").
		Where("delivery.tenant_id = ? AND delivery.endpoint_id = ? AND message.topic = ?", domain.TenantID, endpoint.ID, "developer.webhook.delivery").
		Count(&joined).Error; err != nil {
		t.Fatal(err)
	}
	if joined != 1 {
		t.Fatalf("joined Webhook delivery count = %d, want 1", joined)
	}
}
