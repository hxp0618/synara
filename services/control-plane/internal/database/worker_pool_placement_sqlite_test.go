package database

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSQLiteExecutionPlacementPolicyUpdateFencesOptionalPoolScope(t *testing.T) {
	fixture := openStage4SQLiteFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	otherTargetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: otherTargetID, TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "local", Name: "Other placement target", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	newPool := func(targetID uuid.UUID, name string) persistence.WorkerPool {
		return persistence.WorkerPool{
			ID: uuid.New(), TenantID: &fixture.tenantID, ExecutionTargetID: targetID,
			Name: name, Mode: "resident", CapacityClass: "standard",
			SchedulingTemplate: map[string]any{}, Status: "active", Version: 1,
			MaxActiveUnits: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
	defaultPool := newPool(fixture.targetID, "default-placement")
	balancedPool := newPool(fixture.targetID, "balanced-placement")
	foreignPool := newPool(otherTargetID, "foreign-placement")
	for _, pool := range []*persistence.WorkerPool{&defaultPool, &balancedPool, &foreignPool} {
		if err := fixture.db.Create(pool).Error; err != nil {
			t.Fatalf("create Worker pool %s: %v", pool.Name, err)
		}
	}
	policy := persistence.ExecutionPlacementPolicy{
		TenantID: &fixture.tenantID, ExecutionTargetID: fixture.targetID,
		Version: 1, DefaultPoolID: defaultPool.ID, UpdatedAt: now,
	}
	if err := fixture.db.Create(&policy).Error; err != nil {
		t.Fatalf("create placement policy: %v", err)
	}

	assertSQLiteStage4Rejected(
		t,
		fixture.db.Exec(`
			UPDATE execution_placement_policies
			SET version = 2, balanced_pool_id = ?
			WHERE execution_target_id = ?`, foreignPool.ID, fixture.targetID,
		).Error,
		"invalid Execution placement policy update",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Exec(`
			UPDATE execution_placement_policies
			SET version = 2, low_latency_pool_id = ?
			WHERE execution_target_id = ?`, uuid.New(), fixture.targetID,
		).Error,
		"invalid Execution placement policy update",
	)
	if err := fixture.db.Exec(`
		UPDATE execution_placement_policies
		SET version = 2, balanced_pool_id = ?
		WHERE execution_target_id = ?`, balancedPool.ID, fixture.targetID,
	).Error; err != nil {
		t.Fatalf("SQLite rejected valid optional placement pool update: %v", err)
	}
}
