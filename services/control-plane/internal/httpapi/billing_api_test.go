package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/billing"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestBillingTariffRoutesAuthorizeCreateAndList(t *testing.T) {
	fixture := newBillingHTTPFixture(t)
	basePath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/tariffs"
	firstStartAt := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	firstEndAt := firstStartAt.Add(time.Hour)
	firstBody := map[string]any{
		"provider": "aws", "region": "us-east-1", "currencyCode": "usd", "version": 1,
		"effectiveStartAt": firstStartAt, "effectiveEndAt": firstEndAt,
		"cpuCoreHourRateMicros": 3_600_000, "memoryGiBHourRateMicros": 1_000_000,
		"ephemeralGiBHourRateMicros": 500_000, "requestRateMicros": 300_000, "podHourRateMicros": 3_600_000,
	}
	var first billing.Tariff
	if recorder := fixture.request(t, http.MethodPost, basePath, fixture.ownerToken, firstBody); recorder.Code != http.StatusCreated {
		t.Fatalf("owner tariff create status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Provider != "aws" || first.CurrencyCode != "USD" || first.Version != 1 {
		t.Fatalf("unexpected first tariff response: %#v", first)
	}

	secondBody := map[string]any{
		"provider": "aws", "region": "us-east-1", "currencyCode": "USD", "version": 2,
		"effectiveStartAt": firstEndAt, "effectiveEndAt": firstStartAt.Add(4 * time.Hour),
		"cpuCoreHourRateMicros": 7_200_000, "memoryGiBHourRateMicros": 2_000_000,
		"ephemeralGiBHourRateMicros": 1_000_000, "requestRateMicros": 300_000, "podHourRateMicros": 7_200_000,
	}
	if recorder := fixture.request(t, http.MethodPost, basePath, fixture.billingAdminToken, secondBody); recorder.Code != http.StatusCreated {
		t.Fatalf("billing admin tariff create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var all struct {
		Items []billing.Tariff `json:"items"`
	}
	listPath := basePath + "?provider=aws&currencyCode=usd"
	if recorder := fixture.request(t, http.MethodGet, listPath, fixture.billingAdminToken, nil); recorder.Code != http.StatusOK {
		t.Fatalf("billing admin tariff list status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 2 || all.Items[0].Version != 1 || all.Items[1].Version != 2 {
		t.Fatalf("unexpected tariff list response: %#v", all.Items)
	}

	var filtered struct {
		Items []billing.Tariff `json:"items"`
	}
	filterPath := basePath + "?provider=aws&currencyCode=USD&effectiveAt=" + url.QueryEscape(firstStartAt.Add(30*time.Minute).Format(time.RFC3339Nano))
	if recorder := fixture.request(t, http.MethodGet, filterPath, fixture.ownerToken, nil); recorder.Code != http.StatusOK {
		t.Fatalf("owner filtered tariff list status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Version != 1 {
		t.Fatalf("unexpected filtered tariff list response: %#v", filtered.Items)
	}

	assertProblemResponse(t, fixture.request(t, http.MethodPost, basePath, fixture.securityAdminToken, firstBody), http.StatusForbidden, "tenant_forbidden")
	assertProblemResponse(t, fixture.request(t, http.MethodGet, listPath, fixture.memberToken, nil), http.StatusForbidden, "tenant_forbidden")
	assertProblemResponse(t, fixture.request(t, http.MethodGet, listPath, fixture.crossTenantToken, nil), http.StatusNotFound, "tenant_not_found")
	assertProblemResponse(
		t,
		fixture.request(
			t,
			http.MethodPost,
			"/v1/tenants/"+fixture.otherTenantID.String()+"/billing/tariffs",
			fixture.crossTenantToken,
			firstBody,
		),
		http.StatusForbidden,
		"billing_tariff_operator_forbidden",
	)

	actions := fixture.auditActions(t)
	if len(actions) != 2 || actions[0] != "billing.tariff_created" || actions[1] != "billing.tariff_created" {
		t.Fatalf("unexpected billing tariff audit actions: %v", actions)
	}
}

func TestBillingTariffCreateFailsClosedWithoutConfiguredOperatorTenant(t *testing.T) {
	fixture := newBillingHTTPFixtureWithOptions(t, billing.WithTariffOperatorTenant(uuid.Nil))
	path := "/v1/tenants/" + fixture.tenantID.String() + "/billing/tariffs"
	assertProblemResponse(
		t,
		fixture.request(t, http.MethodPost, path, fixture.ownerToken, map[string]any{}),
		http.StatusServiceUnavailable,
		"billing_tariff_management_unavailable",
	)
}

func TestBillingSharedTargetCoverageAndSweepRoutesAreOperatorScopedAndReplaySafe(t *testing.T) {
	fixture := newBillingHTTPFixture(t)
	periodStart := time.Date(2020, time.July, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.Add(time.Hour)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "billing-shared-http", Status: "active",
		Capabilities: map[string]any{}, CreatedAt: periodStart.Add(-time.Hour), UpdatedAt: periodStart.Add(-time.Hour),
	}
	if err := fixture.db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	coveragePath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/shared-targets/" +
		target.ID.String() + "/ledger-coverage"
	coverageBody := map[string]any{
		"completeFromAt":              periodStart.Add(-time.Minute),
		"minimumWriterVersion":        "stage4-http-v1",
		"deploymentAttestationSHA256": strings.Repeat("a", 64),
	}

	assertProblemResponse(
		t,
		fixture.request(t, http.MethodGet, coveragePath, fixture.ownerToken, nil),
		http.StatusNotFound,
		"billing_shared_ledger_coverage_not_found",
	)
	var sealed billing.SharedTargetLedgerCoverage
	if recorder := fixture.request(t, http.MethodPost, coveragePath, fixture.ownerToken, coverageBody); recorder.Code != http.StatusCreated {
		t.Fatalf("shared coverage seal status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &sealed); err != nil {
		t.Fatal(err)
	}
	if sealed.ExecutionTargetID != target.ID || sealed.MinimumWriterVersion != "stage4-http-v1" || sealed.SealedBy == uuid.Nil {
		t.Fatalf("unexpected sealed shared coverage: %#v", sealed)
	}
	if recorder := fixture.request(t, http.MethodPost, coveragePath, fixture.billingAdminToken, coverageBody); recorder.Code != http.StatusOK {
		t.Fatalf("shared coverage exact replay status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var loaded billing.SharedTargetLedgerCoverage
	if recorder := fixture.request(t, http.MethodGet, coveragePath, fixture.billingAdminToken, nil); recorder.Code != http.StatusOK {
		t.Fatalf("shared coverage get status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.ID != sealed.ID || loaded.SealedBy != sealed.SealedBy {
		t.Fatalf("shared coverage replay changed authority: sealed=%#v loaded=%#v", sealed, loaded)
	}

	conflictingBody := maps.Clone(coverageBody)
	conflictingBody["minimumWriterVersion"] = "stage4-http-v2"
	assertProblemResponse(
		t,
		fixture.request(t, http.MethodPost, coveragePath, fixture.ownerToken, conflictingBody),
		http.StatusConflict,
		"billing_shared_ledger_coverage_conflict",
	)
	assertProblemResponse(
		t,
		fixture.request(t, http.MethodPost, coveragePath, fixture.securityAdminToken, coverageBody),
		http.StatusForbidden,
		"tenant_forbidden",
	)
	otherTenantPath := "/v1/tenants/" + fixture.otherTenantID.String() + "/billing/shared-targets/" +
		target.ID.String() + "/ledger-coverage"
	assertProblemResponse(
		t,
		fixture.request(t, http.MethodGet, otherTenantPath, fixture.crossTenantToken, nil),
		http.StatusForbidden,
		"billing_shared_operator_forbidden",
	)

	sweepPath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/shared-targets/" +
		target.ID.String() + "/allocations:sweep"
	sweepBody := map[string]any{
		"provider": "aws", "currencyCode": "usd",
		"billingPeriodStartAt": periodStart, "billingPeriodEndAt": periodEnd,
	}
	var empty billing.SharedUsageAllocationSweepResult
	if recorder := fixture.request(t, http.MethodPost, sweepPath, fixture.ownerToken, sweepBody); recorder.Code != http.StatusOK {
		t.Fatalf("empty shared allocation sweep status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Status != "completed" || empty.WorkerCount != 0 || empty.FailedWorkerCount != 0 {
		t.Fatalf("unexpected empty shared allocation sweep: %#v", empty)
	}

	var replayedSweep billing.SharedUsageAllocationSweepResult
	if recorder := fixture.request(t, http.MethodPost, sweepPath, fixture.billingAdminToken, sweepBody); recorder.Code != http.StatusOK {
		t.Fatalf("replayed empty shared allocation sweep status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &replayedSweep); err != nil {
		t.Fatal(err)
	}
	if replayedSweep.Status != "completed" || replayedSweep.WorkerCount != 0 || replayedSweep.FailedWorkerCount != 0 {
		t.Fatalf("unexpected replayed empty shared allocation sweep: %#v", replayedSweep)
	}

	actions := fixture.auditActions(t)
	wantActions := []string{
		"billing.shared_cost_allocation_sweep_requested",
		"billing.shared_cost_allocation_sweep_requested",
		"billing.shared_target_ledger_coverage_sealed",
	}
	if !slices.Equal(actions, wantActions) {
		t.Fatalf("unexpected shared billing audit actions: got=%v want=%v", actions, wantActions)
	}
}

func TestBillingSharedTargetManagementFailsClosedWithoutConfiguredOperatorTenant(t *testing.T) {
	fixture := newBillingHTTPFixtureWithOptions(t, billing.WithTariffOperatorTenant(uuid.Nil))
	path := "/v1/tenants/" + fixture.tenantID.String() + "/billing/shared-targets/" +
		uuid.NewString() + "/ledger-coverage"
	assertProblemResponse(
		t,
		fixture.request(t, http.MethodPost, path, fixture.ownerToken, map[string]any{
			"completeFromAt":              time.Date(2020, time.July, 1, 0, 0, 0, 0, time.UTC),
			"minimumWriterVersion":        "stage4-http-v1",
			"deploymentAttestationSHA256": strings.Repeat("a", 64),
		}),
		http.StatusServiceUnavailable,
		"billing_shared_management_unavailable",
	)
}

func TestBillingTariffCreateDrivesEstimateImportAndReconcileFlow(t *testing.T) {
	fixture := newBillingHTTPFixture(t)
	basePath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/tariffs"
	periodStart := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	periodEnd := periodStart.Add(4 * time.Hour)
	targetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &fixture.tenantID, Kind: "kubernetes", Name: "billing-http-target",
		Status: "active", Capabilities: map[string]any{}, CreatedAt: periodStart, UpdatedAt: periodStart,
	}).Error; err != nil {
		t.Fatal(err)
	}
	worker := insertBillingHTTPWorker(t, fixture.db, periodStart, fixture.tenantID, targetID)

	var first, second billing.Tariff
	for _, request := range []struct {
		body  map[string]any
		token string
		out   *billing.Tariff
	}{
		{
			token: fixture.ownerToken,
			out:   &first,
			body: map[string]any{
				"provider": "aws", "region": "us-east-1", "currencyCode": "USD", "version": 1,
				"effectiveStartAt": periodStart, "effectiveEndAt": periodStart.Add(time.Hour),
				"cpuCoreHourRateMicros": 3_600_000, "memoryGiBHourRateMicros": 1_000_000,
				"ephemeralGiBHourRateMicros": 500_000, "requestRateMicros": 300_000, "podHourRateMicros": 3_600_000,
			},
		},
		{
			token: fixture.billingAdminToken,
			out:   &second,
			body: map[string]any{
				"provider": "aws", "region": "us-east-1", "currencyCode": "USD", "version": 2,
				"effectiveStartAt": periodStart.Add(time.Hour), "effectiveEndAt": periodEnd,
				"cpuCoreHourRateMicros": 7_200_000, "memoryGiBHourRateMicros": 2_000_000,
				"ephemeralGiBHourRateMicros": 1_000_000, "requestRateMicros": 300_000, "podHourRateMicros": 7_200_000,
			},
		},
	} {
		recorder := fixture.request(t, http.MethodPost, basePath, request.token, request.body)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("tariff create status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), request.out); err != nil {
			t.Fatal(err)
		}
	}

	estimated, err := fixture.billingService.EstimateUsageCharges(context.Background(), billing.EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID: worker.WorkerID, WorkerIncarnation: worker.WorkerIncarnation,
		BillingPeriodStartAt: periodStart, BillingPeriodEndAt: periodEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(estimated) != 9 {
		t.Fatalf("estimate row count = %d, want 9", len(estimated))
	}
	tariffIDs := make(map[uuid.UUID]struct{})
	for _, charge := range estimated {
		tariffIDs[charge.TariffID] = struct{}{}
	}
	if len(tariffIDs) != 2 {
		t.Fatalf("estimated tariff ids = %v, want 2", tariffIDs)
	}
	if _, ok := tariffIDs[first.ID]; !ok {
		t.Fatalf("first tariff id %s missing from estimates", first.ID)
	}
	if _, ok := tariffIDs[second.ID]; !ok {
		t.Fatalf("second tariff id %s missing from estimates", second.ID)
	}

	resourceKey := billingHTTPWorkerResourceKey(worker)
	fixture.adapter.setInvoice(billing.FixtureInvoice{
		TenantID: fixture.tenantID,
		Provider: "aws",
		Invoice: billing.ImportedActualInvoice{
			ExternalImportID:     fixture.externalImportID,
			BillingPeriodStartAt: periodStart,
			BillingPeriodEndAt:   periodEnd,
			CurrencyCode:         "USD",
			Lines:                billingHTTPInvoiceLines(resourceKey, billingHTTPTotalsByKind(estimated), periodStart, periodEnd),
		},
	})

	importPath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/imports/aws/" + fixture.externalImportID
	var imported billing.ImportActualInvoiceResult
	if recorder := fixture.request(t, http.MethodPost, importPath, fixture.ownerToken, nil); recorder.Code != http.StatusOK {
		t.Fatalf("tariff-driven import status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}

	reconcilePath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/imports/" + imported.Import.ID.String() + "/reconcile"
	var report billing.ReconciliationReport
	if recorder := fixture.request(t, http.MethodPost, reconcilePath, fixture.billingAdminToken, nil); recorder.Code != http.StatusOK {
		t.Fatalf("tariff-driven reconcile status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Lines) != 5 {
		t.Fatalf("reconciled line count = %d, want 5", len(report.Lines))
	}
	if len(report.UnmatchedEstimateIDs) != 0 || report.UnmatchedEstimateAmountMicros != 0 {
		t.Fatalf("unexpected unmatched estimates: ids=%v amount=%d", report.UnmatchedEstimateIDs, report.UnmatchedEstimateAmountMicros)
	}
	cpuLine := findBillingHTTPLineByKind(t, report, billing.ChargeKindCPU)
	if len(cpuLine.MatchedEstimateIDs) != 2 {
		t.Fatalf("cpu matched estimate ids = %v, want 2 tariff-split rows", cpuLine.MatchedEstimateIDs)
	}
}

func TestBillingImportAndReconcileRoutesAuthorizeAndAudit(t *testing.T) {
	fixture := newBillingHTTPFixture(t)
	importPath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/imports/aws/" + fixture.externalImportID

	var imported billing.ImportActualInvoiceResult
	if recorder := fixture.request(t, http.MethodPost, importPath, fixture.ownerToken, nil); recorder.Code != http.StatusOK {
		t.Fatalf("owner import status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Import.ExternalImportID != fixture.externalImportID || len(imported.Lines) != 1 {
		t.Fatalf("unexpected imported invoice response: %#v", imported)
	}

	reconcilePath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/imports/" + imported.Import.ID.String() + "/reconcile"
	var report billing.ReconciliationReport
	if recorder := fixture.request(t, http.MethodPost, reconcilePath, fixture.billingAdminToken, nil); recorder.Code != http.StatusOK {
		t.Fatalf("billing admin reconcile status = %d, body = %s", recorder.Code, recorder.Body.String())
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Import.ID != imported.Import.ID || len(report.Lines) != 1 {
		t.Fatalf("unexpected reconciliation report: %#v", report)
	}

	assertProblemResponse(t, fixture.request(
		t,
		http.MethodPost,
		"/v1/tenants/"+fixture.tenantID.String()+"/billing/imports/aws/not-configured",
		fixture.ownerToken,
		nil,
	), http.StatusNotFound, "billing_import_not_configured")
	assertProblemResponse(t, fixture.request(t, http.MethodPost, importPath, fixture.securityAdminToken, nil), http.StatusForbidden, "tenant_forbidden")
	assertProblemResponse(t, fixture.request(t, http.MethodPost, importPath, fixture.memberToken, nil), http.StatusForbidden, "tenant_forbidden")
	assertProblemResponse(t, fixture.request(t, http.MethodPost, importPath, fixture.crossTenantToken, nil), http.StatusNotFound, "tenant_not_found")

	actions := fixture.auditActions(t)
	if len(actions) != 2 || actions[0] != "billing.invoice_import_triggered" || actions[1] != "billing.invoice_reconciled" {
		t.Fatalf("unexpected billing import audit actions: %v", actions)
	}
}

func TestBillingImportRouteRunsConfiguredEstimateSweepOnEveryReplay(t *testing.T) {
	fixture := newBillingHTTPFixture(t)
	targetID := uuid.New()
	sweeper := &billingHTTPRecordingSweeper{}
	billing.WithConfiguredImports([]billing.ConfiguredImport{{
		TenantID: fixture.tenantID, Provider: "aws", ExternalImportID: fixture.externalImportID,
		Format: billing.ExportObjectFormatAWSCURCSV, ObjectKey: "billing/aws/2026-07.csv",
		ExecutionTargetIDs: []uuid.UUID{targetID}, EstimateAfterImport: true,
	}})(fixture.billingService)
	billing.WithEstimateSweeper(sweeper)(fixture.billingService)
	importPath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/imports/aws/" + fixture.externalImportID

	for attempt := 1; attempt <= 2; attempt++ {
		if recorder := fixture.request(t, http.MethodPost, importPath, fixture.ownerToken, nil); recorder.Code != http.StatusOK {
			t.Fatalf("import attempt %d status = %d, body = %s", attempt, recorder.Code, recorder.Body.String())
		}
	}
	if len(sweeper.requests) != 2 {
		t.Fatalf("manual import estimate sweep calls = %d, want 2", len(sweeper.requests))
	}
	for _, request := range sweeper.requests {
		if request.TenantID != fixture.tenantID || request.Provider != "aws" ||
			len(request.ExecutionTargetIDs) != 1 || request.ExecutionTargetIDs[0] != targetID ||
			request.Import.ExternalImportID != fixture.externalImportID || request.RequestedBy == "" {
			t.Fatalf("unexpected manual import estimate sweep request: %#v", request)
		}
	}
}

func TestBillingImportRouteReturnsCommittedImportWhenEstimateSweepNeedsRetry(t *testing.T) {
	fixture := newBillingHTTPFixture(t)
	targetID := uuid.New()
	sweeper := &billingHTTPRecordingSweeper{err: errors.New("estimate backend unavailable")}
	billing.WithConfiguredImports([]billing.ConfiguredImport{{
		TenantID: fixture.tenantID, Provider: "aws", ExternalImportID: fixture.externalImportID,
		Format: billing.ExportObjectFormatAWSCURCSV, ObjectKey: "billing/aws/2026-07.csv",
		ExecutionTargetIDs: []uuid.UUID{targetID}, EstimateAfterImport: true,
	}})(fixture.billingService)
	billing.WithEstimateSweeper(sweeper)(fixture.billingService)
	importPath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/imports/aws/" + fixture.externalImportID

	recorder := fixture.request(t, http.MethodPost, importPath, fixture.ownerToken, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("post-commit sweep failure status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var result billing.ConfiguredInvoiceImportResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Import.ID == uuid.Nil || result.EstimateSweep == nil ||
		result.EstimateSweep.Status != "retry-required" ||
		result.EstimateSweep.ErrorCode != "billing_estimate_sweep_failed" {
		t.Fatalf("post-commit sweep response = %#v", result)
	}
	var importCount int64
	if err := fixture.db.Model(&persistence.BillingActualInvoiceImport{}).Count(&importCount).Error; err != nil {
		t.Fatal(err)
	}
	if importCount != 1 {
		t.Fatalf("committed invoice import count = %d, want 1", importCount)
	}
}

func TestBillingImportAuditFailureRollsBackMutation(t *testing.T) {
	fixture := newBillingHTTPFixtureWithOptions(t, billing.WithAuditRecorder(func(context.Context, *gorm.DB, audit.Entry) error {
		return errors.New("audit unavailable")
	}))
	importPath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/imports/aws/" + fixture.externalImportID

	assertProblemResponse(t, fixture.request(t, http.MethodPost, importPath, fixture.ownerToken, nil), http.StatusInternalServerError, "internal_error")

	var importCount int64
	if err := fixture.db.Model(&persistence.BillingActualInvoiceImport{}).Count(&importCount).Error; err != nil {
		t.Fatal(err)
	}
	if importCount != 0 {
		t.Fatalf("billing invoice import count = %d, want 0 after audit failure", importCount)
	}

	var lineCount int64
	if err := fixture.db.Model(&persistence.BillingActualInvoiceLine{}).Count(&lineCount).Error; err != nil {
		t.Fatal(err)
	}
	if lineCount != 0 {
		t.Fatalf("billing invoice line count = %d, want 0 after audit failure", lineCount)
	}

	if actions := fixture.auditActions(t); len(actions) != 0 {
		t.Fatalf("unexpected audit entries after audit failure rollback: %v", actions)
	}
}

func TestBillingRoutesReturnBillingUnavailableWhenServiceIsNotConfigured(t *testing.T) {
	fixture := newBillingHTTPFixtureWithoutBilling(t)
	tariffPath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/tariffs"
	importPath := "/v1/tenants/" + fixture.tenantID.String() + "/billing/imports/aws/" + fixture.externalImportID

	assertProblemResponse(t, fixture.request(t, http.MethodGet, tariffPath, fixture.ownerToken, nil), http.StatusServiceUnavailable, "billing_unavailable")
	assertProblemResponse(
		t,
		fixture.request(t, http.MethodPost, tariffPath, fixture.ownerToken, map[string]any{"provider": "aws"}),
		http.StatusServiceUnavailable,
		"billing_unavailable",
	)
	assertProblemResponse(t, fixture.request(t, http.MethodPost, importPath, fixture.ownerToken, nil), http.StatusServiceUnavailable, "billing_unavailable")
	assertProblemResponse(
		t,
		fixture.request(t, http.MethodPost, "/v1/tenants/"+fixture.tenantID.String()+"/billing/imports/"+uuid.NewString()+"/reconcile", fixture.ownerToken, nil),
		http.StatusServiceUnavailable,
		"billing_unavailable",
	)
}

type billingHTTPFixture struct {
	db                 *gorm.DB
	handler            http.Handler
	adapter            *billingHTTPMutableAdapter
	billingService     *billing.Service
	cookieName         string
	tenantID           uuid.UUID
	otherTenantID      uuid.UUID
	ownerToken         string
	billingAdminToken  string
	securityAdminToken string
	memberToken        string
	crossTenantToken   string
	externalImportID   string
}

func newBillingHTTPFixture(t *testing.T) billingHTTPFixture {
	t.Helper()
	return newBillingHTTPFixtureWithOptions(t)
}

func newBillingHTTPFixtureWithOptions(t *testing.T, options ...billing.ServiceOption) billingHTTPFixture {
	t.Helper()
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "billing-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	ownerToken := createWorkerManifestHTTPLogin(t, store.DB(), domain.UserID, domain.TenantID)
	billingAdminToken := createBillingHTTPUser(t, store.DB(), domain.TenantID, "billing_admin", now)
	securityAdminToken := createBillingHTTPUser(t, store.DB(), domain.TenantID, "security_admin", now)
	memberToken := createBillingHTTPUser(t, store.DB(), domain.TenantID, "member", now)

	otherTenantID := uuid.New()
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: otherTenantID, Slug: "billing-http-other-" + uuid.NewString(), Name: "Other tenant",
			Status: "active", PlanCode: "developer", Region: "local", Settings: map[string]any{},
			CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: otherTenantID, UserID: domain.UserID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	crossTenantToken := createWorkerManifestHTTPLogin(t, store.DB(), domain.UserID, otherTenantID)

	externalImportID := "aws-2026-07"
	adapter := newBillingHTTPMutableAdapter(billing.FixtureInvoice{
		TenantID: domain.TenantID,
		Provider: "aws",
		Invoice: billing.ImportedActualInvoice{
			ExternalImportID:     externalImportID,
			BillingPeriodStartAt: time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			BillingPeriodEndAt:   time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
			CurrencyCode:         "USD",
			Lines: []billing.ImportedActualInvoiceLine{
				{
					ExternalLineID:         "line-1",
					ChargeKind:             billing.ChargeKindCPU,
					ResourceCorrelationKey: "aws:123456789012:us-east-1:i-123456",
					BillingPeriodStartAt:   time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
					BillingPeriodEndAt:     time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
					CurrencyCode:           "USD",
					AmountMicros:           4200000,
				},
			},
		},
	})
	billingService := billing.NewService(
		store.DB(),
		adapter,
		billing.WithConfiguredImports([]billing.ConfiguredImport{
			{
				TenantID:         domain.TenantID,
				Provider:         "aws",
				ExternalImportID: externalImportID,
				Format:           billing.ExportObjectFormatAWSCURCSV,
				ObjectKey:        "billing/aws/2026-07.csv",
			},
		}),
		billing.WithPlatformBillingOperatorTenant(domain.TenantID),
	)
	for _, option := range options {
		if option != nil {
			option(billingService)
		}
	}

	cfg := config.Config{
		Platform: profile, CookieName: "synara_billing_session", CookiePath: "/",
		SessionTTL: time.Hour, SessionIdleTTL: time.Hour,
		SSEPollInterval: 20 * time.Millisecond, SSEHeartbeatInterval: time.Second,
		SSEWriteTimeout: time.Second, SSELeaseTTL: time.Minute,
		SSEMaxConnectionsPerUser: 1, SSEMaxConnectionsPerTenant: 10,
	}
	identityService := identity.NewService(store.DB(), cfg.SessionTTL, cfg.SessionIdleTTL)
	projectService := projects.NewService(store.DB())
	targetService := executiontargets.NewService(store.DB(), profile, nil)
	sessionService := sessions.NewService(store.DB(), projectService, targetService)
	executionService := executions.NewService(
		store.DB(), sessionService, time.Minute, 2*time.Minute, time.Hour, nil, targetService,
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(
		cfg, store.DB(), identityService, nil, projectService, sessionService, executionService, targetService,
		nil, nil, nil, nil, nil, observability.New(store.DB(), observability.Config{SessionIdleTTL: cfg.SessionIdleTTL}), nil, nil, nil, nil, nil, logger,
		WithBilling(billingService),
	)
	if err != nil {
		t.Fatal(err)
	}

	return billingHTTPFixture{
		db:                 store.DB(),
		handler:            server.Handler(),
		adapter:            adapter,
		billingService:     billingService,
		cookieName:         cfg.CookieName,
		tenantID:           domain.TenantID,
		otherTenantID:      otherTenantID,
		ownerToken:         ownerToken,
		billingAdminToken:  billingAdminToken,
		securityAdminToken: securityAdminToken,
		memberToken:        memberToken,
		crossTenantToken:   crossTenantToken,
		externalImportID:   externalImportID,
	}
}

type billingHTTPRecordingSweeper struct {
	requests []billing.ScheduledEstimateSweepRequest
	err      error
}

func (s *billingHTTPRecordingSweeper) SweepImportedInvoice(
	_ context.Context,
	request billing.ScheduledEstimateSweepRequest,
) (billing.ScheduledEstimateSweepResult, error) {
	s.requests = append(s.requests, request)
	return billing.ScheduledEstimateSweepResult{}, s.err
}

func newBillingHTTPFixtureWithoutBilling(t *testing.T) billingHTTPFixture {
	t.Helper()
	fixture := newBillingHTTPFixture(t)
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Platform: profile, CookieName: fixture.cookieName, CookiePath: "/",
		SessionTTL: time.Hour, SessionIdleTTL: time.Hour,
		SSEPollInterval: 20 * time.Millisecond, SSEHeartbeatInterval: time.Second,
		SSEWriteTimeout: time.Second, SSELeaseTTL: time.Minute,
		SSEMaxConnectionsPerUser: 1, SSEMaxConnectionsPerTenant: 10,
	}
	identityService := identity.NewService(fixture.db, cfg.SessionTTL, cfg.SessionIdleTTL)
	projectService := projects.NewService(fixture.db)
	targetService := executiontargets.NewService(fixture.db, profile, nil)
	sessionService := sessions.NewService(fixture.db, projectService, targetService)
	executionService := executions.NewService(
		fixture.db, sessionService, time.Minute, 2*time.Minute, time.Hour, nil, targetService,
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(
		cfg, fixture.db, identityService, nil, projectService, sessionService, executionService, targetService,
		nil, nil, nil, nil, nil, observability.New(fixture.db, observability.Config{SessionIdleTTL: cfg.SessionIdleTTL}), nil, nil, nil, nil, nil, logger,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler = server.Handler()
	return fixture
}

func (f billingHTTPFixture) request(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = encoded
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if len(payload) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

type billingHTTPMutableAdapter struct {
	invoices map[string]billing.ImportedActualInvoice
}

func newBillingHTTPMutableAdapter(items ...billing.FixtureInvoice) *billingHTTPMutableAdapter {
	adapter := &billingHTTPMutableAdapter{invoices: make(map[string]billing.ImportedActualInvoice, len(items))}
	for _, item := range items {
		adapter.setInvoice(item)
	}
	return adapter
}

func (a *billingHTTPMutableAdapter) setInvoice(item billing.FixtureInvoice) {
	if a == nil {
		return
	}
	a.invoices[billingHTTPInvoiceKey(item.TenantID, item.Provider, item.Invoice.ExternalImportID)] = item.Invoice
}

func (a *billingHTTPMutableAdapter) FetchActualInvoice(_ context.Context, request billing.ImportActualInvoiceRequest) (billing.ImportedActualInvoice, error) {
	if a == nil {
		return billing.ImportedActualInvoice{}, billing.ErrInvoiceNotFound
	}
	item, ok := a.invoices[billingHTTPInvoiceKey(request.TenantID, request.Provider, request.ExternalImportID)]
	if !ok {
		return billing.ImportedActualInvoice{}, billing.ErrInvoiceNotFound
	}
	return item, nil
}

func (f billingHTTPFixture) auditActions(t *testing.T) []string {
	t.Helper()
	var entries []persistence.AuditLog
	if err := f.db.Where("tenant_id = ? AND action LIKE ?", f.tenantID, "billing.%").Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	sort.Strings(actions)
	return actions
}

func createBillingHTTPUser(t *testing.T, db *gorm.DB, tenantID uuid.UUID, role string, now time.Time) string {
	t.Helper()
	userID := uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Billing " + role,
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: role, Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	return createWorkerManifestHTTPLogin(t, db, userID, tenantID)
}

func billingHTTPInvoiceKey(tenantID uuid.UUID, provider, externalImportID string) string {
	return tenantID.String() + "\x00" + provider + "\x00" + externalImportID
}

func insertBillingHTTPWorker(
	t *testing.T,
	db *gorm.DB,
	base time.Time,
	tenantID uuid.UUID,
	targetID uuid.UUID,
) persistence.WorkerIncarnationFact {
	t.Helper()
	cpu := int64(1000)
	memory := int64(2 * 1024 * 1024 * 1024)
	ephemeral := int64(4 * 1024 * 1024 * 1024)
	terminatedAt := base.Add(2 * time.Hour)
	workerID := uuid.New()
	instanceUID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	worker := persistence.WorkerInstance{
		ID:                    workerID,
		Incarnation:           1,
		InstanceUID:           instanceUID,
		ExecutionTargetID:     targetID,
		TargetKind:            "kubernetes",
		WorkerMode:            "general-pool",
		RegistrationTrustMode: "shared-token",
		ClusterID:             "cluster-a",
		Namespace:             "default",
		PodName:               "worker-a",
		Version:               "legacy",
		ProtocolVersion:       2,
		Capabilities:          map[string]any{},
		CompatibilityStatus:   "unknown",
		WorkerReleaseStatus:   "unmanaged",
		LeaseSupported:        true,
		FencingSupported:      true,
		AuthTokenHash:         []byte(uuid.NewString()),
		Status:                "terminated",
		AdministrativeStatus:  "active",
		RegisteredAt:          base,
		LastHeartbeatAt:       terminatedAt,
		TerminatedAt:          &terminatedAt,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	terminalReason := "completed"
	fact := persistence.WorkerIncarnationFact{
		WorkerID:                       workerID,
		WorkerIncarnation:              1,
		TenantID:                       &tenantID,
		ExecutionTargetID:              targetID,
		TargetKind:                     "kubernetes",
		WorkerMode:                     "general-pool",
		ClusterID:                      worker.ClusterID,
		Region:                         "us-east-1",
		Namespace:                      worker.Namespace,
		PodName:                        worker.PodName,
		InstanceUID:                    instanceUID,
		RegisteredAt:                   base,
		CurrentState:                   "terminated",
		StateChangedAt:                 terminatedAt,
		TerminatedAt:                   &terminatedAt,
		TerminalReason:                 &terminalReason,
		AccumulatedActiveSeconds:       3600,
		AccumulatedIdleSeconds:         3600,
		ClaimCount:                     3,
		RequestedCPUMillicores:         &cpu,
		RequestedMemoryBytes:           &memory,
		RequestedEphemeralStorageBytes: &ephemeral,
		CreatedAt:                      base,
		UpdatedAt:                      terminatedAt,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}
	return fact
}

func billingHTTPTotalsByKind(charges []persistence.BillingEstimatedUsageCharge) map[string]int64 {
	totals := make(map[string]int64)
	for _, charge := range charges {
		totals[charge.ChargeKind] += charge.AmountMicros
	}
	return totals
}

func billingHTTPWorkerResourceKey(worker persistence.WorkerIncarnationFact) string {
	return "kubernetes:cluster-a:us-east-1:default:worker-a:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
}

func billingHTTPInvoiceLines(
	resourceKey string,
	totals map[string]int64,
	periodStart time.Time,
	periodEnd time.Time,
) []billing.ImportedActualInvoiceLine {
	order := []string{
		billing.ChargeKindCPU,
		billing.ChargeKindMemory,
		billing.ChargeKindEphemeralStorage,
		billing.ChargeKindRequest,
		billing.ChargeKindPod,
	}
	lines := make([]billing.ImportedActualInvoiceLine, 0, len(order))
	for _, kind := range order {
		amount := totals[kind]
		if amount <= 0 {
			continue
		}
		lines = append(lines, billing.ImportedActualInvoiceLine{
			ExternalLineID:         kind,
			ChargeKind:             kind,
			ResourceCorrelationKey: resourceKey,
			BillingPeriodStartAt:   periodStart,
			BillingPeriodEndAt:     periodEnd,
			CurrencyCode:           "USD",
			AmountMicros:           amount,
		})
	}
	return lines
}

func findBillingHTTPLineByKind(
	t *testing.T,
	report billing.ReconciliationReport,
	kind string,
) billing.ReconciledActualInvoiceLine {
	t.Helper()
	for _, line := range report.Lines {
		if line.Line.ChargeKind == kind {
			return line
		}
	}
	t.Fatalf("reconciled line kind %q not found", kind)
	return billing.ReconciledActualInvoiceLine{}
}
