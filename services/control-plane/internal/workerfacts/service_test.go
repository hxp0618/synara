package workerfacts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestEnsureCurrentInTransactionCreatesAndReusesImmutableFact(t *testing.T) {
	fixture := newFixture(t)
	firstObservedAt := fixture.worker.RegisteredAt.Add(5*time.Second + 400*time.Millisecond)

	first, err := EnsureCurrentInTransaction(context.Background(), fixture.db, EnsureInput{
		Worker:             fixture.worker,
		Target:             fixture.target,
		Pool:               fixture.pool,
		RequestedResources: fixture.resources,
		ObservedAt:         firstObservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.CurrentState != StateIdle {
		t.Fatalf("current state = %q, want %q", first.CurrentState, StateIdle)
	}
	if !first.StateChangedAt.Equal(firstObservedAt.UTC()) {
		t.Fatalf("stateChangedAt = %s, want %s", first.StateChangedAt, firstObservedAt.UTC())
	}
	if first.ClaimCount != 0 {
		t.Fatalf("claimCount = %d, want 0", first.ClaimCount)
	}

	secondObservedAt := firstObservedAt.Add(3 * time.Second)
	second, err := EnsureCurrentInTransaction(context.Background(), fixture.db, EnsureInput{
		Worker:             fixture.worker,
		Target:             fixture.target,
		Pool:               fixture.pool,
		RequestedResources: fixture.resources,
		ObservedAt:         secondObservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.StateChangedAt.Equal(first.StateChangedAt) || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("Ensure updated an existing fact unexpectedly: before=%#v after=%#v", first, second)
	}
	if second.Region != fixture.pool.Region {
		t.Fatalf("region = %q, want %q", second.Region, fixture.pool.Region)
	}
}

func TestEnsureCurrentInTransactionRejectsImmutableIdentityMismatch(t *testing.T) {
	fixture := newFixture(t)
	now := fixture.worker.RegisteredAt.Add(time.Second)
	if _, err := EnsureCurrentInTransaction(context.Background(), fixture.db, EnsureInput{
		Worker:             fixture.worker,
		Target:             fixture.target,
		Pool:               fixture.pool,
		RequestedResources: fixture.resources,
		ObservedAt:         now,
	}); err != nil {
		t.Fatal(err)
	}

	mismatchedWorker := fixture.worker
	mismatchedWorker.Namespace = "other"
	_, err := EnsureCurrentInTransaction(context.Background(), fixture.db, EnsureInput{
		Worker:             mismatchedWorker,
		Target:             fixture.target,
		Pool:               fixture.pool,
		RequestedResources: fixture.resources,
		ObservedAt:         now.Add(time.Second),
	})
	assertProblemCode(t, err, "worker_fact_identity_conflict")
}

func TestEnsureCurrentInTransactionKeepsRegisteredPoolSnapshotAfterPoolUpdate(t *testing.T) {
	fixture := newFixture(t)
	now := fixture.worker.RegisteredAt.Add(time.Second)
	first, err := EnsureCurrentInTransaction(context.Background(), fixture.db, EnsureInput{
		Worker: fixture.worker, Target: fixture.target, Pool: fixture.pool,
		RequestedResources: fixture.resources, ObservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	updatedPool := *fixture.pool
	updatedPool.Version++
	updatedPool.Region = "cn-west-1"
	second, err := EnsureCurrentInTransaction(context.Background(), fixture.db, EnsureInput{
		Worker: fixture.worker, Target: fixture.target, Pool: &updatedPool,
		RequestedResources: fixture.resources, ObservedAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkerPoolVersion == nil || *second.WorkerPoolVersion != *fixture.worker.WorkerPoolVersion {
		t.Fatalf("worker pool version = %#v, want registered version %d", second.WorkerPoolVersion, *fixture.worker.WorkerPoolVersion)
	}
	if second.Region != first.Region {
		t.Fatalf("region changed from %q to %q after mutable Pool update", first.Region, second.Region)
	}
}

func TestTransitionCurrentInTransactionAccruesWholeSecondsAndClaimCount(t *testing.T) {
	fixture := newFixture(t)
	base := fixture.worker.RegisteredAt

	fact, err := EnsureCurrentInTransaction(context.Background(), fixture.db, EnsureInput{
		Worker:             fixture.worker,
		Target:             fixture.target,
		Pool:               fixture.pool,
		RequestedResources: fixture.resources,
		ObservedAt:         base,
	})
	if err != nil {
		t.Fatal(err)
	}

	fact, err = TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(5*time.Second + 900*time.Millisecond),
		},
		NextState:           StateActive,
		IncrementClaimCount: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.AccumulatedIdleSeconds != 5 || fact.ClaimCount != 1 || fact.CurrentState != StateActive {
		t.Fatalf("active transition fact = %#v", fact)
	}
	activeStateChangedAt := fact.StateChangedAt

	fact, err = TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(7*time.Second + 400*time.Millisecond),
		},
		NextState: StateActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.ClaimCount != 1 || !fact.StateChangedAt.Equal(activeStateChangedAt) {
		t.Fatalf("same-state no-op fact = %#v", fact)
	}

	fact, err = TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(7*time.Second + 800*time.Millisecond),
		},
		NextState:           StateActive,
		IncrementClaimCount: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.ClaimCount != 2 || !fact.StateChangedAt.Equal(activeStateChangedAt) {
		t.Fatalf("same-state claim increment fact = %#v", fact)
	}

	fact, err = TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(9*time.Second + 100*time.Millisecond),
		},
		NextState: StateDraining,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.AccumulatedActiveSeconds != 3 || fact.CurrentState != StateDraining {
		t.Fatalf("draining transition fact = %#v", fact)
	}

	fact, err = TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(12 * time.Second),
		},
		NextState: StateIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.AccumulatedActiveSeconds != 3 || fact.CurrentState != StateIdle {
		t.Fatalf("idle transition fact = %#v", fact)
	}

	reason := "scale-down"
	fact, err = TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(14*time.Second + 400*time.Millisecond),
		},
		NextState:      StateTerminated,
		TerminalReason: &reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.AccumulatedIdleSeconds != 7 || fact.CurrentState != StateTerminated || fact.TerminatedAt == nil {
		t.Fatalf("terminated transition fact = %#v", fact)
	}
	if fact.TerminalReason == nil || *fact.TerminalReason != reason {
		t.Fatalf("terminal reason = %#v, want %q", fact.TerminalReason, reason)
	}
}

func TestTransitionCurrentInTransactionAdoptsMissingFactIntoRequestedState(t *testing.T) {
	fixture := newFixture(t)
	now := fixture.worker.RegisteredAt.Add(20 * time.Second)

	fact, err := TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         now,
		},
		NextState:           StateActive,
		IncrementClaimCount: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.CurrentState != StateActive || fact.AccumulatedIdleSeconds != 0 || fact.AccumulatedActiveSeconds != 0 || fact.ClaimCount != 1 {
		t.Fatalf("adopted transition fact = %#v", fact)
	}
	if !fact.StateChangedAt.Equal(now.UTC()) {
		t.Fatalf("stateChangedAt = %s, want %s", fact.StateChangedAt, now.UTC())
	}
}

func TestTransitionCurrentInTransactionRejectsClockRegressionAndReterminalization(t *testing.T) {
	fixture := newFixture(t)
	base := fixture.worker.RegisteredAt

	if _, err := EnsureCurrentInTransaction(context.Background(), fixture.db, EnsureInput{
		Worker:             fixture.worker,
		Target:             fixture.target,
		Pool:               fixture.pool,
		RequestedResources: fixture.resources,
		ObservedAt:         base.Add(10 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(20 * time.Second),
		},
		NextState: StateActive,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(19 * time.Second),
		},
		NextState: StateIdle,
	})
	assertProblemCode(t, err, "worker_fact_clock_regressed")

	reason := "completed"
	if _, err := TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(30 * time.Second),
		},
		NextState:      StateTerminated,
		TerminalReason: &reason,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = TransitionCurrentInTransaction(context.Background(), fixture.db, TransitionInput{
		EnsureInput: EnsureInput{
			Worker:             fixture.worker,
			Target:             fixture.target,
			Pool:               fixture.pool,
			RequestedResources: fixture.resources,
			ObservedAt:         base.Add(31 * time.Second),
		},
		NextState: StateActive,
	})
	assertProblemCode(t, err, "worker_fact_terminalized")
}

type testFixture struct {
	db        *gorm.DB
	worker    persistence.WorkerInstance
	target    TargetSnapshot
	pool      *PoolSnapshot
	resources RequestedResources
}

func newFixture(t *testing.T) testFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.Tenant{},
		&persistence.ExecutionTarget{},
		&persistence.WorkerPool{},
		&persistence.WorkerInstance{},
		&persistence.WorkerIncarnationFact{},
	); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 24, 10, 0, 0, 500_000_000, time.UTC)
	tenantID := uuid.New()
	targetID := uuid.New()
	poolID := uuid.New()
	workerID := uuid.New()
	version := int64(3)
	capacityClass := "interactive"
	requestedCPU := int64(500)
	requestedMemory := int64(4096)
	requestedEphemeral := int64(8192)

	if err := db.Create(&persistence.Tenant{
		ID: tenantID, Slug: "workerfacts-" + uuid.NewString()[:8], Name: "Worker Facts", Status: "active",
		PlanCode: "enterprise", Region: "default", Settings: map[string]any{}, CreatedBy: uuid.New(),
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &tenantID, Kind: "kubernetes", Name: "target", Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.WorkerPool{
		ID: poolID, TenantID: &tenantID, ExecutionTargetID: targetID,
		Name: "warm", Mode: "warm", CapacityClass: capacityClass,
		ClusterID: "cluster-a", Region: "cn-east-1", Namespace: "default",
		DesiredIdleUnits: 1, MaxActiveUnits: 2, SchedulingTemplate: map[string]any{},
		Status: "active", Version: version, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	worker := persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID,
		TargetKind: "kubernetes", WorkerMode: "warm-pool", WorkerPoolID: &poolID, WorkerPoolVersion: &version,
		CapacityClass: &capacityClass, RegistrationTrustMode: "kubernetes-pod-bound-v1",
		ClusterID: "cluster-a", Namespace: "default", PodName: "warm-0",
		Version: "test", ProtocolVersion: 2, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("hash"),
		Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}

	return testFixture{
		db:     db,
		worker: worker,
		target: TargetSnapshot{TenantID: &tenantID},
		pool: &PoolSnapshot{
			ID: poolID, Version: version, Mode: "warm", CapacityClass: capacityClass, Region: "cn-east-1",
		},
		resources: RequestedResources{
			CPUMillicores:         &requestedCPU,
			MemoryBytes:           &requestedMemory,
			EphemeralStorageBytes: &requestedEphemeral,
		},
	}
}

func assertProblemCode(t *testing.T, err error, expected string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != expected {
		t.Fatalf("problem code = %v, want %q (error: %v)", apiError, expected, err)
	}
}
