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

	now := time.Now().UTC().Truncate(time.Second)
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
