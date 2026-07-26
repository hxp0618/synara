package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestExecutionGenerationFactsMigrationFencesIdentityTimelineAndTerminalOutcome(t *testing.T) {
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
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatalf("load seeded execution: %v", err)
	}

	var fact persistence.ExecutionGenerationFact
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?",
		seed.tenantID, seed.executionID, execution.Generation,
	).Take(&fact).Error; err != nil {
		t.Fatalf("load backfilled generation fact: %v", err)
	}
	if fact.RecoveryReason != "initial-claim" || fact.WarmPoolResult != "not-requested" ||
		fact.DispatchRequestedAt == nil ||
		fact.SessionID != seed.sessionID || fact.TurnID != seed.turnID ||
		fact.ExecutionTargetID != execution.ExecutionTargetID {
		t.Fatalf("backfilled generation fact = %#v", fact)
	}
	dispatchedAt := *fact.DispatchRequestedAt

	invalidReadyAt := dispatchedAt.Add(time.Second)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionGenerationFact{}).
			Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
			Updates(map[string]any{
				"execution_started_at": dispatchedAt.Add(2 * time.Second),
				"provider_ready_at":    invalidReadyAt,
				"updated_at":           invalidReadyAt,
			}).Error,
		"chk_execution_generation_facts_timeline",
	)

	leasedAt := dispatchedAt.Add(time.Second)
	startedAt := dispatchedAt.Add(2 * time.Second)
	readyAt := dispatchedAt.Add(3 * time.Second)
	if err := db.Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
		Updates(map[string]any{
			"leased_at": leasedAt, "execution_started_at": startedAt,
			"provider_ready_at": readyAt, "updated_at": readyAt,
		}).Error; err != nil {
		t.Fatalf("advance valid generation fact: %v", err)
	}
	podAppliedAt := dispatchedAt.Add(500 * time.Millisecond)
	podPendingAt := podAppliedAt.Add(time.Second)
	podRunningAt := podPendingAt.Add(time.Second)
	if err := db.Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
		Updates(map[string]any{
			"pod_provisioning_started_at": podAppliedAt,
			"pod_pending_since_at":        podPendingAt,
			"pod_running_at":              podRunningAt,
			"pod_last_observed_at":        podRunningAt,
			"updated_at":                  readyAt,
		}).Error; err != nil {
		t.Fatalf("advance valid Pod generation fact: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionGenerationFact{}).
			Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
			Update("pod_running_at", podRunningAt.Add(time.Second)).Error,
		"chk_execution_generation_pod_running_immutable",
	)
	podUID := uuid.NewString()
	failure := persistence.ExecutionGenerationPodFailureFact{
		TenantID: seed.tenantID, ExecutionID: seed.executionID, Generation: 1,
		FailureClass: "oom-killed", ExecutionTargetID: execution.ExecutionTargetID,
		Namespace: "default", PodName: "migration-worker", PodUID: &podUID,
		ReasonCode: "oom-killed", FirstObservedAt: podRunningAt,
		LastObservedAt: podRunningAt, CreatedAt: podRunningAt, UpdatedAt: podRunningAt,
	}
	if err := db.Create(&failure).Error; err != nil {
		t.Fatalf("create Pod failure fact: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&failure).Update("reason_code", "phase-failed").Error,
		"chk_execution_generation_pod_failure_identity_immutable",
	)
	invalidScope := failure
	invalidScope.FailureClass = "evicted"
	invalidScope.ExecutionTargetID = uuid.New()
	invalidScope.ReasonCode = "evicted"
	assertStage4MigrationRejected(
		t,
		db.Create(&invalidScope).Error,
		"chk_execution_generation_pod_failure_scope",
	)

	completed := "completed"
	terminalAt := readyAt.Add(time.Second)
	if err := db.Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
		Updates(map[string]any{
			"terminal_at": terminalAt, "terminal_outcome": completed, "updated_at": terminalAt,
		}).Error; err != nil {
		t.Fatalf("complete generation fact: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionGenerationFact{}).
			Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
			Updates(map[string]any{"terminal_outcome": "failed", "updated_at": terminalAt.Add(time.Second)}).Error,
		"chk_execution_generation_facts_terminal_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionGenerationFact{}).
			Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
			Update("provider", "claudeAgent").Error,
		"chk_execution_generation_facts_identity_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Delete(&persistence.ExecutionGenerationFact{},
			"tenant_id = ? AND execution_id = ? AND generation = ?",
			seed.tenantID, seed.executionID, execution.Generation,
		).Error,
		"chk_execution_generation_facts_delete_immutable",
	)
}
