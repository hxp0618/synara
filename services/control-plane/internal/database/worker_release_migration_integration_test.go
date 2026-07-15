package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkerReleaseRolloutMigrationEnforcesRevisionPolicyAndClaimFencing(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000036_project_git_binding_authority.sql")); err != nil {
		t.Fatal(err)
	}

	seed := seedWorkerReleaseMigrationBase(t, db)
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var legacyWorker persistence.WorkerInstance
	if err := db.Where("id = ?", seed.workerID).Take(&legacyWorker).Error; err != nil {
		t.Fatal(err)
	}
	if legacyWorker.WorkerReleaseStatus != "unmanaged" || legacyWorker.WorkerReleaseRevisionID != nil ||
		legacyWorker.WorkerReleaseChannel != nil || legacyWorker.WorkerReleaseReason != nil ||
		legacyWorker.WorkerReleaseCheckedAt != nil {
		t.Fatalf("legacy Worker release backfill = %#v", legacyWorker)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	firstRevisionID, secondRevisionID, otherTargetRevisionID := uuid.New(), uuid.New(), uuid.New()
	first := persistence.WorkerReleaseRevision{
		ID: firstRevisionID, TenantID: seed.tenantID, ExecutionTargetID: seed.targetID,
		Revision: 1, WorkerManifestID: seed.firstManifestID,
		Description: "initial", CreatedBy: seed.userID, CreatedAt: now,
	}
	second := persistence.WorkerReleaseRevision{
		ID: secondRevisionID, TenantID: seed.tenantID, ExecutionTargetID: seed.targetID,
		Revision: 2, WorkerManifestID: seed.secondManifestID,
		Description: "canary", CreatedBy: seed.userID, CreatedAt: now,
	}
	otherTarget := persistence.WorkerReleaseRevision{
		ID: otherTargetRevisionID, TenantID: seed.tenantID, ExecutionTargetID: seed.otherTargetID,
		Revision: 1, WorkerManifestID: seed.thirdManifestID,
		Description: "other target", CreatedBy: seed.userID, CreatedAt: now,
	}
	for _, revision := range []*persistence.WorkerReleaseRevision{&first, &second, &otherTarget} {
		if err := db.Create(revision).Error; err != nil {
			t.Fatalf("insert Worker release revision %#v: %v", revision, err)
		}
	}
	if err := db.Create(&persistence.WorkerReleaseRevision{
		ID: uuid.New(), TenantID: seed.tenantID, ExecutionTargetID: seed.targetID,
		Revision: 2, WorkerManifestID: seed.thirdManifestID,
		Description: "duplicate number", CreatedBy: seed.userID, CreatedAt: now,
	}).Error; err == nil {
		t.Fatal("migration accepted a duplicate target-scoped Worker release number")
	}
	if err := db.Model(&persistence.WorkerReleaseRevision{}).
		Where("id = ?", firstRevisionID).Update("description", "mutated").Error; err == nil {
		t.Fatal("migration accepted mutation of an immutable Worker release revision")
	}
	if err := db.Delete(&persistence.WorkerReleaseRevision{}, "id = ?", secondRevisionID).Error; err == nil {
		t.Fatal("migration accepted deletion of an immutable Worker release revision")
	}

	policy := persistence.WorkerReleasePolicy{
		TenantID: seed.tenantID, ExecutionTargetID: seed.targetID, PolicyVersion: 1,
		PromotedRevisionID: firstRevisionID, UpdatedBy: seed.userID, UpdatedAt: now,
	}
	if err := db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.WorkerReleasePolicy{
		TenantID: seed.tenantID, ExecutionTargetID: seed.otherTargetID, PolicyVersion: 1,
		PromotedRevisionID: firstRevisionID, UpdatedBy: seed.userID, UpdatedAt: now,
	}).Error; err == nil {
		t.Fatal("migration accepted a promoted revision owned by another Execution Target")
	}
	if err := db.Model(&persistence.WorkerReleasePolicy{}).
		Where("execution_target_id = ?", seed.targetID).
		Updates(map[string]any{"policy_version": 3, "updated_at": now.Add(time.Second)}).Error; err == nil {
		t.Fatal("migration accepted a Worker release policy version skip")
	}
	if err := db.Model(&persistence.WorkerReleasePolicy{}).
		Where("execution_target_id = ? AND policy_version = ?", seed.targetID, 1).
		Updates(map[string]any{
			"policy_version": 2, "canary_revision_id": secondRevisionID,
			"canary_percent": 10, "updated_at": now.Add(time.Second),
		}).Error; err != nil {
		t.Fatalf("exact Worker release policy CAS failed: %v", err)
	}
	if err := db.Delete(&persistence.WorkerReleasePolicy{}, "execution_target_id = ?", seed.targetID).Error; err == nil {
		t.Fatal("migration accepted deletion of a Worker release policy")
	}

	requestID := "release-migration-test"
	invalidTransition := persistence.WorkerReleaseTransition{
		ID: uuid.New(), TenantID: seed.tenantID, ExecutionTargetID: seed.targetID,
		PolicyVersion: 2, Action: "canary", FromPromotedRevisionID: &firstRevisionID,
		ToPromotedRevisionID: secondRevisionID, ToCanaryRevisionID: &firstRevisionID,
		CanaryPercent: 10, Reason: "must match the committed policy", ActorID: seed.userID,
		RequestID: &requestID, OccurredAt: now.Add(time.Second),
	}
	if err := db.Create(&invalidTransition).Error; err == nil {
		t.Fatal("migration accepted Worker release history that did not match the committed policy")
	}
	transition := persistence.WorkerReleaseTransition{
		ID: uuid.New(), TenantID: seed.tenantID, ExecutionTargetID: seed.targetID,
		PolicyVersion: 2, Action: "canary", FromPromotedRevisionID: &firstRevisionID,
		ToPromotedRevisionID: firstRevisionID, ToCanaryRevisionID: &secondRevisionID,
		CanaryPercent: 10, Reason: "migration invariant", ActorID: seed.userID,
		RequestID: &requestID, OccurredAt: now.Add(time.Second),
	}
	if err := db.Create(&transition).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerReleaseTransition{}).
		Where("id = ?", transition.ID).Update("reason", "mutated").Error; err == nil {
		t.Fatal("migration accepted mutation of immutable Worker release history")
	}
	if err := db.Delete(&persistence.WorkerReleaseTransition{}, "id = ?", transition.ID).Error; err == nil {
		t.Fatal("migration accepted deletion of immutable Worker release history")
	}

	checkedAt := now.Add(2 * time.Second)
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", seed.workerID).
		Updates(map[string]any{
			"worker_release_status": "active", "worker_release_revision_id": firstRevisionID,
			"worker_release_channel": "promoted", "worker_release_checked_at": checkedAt,
		}).Error; err != nil {
		t.Fatalf("valid Worker release activation failed: %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", seed.workerID).
		Updates(map[string]any{
			"worker_release_revision_id": nil, "worker_release_channel": nil,
		}).Error; err == nil {
		t.Fatal("migration accepted an active Worker without a release revision and channel")
	}

	projectID, sessionID, turnID := uuid.New(), uuid.New(), uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.Project{
				ID: projectID, TenantID: seed.tenantID, OrganizationID: seed.organizationID,
				Name: "Worker release migration", DefaultBranch: "main", Visibility: "organization",
				CreatedBy: seed.userID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentSession{
				ID: sessionID, TenantID: seed.tenantID, OrganizationID: seed.organizationID,
				ProjectID: projectID, CreatedBy: seed.userID, Title: "Worker release migration",
				Status: "active", Visibility: "private", Provider: "codex",
				ExecutionTargetID: seed.targetID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentTurn{
				ID: turnID, TenantID: seed.tenantID, SessionID: sessionID, CreatedBy: seed.userID,
				Status: "queued", InputText: "test release fence", TurnKind: "message",
				RuntimeMode: "approval-required", InteractionMode: "default", CreatedAt: now,
			},
		}
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	validChannel := "promoted"
	if err := db.Create(&persistence.AgentExecution{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: seed.targetID, TargetKind: "local",
		WorkerReleaseRevisionID: &firstRevisionID, WorkerReleaseChannel: &validChannel,
		RequestedBy: seed.userID, QueuedAt: now,
	}).Error; err != nil {
		t.Fatalf("valid Execution release selection failed: %v", err)
	}
	otherTurnID := uuid.New()
	if err := db.Create(&persistence.AgentTurn{
		ID: otherTurnID, TenantID: seed.tenantID, SessionID: sessionID, CreatedBy: seed.userID,
		Status: "queued", InputText: "cross-target release", TurnKind: "message",
		RuntimeMode: "approval-required", InteractionMode: "default", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.AgentExecution{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: sessionID, TurnID: otherTurnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: seed.targetID, TargetKind: "local",
		WorkerReleaseRevisionID: &otherTargetRevisionID, WorkerReleaseChannel: &validChannel,
		RequestedBy: seed.userID, QueuedAt: now,
	}).Error; err == nil {
		t.Fatal("migration accepted an Execution release revision owned by another Target")
	}

	assertCredentialBindingFKIndexes(t, db)
}

type workerReleaseMigrationSeed struct {
	userID, tenantID, organizationID, targetID, otherTargetID uuid.UUID
	firstManifestID, secondManifestID, thirdManifestID        uuid.UUID
	workerID                                                  uuid.UUID
}

func seedWorkerReleaseMigrationBase(t *testing.T, db *gorm.DB) workerReleaseMigrationSeed {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	seed := workerReleaseMigrationSeed{
		userID: uuid.New(), tenantID: uuid.New(), organizationID: uuid.New(),
		targetID: uuid.New(), otherTargetID: uuid.New(), firstManifestID: uuid.New(),
		secondManifestID: uuid.New(), thirdManifestID: uuid.New(), workerID: uuid.New(),
	}
	models := []any{
		&persistence.User{
			ID: seed.userID, Email: uuid.NewString() + "@example.com", DisplayName: "Release migration",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Tenant{
			ID: seed.tenantID, Slug: "release-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			Name: "Release migration", Status: "active", PlanCode: "free", Region: "default",
			Settings: map[string]any{}, CreatedBy: seed.userID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.TenantMembership{
			TenantID: seed.tenantID, UserID: seed.userID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Organization{
			ID: seed.organizationID, TenantID: seed.tenantID, Slug: "root", Name: "Root",
			Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: seed.userID,
			CreatedAt: now, UpdatedAt: now,
		},
		&persistence.ExecutionTarget{
			ID: seed.targetID, TenantID: &seed.tenantID, OrganizationID: &seed.organizationID,
			Kind: "local", Name: "release-target", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.ExecutionTarget{
			ID: seed.otherTargetID, TenantID: &seed.tenantID, OrganizationID: &seed.organizationID,
			Kind: "local", Name: "other-release-target", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, manifest := range []persistence.WorkerManifest{
		{ID: seed.firstManifestID, ManifestHash: strings.Repeat("a", 64), WorkerBuildVersion: "worker-v1", WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2, RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now},
		{ID: seed.secondManifestID, ManifestHash: strings.Repeat("b", 64), WorkerBuildVersion: "worker-v2", WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2, RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now},
		{ID: seed.thirdManifestID, ManifestHash: strings.Repeat("c", 64), WorkerBuildVersion: "worker-v3", WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2, RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now},
	} {
		manifest := manifest
		models = append(models, &manifest)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		worker := persistence.WorkerInstance{
			ID: seed.workerID, Incarnation: 1, InstanceUID: uuid.NewString(),
			ExecutionTargetID: seed.targetID, TargetKind: "local", ClusterID: "release-migration",
			Namespace: "default", PodName: "worker-v1", Version: "worker-v1", ProtocolVersion: 2,
			Capabilities: map[string]any{}, CurrentManifestID: &seed.firstManifestID,
			CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
			LeaseSupported: true, FencingSupported: true, AuthTokenHash: secret.HashToken(uuid.NewString()),
			Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
		}
		return tx.Omit(
			"WorkerReleaseRevisionID", "WorkerReleaseChannel", "WorkerReleaseStatus",
			"WorkerReleaseReason", "WorkerReleaseCheckedAt",
		).Create(&worker).Error
	}); err != nil {
		t.Fatal(err)
	}
	return seed
}

func assertCredentialBindingFKIndexes(t *testing.T, db *gorm.DB) {
	t.Helper()
	rows := make([]struct {
		IndexName string `gorm:"column:indexname"`
		IndexDef  string `gorm:"column:indexdef"`
	}, 0)
	if err := db.Raw(`
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND indexname IN (
		    'idx_credential_bindings_project_fk',
		    'idx_credential_bindings_execution_target_fk',
		    'idx_credential_bindings_created_by_fk',
		    'idx_credential_bindings_disabled_by_fk'
		  )
		ORDER BY indexname
	`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("credential Binding FK indexes = %#v", rows)
	}
	wants := map[string][]string{
		"idx_credential_bindings_project_fk":          {"tenant_id", "project_id", "project_id IS NOT NULL"},
		"idx_credential_bindings_execution_target_fk": {"tenant_id", "execution_target_id", "execution_target_id IS NOT NULL"},
		"idx_credential_bindings_created_by_fk":       {"tenant_id", "created_by"},
		"idx_credential_bindings_disabled_by_fk":      {"tenant_id", "disabled_by", "disabled_by IS NOT NULL"},
	}
	for _, row := range rows {
		for _, fragment := range wants[row.IndexName] {
			if !strings.Contains(row.IndexDef, fragment) {
				t.Fatalf("%s definition %q omits %q", row.IndexName, row.IndexDef, fragment)
			}
		}
	}
}
