package targetcapacity

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestObserveExecutionTargetCapacityAdvancesExactVectors(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "target-capacity-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.DB().Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "capacity", Status: "active",
		ConfigurationEncrypted: []byte("fixture"), Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB())
	cpuRequest, cpuTotal, cpuAllocated, cpuAvailable := int64(500), int64(4_000), int64(1_500), int64(2_500)
	memoryRequest, memoryTotal, memoryAllocated, memoryAvailable := int64(1<<30), int64(8<<30), int64(3<<30), int64(5<<30)
	gpuName := "nvidia.com/gpu"
	gpuRequest, gpuTotal, gpuAllocated, gpuAvailable := int64(1), int64(4), int64(1), int64(3)
	input := Observation{
		TenantID: domain.TenantID, ExecutionTargetID: targetID,
		TargetKind: "kubernetes", Source: "capacity-test",
		TotalPods: 10, AllocatedPods: 3, AvailablePods: 7, SchedulableUnits: 3,
		CPU:             Vector{Request: &cpuRequest, Total: &cpuTotal, Allocated: &cpuAllocated, Available: &cpuAvailable},
		Memory:          Vector{Request: &memoryRequest, Total: &memoryTotal, Allocated: &memoryAllocated, Available: &memoryAvailable},
		GPUResourceName: &gpuName,
		GPU:             Vector{Request: &gpuRequest, Total: &gpuTotal, Allocated: &gpuAllocated, Available: &gpuAvailable},
		ObservedAt:      now, TTL: time.Minute,
	}
	created, err := service.Observe(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || created.SchedulableUnits != 3 || created.AvailableCPUMillicores == nil ||
		*created.AvailableCPUMillicores != cpuAvailable || created.AvailableGPUUnits == nil || *created.AvailableGPUUnits != 3 {
		t.Fatalf("created target capacity = %#v", created)
	}
	input.ObservedAt = now.Add(time.Second)
	input.AllocatedPods = 4
	input.AvailablePods = 6
	updated, err := service.Observe(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.AvailablePods != 6 {
		t.Fatalf("updated target capacity = %#v", updated)
	}
	if _, err := service.Observe(ctx, input); err == nil {
		t.Fatal("stale target capacity observation was accepted")
	}
}
