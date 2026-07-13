package database

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestExecutionProviderSnapshotMigrationFencesLegacyStateAndManifestMutation(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000029_provider_runtime_release_policy.sql")); err != nil {
		t.Fatal(err)
	}

	var legacySession persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&legacySession).Error; err != nil {
		t.Fatal(err)
	}
	legacyCiphertext := bytes.Clone(legacySession.ProviderResumeCursorEncrypted)

	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.ProviderCredentialIDSnapshot != nil || execution.ProviderCredentialVersionSnapshot != nil ||
		execution.ProviderResumeStrategySnapshot != "authoritative-history" ||
		execution.ProviderCursorBindingVersion != nil || len(execution.ProviderCursorBindingDigest) != 0 {
		t.Fatalf("legacy Execution received a guessed Provider snapshot: %#v", execution)
	}
	var migratedSession persistence.AgentSession
	if err := db.Select(
		"id", "tenant_id", "provider_resume_cursor_encrypted", "provider_resume_cursor_state",
		"provider_resume_cursor_source_execution_id", "provider_resume_cursor_source_generation",
		"provider_resume_cursor_history_sequence",
	).Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&migratedSession).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(migratedSession.ProviderResumeCursorEncrypted, legacyCiphertext) {
		t.Fatal("000030 modified the legacy encrypted Provider Cursor")
	}
	if migratedSession.ProviderResumeCursorState != "quarantined" ||
		migratedSession.ProviderResumeCursorSourceExecutionID != nil ||
		migratedSession.ProviderResumeCursorSourceGeneration != nil ||
		migratedSession.ProviderResumeCursorHistorySequence != nil {
		t.Fatalf("000031 did not quarantine the legacy Provider Cursor safely: %#v", migratedSession)
	}

	assertMigrationIndex(
		t,
		db,
		"idx_agent_executions_provider_credential_snapshot",
		"tenant_id,provider_credential_id_snapshot,provider_credential_version_snapshot,id",
		"provider_credential_id_snapshot IS NOT NULL",
	)

	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"generation":                           2,
			"provider_credential_id_snapshot":      seed.credentialID,
			"provider_credential_version_snapshot": 1,
		}).Error; err != nil {
		t.Fatalf("write next-generation Credential snapshot: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("provider_credential_version_snapshot", 2).Error; err == nil {
		t.Fatal("000030 allowed a same-generation Credential snapshot rewrite")
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"generation":                           3,
			"provider_credential_id_snapshot":      nil,
			"provider_credential_version_snapshot": 1,
		}).Error; err == nil {
		t.Fatal("000030 accepted a partial Credential snapshot")
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"generation":                           3,
			"provider_credential_id_snapshot":      uuid.New(),
			"provider_credential_version_snapshot": 1,
		}).Error; err == nil {
		t.Fatal("000030 accepted a Credential snapshot outside the Execution tenant")
	}

	manifestID := uuid.New()
	manifest := persistence.WorkerManifest{
		ID: manifestID, ManifestHash: strings.Repeat("c", 64), WorkerBuildVersion: "snapshot-worker",
		WorkerBuildGitSHA: nil, WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{},
	}
	providerManifest := persistence.WorkerProviderManifest{
		WorkerManifestID: manifestID, Provider: "codex", SupportTier: "tier-1",
		CompatibilityStatus: "compatible", ProviderHostMajor: 2, ProviderHostMinor: 1,
		HostBuildVersion: "snapshot-provider-host", AdapterVersion: "snapshot-adapter",
		RuntimeKind: "cli", RuntimeName: "codex", RuntimeVersion: stringPointer("0.144.1"),
		RuntimeAvailable: true, RuntimeVersionSource: "probe", RuntimeMinimumInclusive: "0.144.0",
		RuntimeCompatible: true, ReleaseRequiresExplicitEnablement: false, ReleaseEnabled: true,
		MaximumCommandBytes: 2 << 20, MaximumMessageBytes: 1 << 20,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		CredentialDeliveryModes: []string{"anonymous-fd"}, ResumeStrategies: []string{"native-cursor"},
		CapabilityDescriptorHash: strings.Repeat("d", 64),
		Capabilities:             map[string]any{"send-turn": "native", "resume-session": "native"},
	}
	if err := db.Create(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&providerManifest).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"generation":                        3,
			"worker_manifest_id":                manifestID,
			"provider_resume_strategy_snapshot": "native-cursor",
			"provider_cursor_binding_version":   1,
			"provider_cursor_binding_digest":    []byte("too-short"),
		}).Error; err == nil {
		t.Fatal("000030 accepted a malformed native Cursor binding digest")
	}
	digest := bytes.Repeat([]byte{0x5a}, 32)
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"generation":                        3,
			"worker_manifest_id":                manifestID,
			"provider_resume_strategy_snapshot": "native-cursor",
			"provider_cursor_binding_version":   1,
			"provider_cursor_binding_digest":    digest,
		}).Error; err != nil {
		t.Fatalf("write valid native Cursor generation snapshot: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("provider_cursor_binding_digest", bytes.Repeat([]byte{0x6b}, 32)).Error; err == nil {
		t.Fatal("000030 allowed a same-generation native Cursor binding rewrite")
	}
	if err := db.Model(&persistence.WorkerManifest{}).
		Where("id = ?", manifestID).
		Update("worker_build_version", "mutated-worker").Error; err == nil {
		t.Fatal("000030 allowed a content-addressed Worker manifest update")
	}
	if err := db.Model(&persistence.WorkerProviderManifest{}).
		Where("worker_manifest_id = ? AND provider = ?", manifestID, "codex").
		Update("adapter_version", "mutated-adapter").Error; err == nil {
		t.Fatal("000030 allowed a content-addressed Worker Provider manifest update")
	}
}

func TestSessionActiveExecutionMigrationRejectsAmbiguousLegacyQueue(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000030_execution_provider_cursor_snapshots.sql")); err != nil {
		t.Fatal(err)
	}
	var first persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&first).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	turnID := uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.AgentTurn{
			ID: turnID, TenantID: seed.tenantID, SessionID: seed.sessionID,
			CreatedBy: first.RequestedBy, Status: "queued", InputText: "ambiguous queued Turn", CreatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.AgentExecution{
			ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: turnID,
			Attempt: 1, Status: "queued", ExecutionTargetID: first.ExecutionTargetID, TargetKind: first.TargetKind,
			Provider: first.Provider, ProviderRuntimeBindingID: first.ProviderRuntimeBindingID,
			Generation: 0, RequestedBy: first.RequestedBy, QueuedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	err := Migrate(context.Background(), db, migrations.Files)
	if err == nil || !strings.Contains(err.Error(), "duplicate active rows exist") {
		t.Fatalf("000031 did not fail closed on ambiguous active Session executions: %v", err)
	}
	var applied int64
	if err := db.Table("control_plane_schema_migrations").Where("version = ?", 31).Count(&applied).Error; err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatal("000031 recorded a failed active Session migration as applied")
	}
}

func stringPointer(value string) *string { return &value }
