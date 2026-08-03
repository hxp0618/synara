package database

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresExecutionTraceContextMigrationInstallsConstraintAndImmutableTrigger(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var constraintDefinition string
	if err := db.Raw(`
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'agent_executions'::regclass
		  AND conname = 'agent_executions_traceparent_check'
	`).Scan(&constraintDefinition).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(constraintDefinition, "traceparent") ||
		!strings.Contains(constraintDefinition, "repeat('0'::text, 32)") {
		t.Fatalf("Execution trace constraint = %q", constraintDefinition)
	}
	var triggerCount int64
	if err := db.Raw(`
		SELECT count(*)
		FROM pg_trigger
		WHERE tgrelid = 'agent_executions'::regclass
		  AND tgname = 'trg_agent_executions_traceparent_immutable'
		  AND NOT tgisinternal
	`).Scan(&triggerCount).Error; err != nil {
		t.Fatal(err)
	}
	if triggerCount != 1 {
		t.Fatalf("Execution trace immutability trigger count = %d, want 1", triggerCount)
	}
}
