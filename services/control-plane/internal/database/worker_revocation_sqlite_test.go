package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteWorkerRevocationFencesLogicalIdentityAndPreservesTombstone(t *testing.T) {
	ctx := context.Background()
	store, domain := openSQLiteWorkerRevocationStore(t, ctx, true)

	for _, name := range []string{
		"uq_worker_instances_active_logical_identity",
		"idx_worker_instances_claimability",
		"idx_worker_instances_logical_identity",
		"trg_worker_instances_revocation_shape_insert",
		"trg_worker_instances_revocation_shape_update",
		"trg_worker_instances_identity_immutable",
		"trg_worker_instances_revoked_immutable_update",
		"trg_worker_instances_revoked_immutable_delete",
		"trg_worker_instances_tombstone_update",
		"trg_worker_identity_tombstones_immutable_delete",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite Worker revocation safety object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	worker := sqliteRevocationWorker(domain.ExecutionTargetID, "revoked-worker", now)
	worker.Status = "terminated"
	worker.TerminatedAt = &now
	if err := store.DB().Create(&worker).Error; err != nil {
		t.Fatalf("create Worker before revocation: %v", err)
	}
	reason := "operator-compromise-response"
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", worker.ID).
		Updates(map[string]any{
			"administrative_status": "revoked",
			"revoked_at":            now,
			"revoked_by":            domain.UserID,
			"revocation_reason":     reason,
		}).Error; err != nil {
		t.Fatalf("revoke Worker: %v", err)
	}

	var tombstone persistence.WorkerIdentityTombstone
	if err := store.DB().Where(
		"execution_target_id = ? AND cluster_id = ? AND namespace = ? AND pod_name = ?",
		worker.ExecutionTargetID, worker.ClusterID, worker.Namespace, worker.PodName,
	).Take(&tombstone).Error; err != nil {
		t.Fatalf("load Worker identity tombstone: %v", err)
	}
	if tombstone.WorkerID != worker.ID || tombstone.WorkerIncarnation != worker.Incarnation ||
		tombstone.RevokedBy == nil || *tombstone.RevokedBy != domain.UserID ||
		tombstone.RevocationReason != reason {
		t.Fatalf("unexpected Worker identity tombstone: %#v", tombstone)
	}

	replacement := sqliteRevocationWorker(domain.ExecutionTargetID, worker.PodName, now.Add(time.Second))
	if err := store.DB().Create(&replacement).Error; err == nil {
		t.Fatal("SQLite allowed a new instanceUid to bypass a revoked logical Worker identity")
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", worker.ID).Update("status", "offline").Error; err == nil {
		t.Fatal("SQLite allowed a revoked Worker row to mutate")
	}
	if err := store.DB().Delete(&persistence.WorkerInstance{}, "id = ?", worker.ID).Error; err == nil {
		t.Fatal("SQLite allowed a revoked Worker row to be deleted")
	}
	if err := store.DB().Delete(&persistence.WorkerIdentityTombstone{},
		"execution_target_id = ? AND cluster_id = ? AND namespace = ? AND pod_name = ?",
		worker.ExecutionTargetID, worker.ClusterID, worker.Namespace, worker.PodName,
	).Error; err == nil {
		t.Fatal("SQLite allowed a Worker identity tombstone to be deleted")
	}

	terminatedHistory := sqliteRevocationWorker(domain.ExecutionTargetID, "terminated-history", now)
	terminatedHistory.Status = "terminated"
	terminatedHistory.TerminatedAt = &now
	if err := store.DB().Create(&terminatedHistory).Error; err != nil {
		t.Fatalf("create SQLite terminated Worker history: %v", err)
	}
	currentAfterTermination := sqliteRevocationWorker(
		domain.ExecutionTargetID, terminatedHistory.PodName, now.Add(time.Second),
	)
	currentAfterTermination.Incarnation = terminatedHistory.Incarnation + 1
	if err := store.DB().Create(&currentAfterTermination).Error; err != nil {
		t.Fatalf("create SQLite current Worker after terminated history: %v", err)
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).Where("id = ?", currentAfterTermination.ID).
		Updates(map[string]any{
			"administrative_status": "revoked",
			"revoked_at":            now.Add(2 * time.Second),
			"revoked_by":            domain.UserID,
			"revocation_reason":     "revoke-current-after-terminated-history",
		}).Error; err != nil {
		t.Fatalf("SQLite terminated Worker history blocked current revocation: %v", err)
	}

	missingActor := sqliteRevocationWorker(domain.ExecutionTargetID, "missing-actor", now)
	if err := store.DB().Create(&missingActor).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", missingActor.ID).
		Updates(map[string]any{
			"administrative_status": "revoked",
			"revoked_at":            now,
			"revocation_reason":     "missing-actor-must-fail",
		}).Error; err == nil {
		t.Fatal("SQLite accepted a new Worker revocation without an actor")
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", missingActor.ID).Update("compatibility_status", "revoked").Error; err == nil {
		t.Fatal("SQLite retained compatibility_status='revoked' as an administrative shortcut")
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", missingActor.ID).Update("pod_name", "moved-worker").Error; err == nil {
		t.Fatal("SQLite allowed a Worker logical identity to change")
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", missingActor.ID).
		Updates(map[string]any{"status": "draining", "draining_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var selfDraining persistence.WorkerInstance
	if err := store.DB().Where("id = ?", missingActor.ID).Take(&selfDraining).Error; err != nil {
		t.Fatal(err)
	}
	if selfDraining.Status != "draining" || selfDraining.AdministrativeStatus != "active" {
		t.Fatalf("SQLite safety rerun coupled observation and administrative status: %#v", selfDraining)
	}
}

func TestSQLiteWorkerRevocationBackfillsLegacyCompatibilityRevocation(t *testing.T) {
	ctx := context.Background()
	store, domain := openSQLiteWorkerRevocationStore(t, ctx, false)
	now := time.Now().UTC().Truncate(time.Second)
	worker := sqliteRevocationWorker(domain.ExecutionTargetID, "legacy-revoked", now)
	worker.CompatibilityStatus = "revoked"
	worker.CompatibilityReason = nil
	worker.CompatibilityCheckedAt = &now
	worker.Incarnation = 2
	previous := worker
	previous.ID = uuid.New()
	previous.Incarnation = 1
	previous.InstanceUID = uuid.NewString()
	previous.AuthTokenHash = []byte(uuid.NewString())
	for _, legacy := range []*persistence.WorkerInstance{&previous, &worker} {
		if err := store.DB().Create(legacy).Error; err != nil {
			t.Fatalf("create legacy compatibility-revoked Worker: %v", err)
		}
	}

	if err := migrateWorkerRevocationSQLiteSafety(ctx, store.DB()); err != nil {
		t.Fatal(err)
	}
	var migrated persistence.WorkerInstance
	if err := store.DB().Where("id = ?", worker.ID).Take(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if migrated.AdministrativeStatus != "revoked" || migrated.RevokedAt == nil || migrated.RevokedBy != nil ||
		migrated.RevocationReason == nil || *migrated.RevocationReason != "legacy-compatibility-revoked" ||
		migrated.CompatibilityStatus != "incompatible" || migrated.CompatibilityCheckedAt == nil {
		t.Fatalf("unexpected legacy Worker revocation backfill: %#v", migrated)
	}
	var tombstone persistence.WorkerIdentityTombstone
	if err := store.DB().
		Where("execution_target_id = ? AND cluster_id = ? AND namespace = ? AND pod_name = ?",
			worker.ExecutionTargetID, worker.ClusterID, worker.Namespace, worker.PodName).
		Take(&tombstone).Error; err != nil {
		t.Fatal(err)
	}
	if tombstone.WorkerID != worker.ID || tombstone.WorkerIncarnation != 2 {
		t.Fatalf("legacy Worker identity tombstone did not retain the latest incarnation: %#v", tombstone)
	}
}

func openSQLiteWorkerRevocationStore(
	t *testing.T,
	ctx context.Context,
	withSafety bool,
) (MetadataStore, bootstrap.Result) {
	t.Helper()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if withSafety {
		err = store.Migrate(ctx, migrations.Files)
	} else {
		err = store.DB().WithContext(ctx).AutoMigrate(persistence.AllModels()...)
	}
	if err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-worker-revoke-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return store, domain
}

func sqliteRevocationWorker(targetID uuid.UUID, podName string, now time.Time) persistence.WorkerInstance {
	return persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(),
		ExecutionTargetID: targetID, TargetKind: "local", ClusterID: "sqlite",
		Namespace: "default", PodName: podName, Version: "test", ProtocolVersion: 2,
		Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: []byte(uuid.NewString()), Status: "online", AdministrativeStatus: "active",
		CompatibilityStatus: "unknown", RegisteredAt: now, LastHeartbeatAt: now,
	}
}
