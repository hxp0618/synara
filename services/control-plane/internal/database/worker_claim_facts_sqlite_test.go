package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteWorkerClaimFactSafetyRejectsScopeAndMutation(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"idx_worker_claim_facts_request",
		"uq_worker_claim_facts_execution_generation",
		"uq_worker_claim_facts_cleanup_dispatch",
		"idx_worker_claim_facts_billing",
		"trg_worker_claim_facts_insert",
		"trg_worker_claim_facts_update",
		"trg_worker_claim_facts_delete",
		"trg_worker_claim_facts_restrict_tenant_delete",
		"trg_worker_claim_facts_restrict_target_delete",
		"trg_worker_claim_facts_restrict_worker_fact_delete",
		"trg_worker_claim_facts_restrict_execution_delete",
		"trg_worker_claim_facts_restrict_cleanup_delete",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite worker claim fact safety object %s count = %d, want 1", name, count)
		}
	}

	fixture := seedWorkerClaimFactFixture(t, store.DB())
	invalidReincarnatedExecution := invalidReincarnatedExecutionWorkerClaimFact(
		t,
		store.DB(),
		fixture,
		"sqlite-worker-claim-execution-reincarnated",
	)
	assertSQLiteStage4Rejected(
		t,
		store.DB().Create(&invalidReincarnatedExecution).Error,
		"invalid Worker claim fact",
	)
	executionClaim := validExecutionWorkerClaimFact(fixture, "sqlite-worker-claim-execution")
	if err := store.DB().Create(&executionClaim).Error; err != nil {
		t.Fatalf("create valid execution worker claim fact: %v", err)
	}
	cleanupClaim := validCleanupWorkerClaimFact(fixture, "sqlite-worker-claim-cleanup")
	if err := store.DB().Create(&cleanupClaim).Error; err != nil {
		t.Fatalf("create valid cleanup worker claim fact: %v", err)
	}
	for _, name := range []string{
		"idx_worker_claim_release_facts_released",
		"trg_worker_claim_release_facts_insert",
		"trg_worker_claim_release_facts_update",
		"trg_worker_claim_release_facts_delete",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite worker claim release fact safety object %s count = %d, want 1", name, count)
		}
	}
	tooEarlyRelease := validWorkerClaimReleaseFact(executionClaim, "execution_completed", fixture.executionClaimAt.Add(-time.Second))
	assertSQLiteStage4Rejected(t, store.DB().Create(&tooEarlyRelease).Error, "invalid Worker claim release fact")
	invalidTimeline := validWorkerClaimReleaseFact(executionClaim, "execution_completed", fixture.executionClaimAt.Add(time.Minute))
	invalidTimeline.RecordedAt = invalidTimeline.ReleasedAt.Add(-time.Second)
	assertSQLiteStage4Rejected(t, store.DB().Create(&invalidTimeline).Error, "invalid Worker claim release fact")
	executionRelease := validWorkerClaimReleaseFact(executionClaim, "execution_completed", fixture.executionClaimAt.Add(time.Minute))
	if err := store.DB().Create(&executionRelease).Error; err != nil {
		t.Fatalf("create valid execution worker claim release fact: %v", err)
	}
	cleanupRelease := validWorkerClaimReleaseFact(cleanupClaim, "cleanup_acknowledged", fixture.cleanupClaimAt.Add(time.Minute))
	if err := store.DB().Create(&cleanupRelease).Error; err != nil {
		t.Fatalf("create valid cleanup worker claim release fact: %v", err)
	}
	invalidRelease := validWorkerClaimReleaseFact(persistence.WorkerClaimFact{ID: uuid.New()}, "not_stable", fixture.cleanupClaimAt.Add(time.Minute))
	assertSQLiteStage4Rejected(t, store.DB().Create(&invalidRelease).Error, "invalid Worker claim release fact")
	assertSQLiteStage4Rejected(
		t,
		store.DB().Model(&persistence.WorkerClaimReleaseFact{}).
			Where("claim_fact_id = ?", executionClaim.ID).
			Update("release_reason", "execution_failed").Error,
		"immutable",
	)
	assertSQLiteStage4Rejected(
		t,
		store.DB().Delete(&persistence.WorkerClaimReleaseFact{}, "claim_fact_id = ?", cleanupClaim.ID).Error,
		"immutable",
	)
	duplicateExecutionSource := executionClaim
	duplicateExecutionSource.ID = uuid.New()
	duplicateExecutionSource.RequestID = "sqlite-worker-claim-execution-duplicate-source"
	assertWorkerClaimFactDuplicate(t, store.DB().Create(&duplicateExecutionSource).Error)
	duplicateCleanupSource := cleanupClaim
	duplicateCleanupSource.ID = uuid.New()
	duplicateCleanupSource.RequestID = "sqlite-worker-claim-cleanup-duplicate-source"
	assertWorkerClaimFactDuplicate(t, store.DB().Create(&duplicateCleanupSource).Error)

	invalidExecution := executionClaim
	invalidExecution.ID = uuid.New()
	invalidExecution.RequestID = "sqlite-worker-claim-execution-invalid"
	badGeneration := *executionClaim.ExecutionGeneration + 1
	invalidExecution.ExecutionGeneration = &badGeneration
	assertSQLiteStage4Rejected(
		t,
		store.DB().Create(&invalidExecution).Error,
		"invalid Worker claim fact",
	)

	invalidCleanup := cleanupClaim
	invalidCleanup.ID = uuid.New()
	invalidCleanup.RequestID = "sqlite-worker-claim-cleanup-invalid"
	badDispatchGeneration := *cleanupClaim.CleanupDispatchGeneration + 1
	invalidCleanup.CleanupDispatchGeneration = &badDispatchGeneration
	assertSQLiteStage4Rejected(
		t,
		store.DB().Create(&invalidCleanup).Error,
		"invalid Worker claim fact",
	)
	assertSQLiteStage4Rejected(
		t,
		store.DB().Model(&persistence.WorkerClaimFact{}).
			Where("id = ?", executionClaim.ID).
			Update("claim_kind", "workspace-cleanup").Error,
		"immutable",
	)
	assertSQLiteStage4Rejected(
		t,
		store.DB().Delete(&persistence.WorkerClaimFact{}, "id = ?", cleanupClaim.ID).Error,
		"immutable",
	)
	assertWorkerClaimFactParentRetained(
		t,
		store.DB().Delete(&persistence.AgentExecution{}, "id = ?", fixture.execution.ID).Error,
	)
	assertWorkerClaimFactParentRetained(
		t,
		store.DB().Delete(&persistence.WorkspaceCleanupCommand{}, "id = ?", fixture.cleanupCommand.ID).Error,
	)
}
