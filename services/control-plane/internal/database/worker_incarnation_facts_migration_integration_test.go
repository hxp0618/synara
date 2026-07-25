package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkerIncarnationFactsMigrationFencesIdentityTimelineAndTerminalState(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000059_execution_generation_facts.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedWorkerIncarnationFactMigrationState(t, db)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatalf("load seeded execution: %v", err)
	}
	if execution.WorkerID == nil {
		t.Fatal("seeded execution is missing its worker")
	}

	var worker persistence.WorkerInstance
	if err := db.Where("id = ?", *execution.WorkerID).Take(&worker).Error; err != nil {
		t.Fatalf("load seeded worker: %v", err)
	}
	var fact persistence.WorkerIncarnationFact
	if err := db.Where(
		"worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation,
	).Take(&fact).Error; err != nil {
		t.Fatalf("load backfilled worker incarnation fact: %v", err)
	}
	if fact.ExecutionTargetID != execution.ExecutionTargetID || fact.CurrentState != "active" ||
		fact.ClaimCount != 1 || fact.InstanceUID != worker.InstanceUID {
		t.Fatalf("backfilled worker incarnation fact = %#v", fact)
	}
	now := fact.StateChangedAt

	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.WorkerIncarnationFact{}).
			Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
			Update("region", "cn-west-1").Error,
		"chk_worker_incarnation_facts_identity_immutable",
	)

	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.WorkerIncarnationFact{}).
			Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
			Update("claim_count", 0).Error,
		"chk_worker_incarnation_facts_timeline",
	)

	terminatedAt := now.Add(2 * time.Second)
	terminatedReason := "evicted"
	if err := db.Model(&persistence.WorkerIncarnationFact{}).
		Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
		Updates(map[string]any{
			"current_state":              "terminated",
			"state_changed_at":           now.Add(time.Second),
			"terminated_at":              terminatedAt,
			"terminal_reason":            terminatedReason,
			"accumulated_active_seconds": 12,
			"accumulated_idle_seconds":   3,
			"claim_count":                2,
			"updated_at":                 terminatedAt,
		}).Error; err != nil {
		t.Fatalf("terminate valid worker incarnation fact: %v", err)
	}

	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.WorkerIncarnationFact{}).
			Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
			Updates(map[string]any{"claim_count": 3, "updated_at": terminatedAt.Add(time.Second)}).Error,
		"chk_worker_incarnation_facts_terminal_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Delete(&persistence.WorkerIncarnationFact{},
			"worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation,
		).Error,
		"chk_worker_incarnation_facts_delete_immutable",
	)
}

func seedWorkerIncarnationFactMigrationState(t *testing.T, db *gorm.DB) stage3MigrationSeed {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID, organizationID := uuid.New(), uuid.New(), uuid.New()
	projectID, targetID := uuid.New(), uuid.New()
	sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New()
	workerID, credentialID := uuid.New(), uuid.New()
	repositoryURL := "https://example.com/synara.git"
	instanceUID := uuid.New().String()

	models := []struct {
		value  any
		fields []string
	}{
		{&persistence.User{
			ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Migration test",
			Status: "active", EmailVerifiedAt: &now,
		}, nil},
		{&persistence.Tenant{
			ID: tenantID, Slug: "migration-" + uuid.NewString()[:8], Name: "Migration tenant",
			Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{},
			CreatedBy: userID,
		}, nil},
		{&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now,
		}, nil},
		{&persistence.Organization{
			ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root",
			Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID,
		}, nil},
		{&persistence.OrganizationMembership{
			TenantID: tenantID, OrganizationID: organizationID, UserID: userID, Role: "owner", Status: "active",
		}, nil},
		{&persistence.ProviderCredential{
			ID: credentialID, TenantID: tenantID, OrganizationID: &organizationID,
			Scope: "organization", AutoSelectEnabled: false,
			Name: "Migration provider", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
			EncryptedPayload: []byte("encrypted-provider-payload"),
			EncryptedDataKey: []byte("encrypted-provider-data-key"),
			KMSProvider:      "local", KMSKeyID: "migration", AADVersion: 1, Version: 1,
			CreatedBy: userID, UpdatedBy: userID, CreatedAt: now, UpdatedAt: now,
		}, []string{
			"id", "tenant_id", "organization_id", "scope", "auto_select_enabled",
			"name", "purpose", "provider", "credential_type",
			"encrypted_payload", "encrypted_data_key", "kms_provider", "kms_key_id",
			"aad_version", "version", "created_by", "updated_by", "created_at", "updated_at",
		}},
		{&persistence.Project{
			ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Migration project",
			RepositoryURL: &repositoryURL, DefaultBranch: "main", Visibility: "organization", CreatedBy: userID,
			CreatedAt: now, UpdatedAt: now,
		}, []string{
			"id", "tenant_id", "organization_id", "name", "repository_url", "default_branch",
			"visibility", "created_by", "created_at", "updated_at",
		}},
		{&persistence.ExecutionTarget{
			ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID,
			Kind: "kubernetes", Name: "Migration target", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
			CreatedAt: now, UpdatedAt: now,
		}, []string{
			"id", "tenant_id", "organization_id", "kind", "name", "status",
			"configuration_encrypted", "capabilities", "created_at", "updated_at",
		}},
		{&persistence.AgentSession{
			ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID,
			CreatedBy: userID, Title: "Migration session", Status: "active", Visibility: "private",
			Provider: "codex", ProviderCredentialID: &credentialID, ExecutionTargetID: targetID,
			WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now,
		}, []string{
			"id", "tenant_id", "organization_id", "project_id", "created_by", "title", "status",
			"visibility", "provider", "provider_credential_id", "execution_target_id",
			"warm_pool_mode", "created_at", "updated_at",
		}},
		{&persistence.AgentTurn{
			ID: turnID, TenantID: tenantID, SessionID: sessionID, CreatedBy: userID,
			Status: "running", InputText: "Continue", StartedAt: &now, CreatedAt: now,
		}, []string{
			"id", "tenant_id", "session_id", "created_by", "status", "input_text", "started_at", "created_at",
		}},
		{&persistence.WorkerInstance{
			ID: workerID, Incarnation: 1, InstanceUID: instanceUID,
			ExecutionTargetID: targetID, TargetKind: "kubernetes", WorkerMode: "execution-pinned",
			RegistrationTrustMode: "shared-token",
			ClusterID:             "migration", Namespace: "default", PodName: "migration-worker",
			Version: "legacy", ProtocolVersion: 2, Capabilities: map[string]any{},
			CompatibilityStatus: "unknown", WorkerReleaseStatus: "unmanaged",
			LeaseSupported: true, FencingSupported: true,
			AuthTokenHash: secret.HashToken(stage3MigrationLegacyWorkerToken),
			Status:        "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
		}, []string{
			"id", "incarnation", "instance_uid", "execution_target_id", "target_kind", "worker_mode",
			"registration_trust_mode", "cluster_id", "namespace", "pod_name",
			"version", "protocol_version", "capabilities", "compatibility_status", "worker_release_status",
			"lease_supported", "fencing_supported", "auth_token_hash",
			"status", "administrative_status", "registered_at", "last_heartbeat_at",
		}},
		{&persistence.AgentExecution{
			ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
			Attempt: 1, Status: "waiting-for-approval",
			ExecutionTargetID: targetID, TargetKind: "kubernetes", WorkerID: &workerID,
			ProviderResumeStrategySnapshot: "authoritative-history",
			Generation:                     1, RequestedBy: userID, QueuedAt: now, StartedAt: &now,
		}, []string{
			"id", "tenant_id", "session_id", "turn_id", "attempt", "status",
			"execution_target_id", "target_kind", "worker_id", "provider_resume_strategy_snapshot",
			"generation", "requested_by", "queued_at", "started_at",
		}},
		{&persistence.WorkerLease{
			ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
			WorkerIncarnation: 1, WorkerInstanceUID: instanceUID,
			Generation: 1, LeaseTokenHash: []byte("migration-lease-token"),
			AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour),
		}, []string{
			"execution_id", "tenant_id", "worker_id", "worker_incarnation", "worker_instance_uid",
			"generation", "lease_token_hash", "acquired_at", "heartbeat_at", "expires_at",
		}},
	}

	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			query := tx
			if len(model.fields) > 0 {
				query = query.Select(model.fields)
			}
			if err := query.Create(model.value).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed worker incarnation migration state: %v", err)
	}

	return stage3MigrationSeed{
		tenantID: tenantID, sessionID: sessionID, turnID: turnID, executionID: executionID, credentialID: credentialID,
	}
}
