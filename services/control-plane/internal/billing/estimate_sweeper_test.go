package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestBuiltInEstimateSweeperIsTargetScopedAndIdempotent(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)

	otherTargetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: otherTargetID, TenantID: &fixture.tenantID, Kind: "kubernetes", Name: "other-billing-target",
		Status: "active", Capabilities: map[string]any{}, CreatedAt: fixture.base, UpdatedAt: fixture.base,
	}).Error; err != nil {
		t.Fatal(err)
	}
	otherWorker := insertBillingWorker(
		t, fixture.db, fixture.base, fixture.tenantID, otherTargetID,
		"cluster-b", "us-east-1", "default", "worker-b", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	)

	request := ScheduledEstimateSweepRequest{
		Import: persistence.BillingActualInvoiceImport{
			TenantID: fixture.tenantID, Provider: "aws", CurrencyCode: "USD",
			BillingPeriodStartAt: fixture.base, BillingPeriodEndAt: fixture.base.Add(4 * time.Hour),
		},
		TenantID: fixture.tenantID, Provider: "aws",
		ExecutionTargetIDs: []uuid.UUID{fixture.worker.ExecutionTargetID},
		RequestedBy:        "test",
	}
	first, err := fixture.service.SweepImportedInvoice(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkerCount != 1 || first.FailedWorkerCount != 0 || first.EstimateCount != 9 {
		t.Fatalf("first estimate sweep = %#v, want one worker and nine estimates", first)
	}
	var firstRowCount int64
	if err := fixture.db.Model(&persistence.BillingEstimatedUsageCharge{}).Count(&firstRowCount).Error; err != nil {
		t.Fatal(err)
	}

	replayed, err := fixture.service.SweepImportedInvoice(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first {
		t.Fatalf("replayed estimate sweep = %#v, want %#v", replayed, first)
	}
	var replayedRowCount int64
	if err := fixture.db.Model(&persistence.BillingEstimatedUsageCharge{}).Count(&replayedRowCount).Error; err != nil {
		t.Fatal(err)
	}
	if replayedRowCount != firstRowCount {
		t.Fatalf("idempotent sweep row count = %d, want %d", replayedRowCount, firstRowCount)
	}
	var otherWorkerEstimateCount int64
	if err := fixture.db.Model(&persistence.BillingEstimatedUsageCharge{}).
		Where("worker_id = ? AND worker_incarnation = ?", otherWorker.WorkerID, otherWorker.WorkerIncarnation).
		Count(&otherWorkerEstimateCount).Error; err != nil {
		t.Fatal(err)
	}
	if otherWorkerEstimateCount != 0 {
		t.Fatalf("unconfigured target worker received %d estimates", otherWorkerEstimateCount)
	}
}

func TestBuiltInEstimateSweeperRejectsSharedOrForeignTargets(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	sharedTargetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: sharedTargetID, Kind: "kubernetes", Name: "shared-billing-target", Status: "active",
		Capabilities: map[string]any{}, CreatedAt: fixture.base, UpdatedAt: fixture.base,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err := fixture.service.SweepImportedInvoice(context.Background(), ScheduledEstimateSweepRequest{
		Import: persistence.BillingActualInvoiceImport{
			TenantID: fixture.tenantID, Provider: "aws", CurrencyCode: "USD",
			BillingPeriodStartAt: fixture.base, BillingPeriodEndAt: fixture.base.Add(time.Hour),
		},
		TenantID: fixture.tenantID, Provider: "aws", ExecutionTargetIDs: []uuid.UUID{sharedTargetID},
	})
	assertProblemCode(t, err, "cost_accounting_estimate_sweep_target_scope_mismatch")
}

func TestBuiltInEstimateSweeperContinuesAfterRequestDeltaFailure(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)
	failingWorker := insertBillingWorker(
		t, fixture.db, fixture.base.Add(-time.Hour), fixture.tenantID, fixture.worker.ExecutionTargetID,
		"cluster-a", "us-east-1", "default", "worker-cross-period", "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	)

	result, err := fixture.service.SweepImportedInvoice(ctx, ScheduledEstimateSweepRequest{
		Import: persistence.BillingActualInvoiceImport{
			TenantID: fixture.tenantID, Provider: "aws", CurrencyCode: "USD",
			BillingPeriodStartAt: fixture.base, BillingPeriodEndAt: fixture.base.Add(4 * time.Hour),
		},
		TenantID: fixture.tenantID, Provider: "aws",
		ExecutionTargetIDs: []uuid.UUID{fixture.worker.ExecutionTargetID},
	})
	assertProblemCode(t, err, "cost_accounting_estimate_sweep_partial_failure")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != 409 ||
		apiError.Details["failedWorkerCount"] != 1 {
		t.Fatalf("partial sweep problem = %#v", apiError)
	}
	if result.WorkerCount != 2 || result.FailedWorkerCount != 1 || result.EstimateCount != 9 {
		t.Fatalf("partial estimate sweep = %#v", result)
	}
	var failingEstimateCount int64
	if err := fixture.db.Model(&persistence.BillingEstimatedUsageCharge{}).
		Where("worker_id = ? AND worker_incarnation = ?", failingWorker.WorkerID, failingWorker.WorkerIncarnation).
		Count(&failingEstimateCount).Error; err != nil {
		t.Fatal(err)
	}
	if failingEstimateCount != 0 {
		t.Fatalf("request-delta failure wrote %d partial estimates", failingEstimateCount)
	}
}

func TestImportSchedulerRetriesBuiltInEstimateSweepOnInvoiceReplay(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)
	externalImportID := "durable-estimate-retry"
	invoice := ImportedActualInvoice{
		ExternalImportID:     externalImportID,
		BillingPeriodStartAt: fixture.base, BillingPeriodEndAt: fixture.base.Add(4 * time.Hour),
		CurrencyCode: "USD",
	}
	adapter := &schedulerAdapter{invoices: map[string]ImportedActualInvoice{
		fixtureKey(fixture.tenantID, "aws", externalImportID): invoice,
	}}
	configuredImport := ConfiguredImport{
		TenantID: fixture.tenantID, Provider: "aws", ExternalImportID: externalImportID,
		Format: ExportObjectFormatAWSCURCSV, ObjectKey: "durable.csv", ScheduleInterval: time.Hour,
		EstimateAfterImport: true, ExecutionTargetIDs: []uuid.UUID{fixture.worker.ExecutionTargetID},
	}
	newService := func() *Service {
		service := NewService(
			fixture.db, adapter,
			WithConfiguredImports([]ConfiguredImport{configuredImport}),
			WithBuiltInEstimateSweeper(),
		)
		service.now = func() time.Time { return fixture.base.Add(5 * time.Hour) }
		return service
	}

	first, err := newService().RunImportSchedulerOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Imported != 1 || first.EstimateWorkers != 1 || first.EstimateSweeps != 9 || first.Failed != 0 {
		t.Fatalf("first scheduler sweep = %#v", first)
	}
	replayed, err := newService().RunImportSchedulerOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Imported != 0 || replayed.EstimateWorkers != 1 || replayed.EstimateSweeps != 9 || replayed.Failed != 0 {
		t.Fatalf("replayed scheduler sweep = %#v", replayed)
	}
	var importCount int64
	if err := fixture.db.Model(&persistence.BillingActualInvoiceImport{}).Count(&importCount).Error; err != nil {
		t.Fatal(err)
	}
	if importCount != 1 {
		t.Fatalf("invoice replay created %d imports", importCount)
	}
}
