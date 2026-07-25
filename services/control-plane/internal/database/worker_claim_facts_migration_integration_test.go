package database

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresWorkerClaimFactsRejectScopeAndMutation(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	fixture := seedWorkerClaimFactFixture(t, db)
	invalidReincarnatedExecution := invalidReincarnatedExecutionWorkerClaimFact(
		t,
		db,
		fixture,
		"postgres-worker-claim-execution-reincarnated",
	)
	assertStage4MigrationRejected(
		t,
		db.Create(&invalidReincarnatedExecution).Error,
		"chk_worker_claim_facts_scope",
	)
	executionClaim := validExecutionWorkerClaimFact(fixture, "postgres-worker-claim-execution")
	if err := db.Create(&executionClaim).Error; err != nil {
		t.Fatalf("create valid execution worker claim fact: %v", err)
	}
	cleanupClaim := validCleanupWorkerClaimFact(fixture, "postgres-worker-claim-cleanup")
	if err := db.Create(&cleanupClaim).Error; err != nil {
		t.Fatalf("create valid cleanup worker claim fact: %v", err)
	}
	duplicateExecutionSource := executionClaim
	duplicateExecutionSource.ID = uuid.New()
	duplicateExecutionSource.RequestID = "postgres-worker-claim-execution-duplicate-source"
	assertWorkerClaimFactDuplicate(t, db.Create(&duplicateExecutionSource).Error)
	duplicateCleanupSource := cleanupClaim
	duplicateCleanupSource.ID = uuid.New()
	duplicateCleanupSource.RequestID = "postgres-worker-claim-cleanup-duplicate-source"
	assertWorkerClaimFactDuplicate(t, db.Create(&duplicateCleanupSource).Error)

	invalidExecution := executionClaim
	invalidExecution.ID = uuid.New()
	invalidExecution.RequestID = "postgres-worker-claim-execution-invalid"
	badGeneration := *executionClaim.ExecutionGeneration + 1
	invalidExecution.ExecutionGeneration = &badGeneration
	assertStage4MigrationRejected(
		t,
		db.Create(&invalidExecution).Error,
		"chk_worker_claim_facts_scope",
	)

	invalidCleanup := cleanupClaim
	invalidCleanup.ID = uuid.New()
	invalidCleanup.RequestID = "postgres-worker-claim-cleanup-invalid"
	badDispatchGeneration := *cleanupClaim.CleanupDispatchGeneration + 1
	invalidCleanup.CleanupDispatchGeneration = &badDispatchGeneration
	assertStage4MigrationRejected(
		t,
		db.Create(&invalidCleanup).Error,
		"chk_worker_claim_facts_scope",
	)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.WorkerClaimFact{}).
			Where("id = ?", executionClaim.ID).
			Update("claim_kind", "workspace-cleanup").Error,
		"chk_worker_claim_facts_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Delete(&persistence.WorkerClaimFact{}, "id = ?", cleanupClaim.ID).Error,
		"chk_worker_claim_facts_immutable",
	)
	assertWorkerClaimFactParentRetained(
		t,
		db.Delete(&persistence.AgentExecution{}, "id = ?", fixture.execution.ID).Error,
	)
	assertWorkerClaimFactParentRetained(
		t,
		db.Delete(&persistence.WorkspaceCleanupCommand{}, "id = ?", fixture.cleanupCommand.ID).Error,
	)
}
