package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestExecutionTargetProvisioningMigrationPostgresConstraints(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "provisioning-migration-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "ssh", Name: "migration-target", Status: "offline", ConfigurationEncrypted: []byte{},
		Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	operation := persistence.ExecutionTargetProvisioningOperation{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: &domain.OrganizationID,
		ExecutionTargetID: target.ID, ActorType: "user", ActorID: domain.UserID,
		Action: "install", IdempotencyKey: "migration-operation", RequestHash: strings.Repeat("a", 64),
		State: "accepted", Result: map[string]any{}, RequestID: "migration", IPAddress: "127.0.0.1",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&operation).Error; err != nil {
		t.Fatal(err)
	}
	duplicateKey := operation
	duplicateKey.ID, duplicateKey.ExecutionTargetID = uuid.New(), target.ID
	if err := db.Create(&duplicateKey).Error; err == nil {
		t.Fatal("idempotency uniqueness accepted a duplicate")
	}
	activeConflict := operation
	activeConflict.ID, activeConflict.IdempotencyKey = uuid.New(), "second-operation"
	if err := db.Create(&activeConflict).Error; err == nil {
		t.Fatal("active-target uniqueness accepted concurrent provisioning")
	}
	invalid := operation
	invalid.ID, invalid.IdempotencyKey, invalid.Action = uuid.New(), "invalid-action", "destroy"
	if err := db.Create(&invalid).Error; err == nil {
		t.Fatal("action constraint accepted an unknown action")
	}
}
