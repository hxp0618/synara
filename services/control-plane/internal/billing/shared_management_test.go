package billing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSweepSharedUsageChargesPersistsSuccessAndReportsRetryableWorkers(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 2)
	fixture.seedClaim(t, fixture.tenantA, fixture.base.Add(10*time.Minute), fixture.base.Add(40*time.Minute), 1)
	fixture.seedClaim(t, fixture.tenantB, fixture.base.Add(70*time.Minute), fixture.base.Add(100*time.Minute), 2)

	input := SweepSharedUsageChargesInput{
		ExecutionTargetID:    fixture.target.ID,
		Provider:             "aws",
		CurrencyCode:         "usd",
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(2 * time.Hour),
	}
	first, err := fixture.service.SweepSharedUsageCharges(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != sharedAllocationSweepStatusComplete || first.WorkerCount != 1 ||
		first.AllocationRunCount != 1 || first.AllocationSliceCount == 0 || first.FailedWorkerCount != 0 {
		t.Fatalf("unexpected first shared sweep result: %#v", first)
	}

	replay, err := fixture.service.SweepSharedUsageCharges(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Status != sharedAllocationSweepStatusComplete || replay.WorkerCount != 1 ||
		replay.AllocationRunCount != 1 || replay.AllocationSliceCount != first.AllocationSliceCount {
		t.Fatalf("unexpected replayed shared sweep result: %#v", replay)
	}
	var runCount int64
	if err := fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Fatalf("shared allocation run count after replay = %d, want 1", runCount)
	}

	cpu := int64(500)
	memory := int64(1 << 30)
	ephemeral := int64(2 << 30)
	nonterminal := persistence.WorkerIncarnationFact{
		WorkerID: uuid.New(), WorkerIncarnation: 1,
		ExecutionTargetID: fixture.target.ID, TargetKind: fixture.target.Kind, WorkerMode: "general-pool",
		ClusterID: "cluster-a", Region: "us-east-1", Namespace: "default", PodName: "shared-running-worker",
		InstanceUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", RegisteredAt: fixture.base.Add(30 * time.Minute),
		CurrentState: "active", StateChangedAt: fixture.base.Add(30 * time.Minute), ClaimCount: 0,
		RequestedCPUMillicores: &cpu, RequestedMemoryBytes: &memory,
		RequestedEphemeralStorageBytes: &ephemeral,
		CreatedAt:                      fixture.base.Add(30 * time.Minute), UpdatedAt: fixture.base.Add(30 * time.Minute),
	}
	if err := fixture.db.Create(&nonterminal).Error; err != nil {
		t.Fatal(err)
	}

	partial, err := fixture.service.SweepSharedUsageCharges(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Status != sharedAllocationSweepStatusRetry || partial.WorkerCount != 2 ||
		partial.AllocationRunCount != 1 || partial.FailedWorkerCount != 1 || len(partial.Failures) != 1 {
		t.Fatalf("unexpected partial shared sweep result: %#v", partial)
	}
	if partial.Failures[0].WorkerID != nonterminal.WorkerID ||
		partial.Failures[0].ErrorCode != "billing_shared_worker_not_terminal" {
		t.Fatalf("unexpected partial shared sweep failure: %#v", partial.Failures[0])
	}
	if err := fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Fatalf("partial sweep rewrote successful allocation history: run count = %d", runCount)
	}
}

func TestSweepSharedUsageChargesRejectsOpenBillingPeriod(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 0)
	_, err := fixture.service.SweepSharedUsageCharges(context.Background(), SweepSharedUsageChargesInput{
		ExecutionTargetID:    fixture.target.ID,
		Provider:             "aws",
		CurrencyCode:         "USD",
		BillingPeriodStartAt: fixture.base.Add(2 * time.Hour),
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	assertProblemCode(t, err, "billing_shared_allocation_period_open")
}
