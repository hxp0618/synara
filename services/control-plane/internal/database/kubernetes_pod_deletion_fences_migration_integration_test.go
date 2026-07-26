package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/podlifecycle"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresKubernetesPodDeletionFenceIsScopedImmutableIdempotentAndTargetCascaded(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	fixture := seedWorkerPoolWarmCapacityFixture(t, db)
	identity, err := podlifecycle.NewExactPodIdentity(
		fixture.target.ID,
		"default",
		"durable-fence",
		uuid.NewString(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		acquired, err := podlifecycle.TryTransactionLogicalIdentityLock(ctx, tx, identity)
		if err != nil {
			return err
		}
		if !acquired {
			t.Fatal("PostgreSQL exact Pod lifecycle lock was unexpectedly unavailable")
		}
		if err := podlifecycle.EnsureDeletionFence(ctx, tx, identity, fixture.now, "first-delete"); err != nil {
			return err
		}
		return podlifecycle.EnsureDeletionFence(ctx, tx, identity, fixture.now.Add(time.Minute), "retry-delete")
	}); err != nil {
		t.Fatal(err)
	}
	var fences []persistence.KubernetesPodDeletionFence
	if err := db.Where(
		"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
		identity.ExecutionTargetID, identity.Namespace, identity.PodName, identity.PodUID,
	).Find(&fences).Error; err != nil {
		t.Fatal(err)
	}
	if len(fences) != 1 || fences[0].Reason != "first-delete" || !fences[0].RequestedAt.Equal(fixture.now) {
		t.Fatalf("idempotent durable fence = %#v", fences)
	}
	invalidClusterWorker := sqliteRevocationWorker(identity.ExecutionTargetID, "invalid-cluster-pod", fixture.now)
	invalidClusterWorker.TargetKind = "kubernetes"
	invalidClusterWorker.ClusterID = "cluster-a"
	invalidClusterWorker.RegistrationTrustMode = "kubernetes-pod-bound-v1"
	if err := db.Create(&invalidClusterWorker).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL direct noncanonical cluster Worker INSERT rejection = %v", err)
	}
	fencedWorker := sqliteRevocationWorker(identity.ExecutionTargetID, identity.PodName, fixture.now)
	fencedWorker.TargetKind = "kubernetes"
	fencedWorker.ClusterID = "kubernetes"
	fencedWorker.RegistrationTrustMode = "kubernetes-pod-bound-v1"
	fencedWorker.Namespace = identity.Namespace
	fencedWorker.InstanceUID = identity.PodUID
	if err := db.Create(&fencedWorker).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL direct fenced Worker INSERT rejection = %v", err)
	}
	replacementWorker := sqliteRevocationWorker(identity.ExecutionTargetID, identity.PodName, fixture.now)
	replacementWorker.TargetKind = "kubernetes"
	replacementWorker.ClusterID = "kubernetes"
	replacementWorker.RegistrationTrustMode = "kubernetes-pod-bound-v1"
	replacementWorker.Namespace = identity.Namespace
	if err := db.Create(&replacementWorker).Error; err != nil {
		t.Fatalf("PostgreSQL rejected replacement Pod UID Worker: %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).
		Where("id = ?", replacementWorker.ID).
		Update("cluster_id", "cluster-a").Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL direct noncanonical cluster Worker UPDATE rejection = %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).
		Where("id = ?", replacementWorker.ID).
		Update("instance_uid", identity.PodUID).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL direct fenced Worker UID UPDATE rejection = %v", err)
	}
	if err := db.Model(&persistence.KubernetesPodDeletionFence{}).
		Where(
			"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
			identity.ExecutionTargetID, identity.Namespace, identity.PodName, identity.PodUID,
		).
		Update("reason", "mutated").Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL fence UPDATE rejection = %v", err)
	}
	if err := db.Delete(&persistence.KubernetesPodDeletionFence{},
		"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
		identity.ExecutionTargetID, identity.Namespace, identity.PodName, identity.PodUID,
	).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL fence DELETE rejection = %v", err)
	}

	replacement := persistence.KubernetesPodDeletionFence{
		ExecutionTargetID: identity.ExecutionTargetID,
		Namespace:         identity.Namespace,
		PodName:           identity.PodName,
		PodUID:            replacementWorker.InstanceUID,
		RequestedAt:       fixture.now,
		Reason:            "replacement-pod",
	}
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatalf("replacement Pod UID fence was incorrectly coupled to the old UID: %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", replacementWorker.ID).
		Updates(map[string]any{"status": "draining", "draining_at": fixture.now}).Error; err != nil {
		t.Fatalf("PostgreSQL fenced Worker could not enter draining: %v", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", replacementWorker.ID).
		Updates(map[string]any{"status": "online", "draining_at": nil}).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL fenced Worker reactivation rejection = %v", err)
	}

	localTarget := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &fixture.tenantID, Kind: "local", Name: "invalid-fence-local", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}
	if err := db.Create(&localTarget).Error; err != nil {
		t.Fatal(err)
	}
	invalidScope := replacement
	invalidScope.ExecutionTargetID = localTarget.ID
	invalidScope.PodUID = uuid.NewString()
	if err := db.Create(&invalidScope).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL invalid target fence rejection = %v", err)
	}

	cascadeTarget := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &fixture.tenantID, Kind: "kubernetes", Name: "fence-cascade", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}
	if err := db.Create(&cascadeTarget).Error; err != nil {
		t.Fatal(err)
	}
	cascadeFence := replacement
	cascadeFence.ExecutionTargetID = cascadeTarget.ID
	cascadeFence.PodName = "cascade-pod"
	cascadeFence.PodUID = uuid.NewString()
	if err := db.Create(&cascadeFence).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&persistence.ExecutionTarget{}, "id = ?", cascadeTarget.ID).Error; err != nil {
		t.Fatalf("PostgreSQL target cascade was blocked by durable fence immutability: %v", err)
	}
	var cascadeCount int64
	if err := db.Model(&persistence.KubernetesPodDeletionFence{}).
		Where("execution_target_id = ?", cascadeTarget.ID).
		Count(&cascadeCount).Error; err != nil {
		t.Fatal(err)
	}
	if cascadeCount != 0 {
		t.Fatalf("PostgreSQL target cascade retained %d Pod deletion fences", cascadeCount)
	}

	assertMigrationIndex(
		t,
		db,
		"idx_kubernetes_pod_deletion_fences_requested",
		"execution_target_id,requested_at",
		"",
	)
	var applied int64
	if err := db.Table("control_plane_schema_migrations").Where("version = ?", 70).Count(&applied).Error; err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("migration 70 applied records = %d, want 1", applied)
	}
}

func TestPostgresKubernetesPodDeletionFenceLogicalIdentityLockSerializesTransactions(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	fixture := seedWorkerPoolWarmCapacityFixture(t, db)
	identity, err := podlifecycle.NewExactPodIdentity(
		fixture.target.ID,
		"default",
		"concurrent-fence",
		uuid.NewString(),
	)
	if err != nil {
		t.Fatal(err)
	}

	first := db.WithContext(ctx).Begin()
	if first.Error != nil {
		t.Fatal(first.Error)
	}
	firstCommitted := false
	t.Cleanup(func() {
		if !firstCommitted {
			_ = first.Rollback().Error
		}
	})
	acquired, err := podlifecycle.TryTransactionLogicalIdentityLock(ctx, first, identity)
	if err != nil || !acquired {
		t.Fatalf("first logical identity lock = acquired=%t err=%v", acquired, err)
	}

	second := db.WithContext(ctx).Begin()
	if second.Error != nil {
		t.Fatal(second.Error)
	}
	acquired, err = podlifecycle.TryTransactionLogicalIdentityLock(ctx, second, identity)
	if err != nil {
		_ = second.Rollback().Error
		t.Fatal(err)
	}
	if acquired {
		_ = second.Rollback().Error
		t.Fatal("concurrent transaction acquired the same logical Worker identity lock")
	}
	contentionErr := podlifecycle.EnsureDeletionFence(
		ctx,
		second,
		identity,
		fixture.now,
		"concurrent-trigger-fence",
	)
	if !podlifecycle.IsLogicalIdentityLockUnavailable(contentionErr) {
		_ = second.Rollback().Error
		t.Fatalf("PostgreSQL trigger contention error = %v", contentionErr)
	}
	if err := second.Rollback().Error; err != nil {
		t.Fatal(err)
	}

	if err := podlifecycle.EnsureDeletionFence(ctx, first, identity, fixture.now, "concurrent-fence"); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit().Error; err != nil {
		t.Fatal(err)
	}
	firstCommitted = true

	afterCommit := db.WithContext(ctx).Begin()
	if afterCommit.Error != nil {
		t.Fatal(afterCommit.Error)
	}
	defer func() { _ = afterCommit.Rollback().Error }()
	acquired, err = podlifecycle.TryTransactionLogicalIdentityLock(ctx, afterCommit, identity)
	if err != nil || !acquired {
		t.Fatalf("post-commit logical identity lock = acquired=%t err=%v", acquired, err)
	}
	fenced, err := podlifecycle.IsDeletionFenced(ctx, afterCommit, identity)
	if err != nil {
		t.Fatal(err)
	}
	if !fenced {
		t.Fatal("post-commit registration transaction did not observe the durable deletion fence")
	}
}
