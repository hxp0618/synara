package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteWorkerIncarnationFactSafetyRejectsIdentityAndTerminalRegression(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-worker-incarnation-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"idx_worker_incarnation_facts_metrics",
		"trg_worker_incarnation_facts_insert",
		"trg_worker_incarnation_facts_update",
		"trg_worker_incarnation_facts_delete",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite Worker incarnation fact safety object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	targetID := uuid.New()
	if err := store.DB().Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "sqlite-worker-incarnation-target", Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create kubernetes target: %v", err)
	}

	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &domain.TenantID, ExecutionTargetID: targetID,
		Name: "warm", Mode: "warm", CapacityClass: "interactive",
		ClusterID: "cluster-a", Region: "cn-east-1", Namespace: "default",
		DesiredIdleUnits: 1, MaxActiveUnits: 2, SchedulingTemplate: map[string]any{},
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&pool).Error; err != nil {
		t.Fatalf("create worker pool: %v", err)
	}
	otherPool := pool
	otherPool.ID = uuid.New()
	otherPool.Name = "warm-other"
	if err := store.DB().Create(&otherPool).Error; err != nil {
		t.Fatalf("create other worker pool: %v", err)
	}

	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID,
		TargetKind: "kubernetes", WorkerMode: "warm-pool", RegistrationTrustMode: "kubernetes-pod-bound-v1",
		WorkerPoolID: &pool.ID, WorkerPoolVersion: &pool.Version, CapacityClass: &pool.CapacityClass,
		ClusterID: "cluster-a", Namespace: "default", PodName: "warm-0",
		Version: "test", ProtocolVersion: 2, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("hash"),
		Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
	}
	if err := store.DB().Create(&worker).Error; err != nil {
		t.Fatalf("create worker: %v", err)
	}

	requestedCPU := int64(500)
	requestedMemory := int64(4096)
	fact := persistence.WorkerIncarnationFact{
		WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, TenantID: &domain.TenantID,
		ExecutionTargetID: targetID, TargetKind: worker.TargetKind, WorkerMode: worker.WorkerMode,
		WorkerPoolID: &pool.ID, WorkerPoolVersion: &pool.Version, PoolMode: &pool.Mode, CapacityClass: &pool.CapacityClass,
		ClusterID: worker.ClusterID, Region: pool.Region, Namespace: worker.Namespace, PodName: worker.PodName, InstanceUID: worker.InstanceUID,
		RegisteredAt: now, CurrentState: "active", StateChangedAt: now, ClaimCount: 1,
		RequestedCPUMillicores: &requestedCPU, RequestedMemoryBytes: &requestedMemory,
		CreatedAt: now, UpdatedAt: now,
	}
	for index, invalidUID := range []string{
		strings.ToUpper(uuid.NewString()),
		"gggggggg-gggg-gggg-gggg-gggggggggggg",
	} {
		invalidWorker := worker
		invalidWorker.ID = uuid.New()
		invalidWorker.InstanceUID = invalidUID
		invalidWorker.PodName = "warm-invalid-" + string(rune('a'+index))
		if err := store.DB().Create(&invalidWorker).Error; err != nil {
			t.Fatalf("create Worker with locally representable invalid instance UID: %v", err)
		}
		invalidFact := fact
		invalidFact.WorkerID = invalidWorker.ID
		invalidFact.InstanceUID = invalidUID
		invalidFact.PodName = invalidWorker.PodName
		assertSQLiteStage4Rejected(
			t,
			store.DB().Create(&invalidFact).Error,
			"invalid Worker incarnation fact",
		)
	}
	wrongFact := fact
	wrongFact.WorkerPoolID = &otherPool.ID
	if err := store.DB().Create(&wrongFact).Error; err == nil {
		t.Fatal("SQLite accepted a Worker fact whose Pool identity differed from its Worker incarnation")
	}
	if err := store.DB().Create(&fact).Error; err != nil {
		t.Fatalf("create valid worker incarnation fact: %v", err)
	}

	assertSQLiteStage4Rejected(
		t,
		store.DB().Model(&persistence.WorkerIncarnationFact{}).
			Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
			Update("region", "cn-west-1").Error,
		"identity is immutable",
	)

	assertSQLiteStage4Rejected(
		t,
		store.DB().Model(&persistence.WorkerIncarnationFact{}).
			Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
			Update("claim_count", 0).Error,
		"timeline or counters regressed",
	)

	terminatedAt := now.Add(2 * time.Second)
	if err := store.DB().Model(&persistence.WorkerIncarnationFact{}).
		Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
		Updates(map[string]any{
			"current_state":              "terminated",
			"state_changed_at":           now.Add(time.Second),
			"terminated_at":              terminatedAt,
			"terminal_reason":            "scale-down",
			"accumulated_active_seconds": 9,
			"updated_at":                 terminatedAt,
		}).Error; err != nil {
		t.Fatalf("terminate valid worker incarnation fact: %v", err)
	}

	assertSQLiteStage4Rejected(
		t,
		store.DB().Model(&persistence.WorkerIncarnationFact{}).
			Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
			Updates(map[string]any{"claim_count": 2, "updated_at": terminatedAt.Add(time.Second)}).Error,
		"terminal state is immutable",
	)
	assertSQLiteStage4Rejected(
		t,
		store.DB().Delete(&persistence.WorkerIncarnationFact{},
			"worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation,
		).Error,
		"cannot be deleted",
	)
}
