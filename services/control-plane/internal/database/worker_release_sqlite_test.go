package database

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkerReleaseSQLiteSafetyAllowsTargetRevisionHistoryAndRejectsMutation(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "worker-release-sqlite-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	now := time.Now().UTC().Truncate(time.Microsecond)
	firstManifestID, secondManifestID := uuid.New(), uuid.New()
	for _, manifest := range []persistence.WorkerManifest{
		{
			ID: firstManifestID, ManifestHash: releaseSQLiteDigest('a'), WorkerBuildVersion: "worker-v1",
			WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
			OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now,
		},
		{
			ID: secondManifestID, ManifestHash: releaseSQLiteDigest('b'), WorkerBuildVersion: "worker-v2",
			WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
			OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now,
		},
	} {
		manifest := manifest
		if err := db.Create(&manifest).Error; err != nil {
			t.Fatal(err)
		}
	}
	firstRevisionID, secondRevisionID := uuid.New(), uuid.New()
	for _, revision := range []persistence.WorkerReleaseRevision{
		{
			ID: firstRevisionID, TenantID: domain.TenantID, ExecutionTargetID: domain.ExecutionTargetID,
			Revision: 1, WorkerManifestID: firstManifestID, Description: "initial",
			CreatedBy: domain.UserID, CreatedAt: now,
		},
		{
			ID: secondRevisionID, TenantID: domain.TenantID, ExecutionTargetID: domain.ExecutionTargetID,
			Revision: 2, WorkerManifestID: secondManifestID, Description: "canary",
			CreatedBy: domain.UserID, CreatedAt: now,
		},
	} {
		revision := revision
		if err := db.Create(&revision).Error; err != nil {
			t.Fatalf("same-Target revision history was rejected: %v", err)
		}
	}
	assertSQLiteIndexColumns(t, db, "uq_worker_release_revision_tenant_id", []string{"tenant_id", "id"})
	assertSQLiteIndexColumns(t, db, "uq_worker_release_revision_target_id", []string{"execution_target_id", "id"})
	assertSQLiteIndexColumns(t, db, "uq_worker_release_revision_target_number", []string{"execution_target_id", "revision"})
	assertSQLiteIndexColumns(t, db, "uq_worker_release_revision_target_manifest", []string{"execution_target_id", "worker_manifest_id"})

	if err := db.Model(&persistence.WorkerReleaseRevision{}).
		Where("id = ?", firstRevisionID).Update("description", "mutated").Error; err == nil {
		t.Fatal("SQLite accepted mutation of an immutable Worker release revision")
	}
	if err := db.Delete(&persistence.WorkerReleaseRevision{}, "id = ?", secondRevisionID).Error; err == nil {
		t.Fatal("SQLite accepted deletion of an immutable Worker release revision")
	}

	policy := persistence.WorkerReleasePolicy{
		TenantID: domain.TenantID, ExecutionTargetID: domain.ExecutionTargetID,
		PolicyVersion: 1, PromotedRevisionID: firstRevisionID,
		UpdatedBy: domain.UserID, UpdatedAt: now,
	}
	if err := db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerReleasePolicy{}).
		Where("execution_target_id = ?", domain.ExecutionTargetID).
		Update("policy_version", 3).Error; err == nil {
		t.Fatal("SQLite accepted a Worker release policy version skip")
	}
	if err := db.Model(&persistence.WorkerReleasePolicy{}).
		Where("execution_target_id = ? AND policy_version = ?", domain.ExecutionTargetID, 1).
		Updates(map[string]any{
			"policy_version": 2, "canary_revision_id": secondRevisionID,
			"canary_percent": 25, "updated_at": now.Add(time.Second),
		}).Error; err != nil {
		t.Fatalf("SQLite exact policy CAS failed: %v", err)
	}
	requestID := "worker-release-sqlite"
	transition := persistence.WorkerReleaseTransition{
		ID: uuid.New(), TenantID: domain.TenantID, ExecutionTargetID: domain.ExecutionTargetID,
		PolicyVersion: 2, Action: "canary", FromPromotedRevisionID: &firstRevisionID,
		ToPromotedRevisionID: firstRevisionID, ToCanaryRevisionID: &secondRevisionID,
		CanaryPercent: 25, Reason: "SQLite safety", ActorID: domain.UserID,
		RequestID: &requestID, OccurredAt: now.Add(time.Second),
	}
	if err := db.Create(&transition).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerReleaseTransition{}).
		Where("id = ?", transition.ID).Update("reason", "mutated").Error; err == nil {
		t.Fatal("SQLite accepted mutation of immutable Worker release history")
	}

	channel := "promoted"
	workerID := uuid.New()
	worker := persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(),
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
		ClusterID: "sqlite-release", Namespace: "default", PodName: "worker-v1",
		Version: "worker-v1", ProtocolVersion: 2, Capabilities: map[string]any{},
		CurrentManifestID: &firstManifestID, CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
		WorkerReleaseRevisionID: &firstRevisionID, WorkerReleaseChannel: &channel,
		WorkerReleaseStatus: "active", WorkerReleaseCheckedAt: &now,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: secret.HashToken(uuid.NewString()),
		Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", workerID).
		Updates(map[string]any{
			"worker_release_revision_id": nil,
			"worker_release_channel":     nil,
		}).Error; err == nil {
		t.Fatal("SQLite accepted an active Worker without a release revision and channel")
	}

	projectID, sessionID, turnID := uuid.New(), uuid.New(), uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.Project{
				ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
				Name: "Worker release SQLite", DefaultBranch: "main", Visibility: "organization",
				CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentSession{
				ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
				ProjectID: projectID, CreatedBy: domain.UserID, Title: "Worker release SQLite",
				Status: "active", Visibility: "private", Provider: "codex",
				ExecutionTargetID: domain.ExecutionTargetID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentTurn{
				ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
				Status: "queued", InputText: "SQLite release fence", TurnKind: "message",
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
	if err := db.Create(&persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
		WorkerReleaseChannel: &channel, RequestedBy: domain.UserID, QueuedAt: now,
	}).Error; err == nil {
		t.Fatal("SQLite accepted an Execution release channel without a release revision")
	}
}

func assertSQLiteIndexColumns(t *testing.T, db *gorm.DB, indexName string, want []string) {
	t.Helper()
	rows, err := db.Raw("PRAGMA index_info('" + indexName + "')").Rows()
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make([]string, 0, len(want))
	for rows.Next() {
		var sequence, columnID int
		var name string
		if err := rows.Scan(&sequence, &columnID, &name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SQLite index %s columns = %#v, want %#v", indexName, got, want)
	}
}

func releaseSQLiteDigest(character byte) string {
	return strings.Repeat(string(character), 64)
}
