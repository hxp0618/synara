package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestWorkerRevocationMigrationBackfillsAndFencesLogicalIdentity(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_WORKER_REVOCATION_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	}
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_WORKER_REVOCATION_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000033_provider_credential_scopes.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "worker-revocation-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	legacyRevoked := postgresPreRevocationWorker(domain.ExecutionTargetID, "legacy-revoked", now)
	legacyRevoked.Status = "terminated"
	legacyRevoked.TerminatedAt = &now
	legacyRevoked.CompatibilityStatus = "revoked"
	legacyRevoked.CompatibilityReason = nil
	legacyRevoked.CompatibilityCheckedAt = &now
	legacyRevoked.Incarnation = 2
	legacyRevokedPrevious := legacyRevoked
	legacyRevokedPrevious.ID = uuid.New()
	legacyRevokedPrevious.Incarnation = 1
	legacyRevokedPrevious.InstanceUID = uuid.NewString()
	legacyRevokedPrevious.AuthTokenHash = []byte(uuid.NewString())
	draining := postgresPreRevocationWorker(domain.ExecutionTargetID, "draining-worker", now)
	draining.Status = "draining"
	draining.DrainingAt = &now
	active := postgresPreRevocationWorker(domain.ExecutionTargetID, "active-worker", now)
	for _, worker := range []*persistence.WorkerInstance{&legacyRevokedPrevious, &legacyRevoked, &draining, &active} {
		if err := insertPreRevocationWorker(db, worker); err != nil {
			t.Fatalf("insert pre-000034 Worker %s: %v", worker.PodName, err)
		}
	}

	if err := Migrate(ctx, db, migrationsThrough(t, "000034_worker_revocation_fencing.sql")); err != nil {
		t.Fatal(err)
	}

	var migrated persistence.WorkerInstance
	if err := db.Where("id = ?", legacyRevoked.ID).Take(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if migrated.AdministrativeStatus != "revoked" || migrated.RevokedAt == nil || migrated.RevokedBy != nil ||
		migrated.RevocationReason == nil || *migrated.RevocationReason != "legacy-compatibility-revoked" ||
		migrated.CompatibilityStatus != "incompatible" || migrated.CompatibilityCheckedAt == nil {
		t.Fatalf("unexpected legacy Worker revocation migration: %#v", migrated)
	}
	for _, expected := range []struct {
		id     uuid.UUID
		status string
	}{
		{id: draining.ID, status: "draining"},
		{id: active.ID, status: "active"},
	} {
		var worker persistence.WorkerInstance
		if err := db.Where("id = ?", expected.id).Take(&worker).Error; err != nil {
			t.Fatal(err)
		}
		if worker.AdministrativeStatus != expected.status || worker.RevokedAt != nil ||
			worker.RevokedBy != nil || worker.RevocationReason != nil {
			t.Fatalf("Worker administrative backfill = %#v, want %s without revocation metadata", worker, expected.status)
		}
	}

	var tombstone persistence.WorkerIdentityTombstone
	if err := db.Where(
		"execution_target_id = ? AND cluster_id = ? AND namespace = ? AND pod_name = ?",
		legacyRevoked.ExecutionTargetID, legacyRevoked.ClusterID, legacyRevoked.Namespace, legacyRevoked.PodName,
	).Take(&tombstone).Error; err != nil {
		t.Fatal(err)
	}
	if tombstone.WorkerID != legacyRevoked.ID || tombstone.WorkerIncarnation != legacyRevoked.Incarnation ||
		tombstone.RevokedBy != nil || tombstone.RevocationReason != "legacy-compatibility-revoked" {
		t.Fatalf("unexpected legacy Worker identity tombstone: %#v", tombstone)
	}

	assertMigrationIndex(
		t, db, "idx_worker_instances_claimability",
		"execution_target_id,administrative_status,compatibility_status,status,last_heartbeat_at,id", "",
	)
	assertMigrationIndex(
		t, db, "idx_worker_instances_logical_identity",
		"execution_target_id,cluster_id,namespace,pod_name,administrative_status,id", "",
	)
	assertMigrationIndex(
		t, db, "uq_worker_instances_active_logical_identity",
		"execution_target_id,cluster_id,namespace,pod_name",
		"(administrative_status <> 'revoked'::text) AND (status <> 'terminated'::text)",
	)
	var oldCompatibilityIndex int64
	if err := db.Raw(`SELECT count(*) FROM pg_class WHERE oid = to_regclass('idx_worker_instances_compatibility')`).
		Scan(&oldCompatibilityIndex).Error; err != nil {
		t.Fatal(err)
	}
	if oldCompatibilityIndex != 0 {
		t.Fatal("000034 retained the redundant pre-administrative compatibility index")
	}

	replacement := postgresCurrentRevocationWorker(domain.ExecutionTargetID, legacyRevoked.PodName, now.Add(time.Second))
	if err := insertPreReleaseWorker(db, &replacement); err == nil {
		t.Fatalf("new instanceUid bypassed a revoked logical identity: %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", legacyRevoked.ID).
		Update("status", "offline").Error; err == nil {
		t.Fatalf("revoked Worker observation mutated: %v", err)
	}
	if err := db.Delete(&persistence.WorkerInstance{}, "id = ?", legacyRevoked.ID).Error; err == nil {
		t.Fatalf("revoked Worker row was deleted: %v", err)
	}

	terminatedHistory := postgresCurrentRevocationWorker(domain.ExecutionTargetID, "terminated-history", now)
	terminatedHistory.Status = "terminated"
	terminatedHistory.TerminatedAt = &now
	if err := insertPreReleaseWorker(db, &terminatedHistory); err != nil {
		t.Fatalf("create terminated Worker history: %v", err)
	}
	currentAfterTermination := postgresCurrentRevocationWorker(
		domain.ExecutionTargetID, terminatedHistory.PodName, now.Add(time.Second),
	)
	currentAfterTermination.Incarnation = terminatedHistory.Incarnation + 1
	if err := insertPreReleaseWorker(db, &currentAfterTermination); err != nil {
		t.Fatalf("create current Worker after terminated history: %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", currentAfterTermination.ID).
		Updates(map[string]any{
			"administrative_status": "revoked",
			"revoked_at":            now.Add(2 * time.Second),
			"revoked_by":            domain.UserID,
			"revocation_reason":     "revoke-current-after-terminated-history",
		}).Error; err != nil {
		t.Fatalf("terminated Worker history blocked current revocation: %v", err)
	}

	fresh := postgresCurrentRevocationWorker(domain.ExecutionTargetID, "fresh-revocation", now)
	if err := insertPreReleaseWorker(db, &fresh); err != nil {
		t.Fatal(err)
	}
	reason := "operator-security-revocation"
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", fresh.ID).
		Updates(map[string]any{
			"administrative_status": "revoked",
			"revoked_at":            now,
			"revocation_reason":     reason,
		}).Error; err == nil {
		t.Fatalf("new Worker revocation without actor = %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", fresh.ID).
		Updates(map[string]any{
			"administrative_status": "revoked",
			"revoked_at":            now,
			"revoked_by":            domain.UserID,
			"revocation_reason":     reason,
		}).Error; err != nil {
		t.Fatalf("valid Worker revocation failed: %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", active.ID).
		Update("compatibility_status", "revoked").Error; err == nil {
		t.Fatalf("compatibility_status retained revoked: %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", active.ID).
		Update("pod_name", "moved-worker").Error; err == nil {
		t.Fatalf("Worker logical identity mutated: %v", err)
	}
	if err := db.Delete(&persistence.WorkerIdentityTombstone{},
		"execution_target_id = ? AND cluster_id = ? AND namespace = ? AND pod_name = ?",
		fresh.ExecutionTargetID, fresh.ClusterID, fresh.Namespace, fresh.PodName,
	).Error; err == nil {
		t.Fatalf("Worker identity tombstone was deleted: %v", err)
	}
}

func insertPreRevocationWorker(db *gorm.DB, worker *persistence.WorkerInstance) error {
	return db.Select(
		"id", "incarnation", "instance_uid", "execution_target_id", "target_kind",
		"cluster_id", "namespace", "pod_name", "version", "protocol_version", "capabilities",
		"compatibility_status", "compatibility_reason", "compatibility_checked_at",
		"lease_supported", "fencing_supported", "auth_token_hash", "status",
		"registered_at", "last_heartbeat_at", "draining_at", "terminated_at",
	).Create(worker).Error
}

func insertPreReleaseWorker(db *gorm.DB, worker *persistence.WorkerInstance) error {
	return db.Omit(
		"WorkerReleaseRevisionID", "WorkerReleaseChannel", "WorkerReleaseStatus",
		"WorkerReleaseReason", "WorkerReleaseCheckedAt",
	).Create(worker).Error
}

func postgresPreRevocationWorker(targetID uuid.UUID, podName string, now time.Time) persistence.WorkerInstance {
	return persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(),
		ExecutionTargetID: targetID, TargetKind: "local", ClusterID: "postgres",
		Namespace: "default", PodName: podName, Version: "test", ProtocolVersion: 2,
		Capabilities: map[string]any{}, CompatibilityStatus: "unknown",
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte(uuid.NewString()),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}
}

func postgresCurrentRevocationWorker(targetID uuid.UUID, podName string, now time.Time) persistence.WorkerInstance {
	worker := postgresPreRevocationWorker(targetID, podName, now)
	worker.AdministrativeStatus = "active"
	return worker
}
