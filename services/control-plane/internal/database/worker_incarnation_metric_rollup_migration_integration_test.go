package database

import (
	"context"
	"io/fs"
	"math"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/metricrollup"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresWorkerIncarnationMetricRollupMigrationAndReplay(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000075_worker_claim_release_facts.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "metric-rollup-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	first := createPostgresMetricRollupWorkerFact(t, db, domain, now.Add(-10*time.Second), now, 4, 6)

	if err := Migrate(ctx, db, workerMetricRollupMigrations(t)); err != nil {
		t.Fatal(err)
	}
	var entry persistence.WorkerIncarnationMetricRollupEntry
	if err := db.Where(
		"worker_id = ? AND worker_incarnation = ?", first.WorkerID, first.WorkerIncarnation,
	).Take(&entry).Error; err != nil {
		t.Fatalf("Migration 079 did not backfill a terminal Worker rollup entry: %v", err)
	}
	if entry.RolledUpAt != nil || !entry.TerminalAt.Equal(now) {
		t.Fatalf("backfilled Worker rollup entry = %#v", entry)
	}

	service := metricrollup.NewService(db)
	summary, err := service.RunOnce(ctx, 100)
	if err != nil || summary.ProcessedFacts != 1 || summary.UpdatedBuckets != 1 {
		t.Fatalf("PostgreSQL metric rollup = %#v, %v", summary, err)
	}
	var rollup persistence.WorkerIncarnationMetricRollup
	if err := db.Take(&rollup).Error; err != nil {
		t.Fatal(err)
	}
	if rollup.FactCount != 1 || rollup.TargetKind != "local" ||
		rollup.PoolMode != "unassigned" || rollup.CapacityClass != "unassigned" ||
		rollup.RunSeconds != 10 || rollup.ActiveSeconds != 4 || rollup.IdleSeconds != 6 ||
		rollup.RequestedCPUSeconds != 5 || rollup.RequestedMemoryByteSeconds != 10240 {
		t.Fatalf("PostgreSQL Worker metric rollup row = %#v", rollup)
	}
	if summary, err = service.RunOnce(ctx, 100); err != nil || summary.ProcessedFacts != 0 {
		t.Fatalf("PostgreSQL metric rollup replay = %#v, %v", summary, err)
	}

	secondRegisteredAt := now.Add(time.Second)
	secondTerminalAt := now.Add(11 * time.Second)
	second := createPostgresMetricRollupWorkerFact(
		t, db, domain, secondRegisteredAt, secondTerminalAt, 2, 8,
	)
	entry = persistence.WorkerIncarnationMetricRollupEntry{}
	if err := db.Where(
		"worker_id = ? AND worker_incarnation = ?", second.WorkerID, second.WorkerIncarnation,
	).Take(&entry).Error; err != nil {
		t.Fatalf("terminal Worker trigger did not enqueue a rollup entry: %v", err)
	}
	if summary, err = service.RunOnce(ctx, 100); err != nil ||
		summary.ProcessedFacts != 1 || summary.UpdatedBuckets != 1 {
		t.Fatalf("PostgreSQL triggered metric rollup = %#v, %v", summary, err)
	}
	if err := db.Take(&rollup).Error; err != nil {
		t.Fatal(err)
	}
	if rollup.FactCount != 2 || rollup.RunSeconds != 20 ||
		rollup.ActiveSeconds != 6 || rollup.IdleSeconds != 14 {
		t.Fatalf("PostgreSQL replay-safe cumulative rollup = %#v", rollup)
	}
	payload, err := observability.New(db).Gather(ctx)
	if err != nil {
		t.Fatalf("gather PostgreSQL rollup metrics: %v", err)
	}
	metrics := string(payload)
	for _, expected := range []string{
		`synara_worker_incarnation_facts{capacity_class="unassigned",mode="unassigned",state="terminated",target_kind="local"} 2`,
		`synara_worker_incarnation_run_seconds{capacity_class="unassigned",mode="unassigned",state="terminated",target_kind="local"} 20`,
		`synara_metric_rollup_pending_facts{kind="worker-incarnation"} 0`,
		`synara_metric_rollup_buckets{kind="worker-incarnation"} 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("PostgreSQL rollup metrics omitted %q:\n%s", expected, metrics)
		}
	}

	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.WorkerIncarnationMetricRollup{}).
			Where("bucket_day = ? AND target_kind = ? AND pool_mode = ? AND capacity_class = ?",
				rollup.BucketDay, rollup.TargetKind, rollup.PoolMode, rollup.CapacityClass).
			Update("fact_count", 1).Error,
		"chk_worker_incarnation_metric_rollup_totals_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.WorkerIncarnationMetricRollup{}).
			Where("bucket_day = ? AND target_kind = ? AND pool_mode = ? AND capacity_class = ?",
				rollup.BucketDay, rollup.TargetKind, rollup.PoolMode, rollup.CapacityClass).
			Update("run_seconds", math.Inf(1)).Error,
		"chk_worker_incarnation_metric_rollup_totals",
	)
	assertStage4MigrationRejected(
		t,
		db.Delete(&persistence.WorkerIncarnationMetricRollupEntry{},
			"worker_id = ? AND worker_incarnation = ?", second.WorkerID, second.WorkerIncarnation,
		).Error,
		"chk_worker_incarnation_metric_rollup_entry_delete_immutable",
	)
}

func workerMetricRollupMigrations(t *testing.T) fs.FS {
	t.Helper()
	result := fstest.MapFS{}
	for _, name := range []string{
		"000078_kubernetes_pod_failure_facts.sql",
		"000079_worker_incarnation_metric_rollups.sql",
	} {
		payload, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		result[name] = &fstest.MapFile{Data: payload}
	}
	return result
}

func createPostgresMetricRollupWorkerFact(
	t *testing.T,
	db *gorm.DB,
	domain bootstrap.Result,
	registeredAt, terminalAt time.Time,
	activeSeconds, idleSeconds int64,
) persistence.WorkerIncarnationFact {
	t.Helper()
	workerID := uuid.New()
	instanceUID := uuid.NewString()
	worker := persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: instanceUID,
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", WorkerMode: "general-pool",
		RegistrationTrustMode: "shared-token", ClusterID: "postgres", Namespace: "default",
		PodName: "metric-rollup-" + workerID.String(), Version: "test", ProtocolVersion: 2,
		Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: []byte("metric-rollup-" + workerID.String()), Status: "terminated", AdministrativeStatus: "active",
		RegisteredAt: registeredAt, LastHeartbeatAt: terminalAt, TerminatedAt: &terminalAt,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatalf("create metric rollup Worker: %v", err)
	}
	cpu := int64(500)
	memory := int64(1024)
	fact := persistence.WorkerIncarnationFact{
		WorkerID: workerID, WorkerIncarnation: 1, TenantID: &domain.TenantID,
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", WorkerMode: "general-pool",
		ClusterID: "postgres", Namespace: "default", PodName: worker.PodName, InstanceUID: instanceUID,
		RegisteredAt: registeredAt, CurrentState: "terminated", StateChangedAt: terminalAt,
		TerminatedAt: &terminalAt, AccumulatedActiveSeconds: activeSeconds,
		AccumulatedIdleSeconds: idleSeconds, RequestedCPUMillicores: &cpu,
		RequestedMemoryBytes: &memory, CreatedAt: registeredAt, UpdatedAt: terminalAt,
	}
	terminalReason := "metric-rollup-test"
	fact.TerminalReason = &terminalReason
	if err := db.Create(&fact).Error; err != nil {
		t.Fatalf("create metric rollup Worker fact: %v", err)
	}
	return fact
}
