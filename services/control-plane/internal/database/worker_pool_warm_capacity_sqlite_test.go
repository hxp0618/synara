package database

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSQLiteWorkerPoolMinIdleAndWarmCapacityScopeMatch(t *testing.T) {
	fixture := openStage4SQLiteFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "kubernetes", Name: "SQLite guaranteed warm target", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &fixture.tenantID, ExecutionTargetID: target.ID,
		Name: "guaranteed-warm", Mode: "warm", CapacityClass: "interactive",
		DesiredIdleUnits: 2, MinIdleUnits: 1, MaxActiveUnits: 3,
		SchedulingTemplate: map[string]any{}, Status: "active", Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}

	invalidPool := pool
	invalidPool.ID = uuid.New()
	invalidPool.Name = "invalid-guaranteed-warm"
	invalidPool.MinIdleUnits = invalidPool.DesiredIdleUnits + 1
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidPool).Error, "invalid Worker pool shape")

	authority := persistence.WorkerPoolWarmCapacity{
		WorkerPoolID: pool.ID, WorkerPoolVersion: pool.Version,
		TenantID: fixture.tenantID, ExecutionTargetID: target.ID,
		CapacityClass: pool.CapacityClass, WarmSupported: true,
		DesiredIdleUnits: pool.DesiredIdleUnits, MinIdleUnits: pool.MinIdleUnits, MaxActiveUnits: pool.MaxActiveUnits,
		DesiredTotalUnits: 2, ReadyIdleUnits: 0, Source: "sqlite/min-idle-test",
		ObservedAt: now, ExpiresAt: now.Add(time.Minute), Version: 1, UpdatedAt: now,
	}
	if err := fixture.db.Create(&authority).Error; err != nil {
		t.Fatalf("SQLite rejected matching warm-capacity scope: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Exec(`
			UPDATE worker_pool_warm_capacity
			SET min_idle_units = 0
			WHERE worker_pool_id = ? AND worker_pool_version = ?`, pool.ID, pool.Version,
		).Error,
		"Worker pool warm capacity scope is invalid",
	)
}
