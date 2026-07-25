package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestExecutionProviderCredentialGrantMigrationFencesSnapshotAndMutation(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_WORKSPACE_CREDENTIAL_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_WORKSPACE_CREDENTIAL_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	providerCredential := postgresWorkspaceCredential(
		seed.tenantID, session.OrganizationID, session.CreatedBy,
		"provider", "codex", "api_key", now,
	)
	if err := db.Create(&providerCredential).Error; err != nil {
		t.Fatalf("create Provider Credential: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"generation":                           2,
			"provider_credential_id_snapshot":      providerCredential.ID,
			"provider_credential_version_snapshot": providerCredential.Version,
			"provider":                             "codex",
		}).Error; err != nil {
		t.Fatalf("bind Execution Provider Credential snapshot: %v", err)
	}

	grant := persistence.ExecutionProviderCredentialGrant{
		ID: uuid.New(), TenantID: seed.tenantID, ExecutionID: seed.executionID, Generation: 2,
		CredentialID: providerCredential.ID, CredentialVersion: providerCredential.Version,
		CreatedAt: now,
	}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatalf("create valid Execution Provider Credential Grant: %v", err)
	}
	stale := grant
	stale.ID = uuid.New()
	stale.Generation = 3
	if err := db.Create(&stale).Error; err == nil {
		t.Fatal("PostgreSQL accepted a stale Execution Provider Credential Grant generation")
	}
	if err := db.Model(&persistence.ExecutionProviderCredentialGrant{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, grant.ID).
		Update("credential_version", 2).Error; err == nil {
		t.Fatal("PostgreSQL allowed an Execution Provider Credential Grant to mutate")
	}
	if err := db.Delete(&persistence.ExecutionProviderCredentialGrant{},
		"tenant_id = ? AND id = ?", seed.tenantID, grant.ID).Error; err == nil {
		t.Fatal("PostgreSQL allowed an Execution Provider Credential Grant to be deleted")
	}

	assertMigrationIndex(
		t, db, "idx_execution_provider_credential_grants_execution",
		"tenant_id,execution_id,generation,id", "",
	)
}
