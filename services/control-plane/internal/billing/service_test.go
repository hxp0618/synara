package billing

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestCostManagementRejectsInactiveTenantBeforeConfigurationOrStorage(t *testing.T) {
	ctx := context.Background()
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	service := NewService(nil, nil)

	_, err := service.ListTariffsAuthorized(ctx, principal, requestedTenantID, ListTariffsFilter{})
	assertProblemCode(t, err, "tenant_not_found")
	_, err = service.CreateTariffAuthorized(
		ctx, principal, requestedTenantID, CreateTariffInput{},
		"billing-inactive-tariff", "127.0.0.1",
	)
	assertProblemCode(t, err, "tenant_not_found")
	_, err = service.ImportConfiguredInvoiceAuthorized(
		ctx, principal, requestedTenantID, "", "",
		"billing-inactive-import", "127.0.0.1",
	)
	assertProblemCode(t, err, "tenant_not_found")
	_, err = service.ReconcileActualInvoiceImportAuthorized(
		ctx, principal, requestedTenantID, uuid.New(),
		"billing-inactive-reconcile", "127.0.0.1",
	)
	assertProblemCode(t, err, "tenant_not_found")
	_, err = service.GetSharedTargetLedgerCoverageAuthorized(
		ctx, principal, requestedTenantID, uuid.New(),
	)
	assertProblemCode(t, err, "tenant_not_found")
	_, _, err = service.SealSharedTargetLedgerCoverageAuthorized(
		ctx, principal, requestedTenantID, uuid.New(), SealSharedTargetLedgerCoverageInput{},
		"billing-inactive-seal", "127.0.0.1",
	)
	assertProblemCode(t, err, "tenant_not_found")
	_, err = service.SweepSharedUsageChargesAuthorized(
		ctx, principal, requestedTenantID, SweepSharedUsageChargesInput{},
		"billing-inactive-sweep", "127.0.0.1",
	)
	assertProblemCode(t, err, "tenant_not_found")
	_, err = service.AllocateSharedActualInvoiceAuthorized(
		ctx, principal, requestedTenantID, AllocateSharedActualInvoiceInput{},
		"billing-inactive-allocate", "127.0.0.1",
	)
	assertProblemCode(t, err, "tenant_not_found")
}

const (
	workerClaimKindExecution        = "execution"
	workerClaimKindWorkspaceCleanup = "workspace-cleanup"
)

func TestCreateTariffRejectsOverlapButAllowsAdjacency(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	base := fixture.base

	first, err := fixture.service.CreateTariff(ctx, CreateTariffInput{
		Provider: "aws", Region: "us-east-1", CurrencyCode: "usd", Version: 1,
		EffectiveStartAt: base, EffectiveEndAt: timePointer(base.Add(time.Hour)),
		CPUCoreHourRateMicros: 3_600_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Provider != "aws" || first.CurrencyCode != "USD" {
		t.Fatalf("normalized tariff = %#v", first)
	}

	_, err = fixture.service.CreateTariff(ctx, CreateTariffInput{
		Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 2,
		EffectiveStartAt: base.Add(30 * time.Minute), EffectiveEndAt: timePointer(base.Add(90 * time.Minute)),
		CPUCoreHourRateMicros: 4_200_000,
	})
	assertProblemCode(t, err, "cost_accounting_tariff_overlap")

	if _, err := fixture.service.CreateTariff(ctx, CreateTariffInput{
		Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 3,
		EffectiveStartAt: base.Add(time.Hour), EffectiveEndAt: timePointer(base.Add(2 * time.Hour)),
		CPUCoreHourRateMicros: 4_200_000,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEstimateUsageChargesSplitsTariffsAndStaysIdempotent(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)
	insertBillingClaimFacts(t, fixture.db, fixture.worker, []billingClaimFactSeed{
		{
			ClaimKind: workerClaimKindExecution,
			RequestID: "billing-claim-execution-a",
			ClaimedAt: fixture.base.Add(15 * time.Minute),
		},
		{
			ClaimKind: workerClaimKindWorkspaceCleanup,
			RequestID: "billing-claim-cleanup-a",
			ClaimedAt: fixture.base.Add(75 * time.Minute),
		},
		{
			ClaimKind: workerClaimKindExecution,
			RequestID: "billing-claim-execution-b",
			ClaimedAt: fixture.base.Add(85 * time.Minute),
		},
	})

	estimated, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             fixture.worker.WorkerID,
		WorkerIncarnation:    fixture.worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(estimated) != 10 {
		t.Fatalf("estimate row count = %d, want 10", len(estimated))
	}

	totals := sumChargesByKind(estimated)
	if totals[ChargeKindCPU] != 10_800_000 {
		t.Fatalf("cpu total = %d, want 10800000", totals[ChargeKindCPU])
	}
	if totals[ChargeKindMemory] != 6_000_000 {
		t.Fatalf("memory total = %d, want 6000000", totals[ChargeKindMemory])
	}
	if totals[ChargeKindEphemeralStorage] != 6_000_000 {
		t.Fatalf("ephemeral total = %d, want 6000000", totals[ChargeKindEphemeralStorage])
	}
	if totals[ChargeKindPod] != 10_800_000 {
		t.Fatalf("pod total = %d, want 10800000", totals[ChargeKindPod])
	}
	if totals[ChargeKindRequest] != 900_000 {
		t.Fatalf("request total = %d, want 900000", totals[ChargeKindRequest])
	}

	requestRows := filterChargesByKind(estimated, ChargeKindRequest)
	if len(requestRows) != 2 || requestRows[0].ClaimCount != 1 || requestRows[1].ClaimCount != 2 {
		t.Fatalf("request rows = %#v, want two segment rows with claimCount=1 and claimCount=2", requestRows)
	}

	var rowCount int64
	if err := fixture.db.Model(&persistence.BillingEstimatedUsageCharge{}).Count(&rowCount).Error; err != nil {
		t.Fatal(err)
	}
	if rowCount != 10 {
		t.Fatalf("persisted estimate row count = %d, want 10", rowCount)
	}

	replayed, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             fixture.worker.WorkerID,
		WorkerIncarnation:    fixture.worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != len(estimated) {
		t.Fatalf("replayed estimate row count = %d, want %d", len(replayed), len(estimated))
	}
	if err := fixture.db.Model(&persistence.BillingEstimatedUsageCharge{}).Count(&rowCount).Error; err != nil {
		t.Fatal(err)
	}
	if rowCount != 10 {
		t.Fatalf("persisted estimate row count after replay = %d, want 10", rowCount)
	}
}

func TestEstimateUsageChargesFailsClosedWithoutTenantAttribution(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)

	sharedWorker := insertBillingWorkerWithTenant(
		t,
		fixture.db,
		fixture.base,
		nil,
		uuid.New(),
		"cluster-a",
		"us-east-1",
		"default",
		"shared-worker",
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	)

	_, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             sharedWorker.WorkerID,
		WorkerIncarnation:    sharedWorker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	assertProblemCode(t, err, "cost_accounting_worker_fact_tenant_unattributed")
}

func TestEstimateUsageChargesUsesCompleteLedgerAcrossBillingBoundary(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)

	startedBeforePeriod := fixture.base.Add(-time.Hour)
	worker := insertBillingWorkerWithTenantWindow(
		t,
		fixture.db,
		startedBeforePeriod,
		3*time.Hour,
		3,
		&fixture.tenantID,
		uuid.New(),
		"cluster-a",
		"us-east-1",
		"default",
		"boundary-worker",
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	)
	insertBillingClaimFacts(t, fixture.db, worker, []billingClaimFactSeed{
		{
			ClaimKind: workerClaimKindExecution,
			RequestID: "billing-boundary-before-period",
			ClaimedAt: fixture.base.Add(-30 * time.Minute),
		},
		{
			ClaimKind: workerClaimKindExecution,
			RequestID: "billing-boundary-in-period-a",
			ClaimedAt: fixture.base.Add(30 * time.Minute),
		},
		{
			ClaimKind: workerClaimKindWorkspaceCleanup,
			RequestID: "billing-boundary-in-period-b",
			ClaimedAt: fixture.base.Add(90 * time.Minute),
		},
	})

	estimated, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             worker.WorkerID,
		WorkerIncarnation:    worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestRows := filterChargesByKind(estimated, ChargeKindRequest)
	if len(requestRows) != 2 || requestRows[0].ClaimCount != 1 || requestRows[1].ClaimCount != 1 {
		t.Fatalf("boundary request rows = %#v, want two in-period request rows with claimCount=1", requestRows)
	}
	if totalsByKind(estimated)[ChargeKindRequest] != 600_000 {
		t.Fatalf("boundary request total = %d, want 600000", totalsByKind(estimated)[ChargeKindRequest])
	}
}

func TestEstimateUsageChargesIncludesClaimAtTerminationBoundary(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)

	worker := insertBillingWorkerWithTenantWindow(
		t,
		fixture.db,
		fixture.base,
		2*time.Hour,
		1,
		&fixture.tenantID,
		uuid.New(),
		"cluster-a",
		"us-east-1",
		"default",
		"termination-boundary-worker",
		"cccccccc-cccc-4ccc-8ccc-ccccccccccce",
	)
	insertBillingClaimFacts(t, fixture.db, worker, []billingClaimFactSeed{{
		ClaimKind: workerClaimKindExecution,
		RequestID: "billing-claim-at-termination",
		ClaimedAt: *worker.TerminatedAt,
	}})

	estimated, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             worker.WorkerID,
		WorkerIncarnation:    worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestRows := filterChargesByKind(estimated, ChargeKindRequest)
	if len(requestRows) != 1 || requestRows[0].ClaimCount != 1 || requestRows[0].AmountMicros != 300_000 {
		t.Fatalf("termination-boundary request rows = %#v, want one charged terminal claim", requestRows)
	}
}

func TestEstimateUsageChargesFailsClosedWhenRequestClaimLedgerIsIncompleteAcrossBillingBoundary(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)

	startedBeforePeriod := fixture.base.Add(-time.Hour)
	worker := insertBillingWorkerWithTenantWindow(
		t,
		fixture.db,
		startedBeforePeriod,
		3*time.Hour,
		3,
		&fixture.tenantID,
		uuid.New(),
		"cluster-a",
		"us-east-1",
		"default",
		"boundary-worker-incomplete",
		"cccccccc-cccc-4ccc-8ccc-cccccccccccd",
	)

	_, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             worker.WorkerID,
		WorkerIncarnation:    worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	assertProblemCode(t, err, "cost_accounting_request_charge_delta_unavailable")
}

func TestEstimateUsageChargesSplitsRequestChargesAcrossTariffChangesWhenLedgerComplete(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	for _, input := range []CreateTariffInput{
		{
			Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 1,
			EffectiveStartAt: fixture.base, EffectiveEndAt: timePointer(fixture.base.Add(time.Hour)),
			CPUCoreHourRateMicros:      3_600_000,
			MemoryGiBHourRateMicros:    1_000_000,
			EphemeralGiBHourRateMicros: 500_000,
			RequestRateMicros:          200_000,
			PodHourRateMicros:          3_600_000,
		},
		{
			Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 2,
			EffectiveStartAt: fixture.base.Add(time.Hour), EffectiveEndAt: timePointer(fixture.base.Add(4 * time.Hour)),
			CPUCoreHourRateMicros:      7_200_000,
			MemoryGiBHourRateMicros:    2_000_000,
			EphemeralGiBHourRateMicros: 1_000_000,
			RequestRateMicros:          300_000,
			PodHourRateMicros:          7_200_000,
		},
	} {
		if _, err := fixture.service.CreateTariff(ctx, input); err != nil {
			t.Fatalf("create tariff %#v: %v", input, err)
		}
	}
	insertBillingClaimFacts(t, fixture.db, fixture.worker, []billingClaimFactSeed{
		{
			ClaimKind: workerClaimKindExecution,
			RequestID: "billing-rate-split-a",
			ClaimedAt: fixture.base.Add(15 * time.Minute),
		},
		{
			ClaimKind: workerClaimKindWorkspaceCleanup,
			RequestID: "billing-rate-split-b",
			ClaimedAt: fixture.base.Add(75 * time.Minute),
		},
		{
			ClaimKind: workerClaimKindExecution,
			RequestID: "billing-rate-split-c",
			ClaimedAt: fixture.base.Add(85 * time.Minute),
		},
	})

	estimated, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             fixture.worker.WorkerID,
		WorkerIncarnation:    fixture.worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestRows := filterChargesByKind(estimated, ChargeKindRequest)
	if len(requestRows) != 2 || requestRows[0].ClaimCount != 1 || requestRows[1].ClaimCount != 2 {
		t.Fatalf("split-rate request rows = %#v, want segment counts 1 and 2", requestRows)
	}
	if requestRows[0].AmountMicros != 200_000 || requestRows[1].AmountMicros != 600_000 {
		t.Fatalf("split-rate request rows = %#v, want amounts 200000 and 600000", requestRows)
	}
}

func TestEstimateUsageChargesFailsClosedWhenRequestTariffsChangeWithinPeriodAndClaimLedgerIsIncomplete(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	for _, input := range []CreateTariffInput{
		{
			Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 1,
			EffectiveStartAt: fixture.base, EffectiveEndAt: timePointer(fixture.base.Add(time.Hour)),
			CPUCoreHourRateMicros:      3_600_000,
			MemoryGiBHourRateMicros:    1_000_000,
			EphemeralGiBHourRateMicros: 500_000,
			RequestRateMicros:          200_000,
			PodHourRateMicros:          3_600_000,
		},
		{
			Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 2,
			EffectiveStartAt: fixture.base.Add(time.Hour), EffectiveEndAt: timePointer(fixture.base.Add(4 * time.Hour)),
			CPUCoreHourRateMicros:      7_200_000,
			MemoryGiBHourRateMicros:    2_000_000,
			EphemeralGiBHourRateMicros: 1_000_000,
			RequestRateMicros:          300_000,
			PodHourRateMicros:          7_200_000,
		},
	} {
		if _, err := fixture.service.CreateTariff(ctx, input); err != nil {
			t.Fatalf("create tariff %#v: %v", input, err)
		}
	}

	_, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             fixture.worker.WorkerID,
		WorkerIncarnation:    fixture.worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	assertProblemCode(t, err, "cost_accounting_request_charge_delta_unavailable")
}

func TestImportAndReconcileActualInvoice(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)

	estimated, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             fixture.worker.WorkerID,
		WorkerIncarnation:    fixture.worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	resourceKey := workerResourceCorrelationKey(fixture.worker)
	missingResource := "kubernetes:cluster-a:us-east-1:default:missing:missing"
	fixture.service.adapter = NewFixtureAdapter(FixtureInvoice{
		TenantID: fixture.tenantID,
		Provider: "aws",
		Invoice: ImportedActualInvoice{
			ExternalImportID:     "aws-2026-07",
			BillingPeriodStartAt: fixture.base,
			BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
			CurrencyCode:         "USD",
			Lines: []ImportedActualInvoiceLine{
				{
					ExternalLineID:         "cpu",
					ChargeKind:             ChargeKindCPU,
					ResourceCorrelationKey: resourceKey,
					AmountMicros:           10_800_000,
				},
				{
					ExternalLineID:         "memory",
					ChargeKind:             ChargeKindMemory,
					ResourceCorrelationKey: resourceKey,
					AmountMicros:           6_500_000,
				},
				{
					ExternalLineID:         "pod",
					ChargeKind:             ChargeKindPod,
					ResourceCorrelationKey: resourceKey,
					AmountMicros:           10_800_000,
				},
				{
					ExternalLineID:         "unmatched",
					ChargeKind:             ChargeKindRequest,
					ResourceCorrelationKey: missingResource,
					AmountMicros:           300_000,
				},
			},
		},
	})

	imported, err := fixture.service.ImportActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: fixture.tenantID,
		Provider: "aws", ExternalImportID: "aws-2026-07",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Lines) != 4 {
		t.Fatalf("imported line count = %d, want 4", len(imported.Lines))
	}
	replayed, err := fixture.service.ImportActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: fixture.tenantID,
		Provider: "aws", ExternalImportID: "aws-2026-07",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Import.ID != imported.Import.ID || len(replayed.Lines) != 4 {
		t.Fatalf("replayed import = %#v, want same import id %s and four lines", replayed.Import, imported.Import.ID)
	}

	report, err := fixture.service.ReconcileActualInvoiceImport(ctx, fixture.tenantID, imported.Import.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Lines) != 4 {
		t.Fatalf("reconciled line count = %d, want 4", len(report.Lines))
	}
	if len(report.UnmatchedEstimateIDs) != 3 {
		t.Fatalf("unmatched estimate count = %d, want 3", len(report.UnmatchedEstimateIDs))
	}
	if report.UnmatchedEstimateAmountMicros != 6_900_000 {
		t.Fatalf("unmatched estimate amount = %d, want 6900000", report.UnmatchedEstimateAmountMicros)
	}

	byExternalID := make(map[string]ReconciledActualInvoiceLine, len(report.Lines))
	for _, line := range report.Lines {
		byExternalID[line.Line.ExternalLineID] = line
	}
	if byExternalID["cpu"].Line.ReconciliationState != reconciliationStateMatched ||
		byExternalID["cpu"].Line.MatchedEstimateCount != 2 ||
		byExternalID["cpu"].Line.MatchedEstimateAmountMicros != totalsByKind(estimated)[ChargeKindCPU] {
		t.Fatalf("cpu reconciliation = %#v", byExternalID["cpu"])
	}
	if byExternalID["memory"].Line.ReconciliationState != reconciliationStateVariance ||
		byExternalID["memory"].Line.MatchedEstimateCount != 2 ||
		byExternalID["memory"].Line.MatchedEstimateAmountMicros != totalsByKind(estimated)[ChargeKindMemory] {
		t.Fatalf("memory reconciliation = %#v", byExternalID["memory"])
	}
	if byExternalID["pod"].Line.ReconciliationState != reconciliationStateMatched ||
		byExternalID["pod"].Line.MatchedEstimateCount != 2 {
		t.Fatalf("pod reconciliation = %#v", byExternalID["pod"])
	}
	if byExternalID["unmatched"].Line.ReconciliationState != reconciliationStateUnmatchedActual ||
		byExternalID["unmatched"].Line.MatchedEstimateCount != 0 {
		t.Fatalf("unmatched reconciliation = %#v", byExternalID["unmatched"])
	}
}

func TestImportAndReconcileActualInvoiceIsolatesTenants(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	ctx := context.Background()
	seedTwoTariffs(t, ctx, fixture)

	secondTenantID := uuid.New()
	secondWorker := insertBillingWorker(
		t,
		fixture.db,
		fixture.base,
		secondTenantID,
		uuid.New(),
		"cluster-a",
		"us-east-1",
		"default",
		"worker-a",
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	)

	estimatedA, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             fixture.worker.WorkerID,
		WorkerIncarnation:    fixture.worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	estimatedB, err := fixture.service.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD",
		WorkerID:             secondWorker.WorkerID,
		WorkerIncarnation:    secondWorker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base,
		BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	resourceKey := workerResourceCorrelationKey(fixture.worker)
	if workerResourceCorrelationKey(secondWorker) != resourceKey {
		t.Fatalf("second worker resource key = %q, want %q", workerResourceCorrelationKey(secondWorker), resourceKey)
	}

	fixture.service.adapter = NewFixtureAdapter(
		FixtureInvoice{
			TenantID: fixture.tenantID,
			Provider: "aws",
			Invoice: ImportedActualInvoice{
				ExternalImportID:     "aws-shared-2026-07",
				BillingPeriodStartAt: fixture.base,
				BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
				CurrencyCode:         "USD",
				Lines: []ImportedActualInvoiceLine{{
					ExternalLineID:         "cpu-tenant-a",
					ChargeKind:             ChargeKindCPU,
					ResourceCorrelationKey: resourceKey,
					AmountMicros:           totalsByKind(estimatedA)[ChargeKindCPU],
				}},
			},
		},
		FixtureInvoice{
			TenantID: secondTenantID,
			Provider: "aws",
			Invoice: ImportedActualInvoice{
				ExternalImportID:     "aws-shared-2026-07",
				BillingPeriodStartAt: fixture.base,
				BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
				CurrencyCode:         "USD",
				Lines: []ImportedActualInvoiceLine{{
					ExternalLineID:         "cpu-tenant-b",
					ChargeKind:             ChargeKindCPU,
					ResourceCorrelationKey: resourceKey,
					AmountMicros:           totalsByKind(estimatedB)[ChargeKindCPU],
				}},
			},
		},
	)

	importedA, err := fixture.service.ImportActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: fixture.tenantID, Provider: "aws", ExternalImportID: "aws-shared-2026-07",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayedA, err := fixture.service.ImportActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: fixture.tenantID, Provider: "aws", ExternalImportID: "aws-shared-2026-07",
	})
	if err != nil {
		t.Fatal(err)
	}
	importedB, err := fixture.service.ImportActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: secondTenantID, Provider: "aws", ExternalImportID: "aws-shared-2026-07",
	})
	if err != nil {
		t.Fatal(err)
	}

	if importedA.Import.ID != replayedA.Import.ID {
		t.Fatalf("replayed tenant A import id = %s, want %s", replayedA.Import.ID, importedA.Import.ID)
	}
	if importedA.Import.ID == importedB.Import.ID {
		t.Fatalf("tenant-scoped import ids collided: %s", importedA.Import.ID)
	}
	if len(importedA.Lines) != 1 || importedA.Lines[0].ExternalLineID != "cpu-tenant-a" {
		t.Fatalf("tenant A imported lines = %#v", importedA.Lines)
	}
	if len(importedB.Lines) != 1 || importedB.Lines[0].ExternalLineID != "cpu-tenant-b" {
		t.Fatalf("tenant B imported lines = %#v", importedB.Lines)
	}
	if importedA.Lines[0].ID == importedB.Lines[0].ID {
		t.Fatalf("tenant-scoped line ids collided: %s", importedA.Lines[0].ID)
	}

	reportA, err := fixture.service.ReconcileActualInvoiceImport(ctx, fixture.tenantID, importedA.Import.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reportA.Lines) != 1 {
		t.Fatalf("tenant A reconciled line count = %d, want 1", len(reportA.Lines))
	}
	lineA := reportA.Lines[0]
	if lineA.Line.ReconciliationState != reconciliationStateMatched ||
		lineA.Line.MatchedEstimateCount != 2 ||
		lineA.Line.MatchedEstimateAmountMicros != totalsByKind(estimatedA)[ChargeKindCPU] {
		t.Fatalf("tenant A reconciliation = %#v", lineA)
	}
	if len(reportA.UnmatchedEstimateIDs) != len(estimatedA)-len(lineA.MatchedEstimateIDs) {
		t.Fatalf("tenant A unmatched estimate count = %d, want %d", len(reportA.UnmatchedEstimateIDs), len(estimatedA)-len(lineA.MatchedEstimateIDs))
	}

	tenantAEstimateIDs := estimateIDSet(estimatedA)
	tenantBEstimateIDs := estimateIDSet(estimatedB)
	assertIDsStayWithinTenant(t, lineA.MatchedEstimateIDs, tenantAEstimateIDs, tenantBEstimateIDs, "matched")
	assertIDsStayWithinTenant(t, reportA.UnmatchedEstimateIDs, tenantAEstimateIDs, tenantBEstimateIDs, "unmatched")
}

func TestImportActualInvoiceRejectsConflictingReplay(t *testing.T) {
	fixture := newBillingFixture(t, nil)
	fixture.service.adapter = NewFixtureAdapter(FixtureInvoice{
		TenantID: fixture.tenantID,
		Provider: "aws",
		Invoice: ImportedActualInvoice{
			ExternalImportID:     "aws-conflict",
			BillingPeriodStartAt: time.Date(2026, time.July, 24, 0, 0, 0, 0, time.UTC),
			BillingPeriodEndAt:   time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC),
			CurrencyCode:         "USD",
			Lines: []ImportedActualInvoiceLine{{
				ExternalLineID:         "cpu",
				ChargeKind:             ChargeKindCPU,
				ResourceCorrelationKey: "resource-a",
				AmountMicros:           1_000_000,
			}},
		},
	})
	ctx := context.Background()

	if _, err := fixture.service.ImportActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: fixture.tenantID,
		Provider: "aws", ExternalImportID: "aws-conflict",
	}); err != nil {
		t.Fatal(err)
	}

	fixture.service.adapter = NewFixtureAdapter(FixtureInvoice{
		TenantID: fixture.tenantID,
		Provider: "aws",
		Invoice: ImportedActualInvoice{
			ExternalImportID:     "aws-conflict",
			BillingPeriodStartAt: time.Date(2026, time.July, 24, 0, 0, 0, 0, time.UTC),
			BillingPeriodEndAt:   time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC),
			CurrencyCode:         "USD",
			Lines: []ImportedActualInvoiceLine{{
				ExternalLineID:         "cpu",
				ChargeKind:             ChargeKindCPU,
				ResourceCorrelationKey: "resource-a",
				AmountMicros:           2_000_000,
			}},
		},
	})
	_, err := fixture.service.ImportActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: fixture.tenantID,
		Provider: "aws", ExternalImportID: "aws-conflict",
	})
	assertProblemCode(t, err, "cost_accounting_invoice_import_conflict")
}

type billingFixture struct {
	db       *gorm.DB
	service  *Service
	base     time.Time
	tenantID uuid.UUID
	worker   persistence.WorkerIncarnationFact
}

func newBillingFixture(t *testing.T, adapter Adapter) billingFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cost_accounting.sqlite")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ExecutionTarget{},
		&persistence.WorkerIncarnationFact{},
		&persistence.WorkerClaimFact{},
		&persistence.AuditLog{},
		&persistence.BillingProviderTariff{},
		&persistence.BillingEstimatedUsageCharge{},
		&persistence.BillingActualInvoiceImport{},
		&persistence.BillingActualInvoiceLine{},
	); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	targetID := uuid.New()
	if err := db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &tenantID, Kind: "kubernetes", Name: "billing-target",
		Status: "active", Capabilities: map[string]any{}, CreatedAt: base, UpdatedAt: base,
	}).Error; err != nil {
		t.Fatal(err)
	}
	worker := insertBillingWorker(
		t,
		db,
		base,
		tenantID,
		targetID,
		"cluster-a",
		"us-east-1",
		"default",
		"worker-a",
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	)

	service := NewService(db, adapter)
	service.now = func() time.Time { return base.Add(5 * time.Hour) }
	return billingFixture{db: db, service: service, base: base, tenantID: tenantID, worker: worker}
}

func seedTwoTariffs(t *testing.T, ctx context.Context, fixture billingFixture) {
	t.Helper()
	for _, input := range []CreateTariffInput{
		{
			Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 1,
			EffectiveStartAt: fixture.base, EffectiveEndAt: timePointer(fixture.base.Add(time.Hour)),
			CPUCoreHourRateMicros:      3_600_000,
			MemoryGiBHourRateMicros:    1_000_000,
			EphemeralGiBHourRateMicros: 500_000,
			RequestRateMicros:          300_000,
			PodHourRateMicros:          3_600_000,
		},
		{
			Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 2,
			EffectiveStartAt: fixture.base.Add(time.Hour), EffectiveEndAt: timePointer(fixture.base.Add(4 * time.Hour)),
			CPUCoreHourRateMicros:      7_200_000,
			MemoryGiBHourRateMicros:    2_000_000,
			EphemeralGiBHourRateMicros: 1_000_000,
			RequestRateMicros:          300_000,
			PodHourRateMicros:          7_200_000,
		},
	} {
		if _, err := fixture.service.CreateTariff(ctx, input); err != nil {
			t.Fatalf("create tariff %#v: %v", input, err)
		}
	}
}

func insertBillingWorker(
	t *testing.T,
	db *gorm.DB,
	base time.Time,
	tenantID uuid.UUID,
	targetID uuid.UUID,
	clusterID string,
	region string,
	namespace string,
	podName string,
	instanceUID string,
) persistence.WorkerIncarnationFact {
	t.Helper()
	return insertBillingWorkerWithTenant(t, db, base, &tenantID, targetID, clusterID, region, namespace, podName, instanceUID)
}

func insertBillingWorkerWithTenant(
	t *testing.T,
	db *gorm.DB,
	base time.Time,
	tenantID *uuid.UUID,
	targetID uuid.UUID,
	clusterID string,
	region string,
	namespace string,
	podName string,
	instanceUID string,
) persistence.WorkerIncarnationFact {
	t.Helper()
	return insertBillingWorkerWithTenantWindow(
		t,
		db,
		base,
		2*time.Hour,
		3,
		tenantID,
		targetID,
		clusterID,
		region,
		namespace,
		podName,
		instanceUID,
	)
}

func insertBillingWorkerWithTenantWindow(
	t *testing.T,
	db *gorm.DB,
	registeredAt time.Time,
	lifetime time.Duration,
	claimCount int64,
	tenantID *uuid.UUID,
	targetID uuid.UUID,
	clusterID string,
	region string,
	namespace string,
	podName string,
	instanceUID string,
) persistence.WorkerIncarnationFact {
	t.Helper()

	cpu := int64(1000)
	memory := int64(2 * 1024 * 1024 * 1024)
	ephemeral := int64(4 * 1024 * 1024 * 1024)
	terminatedAt := registeredAt.Add(lifetime)
	worker := persistence.WorkerIncarnationFact{
		WorkerID:                       uuid.New(),
		WorkerIncarnation:              1,
		TenantID:                       tenantID,
		ExecutionTargetID:              targetID,
		TargetKind:                     "kubernetes",
		WorkerMode:                     "execution-pinned",
		ClusterID:                      clusterID,
		Region:                         region,
		Namespace:                      namespace,
		PodName:                        podName,
		InstanceUID:                    instanceUID,
		RegisteredAt:                   registeredAt,
		CurrentState:                   "terminated",
		StateChangedAt:                 terminatedAt,
		TerminatedAt:                   &terminatedAt,
		TerminalReason:                 stringPointer("completed"),
		AccumulatedActiveSeconds:       3600,
		AccumulatedIdleSeconds:         3600,
		ClaimCount:                     claimCount,
		RequestedCPUMillicores:         &cpu,
		RequestedMemoryBytes:           &memory,
		RequestedEphemeralStorageBytes: &ephemeral,
		CreatedAt:                      registeredAt,
		UpdatedAt:                      terminatedAt,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	return worker
}

type billingClaimFactSeed struct {
	ClaimKind string
	RequestID string
	ClaimedAt time.Time
}

func insertBillingClaimFacts(
	t *testing.T,
	db *gorm.DB,
	worker persistence.WorkerIncarnationFact,
	seeds []billingClaimFactSeed,
) {
	t.Helper()
	if worker.TenantID == nil {
		t.Fatal("billing claim facts require a tenant-owned worker")
	}
	for index, seed := range seeds {
		claim := persistence.WorkerClaimFact{
			ID:                uuid.New(),
			WorkerID:          worker.WorkerID,
			WorkerIncarnation: worker.WorkerIncarnation,
			TenantID:          *worker.TenantID,
			ExecutionTargetID: worker.ExecutionTargetID,
			TargetKind:        worker.TargetKind,
			ClaimKind:         seed.ClaimKind,
			RequestID:         seed.RequestID,
			ClaimedAt:         seed.ClaimedAt,
			CreatedAt:         seed.ClaimedAt,
		}
		switch seed.ClaimKind {
		case workerClaimKindExecution:
			executionID := uuid.New()
			generation := int64(index + 1)
			claim.ExecutionID = &executionID
			claim.ExecutionGeneration = &generation
		case workerClaimKindWorkspaceCleanup:
			cleanupID := uuid.New()
			dispatchGeneration := int64(index + 1)
			claim.CleanupCommandID = &cleanupID
			claim.CleanupDispatchGeneration = &dispatchGeneration
		default:
			t.Fatalf("unsupported billing claim kind %q", seed.ClaimKind)
		}
		if err := db.Create(&claim).Error; err != nil {
			t.Fatalf("create billing claim fact %#v: %v", claim, err)
		}
	}
}

func sumChargesByKind(charges []persistence.BillingEstimatedUsageCharge) map[string]int64 {
	result := make(map[string]int64)
	for _, charge := range charges {
		result[charge.ChargeKind] += charge.AmountMicros
	}
	return result
}

func totalsByKind(charges []persistence.BillingEstimatedUsageCharge) map[string]int64 {
	return sumChargesByKind(charges)
}

func filterChargesByKind(
	charges []persistence.BillingEstimatedUsageCharge,
	kind string,
) []persistence.BillingEstimatedUsageCharge {
	filtered := make([]persistence.BillingEstimatedUsageCharge, 0)
	for _, charge := range charges {
		if charge.ChargeKind == kind {
			filtered = append(filtered, charge)
		}
	}
	return filtered
}

func estimateIDSet(charges []persistence.BillingEstimatedUsageCharge) map[uuid.UUID]struct{} {
	ids := make(map[uuid.UUID]struct{}, len(charges))
	for _, charge := range charges {
		ids[charge.ID] = struct{}{}
	}
	return ids
}

func assertIDsStayWithinTenant(
	t *testing.T,
	ids []uuid.UUID,
	allowed map[uuid.UUID]struct{},
	forbidden map[uuid.UUID]struct{},
	label string,
) {
	t.Helper()
	for _, id := range ids {
		if _, ok := allowed[id]; !ok {
			t.Fatalf("%s estimate id %s was not created for the expected tenant", label, id)
		}
		if _, forbiddenHit := forbidden[id]; forbiddenHit {
			t.Fatalf("%s estimate id %s leaked from another tenant", label, id)
		}
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func stringPointer(value string) *string {
	return &value
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
