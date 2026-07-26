package database

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/metricrollup"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresExecutionGenerationMetricRollupMigrationAndReplay(t *testing.T) {
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
	if err := Migrate(ctx, db, migrationsThrough(t, "000080_billing_shared_allocation_calendar_schedules.sql")); err != nil {
		t.Fatal(err)
	}
	var fact persistence.ExecutionGenerationFact
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1,
	).Take(&fact).Error; err != nil {
		t.Fatal(err)
	}
	if fact.DispatchRequestedAt == nil {
		t.Fatalf("backfilled Generation fact has no dispatch time: %#v", fact)
	}
	dispatchedAt := fact.DispatchRequestedAt.UTC()
	podAppliedAt := dispatchedAt.Add(time.Second)
	podRunningAt := dispatchedAt.Add(5 * time.Second)
	readyAt := dispatchedAt.Add(10 * time.Second)
	terminalAt := dispatchedAt.Add(11 * time.Second)
	if err := db.Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
		Updates(map[string]any{
			"pod_provisioning_started_at": podAppliedAt,
			"pod_pending_since_at":        podAppliedAt,
			"pod_running_at":              podRunningAt,
			"pod_last_observed_at":        podRunningAt,
			"provider_ready_at":           readyAt,
			"terminal_at":                 terminalAt,
			"terminal_outcome":            "completed",
			"updated_at":                  terminalAt,
		}).Error; err != nil {
		t.Fatalf("prepare terminal Generation fact before Migration 081: %v", err)
	}
	podUID := uuid.NewString()
	failure := persistence.ExecutionGenerationPodFailureFact{
		TenantID: seed.tenantID, ExecutionID: seed.executionID, Generation: 1,
		FailureClass: "image-pull", ExecutionTargetID: fact.ExecutionTargetID,
		Namespace: "default", PodName: "metric-rollup-worker", PodUID: &podUID,
		ReasonCode: "image-pull-backoff", FirstObservedAt: podAppliedAt,
		LastObservedAt: podAppliedAt, CreatedAt: podAppliedAt, UpdatedAt: podAppliedAt,
	}
	if err := db.Create(&failure).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, executionGenerationMetricRollupMigrationOnly(t)); err != nil {
		t.Fatal(err)
	}
	var generationEntry persistence.ExecutionGenerationMetricRollupEntry
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1,
	).Take(&generationEntry).Error; err != nil {
		t.Fatalf("Migration 081 did not backfill Generation membership: %v", err)
	}
	var failureEntry persistence.ExecutionGenerationPodFailureMetricRollupEntry
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND generation = ? AND failure_class = ?",
		seed.tenantID, seed.executionID, 1, failure.FailureClass,
	).Take(&failureEntry).Error; err != nil {
		t.Fatalf("Migration 081 did not backfill Pod failure membership: %v", err)
	}
	var schemaName string
	if err := db.Raw("SELECT current_schema()").Scan(&schemaName).Error; err != nil {
		t.Fatal(err)
	}
	secondOptions := DefaultOptions()
	secondOptions.MaxOpenConnections = 1
	secondOptions.MaxIdleConnections = 1
	secondDB, err := Open(ctx, databaseURL, secondOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, dbErr := secondDB.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	if err := secondDB.Exec(`SET search_path TO "` + schemaName + `"`).Error; err != nil {
		t.Fatal(err)
	}
	type concurrentResult struct {
		summary metricrollup.Summary
		err     error
	}
	start := make(chan struct{})
	results := make(chan concurrentResult, 2)
	var waitGroup sync.WaitGroup
	for _, service := range []*metricrollup.Service{metricrollup.NewService(db), metricrollup.NewService(secondDB)} {
		waitGroup.Add(1)
		go func(service *metricrollup.Service) {
			defer waitGroup.Done()
			<-start
			summary, runErr := service.RunOnce(ctx, 100)
			results <- concurrentResult{summary: summary, err: runErr}
		}(service)
	}
	close(start)
	waitGroup.Wait()
	close(results)
	var processedGenerations, processedFailures int
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent PostgreSQL Generation metric rollup: %v", result.err)
		}
		processedGenerations += result.summary.ProcessedGenerationFacts
		processedFailures += result.summary.ProcessedPodFailureFacts
	}
	if processedGenerations != 1 || processedFailures != 1 {
		t.Fatalf("concurrent PostgreSQL Generation metric rollup processed generation/failure = %d/%d", processedGenerations, processedFailures)
	}
	service := metricrollup.NewService(db)
	summary, err := service.RunOnce(ctx, 100)
	if err != nil ||
		summary.ProcessedGenerationFacts != 0 || summary.ProcessedPodFailureFacts != 0 {
		t.Fatalf("PostgreSQL Generation metric rollup replay = %#v, %v", summary, err)
	}
	var outcomeRollup persistence.ExecutionGenerationMetricRollup
	if err := db.Where("metric_kind = ?", "outcome").Take(&outcomeRollup).Error; err != nil {
		t.Fatal(err)
	}
	if outcomeRollup.SampleCount != 1 || outcomeRollup.Outcome != "completed" {
		t.Fatalf("PostgreSQL Generation outcome rollup = %#v", outcomeRollup)
	}
	payload, err := observability.New(db).Gather(ctx)
	if err != nil {
		t.Fatalf("gather PostgreSQL Generation rollup metrics: %v", err)
	}
	metrics := string(payload)
	for _, expected := range []string{
		`synara_execution_generation_outcomes_30d{outcome="completed",recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_execution_cold_start_samples_30d{recovery_reason="initial-claim",target_kind="kubernetes",warm_pool_mode="disabled",warm_pool_result="not-requested"} 1`,
		`synara_execution_pod_failure_generations_30d{failure_class="image-pull",target_kind="kubernetes"} 1`,
		`synara_execution_generation_metric_rollup_pending_facts{kind="generation"} 0`,
		`synara_execution_generation_metric_rollup_pending_facts{kind="pod-failure"} 0`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("PostgreSQL Generation rollup metrics omitted %q:\n%s", expected, metrics)
		}
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionGenerationFact{}).
			Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1).
			Update("provider_ready_at", readyAt.Add(time.Second)).Error,
		"chk_execution_generation_metric_source_sealed",
	)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionGenerationMetricRollup{}).
			Where(
				"bucket_day = ? AND metric_kind = ? AND target_kind = ? AND recovery_reason = ? AND outcome = ? AND warm_pool_mode = ? AND warm_pool_result = ? AND failure_class = ? AND histogram_bucket = ?",
				outcomeRollup.BucketDay, outcomeRollup.MetricKind, outcomeRollup.TargetKind,
				outcomeRollup.RecoveryReason, outcomeRollup.Outcome, outcomeRollup.WarmPoolMode,
				outcomeRollup.WarmPoolResult, outcomeRollup.FailureClass, outcomeRollup.HistogramBucket,
			).
			Update("sample_count", 0).Error,
		"chk_execution_generation_metric_rollup_totals_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Delete(
			&persistence.ExecutionGenerationMetricRollupEntry{},
			"tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, 1,
		).Error,
		"chk_execution_generation_metric_rollup_entry_delete_immutable",
	)
	if err := Migrate(ctx, db, executionGenerationMetricRollupMigrationOnly(t)); err != nil {
		t.Fatalf("replay Migration 081: %v", err)
	}
	var applied int64
	if err := db.Table("control_plane_schema_migrations").Where("version = ?", 81).Count(&applied).Error; err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("Migration 081 records = %d, want 1", applied)
	}
}

func executionGenerationMetricRollupMigrationOnly(t *testing.T) fs.FS {
	t.Helper()
	const name = "000081_execution_generation_metric_rollups.sql"
	payload, err := fs.ReadFile(migrations.Files, name)
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{name: &fstest.MapFile{Data: payload}}
}
