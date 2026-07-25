package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestWorkerIdentitySnapshotMigrationFencesAssignmentPoolAndDemand(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(ctx, db, migrationsThrough(t, "000059_execution_generation_facts.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrationsThrough(t, "000060_worker_identity_snapshots.sql")); err != nil {
		t.Fatal(err)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.WarmPoolModeSnapshot != "disabled" {
		t.Fatalf("warm pool mode snapshot = %q, want disabled", execution.WarmPoolModeSnapshot)
	}
	if execution.WorkerID == nil {
		t.Fatal("seeded Execution is missing its Worker")
	}
	var worker persistence.WorkerInstance
	if err := db.Where("id = ?", *execution.WorkerID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Updates(map[string]any{
			"worker_mode": "execution-pinned", "assigned_execution_id": execution.ID,
			"worker_pool_id": nil, "worker_pool_version": nil, "capacity_class": nil,
		}).Error; err != nil {
		t.Fatalf("bind valid pinned Worker identity: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
			Update("assigned_execution_id", nil).Error,
		"chk_worker_instances_identity_scope",
	)

	now := time.Now().UTC().Truncate(time.Second)
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &seed.tenantID, ExecutionTargetID: execution.ExecutionTargetID,
		Name: "migration-warm", Mode: "warm", CapacityClass: "interactive",
		ClusterID: "migration", Namespace: "default", DesiredIdleUnits: 1, MaxActiveUnits: 1,
		SchedulingTemplate: map[string]any{}, Status: "active", Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Updates(map[string]any{
			"worker_mode": "warm-pool", "assigned_execution_id": nil,
			"worker_pool_id": pool.ID, "worker_pool_version": pool.Version,
			"capacity_class": pool.CapacityClass,
		}).Error; err != nil {
		t.Fatalf("bind valid warm Worker identity: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
			Update("worker_pool_version", int64(2)).Error,
		"chk_worker_instances_identity_scope",
	)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
			Update("warm_pool_mode_snapshot", "balanced").Error,
		"chk_agent_executions_warm_pool_mode_snapshot_immutable",
	)
}
