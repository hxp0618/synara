package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestRunImportSchedulerOnceContinuesAfterFailuresAndScopesStatePerService(t *testing.T) {
	tenantID := uuid.New()
	targetID := uuid.New()
	fixture := newBillingFixture(t, nil)
	successRequest := ImportActualInvoiceRequest{TenantID: tenantID, Provider: "aws", ExternalImportID: "success"}
	manualEstimateRequest := ImportActualInvoiceRequest{TenantID: tenantID, Provider: "aws", ExternalImportID: "manual"}
	failingRequest := ImportActualInvoiceRequest{TenantID: tenantID, Provider: "aws", ExternalImportID: "missing"}

	successInvoice := ImportedActualInvoice{
		ExternalImportID:     "success",
		BillingPeriodStartAt: time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEndAt:   time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
		CurrencyCode:         "USD",
	}
	manualInvoice := successInvoice
	manualInvoice.ExternalImportID = "manual"

	adapter := &schedulerAdapter{
		invoices: map[string]ImportedActualInvoice{
			fixtureKey(successRequest.TenantID, successRequest.Provider, successRequest.ExternalImportID):                      successInvoice,
			fixtureKey(manualEstimateRequest.TenantID, manualEstimateRequest.Provider, manualEstimateRequest.ExternalImportID): manualInvoice,
		},
		errs: map[string]error{
			fixtureKey(failingRequest.TenantID, failingRequest.Provider, failingRequest.ExternalImportID): ErrInvoiceNotFound,
		},
	}
	sweeper := &schedulerEstimateSweeper{estimateCount: 7}
	service := NewService(
		fixture.db,
		adapter,
		WithConfiguredImports([]ConfiguredImport{
			{TenantID: tenantID, Provider: "aws", ExternalImportID: "success", Format: ExportObjectFormatAWSCURCSV, ObjectKey: "cur.csv", ExecutionTargetIDs: []uuid.UUID{targetID}, ScheduleInterval: time.Hour, EstimateAfterImport: true},
			{TenantID: tenantID, Provider: "aws", ExternalImportID: "manual", Format: ExportObjectFormatAWSCURCSV, ObjectKey: "manual.csv", ScheduleInterval: time.Hour},
			{TenantID: tenantID, Provider: "aws", ExternalImportID: "missing", Format: ExportObjectFormatAWSCURCSV, ObjectKey: "missing.csv", ScheduleInterval: time.Hour},
		}),
		WithEstimateSweeper(sweeper),
	)
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	summary, err := service.RunImportSchedulerOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "billing invoice import was not found") {
		t.Fatalf("scheduler error = %v, want billing invoice import not found", err)
	}
	if summary.Checked != 3 || summary.Imported != 2 || summary.Failed != 1 || summary.EstimateSweeps != 7 {
		t.Fatalf("unexpected scheduler summary: %#v", summary)
	}
	if sweeper.calls != 1 {
		t.Fatalf("estimate sweeper calls = %d, want 1", sweeper.calls)
	}
	assertScheduledAuditCount(t, fixture.db, 2)

	replayed, replayErr := service.RunImportSchedulerOnce(context.Background())
	if replayErr != nil {
		t.Fatalf("second scheduler run error = %v", replayErr)
	}
	if replayed.Skipped != 3 || replayed.Imported != 0 || replayed.Failed != 0 {
		t.Fatalf("unexpected second scheduler summary: %#v", replayed)
	}
	assertScheduledAuditCount(t, fixture.db, 2)

	secondService := NewService(
		fixture.db,
		adapter,
		WithConfiguredImports([]ConfiguredImport{
			{TenantID: tenantID, Provider: "aws", ExternalImportID: "success", Format: ExportObjectFormatAWSCURCSV, ObjectKey: "cur.csv", ScheduleInterval: time.Hour},
		}),
	)
	secondService.now = func() time.Time { return now }
	secondSummary, secondErr := secondService.RunImportSchedulerOnce(context.Background())
	if secondErr != nil {
		t.Fatalf("second service scheduler run error = %v", secondErr)
	}
	if secondSummary.Imported != 0 || secondSummary.Skipped != 0 {
		t.Fatalf("second service scheduler state leaked across instances: %#v", secondSummary)
	}
	assertScheduledAuditCount(t, fixture.db, 2)
}

func TestRunImportSchedulerOnceSkipsReconciliationAfterPartialEstimateFailure(t *testing.T) {
	tenantID := uuid.New()
	targetID := uuid.New()
	fixture := newBillingFixture(t, nil)
	periodStart := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	externalImportID := "partial-estimate"
	adapter := &schedulerAdapter{invoices: map[string]ImportedActualInvoice{
		fixtureKey(tenantID, "aws", externalImportID): {
			ExternalImportID:     externalImportID,
			BillingPeriodStartAt: periodStart, BillingPeriodEndAt: periodEnd,
			CurrencyCode: "USD",
			Lines: []ImportedActualInvoiceLine{{
				ExternalLineID: "cpu", ChargeKind: ChargeKindCPU,
				ResourceCorrelationKey: "aws:123456789012:us-east-1:i-partial",
				AmountMicros:           42,
			}},
		},
	}}
	sweeper := &schedulerEstimateSweeper{
		estimateCount: 3,
		err:           errors.New("one Worker estimate failed"),
	}
	service := NewService(
		fixture.db,
		adapter,
		WithConfiguredImports([]ConfiguredImport{{
			TenantID: tenantID, Provider: "aws", ExternalImportID: externalImportID,
			Format: ExportObjectFormatAWSCURCSV, ObjectKey: "partial.csv",
			ExecutionTargetIDs: []uuid.UUID{targetID}, ScheduleInterval: time.Hour,
			EstimateAfterImport: true, Reconcile: true,
		}}),
		WithEstimateSweeper(sweeper),
	)

	summary, err := service.RunImportSchedulerOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "one Worker estimate failed") {
		t.Fatalf("scheduler error = %v, want estimate failure", err)
	}
	if summary.Imported != 1 || summary.EstimateSweeps != 3 || summary.Reconciled != 0 || summary.Failed != 1 {
		t.Fatalf("scheduler summary after estimate failure = %#v", summary)
	}
	var line persistence.BillingActualInvoiceLine
	if err := fixture.db.Take(&line).Error; err != nil {
		t.Fatal(err)
	}
	if line.ReconciliationState != reconciliationStatePending || line.ReconciledAt != nil {
		t.Fatalf("invoice line was reconciled from a partial estimate set: %#v", line)
	}
	assertScheduledAuditCount(t, fixture.db, 1)
}

func TestRunSharedAllocationSchedulerOnceWaitsForSettlementAndReplaysOnInterval(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 2)
	fixture.seedClaim(t, fixture.tenantA, fixture.base.Add(10*time.Minute), fixture.base.Add(40*time.Minute), 1)
	fixture.seedClaim(t, fixture.tenantB, fixture.base.Add(70*time.Minute), fixture.base.Add(100*time.Minute), 2)
	if err := fixture.db.AutoMigrate(&persistence.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	job := ConfiguredSharedAllocation{
		ExecutionTargetID: fixture.target.ID, Provider: "aws", CurrencyCode: "USD",
		BillingPeriodStartAt: fixture.base, BillingPeriodEndAt: fixture.base.Add(2 * time.Hour),
		SettlementDelay: 90 * time.Minute, ScheduleInterval: time.Hour,
	}
	service := NewService(
		fixture.db,
		nil,
		WithConfiguredSharedAllocations([]ConfiguredSharedAllocation{job}),
		WithPlatformBillingOperatorTenant(fixture.tenantA),
	)
	now := fixture.base.Add(3 * time.Hour)
	service.now = func() time.Time { return now }

	notSettled, err := service.RunSharedAllocationSchedulerOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if notSettled.Checked != 1 || notSettled.NotSettled != 1 || notSettled.Attempted != 0 {
		t.Fatalf("unexpected pre-settlement scheduler summary: %#v", notSettled)
	}
	assertSharedScheduledAuditCount(t, fixture.db, 0)

	now = fixture.base.Add(4 * time.Hour)
	first, err := service.RunSharedAllocationSchedulerOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Checked != 1 || first.Attempted != 1 || first.Completed != 1 || first.Workers != 1 ||
		first.AllocationRuns != 1 || first.AllocationSlices == 0 || first.FailedWorkers != 0 || first.Failed != 0 {
		t.Fatalf("unexpected first shared scheduler summary: %#v", first)
	}
	assertSharedScheduledAuditCount(t, fixture.db, 1)

	immediate, err := service.RunSharedAllocationSchedulerOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if immediate.Checked != 1 || immediate.Skipped != 1 || immediate.Attempted != 0 {
		t.Fatalf("unexpected immediate shared scheduler summary: %#v", immediate)
	}
	assertSharedScheduledAuditCount(t, fixture.db, 1)

	now = now.Add(time.Hour)
	replayed, err := service.RunSharedAllocationSchedulerOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Completed != 1 || replayed.AllocationRuns != 1 || replayed.AllocationSlices != first.AllocationSlices {
		t.Fatalf("unexpected replayed shared scheduler summary: %#v", replayed)
	}
	assertSharedScheduledAuditCount(t, fixture.db, 2)
	var runCount int64
	if err := fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Fatalf("scheduled replay created %d shared allocation Runs, want 1", runCount)
	}
}

func TestRunSharedAllocationSchedulerOnceReportsPartialFailureAndRequiresOperator(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 0)
	if err := fixture.db.AutoMigrate(&persistence.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	cpu := int64(500)
	memory := int64(1 << 30)
	ephemeral := int64(2 << 30)
	nonterminal := persistence.WorkerIncarnationFact{
		WorkerID: uuid.New(), WorkerIncarnation: 1,
		ExecutionTargetID: fixture.target.ID, TargetKind: fixture.target.Kind, WorkerMode: "general-pool",
		ClusterID: "cluster-a", Region: "us-east-1", Namespace: "default", PodName: "scheduled-running-worker",
		InstanceUID: uuid.NewString(), RegisteredAt: fixture.base.Add(30 * time.Minute),
		CurrentState: "active", StateChangedAt: fixture.base.Add(30 * time.Minute), ClaimCount: 0,
		RequestedCPUMillicores: &cpu, RequestedMemoryBytes: &memory,
		RequestedEphemeralStorageBytes: &ephemeral,
		CreatedAt:                      fixture.base.Add(30 * time.Minute), UpdatedAt: fixture.base.Add(30 * time.Minute),
	}
	if err := fixture.db.Create(&nonterminal).Error; err != nil {
		t.Fatal(err)
	}
	job := ConfiguredSharedAllocation{
		ExecutionTargetID: fixture.target.ID, Provider: "aws", CurrencyCode: "USD",
		BillingPeriodStartAt: fixture.base, BillingPeriodEndAt: fixture.base.Add(2 * time.Hour),
		SettlementDelay: time.Minute, ScheduleInterval: time.Hour,
	}
	service := NewService(
		fixture.db,
		nil,
		WithConfiguredSharedAllocations([]ConfiguredSharedAllocation{job}),
		WithPlatformBillingOperatorTenant(fixture.tenantA),
	)
	service.now = func() time.Time { return fixture.base.Add(3 * time.Hour) }
	partial, err := service.RunSharedAllocationSchedulerOnce(context.Background())
	assertProblemCode(t, err, "billing_shared_allocation_sweep_partial_failure")
	if partial.Attempted != 1 || partial.Completed != 0 || partial.Failed != 1 || partial.Workers != 2 ||
		partial.AllocationRuns != 1 || partial.FailedWorkers != 1 {
		t.Fatalf("unexpected partial shared scheduler summary: %#v", partial)
	}
	assertSharedScheduledAuditCount(t, fixture.db, 1)

	withoutOperator := NewService(
		fixture.db,
		nil,
		WithConfiguredSharedAllocations([]ConfiguredSharedAllocation{job}),
	)
	withoutOperator.now = service.now
	missingOperator, err := withoutOperator.RunSharedAllocationSchedulerOnce(context.Background())
	assertProblemCode(t, err, "billing_shared_scheduler_operator_unavailable")
	if missingOperator.Attempted != 1 || missingOperator.Failed != 1 {
		t.Fatalf("unexpected missing-operator scheduler summary: %#v", missingOperator)
	}
	assertSharedScheduledAuditCount(t, fixture.db, 1)
}

type schedulerAdapter struct {
	invoices map[string]ImportedActualInvoice
	errs     map[string]error
}

func (a *schedulerAdapter) FetchActualInvoice(_ context.Context, request ImportActualInvoiceRequest) (ImportedActualInvoice, error) {
	key := fixtureKey(request.TenantID, request.Provider, request.ExternalImportID)
	if err, ok := a.errs[key]; ok {
		return ImportedActualInvoice{}, err
	}
	invoice, ok := a.invoices[key]
	if !ok {
		return ImportedActualInvoice{}, ErrInvoiceNotFound
	}
	return invoice, nil
}

type schedulerEstimateSweeper struct {
	calls         int
	estimateCount int
	err           error
}

func (s *schedulerEstimateSweeper) SweepImportedInvoice(
	_ context.Context,
	_ ScheduledEstimateSweepRequest,
) (ScheduledEstimateSweepResult, error) {
	s.calls++
	return ScheduledEstimateSweepResult{EstimateCount: s.estimateCount}, s.err
}

func assertScheduledAuditCount(t *testing.T, db *gorm.DB, want int) {
	t.Helper()
	var entries []persistence.AuditLog
	if err := db.Where("action IN ?", []string{
		"billing.invoice_import_scheduled",
		"billing.invoice_reconciled_scheduled",
	}).Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("scheduled audit count = %d, want %d", len(entries), want)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.RequestID, "billing-import-scheduler:") {
			t.Fatalf("scheduled audit request ID = %q, want billing scheduler correlation ID", entry.RequestID)
		}
	}
}

func assertSharedScheduledAuditCount(t *testing.T, db *gorm.DB, want int) {
	t.Helper()
	var entries []persistence.AuditLog
	if err := db.Where("action = ?", "billing.shared_cost_allocation_sweep_scheduled").Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("scheduled shared allocation audit count = %d, want %d", len(entries), want)
	}
	for _, entry := range entries {
		if entry.ActorType != "system" || entry.ActorID != nil ||
			!strings.HasPrefix(entry.RequestID, "billing-shared-allocation-scheduler:") {
			t.Fatalf("unexpected scheduled shared allocation audit: %#v", entry)
		}
	}
}
