package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestWorkerIncarnationFactMetricsExposeDurableRuntimeAndResourceProxyTotals(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.WorkerIncarnationFact{}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	warm := "warm"
	interactive := "interactive"
	cpu500 := int64(500)
	memory2048 := int64(2048)
	storage4096 := int64(4096)
	activeFact := persistence.WorkerIncarnationFact{
		WorkerID: uuid.New(), WorkerIncarnation: 1, ExecutionTargetID: uuid.New(),
		TargetKind: "kubernetes", WorkerMode: "warm-pool",
		PoolMode: &warm, CapacityClass: &interactive,
		ClusterID: "cluster-a", Region: "cn-east-1", Namespace: "default", PodName: "warm-a", InstanceUID: uuid.NewString(),
		RegisteredAt: now.Add(-20 * time.Second), CurrentState: "active", StateChangedAt: now.Add(-5 * time.Second),
		AccumulatedActiveSeconds: 8, AccumulatedIdleSeconds: 7, ClaimCount: 1,
		RequestedCPUMillicores: &cpu500, RequestedMemoryBytes: &memory2048,
		RequestedEphemeralStorageBytes: &storage4096,
		CreatedAt:                      now.Add(-20 * time.Second), UpdatedAt: now,
	}

	standard := "standard"
	cpu1000 := int64(1000)
	memory1024 := int64(1024)
	storage2048 := int64(2048)
	terminatedAt := now.Add(-2 * time.Second)
	terminatedReason := "evicted"
	terminatedFact := persistence.WorkerIncarnationFact{
		WorkerID: uuid.New(), WorkerIncarnation: 2, ExecutionTargetID: uuid.New(),
		TargetKind: "docker", WorkerMode: "general-pool",
		CapacityClass: &standard,
		ClusterID:     "cluster-b", Region: "", Namespace: "default", PodName: "resident-b", InstanceUID: uuid.NewString(),
		RegisteredAt: now.Add(-40 * time.Second), CurrentState: "terminated", StateChangedAt: now.Add(-4 * time.Second),
		TerminatedAt: &terminatedAt, TerminalReason: &terminatedReason,
		AccumulatedActiveSeconds: 12, AccumulatedIdleSeconds: 9, ClaimCount: 3,
		RequestedCPUMillicores: &cpu1000, RequestedMemoryBytes: &memory1024,
		RequestedEphemeralStorageBytes: &storage2048,
		CreatedAt:                      now.Add(-40 * time.Second), UpdatedAt: terminatedAt,
	}

	for _, fact := range []persistence.WorkerIncarnationFact{activeFact, terminatedFact} {
		if err := db.Create(&fact).Error; err != nil {
			t.Fatal(err)
		}
	}

	var output bytes.Buffer
	if err := New(db).writeWorkerIncarnationFactMetrics(context.Background(), &output, now); err != nil {
		t.Fatal(err)
	}
	metrics := output.String()
	for _, expected := range []string{
		`synara_worker_incarnation_facts{capacity_class="interactive",mode="warm",state="active",target_kind="kubernetes"} 1`,
		`synara_worker_incarnation_facts{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 1`,
		`synara_worker_incarnation_run_seconds{capacity_class="interactive",mode="warm",state="active",target_kind="kubernetes"} 20`,
		`synara_worker_incarnation_run_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 38`,
		`synara_worker_incarnation_active_seconds{capacity_class="interactive",mode="warm",state="active",target_kind="kubernetes"} 13`,
		`synara_worker_incarnation_active_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 12`,
		`synara_worker_incarnation_idle_seconds{capacity_class="interactive",mode="warm",state="active",target_kind="kubernetes"} 7`,
		`synara_worker_incarnation_idle_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 9`,
		`synara_worker_incarnation_requested_cpu_seconds{capacity_class="interactive",mode="warm",state="active",target_kind="kubernetes"} 10`,
		`synara_worker_incarnation_requested_cpu_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 38`,
		`synara_worker_incarnation_requested_memory_byte_seconds{capacity_class="interactive",mode="warm",state="active",target_kind="kubernetes"} 40960`,
		`synara_worker_incarnation_requested_memory_byte_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 38912`,
		`synara_worker_incarnation_requested_ephemeral_storage_byte_seconds{capacity_class="interactive",mode="warm",state="active",target_kind="kubernetes"} 81920`,
		`synara_worker_incarnation_requested_ephemeral_storage_byte_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 77824`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
	for _, forbidden := range []string{
		activeFact.WorkerID.String(), terminatedFact.WorkerID.String(),
		activeFact.ExecutionTargetID.String(), terminatedFact.ExecutionTargetID.String(),
	} {
		if strings.Contains(metrics, forbidden) {
			t.Fatalf("metrics leaked high-cardinality identifier %q:\n%s", forbidden, metrics)
		}
	}
}

func TestWorkerIncarnationFactMetricsMergeRollupsAndPendingFactsExactlyOnce(t *testing.T) {
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
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	standard := "standard"
	cpu := int64(1000)
	memory := int64(1024)
	storage := int64(2048)
	processedTerminalAt := now.Add(-2 * time.Second)
	pendingTerminalAt := now.Add(-time.Second)
	processed := persistence.WorkerIncarnationFact{
		WorkerID: uuid.New(), WorkerIncarnation: 1, ExecutionTargetID: uuid.New(),
		TargetKind: "docker", WorkerMode: "general-pool", CapacityClass: &standard,
		ClusterID: "cluster-a", Namespace: "default", PodName: "processed", InstanceUID: uuid.NewString(),
		RegisteredAt: now.Add(-40 * time.Second), CurrentState: "terminated", StateChangedAt: now.Add(-4 * time.Second),
		TerminatedAt: &processedTerminalAt, AccumulatedActiveSeconds: 12, AccumulatedIdleSeconds: 9,
		RequestedCPUMillicores: &cpu, RequestedMemoryBytes: &memory,
		RequestedEphemeralStorageBytes: &storage, CreatedAt: now.Add(-40 * time.Second), UpdatedAt: processedTerminalAt,
	}
	pending := processed
	pending.WorkerID = uuid.New()
	pending.WorkerIncarnation = 2
	pending.ExecutionTargetID = uuid.New()
	pending.PodName = "pending"
	pending.InstanceUID = uuid.NewString()
	pending.RegisteredAt = now.Add(-10 * time.Second)
	pending.StateChangedAt = now.Add(-3 * time.Second)
	pending.TerminatedAt = &pendingTerminalAt
	pending.AccumulatedActiveSeconds = 3
	pending.AccumulatedIdleSeconds = 2
	pending.CreatedAt = pending.RegisteredAt
	pending.UpdatedAt = pendingTerminalAt
	previousTerminalAt := processedTerminalAt.Add(-24 * time.Hour)
	previousProcessed := processed
	previousProcessed.WorkerID = uuid.New()
	previousProcessed.WorkerIncarnation = 3
	previousProcessed.ExecutionTargetID = uuid.New()
	previousProcessed.PodName = "previous-processed"
	previousProcessed.InstanceUID = uuid.NewString()
	previousProcessed.RegisteredAt = previousTerminalAt.Add(-38 * time.Second)
	previousProcessed.StateChangedAt = previousTerminalAt.Add(-2 * time.Second)
	previousProcessed.TerminatedAt = &previousTerminalAt
	previousProcessed.AccumulatedActiveSeconds = 5
	previousProcessed.AccumulatedIdleSeconds = 4
	previousProcessed.CreatedAt = previousProcessed.RegisteredAt
	previousProcessed.UpdatedAt = previousTerminalAt
	for _, fact := range []persistence.WorkerIncarnationFact{processed, pending, previousProcessed} {
		if err := db.Create(&fact).Error; err != nil {
			t.Fatal(err)
		}
	}
	bucketDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	previousBucketDay := bucketDay.Add(-24 * time.Hour)
	rolledAt := now
	for _, entry := range []persistence.WorkerIncarnationMetricRollupEntry{
		{
			WorkerID: processed.WorkerID, WorkerIncarnation: processed.WorkerIncarnation,
			TerminalAt: processedTerminalAt, BucketDay: bucketDay, RolledUpAt: &rolledAt,
			CreatedAt: processedTerminalAt, UpdatedAt: rolledAt,
		},
		{
			WorkerID: pending.WorkerID, WorkerIncarnation: pending.WorkerIncarnation,
			TerminalAt: pendingTerminalAt, BucketDay: bucketDay,
			CreatedAt: pendingTerminalAt, UpdatedAt: pendingTerminalAt,
		},
		{
			WorkerID: previousProcessed.WorkerID, WorkerIncarnation: previousProcessed.WorkerIncarnation,
			TerminalAt: previousTerminalAt, BucketDay: previousBucketDay, RolledUpAt: &rolledAt,
			CreatedAt: previousTerminalAt, UpdatedAt: rolledAt,
		},
	} {
		if err := db.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, rollup := range []persistence.WorkerIncarnationMetricRollup{
		{
			BucketDay: bucketDay, TargetKind: "docker", PoolMode: "unassigned", CapacityClass: "standard",
			FactCount: 1, RunSeconds: 38, ActiveSeconds: 12, IdleSeconds: 9,
			RequestedCPUSeconds: 38, RequestedMemoryByteSeconds: 38912,
			RequestedEphemeralStorageByteSeconds: 77824, CreatedAt: processedTerminalAt, UpdatedAt: rolledAt,
		},
		{
			BucketDay: previousBucketDay, TargetKind: "docker", PoolMode: "unassigned", CapacityClass: "standard",
			FactCount: 1, RunSeconds: 38, ActiveSeconds: 5, IdleSeconds: 4,
			RequestedCPUSeconds: 38, RequestedMemoryByteSeconds: 38912,
			RequestedEphemeralStorageByteSeconds: 77824, CreatedAt: previousTerminalAt, UpdatedAt: rolledAt,
		},
	} {
		if err := db.Create(&rollup).Error; err != nil {
			t.Fatal(err)
		}
	}

	var output bytes.Buffer
	if err := New(db).writeWorkerIncarnationFactMetrics(context.Background(), &output, now); err != nil {
		t.Fatal(err)
	}
	metrics := output.String()
	for _, expected := range []string{
		`synara_worker_incarnation_facts{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 3`,
		`synara_worker_incarnation_run_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 85`,
		`synara_worker_incarnation_active_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 20`,
		`synara_worker_incarnation_idle_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 15`,
		`synara_worker_incarnation_requested_cpu_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 85`,
		`synara_worker_incarnation_requested_memory_byte_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 87040`,
		`synara_worker_incarnation_requested_ephemeral_storage_byte_seconds{capacity_class="standard",mode="unassigned",state="terminated",target_kind="docker"} 174080`,
		`synara_metric_rollup_pending_facts{kind="worker-incarnation"} 1`,
		`synara_metric_rollup_buckets{kind="worker-incarnation"} 2`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("rollup metrics omitted %q:\n%s", expected, metrics)
		}
	}
}
