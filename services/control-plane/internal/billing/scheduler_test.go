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
}
