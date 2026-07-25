package database

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSemanticCredentialAccessRefreshMigrationBackfillsWatermarkAndFencesLeaseAccess(t *testing.T) {
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
	if err := Migrate(ctx, db, migrationsThrough(t, "000054_kubernetes_terminal_suspend_proof.sql")); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
		Update("last_event_sequence", 7).Error; err != nil {
		t.Fatalf("seed Session last_event_sequence before 000055: %v", err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.MeaningfulActivitySequence != 7 {
		t.Fatalf("000055 did not conservatively backfill meaningful_activity_sequence: %#v", session)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
			Update("meaningful_activity_sequence", 8).Error,
		"chk_agent_sessions_meaningful_activity_sequence",
	)

	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"generation":                           2,
			"provider_credential_id_snapshot":      seed.credentialID,
			"provider_credential_version_snapshot": 1,
			"provider":                             "codex",
		}).Error; err != nil {
		t.Fatalf("bind Execution Provider Credential snapshot: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	grantID := uuid.New()
	if err := db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: grantID, TenantID: seed.tenantID, ExecutionID: seed.executionID, Generation: 2,
		CredentialID: seed.credentialID, CredentialVersion: 1, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create valid Execution Provider Credential Grant: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("generation", 3).Error; err != nil {
		t.Fatalf("bump Execution generation to seed mismatched grant: %v", err)
	}
	mismatchedGrantID := uuid.New()
	if err := db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: mismatchedGrantID, TenantID: seed.tenantID, ExecutionID: seed.executionID, Generation: 3,
		CredentialID: seed.credentialID, CredentialVersion: 1, CreatedAt: now.Add(time.Second),
	}).Error; err != nil {
		t.Fatalf("create mismatched Execution Provider Credential Grant: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("generation", 2).Error; err != nil {
		t.Fatalf("restore Execution generation: %v", err)
	}

	issuedAt := now.Add(2 * time.Second)
	activityAt := now.Add(3 * time.Second)
	renewedAt := now.Add(4 * time.Second)
	expiresAt := now.Add(10 * time.Minute)
	refreshDeadlineAt := now.Add(8 * time.Minute)
	hardExpiresAt := now.Add(30 * time.Minute)
	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Updates(map[string]any{
				"generation":                              2,
				"provider_credential_grant_id":            mismatchedGrantID,
				"provider_credential_access_serial":       1,
				"provider_credential_activity_sequence":   7,
				"provider_credential_activity_at":         activityAt,
				"provider_credential_access_issued_at":    issuedAt,
				"provider_credential_access_renewed_at":   renewedAt,
				"provider_credential_access_expires_at":   expiresAt,
				"provider_credential_refresh_deadline_at": refreshDeadlineAt,
				"provider_credential_hard_expires_at":     hardExpiresAt,
			}).Error,
		"frozen Execution Provider Credential Grant",
	)

	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"generation":                              2,
			"provider_credential_grant_id":            grantID,
			"provider_credential_access_serial":       1,
			"provider_credential_activity_sequence":   7,
			"provider_credential_activity_at":         activityAt,
			"provider_credential_access_issued_at":    issuedAt,
			"provider_credential_access_renewed_at":   renewedAt,
			"provider_credential_access_expires_at":   expiresAt,
			"provider_credential_refresh_deadline_at": refreshDeadlineAt,
			"provider_credential_hard_expires_at":     hardExpiresAt,
		}).Error; err != nil {
		t.Fatalf("attach valid Worker Lease Provider Credential access: %v", err)
	}

	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Update("provider_credential_access_issued_at", issuedAt.Add(time.Minute)).Error,
		"issued-at is immutable",
	)
	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Update("provider_credential_grant_id", mismatchedGrantID).Error,
		"Grant is immutable",
	)
	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":     4,
				"provider_credential_activity_sequence": 8,
				"provider_credential_activity_at":       activityAt.Add(time.Minute),
			}).Error,
		"serial must advance exactly once per state update",
	)
	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":       2,
				"provider_credential_refresh_deadline_at": activityAt.Add(-time.Second),
			}).Error,
		"access fields are invalid",
	)
	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":     2,
				"provider_credential_activity_sequence": 8,
				"provider_credential_activity_at":       activityAt.Add(-time.Second),
			}).Error,
		"activity_at cannot go backwards",
	)
	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":   2,
				"provider_credential_hard_expires_at": hardExpiresAt.Add(time.Minute),
			}).Error,
		"hard expiry cannot be extended",
	)

	secondRenewedAt := renewedAt.Add(time.Minute)
	secondExpiresAt := expiresAt.Add(time.Minute)
	secondRefreshDeadlineAt := refreshDeadlineAt.Add(time.Minute)
	secondActivityAt := activityAt.Add(time.Minute)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"provider_credential_access_serial":       2,
			"provider_credential_activity_sequence":   8,
			"provider_credential_activity_at":         secondActivityAt,
			"provider_credential_access_renewed_at":   secondRenewedAt,
			"provider_credential_access_expires_at":   secondExpiresAt,
			"provider_credential_refresh_deadline_at": secondRefreshDeadlineAt,
		}).Error; err != nil {
		t.Fatalf("renew valid Worker Lease Provider Credential access: %v", err)
	}
	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":     3,
				"provider_credential_activity_sequence": 8,
				"provider_credential_activity_at":       secondActivityAt.Add(time.Minute),
			}).Error,
		"activity_at must stay frozen without a newer activity sequence",
	)
	assertSemanticCredentialAccessRejected(
		t,
		db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":       3,
				"provider_credential_access_renewed_at":   renewedAt,
				"provider_credential_access_expires_at":   expiresAt,
				"provider_credential_refresh_deadline_at": refreshDeadlineAt,
			}).Error,
		"renewal windows cannot move backwards",
	)

	if err := db.Delete(&persistence.WorkerLease{},
		"tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).Error; err != nil {
		t.Fatalf("delete Worker Lease with semantic credential access fields: %v", err)
	}

	assertMigrationIndex(
		t, db, "idx_worker_leases_provider_credential_access_expiry",
		"provider_credential_access_expires_at,tenant_id,execution_id", "provider_credential_grant_id",
	)
	assertMigrationIndex(
		t, db, "idx_worker_leases_provider_credential_refresh_deadline",
		"provider_credential_refresh_deadline_at,tenant_id,execution_id", "provider_credential_grant_id",
	)
}

func assertSemanticCredentialAccessRejected(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil {
		t.Fatalf("semantic credential access migration accepted invalid state (want containing %q)", expected)
	}
	if errors.Is(err, gorm.ErrCheckConstraintViolated) || errors.Is(err, gorm.ErrForeignKeyViolated) {
		return
	}
	if expected != "" && strings.Contains(err.Error(), expected) {
		return
	}
	t.Fatalf("semantic credential access migration returned wrong rejection: %v (want containing %q or a translated constraint violation)", err, expected)
}
