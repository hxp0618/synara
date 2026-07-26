package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresWorkerPoolWarmCapacityRejectsScopeAndMutation(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	fixture := seedWorkerPoolWarmCapacityFixture(t, db)
	releaseChannel := "promoted"
	valid := persistence.WorkerPoolWarmCapacity{
		WorkerPoolID:            fixture.pool.ID,
		WorkerPoolVersion:       fixture.pool.Version,
		TenantID:                fixture.tenantID,
		ExecutionTargetID:       fixture.target.ID,
		CapacityClass:           fixture.pool.CapacityClass,
		WarmSupported:           true,
		WorkerReleaseRevisionID: &fixture.release.ID,
		WorkerReleaseChannel:    &releaseChannel,
		DesiredIdleUnits:        fixture.pool.DesiredIdleUnits,
		MaxActiveUnits:          fixture.pool.MaxActiveUnits,
		DesiredTotalUnits:       3,
		ClaimedUnits:            1,
		ReadyIdleUnits:          2,
		Source:                  "postgres/reconciler",
		ObservedAt:              fixture.now,
		ExpiresAt:               fixture.now.Add(time.Minute),
		Version:                 1,
		UpdatedAt:               fixture.now,
	}
	if err := db.Create(&valid).Error; err != nil {
		t.Fatalf("create valid warm capacity authority: %v", err)
	}

	invalidTarget := valid
	invalidTarget.WorkerPoolID = fixture.otherPool.ID
	invalidTarget.WorkerPoolVersion = fixture.otherPool.Version
	invalidTarget.ExecutionTargetID = fixture.target.ID
	assertWorkerPoolWarmCapacityConstraintRejected(
		t,
		db.Create(&invalidTarget).Error,
		"Worker pool warm capacity scope is invalid",
	)

	invalidRelease := valid
	invalidRelease.WorkerPoolID = fixture.otherWarmPoolSameTarget.ID
	invalidRelease.WorkerPoolVersion = fixture.otherWarmPoolSameTarget.Version
	invalidRelease.WorkerReleaseRevisionID = &fixture.otherTargetRelease.ID
	assertWorkerPoolWarmCapacityConstraintRejected(
		t,
		db.Create(&invalidRelease).Error,
		"Worker pool warm capacity scope is invalid",
	)

	if err := db.Model(&persistence.WorkerPoolWarmCapacity{}).
		Where("tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ?",
			fixture.tenantID, fixture.target.ID, fixture.pool.ID, fixture.pool.Version).
		Updates(map[string]any{
			"version":     2,
			"observed_at": fixture.now.Add(time.Second),
			"expires_at":  fixture.now.Add(2 * time.Minute),
			"updated_at":  fixture.now.Add(time.Second),
		}).Error; err != nil {
		t.Fatalf("valid warm capacity update rejected: %v", err)
	}
	assertWorkerPoolWarmCapacityConstraintRejected(
		t,
		db.Model(&persistence.WorkerPoolWarmCapacity{}).
			Where("tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ?",
				fixture.tenantID, fixture.target.ID, fixture.pool.ID, fixture.pool.Version).
			Updates(map[string]any{
				"version":     4,
				"observed_at": fixture.now.Add(500 * time.Millisecond),
				"updated_at":  fixture.now.Add(2 * time.Second),
			}).Error,
		"Worker pool warm capacity version must advance exactly once and observed_at must advance",
	)
	assertWorkerPoolWarmCapacityConstraintRejected(
		t,
		db.Delete(&persistence.WorkerPoolWarmCapacity{},
			"tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ?",
			fixture.tenantID, fixture.target.ID, fixture.pool.ID, fixture.pool.Version,
		).Error,
		"Worker pool warm capacity observations cannot be deleted",
	)
}

func assertWorkerPoolWarmCapacityConstraintRejected(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil {
		t.Fatalf("PostgreSQL accepted invalid Worker Pool warm capacity state (want containing %q)", expected)
	}
	if strings.Contains(err.Error(), expected) || errors.Is(err, gorm.ErrCheckConstraintViolated) {
		return
	}
	t.Fatalf(
		"PostgreSQL returned the wrong Worker Pool warm capacity rejection: %v (want containing %q or a translated check constraint)",
		err,
		expected,
	)
}

type workerPoolWarmCapacityFixture struct {
	now                     time.Time
	tenantID                uuid.UUID
	target                  persistence.ExecutionTarget
	pool                    persistence.WorkerPool
	otherPool               persistence.WorkerPool
	otherWarmPoolSameTarget persistence.WorkerPool
	release                 persistence.WorkerReleaseRevision
	otherTargetRelease      persistence.WorkerReleaseRevision
}

func seedWorkerPoolWarmCapacityFixture(t *testing.T, db *gorm.DB) workerPoolWarmCapacityFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID := uuid.New()
	tenantID := uuid.New()
	if err := db.Create(&persistence.User{
		ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Warm Capacity PG",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: tenantID, Slug: "warm-capacity-pg-" + uuid.NewString()[:8], Name: "Warm Capacity PG Tenant",
			Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{},
			CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}

	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &tenantID, Kind: "kubernetes", Name: "tenant-k8s-a", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	otherTarget := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &tenantID, Kind: "kubernetes", Name: "tenant-k8s-b", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	for _, targetModel := range []*persistence.ExecutionTarget{&target, &otherTarget} {
		if err := db.Create(targetModel).Error; err != nil {
			t.Fatal(err)
		}
	}

	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &tenantID, ExecutionTargetID: target.ID,
		Name: "warm-a", Mode: "warm", CapacityClass: "interactive",
		DesiredIdleUnits: 2, MaxActiveUnits: 5, SchedulingTemplate: map[string]any{},
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	otherPool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &tenantID, ExecutionTargetID: otherTarget.ID,
		Name: "warm-b", Mode: "warm", CapacityClass: "interactive",
		DesiredIdleUnits: 1, MaxActiveUnits: 4, SchedulingTemplate: map[string]any{},
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	otherWarmPoolSameTarget := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &tenantID, ExecutionTargetID: target.ID,
		Name: "warm-c", Mode: "warm", CapacityClass: "interactive",
		DesiredIdleUnits: 1, MaxActiveUnits: 3, SchedulingTemplate: map[string]any{},
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	for _, poolModel := range []*persistence.WorkerPool{&pool, &otherPool, &otherWarmPoolSameTarget} {
		if err := db.Create(poolModel).Error; err != nil {
			t.Fatal(err)
		}
	}

	firstManifest := persistence.WorkerManifest{
		ID:                    uuid.New(),
		ManifestHash:          strings.Repeat(strings.ReplaceAll(uuid.NewString(), "-", ""), 2),
		WorkerBuildVersion:    "1.0.0",
		WorkerProtocolMinimum: 2,
		WorkerProtocolMaximum: 2,
		RuntimeEventMinimum:   2,
		RuntimeEventMaximum:   2,
		OperatingSystem:       "linux",
		Architecture:          "amd64",
		FeatureFlags:          map[string]any{},
		CreatedAt:             now,
	}
	secondManifest := firstManifest
	secondManifest.ID = uuid.New()
	secondManifest.ManifestHash = strings.Repeat(strings.ReplaceAll(uuid.NewString(), "-", ""), 2)
	if err := db.Create(&firstManifest).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&secondManifest).Error; err != nil {
		t.Fatal(err)
	}

	release := persistence.WorkerReleaseRevision{
		ID: uuid.New(), TenantID: tenantID, ExecutionTargetID: target.ID,
		Revision: 1, WorkerManifestID: firstManifest.ID, Description: "release-a",
		CreatedBy: userID, CreatedAt: now,
	}
	otherTargetRelease := persistence.WorkerReleaseRevision{
		ID: uuid.New(), TenantID: tenantID, ExecutionTargetID: otherTarget.ID,
		Revision: 1, WorkerManifestID: secondManifest.ID, Description: "release-b",
		CreatedBy: userID, CreatedAt: now,
	}
	if err := db.Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&otherTargetRelease).Error; err != nil {
		t.Fatal(err)
	}

	return workerPoolWarmCapacityFixture{
		now:                     now,
		tenantID:                tenantID,
		target:                  target,
		pool:                    pool,
		otherPool:               otherPool,
		otherWarmPoolSameTarget: otherWarmPoolSameTarget,
		release:                 release,
		otherTargetRelease:      otherTargetRelease,
	}
}
