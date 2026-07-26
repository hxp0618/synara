package metricrollup

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestWorkerMetricRollupBatchesAndRollsBackBeforeMembershipCompletion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.WorkerIncarnationFact{},
		&persistence.WorkerIncarnationMetricRollup{},
		&persistence.WorkerIncarnationMetricRollupEntry{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 26, 10, 0, 0, 0, time.UTC)
	for index := 0; index < 2; index++ {
		seedMetricRollupFact(t, db, now.Add(time.Duration(index)*time.Minute), index+1)
	}
	service := NewService(db)
	service.now = func() time.Time { return now.Add(time.Hour) }
	for expectedCount := int64(1); expectedCount <= 2; expectedCount++ {
		summary, err := service.RunOnce(context.Background(), 1)
		if err != nil || summary.ProcessedFacts != 1 || summary.UpdatedBuckets != 1 {
			t.Fatalf("metric rollup batch %d = %#v, %v", expectedCount, summary, err)
		}
		var rollup persistence.WorkerIncarnationMetricRollup
		if err := db.Take(&rollup).Error; err != nil {
			t.Fatal(err)
		}
		if rollup.FactCount != expectedCount {
			t.Fatalf("rollup fact count after batch %d = %d", expectedCount, rollup.FactCount)
		}
	}
	if summary, err := service.RunOnce(context.Background(), 1); err != nil || summary.ProcessedFacts != 0 {
		t.Fatalf("idempotent empty rollup = %#v, %v", summary, err)
	}

	seedMetricRollupFact(t, db, now.Add(2*time.Minute), 3)
	if err := db.Exec(`CREATE TRIGGER reject_metric_rollup_completion
		BEFORE UPDATE ON worker_incarnation_metric_rollup_entries
		BEGIN
		  SELECT RAISE(ABORT, 'injected completion failure');
		END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunOnce(context.Background(), 1); err == nil {
		t.Fatal("expected injected membership completion failure")
	}
	var rollup persistence.WorkerIncarnationMetricRollup
	if err := db.Take(&rollup).Error; err != nil {
		t.Fatal(err)
	}
	if rollup.FactCount != 2 {
		t.Fatalf("failed completion committed rollup increment: %#v", rollup)
	}
	var pending int64
	if err := db.Model(&persistence.WorkerIncarnationMetricRollupEntry{}).
		Where("rolled_up_at IS NULL").Count(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("failed completion lost pending membership: %d", pending)
	}
}

func seedMetricRollupFact(t *testing.T, db *gorm.DB, terminalAt time.Time, incarnation int) {
	t.Helper()
	registeredAt := terminalAt.Add(-10 * time.Second)
	workerID := uuid.New()
	fact := persistence.WorkerIncarnationFact{
		WorkerID: workerID, WorkerIncarnation: int64(incarnation), ExecutionTargetID: uuid.New(),
		TargetKind: "local", WorkerMode: "general-pool", ClusterID: "test", Namespace: "default",
		PodName: "worker-" + workerID.String(), InstanceUID: uuid.NewString(), RegisteredAt: registeredAt,
		CurrentState: "terminated", StateChangedAt: terminalAt, TerminatedAt: &terminalAt,
		AccumulatedActiveSeconds: 4, AccumulatedIdleSeconds: 6,
		CreatedAt: registeredAt, UpdatedAt: terminalAt,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}
	bucketDay := utcDay(terminalAt)
	if err := db.Create(&persistence.WorkerIncarnationMetricRollupEntry{
		WorkerID: workerID, WorkerIncarnation: int64(incarnation), TerminalAt: terminalAt,
		BucketDay: bucketDay, CreatedAt: terminalAt, UpdatedAt: terminalAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestExecutionGenerationMetricRollupBatchesGenerationAndPodFailureExactlyOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ExecutionGenerationFact{},
		&persistence.ExecutionGenerationPodFailureFact{},
		&persistence.ExecutionGenerationMetricRollup{},
		&persistence.ExecutionGenerationMetricRollupEntry{},
		&persistence.ExecutionGenerationPodFailureMetricRollupEntry{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	dispatchedAt := now.Add(-12 * time.Second)
	podAppliedAt := now.Add(-11 * time.Second)
	podRunningAt := now.Add(-7 * time.Second)
	readyAt := now.Add(-2 * time.Second)
	terminalAt := now.Add(-time.Second)
	completed := "completed"
	fact := persistence.ExecutionGenerationFact{
		TenantID: uuid.New(), ExecutionID: uuid.New(), Generation: 1,
		SessionID: uuid.New(), TurnID: uuid.New(), ExecutionTargetID: uuid.New(),
		TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
		WarmPoolMode: "low-latency", WarmPoolResult: "hit",
		DispatchRequestedAt: &dispatchedAt, ProviderReadyAt: &readyAt,
		PodProvisioningStartedAt: &podAppliedAt, PodRunningAt: &podRunningAt,
		TerminalAt: &terminalAt, TerminalOutcome: &completed,
		ProviderResumeStrategy: "authoritative-history", CreatedAt: dispatchedAt, UpdatedAt: terminalAt,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}
	podUID := uuid.NewString()
	failure := persistence.ExecutionGenerationPodFailureFact{
		TenantID: fact.TenantID, ExecutionID: fact.ExecutionID, Generation: fact.Generation,
		FailureClass: "image-pull", ExecutionTargetID: fact.ExecutionTargetID,
		Namespace: "default", PodName: "worker", PodUID: &podUID, ReasonCode: "image-pull-backoff",
		FirstObservedAt: podAppliedAt, LastObservedAt: podAppliedAt,
		CreatedAt: podAppliedAt, UpdatedAt: podAppliedAt,
	}
	if err := db.Create(&failure).Error; err != nil {
		t.Fatal(err)
	}
	bucketDay := utcDay(dispatchedAt)
	if err := db.Create(&persistence.ExecutionGenerationMetricRollupEntry{
		TenantID: fact.TenantID, ExecutionID: fact.ExecutionID, Generation: fact.Generation,
		DispatchRequestedAt: dispatchedAt, TerminalAt: terminalAt, BucketDay: bucketDay,
		CreatedAt: terminalAt, UpdatedAt: terminalAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionGenerationPodFailureMetricRollupEntry{
		TenantID: fact.TenantID, ExecutionID: fact.ExecutionID, Generation: fact.Generation,
		FailureClass: failure.FailureClass, FirstObservedAt: failure.FirstObservedAt,
		BucketDay: utcDay(failure.FirstObservedAt), CreatedAt: failure.FirstObservedAt,
		UpdatedAt: failure.FirstObservedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	service.now = func() time.Time { return now }
	summary, err := service.RunOnce(context.Background(), 100)
	if err != nil || summary.ProcessedGenerationFacts != 1 || summary.ProcessedPodFailureFacts != 1 ||
		summary.ProcessedFacts != 2 || summary.UpdatedBuckets != 6 {
		t.Fatalf("Generation metric rollup = %#v, %v", summary, err)
	}
	var rows []persistence.ExecutionGenerationMetricRollup
	if err := db.Order("metric_kind").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("Generation metric rollup rows = %#v", rows)
	}
	for _, row := range rows {
		if row.SampleCount != 1 {
			t.Fatalf("Generation metric rollup row = %#v", row)
		}
	}
	if summary, err = service.RunOnce(context.Background(), 100); err != nil || summary.ProcessedFacts != 0 {
		t.Fatalf("Generation metric rollup replay = %#v, %v", summary, err)
	}
}

func TestExecutionGenerationMetricRollupRollsBackBeforeMembershipCompletion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ExecutionGenerationFact{},
		&persistence.ExecutionGenerationMetricRollup{},
		&persistence.ExecutionGenerationMetricRollupEntry{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	dispatchedAt := now.Add(-10 * time.Second)
	readyAt := now.Add(-2 * time.Second)
	terminalAt := now.Add(-time.Second)
	completed := "completed"
	fact := persistence.ExecutionGenerationFact{
		TenantID: uuid.New(), ExecutionID: uuid.New(), Generation: 1,
		SessionID: uuid.New(), TurnID: uuid.New(), ExecutionTargetID: uuid.New(),
		TargetKind: "local", Provider: "codex", RecoveryReason: "initial-claim",
		WarmPoolMode: "disabled", WarmPoolResult: "not-requested",
		DispatchRequestedAt: &dispatchedAt, ProviderReadyAt: &readyAt,
		TerminalAt: &terminalAt, TerminalOutcome: &completed,
		ProviderResumeStrategy: "authoritative-history", CreatedAt: dispatchedAt, UpdatedAt: terminalAt,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionGenerationMetricRollupEntry{
		TenantID: fact.TenantID, ExecutionID: fact.ExecutionID, Generation: fact.Generation,
		DispatchRequestedAt: dispatchedAt, TerminalAt: terminalAt, BucketDay: utcDay(dispatchedAt),
		CreatedAt: terminalAt, UpdatedAt: terminalAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_generation_metric_rollup_completion
		BEFORE UPDATE ON execution_generation_metric_rollup_entries
		BEGIN
		  SELECT RAISE(ABORT, 'injected Generation completion failure');
		END`).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	service.now = func() time.Time { return now }
	if _, err := service.RunOnce(context.Background(), 100); err == nil {
		t.Fatal("expected injected Generation membership completion failure")
	}
	var rollupCount int64
	if err := db.Model(&persistence.ExecutionGenerationMetricRollup{}).Count(&rollupCount).Error; err != nil {
		t.Fatal(err)
	}
	if rollupCount != 0 {
		t.Fatalf("failed Generation membership completion committed %d buckets", rollupCount)
	}
	var pending int64
	if err := db.Model(&persistence.ExecutionGenerationMetricRollupEntry{}).
		Where("rolled_up_at IS NULL").Count(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("failed Generation membership completion lost pending entry: %d", pending)
	}
}
