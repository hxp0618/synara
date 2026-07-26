package executiontargets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
)

func TestManagedKubernetesWarmCapacityPublisherWritesScopedObservation(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 2, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).
		Where("id = ?", fixture.targetID).
		Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, time.July, 25, 15, 0, 0, 0, time.UTC)
	publisher := NewManagedKubernetesWarmCapacityPublisher(
		fixture.reconciler.targets,
		ManagedKubernetesWarmCapacityPublisherConfig{ObservationTTL: 10 * time.Second},
	)
	publisher.now = func() time.Time { return observedAt }

	if err := publisher.PublishReconcile(context.Background(), ManagedKubernetesWarmCapacityObservation{
		TenantID: fixture.tenantID, ExecutionTargetID: fixture.targetID,
		WorkerPoolID: pool.ID, WorkerPoolVersion: pool.Version,
		WarmSupported: true, DesiredTotalUnits: 1, ReadyIdleUnits: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var capacity persistence.WorkerPoolWarmCapacity
	if err := fixture.db.Where("worker_pool_id = ? AND worker_pool_version = ?", pool.ID, pool.Version).
		Take(&capacity).Error; err != nil {
		t.Fatal(err)
	}
	if capacity.Source != managedKubernetesWarmCapacityPublisherPrefix+"default" ||
		!capacity.ObservedAt.Equal(observedAt) ||
		!capacity.ExpiresAt.Equal(observedAt.Add(10*time.Second)) {
		t.Fatalf("warm capacity publisher metadata = %#v", capacity)
	}
	if !capacity.WarmSupported || capacity.DesiredTotalUnits != 1 || capacity.ClaimedUnits != 0 || capacity.ReadyIdleUnits != 1 {
		t.Fatalf("warm capacity publisher counters = %#v", capacity)
	}
}

func TestKubernetesReconcilerPublishesPostQueueReadyWarmCapacityOnlyAfterSuccess(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 2, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	currentNow := time.Now().UTC().Truncate(time.Second)
	fixture.reconciler.now = func() time.Time { return currentNow }
	publisher := NewManagedKubernetesWarmCapacityPublisher(
		fixture.reconciler.targets,
		ManagedKubernetesWarmCapacityPublisherConfig{
			PublisherIdentity: "managed-kubernetes-warm-capacity-test",
			ObservationTTL:    10 * time.Second,
		},
	)
	fixture.reconciler.config.PublishWarmCapacity = publisher.PublishReconcile

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	capacity := loadWorkerPoolWarmCapacity(t, fixture, pool)
	if capacity.Version != 1 || capacity.DesiredTotalUnits != 1 || capacity.ReadyIdleUnits != 0 {
		t.Fatalf("initial warm capacity = %#v", capacity)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	fixture.registerWarmWorker(t, pool, warmPod, "online", "active", nil, nil)

	currentNow = currentNow.Add(time.Second)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	capacity = loadWorkerPoolWarmCapacity(t, fixture, pool)
	if capacity.Version != 2 || capacity.ReadyIdleUnits != 1 {
		t.Fatalf("ready warm capacity = %#v", capacity)
	}

	fixture.assignExecutionToPool(t, fixture.executionIDs[0], pool, "queued")
	currentNow = currentNow.Add(time.Second)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	capacity = loadWorkerPoolWarmCapacity(t, fixture, pool)
	if capacity.Version != 3 || capacity.DesiredTotalUnits != 1 || capacity.ReadyIdleUnits != 0 {
		t.Fatalf("post-queue warm capacity = %#v", capacity)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); found {
		t.Fatalf("queued execution received a cold Pod despite reserved ready warm capacity: %#v", client.pods)
	}

	lastObservedAt := capacity.ObservedAt
	lastExpiresAt := capacity.ExpiresAt
	currentNow = currentNow.Add(30 * time.Second)
	fixture.reconciler.factory = failingManagedKubernetesFactory{err: errors.New("API transport unavailable")}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expected failed reconcile")
	}
	capacity = loadWorkerPoolWarmCapacity(t, fixture, pool)
	if capacity.Version != 3 || !capacity.ObservedAt.Equal(lastObservedAt) || !capacity.ExpiresAt.Equal(lastExpiresAt) {
		t.Fatalf("failed reconcile refreshed warm capacity: %#v", capacity)
	}
	if capacity.ExpiresAt.After(currentNow) {
		t.Fatalf("old warm capacity did not expire after failed reconcile: %#v", capacity)
	}
}

func TestKubernetesReconcilerPublishesUnsupportedCanaryCapacityWithRetainedClaim(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	promotedDigest := "sha256:" + strings.Repeat("d", 64)
	canaryDigest := "sha256:" + strings.Repeat("e", 64)
	promotedRevision := fixture.seedReleaseRevision(t, 1, promotedDigest)
	canaryRevision := fixture.seedReleaseRevision(t, 2, canaryDigest)
	fixture.seedReleasePolicy(t, promotedRevision, nil, 0)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 2, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	currentNow := time.Now().UTC().Truncate(time.Second)
	fixture.reconciler.now = func() time.Time { return currentNow }
	publisher := NewManagedKubernetesWarmCapacityPublisher(
		fixture.reconciler.targets,
		ManagedKubernetesWarmCapacityPublisherConfig{
			PublisherIdentity: "managed-kubernetes-warm-capacity-test",
			ObservationTTL:    time.Minute,
		},
	)
	fixture.reconciler.config.PublishWarmCapacity = publisher.PublishReconcile

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	promotedChannel := "promoted"
	worker := fixture.registerWarmWorker(t, pool, warmPod, "online", "active", &promotedRevision, &promotedChannel)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{"worker_id": worker.ID, "generation": 1, "status": "leased"}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.createWarmWorkerLease(t, worker, fixture.executionIDs[0])
	fixture.seedReleasePolicy(t, promotedRevision, &canaryRevision, 20)

	currentNow = currentNow.Add(time.Second)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	capacity := loadWorkerPoolWarmCapacity(t, fixture, pool)
	if capacity.WarmSupported || capacity.DesiredTotalUnits != 0 || capacity.ReadyIdleUnits != 0 || capacity.ClaimedUnits != 1 {
		t.Fatalf("unsupported canary warm capacity = %#v", capacity)
	}
	if capacity.WorkerReleaseRevisionID != nil || capacity.WorkerReleaseChannel != nil {
		t.Fatalf("unsupported canary warm capacity retained a release pair: %#v", capacity)
	}
	if capacity.Reason == nil || *capacity.Reason != "Managed Kubernetes warm capacity is unavailable while a canary Worker release is active." {
		t.Fatalf("unsupported canary reason = %#v", capacity.Reason)
	}
}

func TestKubernetesReconcilerAttemptsEveryActiveWarmCapacityPublication(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	first := fixture.createWarmPool(t, placement.CapacityClassStandard, 0, 1, placement.PoolStatusActive)
	second := fixture.createWarmPool(t, placement.CapacityClassInteractive, 0, 1, placement.PoolStatusActive)
	disabled := fixture.createWarmPool(t, placement.CapacityClassInteractive, 0, 1, placement.PoolStatusDisabled)
	fixture.reconciler.factory = &fakeKubernetesFactory{client: newFakeKubernetesClient()}
	publishFailure := errors.New("warm capacity sink unavailable")
	called := make(map[uuid.UUID]int)
	fixture.reconciler.config.PublishWarmCapacity = func(_ context.Context, observation ManagedKubernetesWarmCapacityObservation) error {
		called[observation.WorkerPoolID]++
		if observation.WorkerPoolID == first.ID {
			return publishFailure
		}
		return nil
	}

	err := fixture.reconciler.ReconcileOnce(context.Background())
	if !errors.Is(err, publishFailure) {
		t.Fatalf("reconcile publication error = %v", err)
	}
	if called[first.ID] != 1 || called[second.ID] != 1 || called[disabled.ID] != 0 || len(called) != 2 {
		t.Fatalf("warm capacity publication calls = %#v", called)
	}
}

func loadWorkerPoolWarmCapacity(
	t *testing.T,
	fixture kubernetesReconcileFixture,
	pool persistence.WorkerPool,
) persistence.WorkerPoolWarmCapacity {
	t.Helper()
	var capacity persistence.WorkerPoolWarmCapacity
	if err := fixture.db.Where("worker_pool_id = ? AND worker_pool_version = ?", pool.ID, pool.Version).
		Take(&capacity).Error; err != nil {
		t.Fatal(err)
	}
	return capacity
}
