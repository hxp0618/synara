package database

import (
	"context"
	"os"
	"testing"
	"time"

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
