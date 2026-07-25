package executiontargets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

type advancingKubernetesClient struct {
	*fakeKubernetesClient
	afterFirstApply func()
	applyCount      int
}

type failingManagedKubernetesFactory struct{ err error }

func (f failingManagedKubernetesFactory) Open(kubernetesTargetConfiguration) (kubernetesClient, error) {
	return nil, f.err
}

func (c *advancingKubernetesClient) Apply(ctx context.Context, path string, object map[string]any) error {
	if err := c.fakeKubernetesClient.Apply(ctx, path, object); err != nil {
		return err
	}
	if c.applyCount == 0 && c.afterFirstApply != nil {
		c.afterFirstApply()
	}
	c.applyCount++
	return nil
}

func TestKubernetesReconcilerPublishesRoutingHealthAtReconcileCompletion(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	startedAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(7 * time.Second)
	currentNow := startedAt
	client := &advancingKubernetesClient{
		fakeKubernetesClient: newFakeKubernetesClient(),
		afterFirstApply: func() {
			currentNow = finishedAt
		},
	}
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	fixture.reconciler.now = func() time.Time { return currentNow }
	publisher := NewManagedKubernetesRoutingPublisher(
		fixture.reconciler.targets,
		ManagedKubernetesRoutingPublisherConfig{
			PublisherIdentity: "managed-kubernetes-routing-test",
			ObservationTTL:    10 * time.Second,
		},
	)
	publisher.now = func() time.Time { return currentNow }
	fixture.reconciler.config.PublishRoutingHealth = publisher.PublishReconcile

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	var health persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&health).Error; err != nil {
		t.Fatal(err)
	}
	if health.Status != routing.HealthHealthy || health.CapacityStatus != routing.CapacitySaturated {
		t.Fatalf("routing health status = %#v", health)
	}
	if health.AvailableCapacityUnits == nil || *health.AvailableCapacityUnits != 1 || health.AllocatedCapacityUnits != 1 {
		t.Fatalf("routing health capacity = %#v", health)
	}
	if health.Source != "managed-kubernetes-routing-test" || health.Version != 1 {
		t.Fatalf("routing health source/version = %#v", health)
	}
	if !health.ObservedAt.Equal(finishedAt) || !health.ExpiresAt.Equal(finishedAt.Add(10*time.Second)) {
		t.Fatalf("routing health timing = %#v", health)
	}
	if health.Reason == nil || *health.Reason != "Managed Kubernetes scheduled pod capacity is fully allocated." {
		t.Fatalf("routing health reason = %#v", health.Reason)
	}
	var readinessCount int64
	if err := fixture.db.Model(&persistence.ExecutionTargetDRReadiness{}).
		Where("execution_target_id = ?", fixture.targetID).
		Count(&readinessCount).Error; err != nil {
		t.Fatal(err)
	}
	if readinessCount != 0 {
		t.Fatalf("unexpected managed DR readiness rows = %d", readinessCount)
	}

	currentNow = finishedAt.Add(30 * time.Second)
	var unchanged persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&unchanged).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != 1 || !unchanged.ObservedAt.Equal(health.ObservedAt) || !unchanged.ExpiresAt.Equal(health.ExpiresAt) {
		t.Fatalf("routing health changed without a fresh reconcile: %#v", unchanged)
	}
}

func TestKubernetesReconcilerIgnoresSharedKubernetesTargetsForRoutingHealth(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	now := time.Date(2026, time.July, 25, 13, 0, 0, 0, time.UTC)
	fixture.reconciler.factory = &fakeKubernetesFactory{client: newFakeKubernetesClient()}
	fixture.reconciler.now = func() time.Time { return now }
	publisher := NewManagedKubernetesRoutingPublisher(
		fixture.reconciler.targets,
		ManagedKubernetesRoutingPublisherConfig{
			PublisherIdentity: "managed-kubernetes-routing-test",
			ObservationTTL:    time.Minute,
		},
	)
	publisher.now = func() time.Time { return now }
	fixture.reconciler.config.PublishRoutingHealth = publisher.PublishReconcile

	encrypted, err := encryptConfiguration(fixture.reconciler.targets.cipher, kubernetesTestConfiguration(""))
	if err != nil {
		t.Fatal(err)
	}
	sharedTargetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID:                     sharedTargetID,
		Kind:                   "kubernetes",
		Name:                   "shared-kubernetes-routing",
		Status:                 "active",
		ConfigurationEncrypted: encrypted,
		Capabilities:           map[string]any{},
		CreatedAt:              now,
		UpdatedAt:              now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	seeded, err := routing.NewService(fixture.db).ObserveHealth(context.Background(), routing.HealthObservation{
		ExecutionTargetID:      sharedTargetID,
		Status:                 routing.HealthHealthy,
		CapacityStatus:         routing.CapacityAvailable,
		AvailableCapacityUnits: managedKubernetesRoutingTestIntPointer(9),
		AllocatedCapacityUnits: 1,
		Source:                 "operator-seed",
		ObservedAt:             now,
		TTL:                    time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	var sharedHealth persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", sharedTargetID).Take(&sharedHealth).Error; err != nil {
		t.Fatal(err)
	}
	if sharedHealth.Source != seeded.Source || sharedHealth.Version != seeded.Version ||
		!sharedHealth.ObservedAt.Equal(seeded.ObservedAt) || !sharedHealth.ExpiresAt.Equal(seeded.ExpiresAt) {
		t.Fatalf("shared target health was overwritten: got=%#v want=%#v", sharedHealth, seeded)
	}
}

func TestKubernetesReconcilerPublishesUnreachableAfterAPIFailure(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	now := time.Date(2026, time.July, 25, 13, 30, 0, 0, time.UTC)
	fixture.reconciler.factory = failingManagedKubernetesFactory{err: errors.New("API transport unavailable")}
	fixture.reconciler.now = func() time.Time { return now }
	publisher := NewManagedKubernetesRoutingPublisher(
		fixture.reconciler.targets,
		ManagedKubernetesRoutingPublisherConfig{
			PublisherIdentity: "managed-kubernetes-routing-test",
			ObservationTTL:    time.Minute,
		},
	)
	fixture.reconciler.config.PublishRoutingHealth = publisher.PublishReconcile

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expected Kubernetes API failure")
	}
	var health persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&health).Error; err != nil {
		t.Fatal(err)
	}
	if health.Status != routing.HealthUnreachable || health.CapacityStatus != routing.CapacityUnknown ||
		health.AvailableCapacityUnits != nil || health.AllocatedCapacityUnits != 0 ||
		health.Source != "managed-kubernetes-routing-test" || !health.ObservedAt.Equal(now) ||
		!health.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("unreachable routing health = %#v", health)
	}
}

func TestManagedKubernetesRoutingPublisherRejectsNonTenantTargets(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	publisher := NewManagedKubernetesRoutingPublisher(
		fixture.reconciler.targets,
		ManagedKubernetesRoutingPublisherConfig{PublisherIdentity: "managed-kubernetes-routing-test"},
	)

	err := publisher.PublishReconcile(context.Background(), ManagedKubernetesRoutingHealthObservation{
		ExecutionTargetID: fixture.targetID,
		Status:            routing.HealthHealthy,
		CapacityStatus:    routing.CapacityAvailable,
	})
	if err == nil {
		t.Fatal("expected publisher to reject non-tenant observation")
	}
}

func TestManagedKubernetesRoutingPublisherRejectsForgedTenantOwnedFlag(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	publisher := NewManagedKubernetesRoutingPublisher(
		fixture.reconciler.targets,
		ManagedKubernetesRoutingPublisherConfig{PublisherIdentity: "managed-kubernetes-routing-test"},
	)
	sharedTargetID := uuid.New()
	now := time.Now().UTC()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID:                     sharedTargetID,
		Kind:                   "kubernetes",
		Name:                   "shared-kubernetes-publisher-" + uuid.NewString(),
		Status:                 "active",
		ConfigurationEncrypted: []byte("operator-owned"),
		Capabilities:           map[string]any{},
		CreatedAt:              now,
		UpdatedAt:              now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	err := publisher.PublishReconcile(context.Background(), ManagedKubernetesRoutingHealthObservation{
		ExecutionTargetID: sharedTargetID,
		TenantOwned:       true,
		Status:            routing.HealthHealthy,
		CapacityStatus:    routing.CapacityAvailable,
	})
	if err == nil {
		t.Fatal("publisher trusted a forged tenant-owned observation for a shared target")
	}
}

func managedKubernetesRoutingTestIntPointer(value int) *int { return &value }
