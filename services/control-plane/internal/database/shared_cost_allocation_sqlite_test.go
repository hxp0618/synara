package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

type sharedCostAllocationFixture struct {
	db           *gorm.DB
	tenantID     uuid.UUID
	target       persistence.ExecutionTarget
	workerFact   persistence.WorkerIncarnationFact
	claim        persistence.WorkerClaimFact
	coverage     persistence.BillingSharedTargetLedgerCoverage
	tariff       persistence.BillingProviderTariff
	run          persistence.BillingSharedCostAllocationRun
	claimAt      time.Time
	releaseAt    time.Time
	terminatedAt time.Time
}

func TestSQLiteSharedCostAllocationSafetyAcceptsLegalGraphAndRejectsInvalidScope(t *testing.T) {
	fixture := seedSQLiteSharedCostAllocationFixture(t, time.Minute, 3*time.Minute, 5*time.Minute, true)

	for _, name := range []string{
		"trg_billing_shared_target_ledger_coverages_insert",
		"trg_billing_shared_cost_allocation_runs_insert",
		"trg_billing_shared_estimated_charge_slices_insert",
		"trg_billing_shared_target_ledger_coverages_update",
		"trg_billing_shared_target_ledger_coverages_delete",
		"trg_billing_shared_cost_allocation_runs_update",
		"trg_billing_shared_cost_allocation_runs_delete",
		"trg_billing_shared_estimated_charge_slices_update",
		"trg_billing_shared_estimated_charge_slices_delete",
		"trg_execution_targets_shared_accounting_restrict_delete",
		"trg_execution_targets_shared_accounting_restrict_update",
		"uq_billing_shared_estimated_charge_slices_semantic",
		"trg_worker_incarnation_facts_shared_accounting_restrict_delete",
		"trg_worker_claim_facts_shared_accounting_restrict_delete",
		"trg_billing_provider_tariffs_shared_accounting_restrict_delete",
		"trg_tenants_shared_accounting_restrict_delete",
	} {
		var count int64
		if err := fixture.db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite shared allocation safety object %s count = %d, want 1", name, count)
		}
	}

	tenantSlice := validSharedTenantChargeSlice(fixture, "cpu", fixture.claimAt, fixture.releaseAt)
	if err := fixture.db.Create(&tenantSlice).Error; err != nil {
		t.Fatalf("create legal tenant claim slice with global fallback tariff: %v", err)
	}
	idleSlice := validSharedIdleChargeSlice(fixture, fixture.run.UsageStartAt, fixture.claimAt)
	if err := fixture.db.Create(&idleSlice).Error; err != nil {
		t.Fatalf("create legal platform idle slice: %v", err)
	}
	requestSlice := validSharedTenantChargeSlice(fixture, "request", fixture.claimAt, fixture.releaseAt)
	requestSlice.BillableSeconds = 0
	if err := fixture.db.Create(&requestSlice).Error; err != nil {
		t.Fatalf("create legal request slice: %v", err)
	}

	duplicateSemanticSlice := tenantSlice
	duplicateSemanticSlice.ID = uuid.New()
	if err := fixture.db.Create(&duplicateSemanticSlice).Error; err == nil || !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("shared allocation accepted duplicate semantic slice or returned wrong error: %v", err)
	}

	wrongRate := validSharedTenantChargeSlice(fixture, "memory", fixture.claimAt, fixture.releaseAt)
	wrongRate.RateMicros = fixture.tariff.MemoryGiBHourRateMicros + 1
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&wrongRate).Error, "invalid billing shared estimated charge slice")

	wrongResourceSnapshot := validSharedTenantChargeSlice(fixture, "ephemeral-storage", fixture.claimAt, fixture.releaseAt)
	wrongResourceSnapshot.RequestedEphemeralStorageBytes = sharedAllocationInt64Pointer(1)
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&wrongResourceSnapshot).Error, "invalid billing shared estimated charge slice")

	foreignTenantSlice := validSharedTenantChargeSlice(fixture, "memory", fixture.claimAt, fixture.releaseAt)
	foreignTenantSlice.TenantID = uuidPointer(uuid.New())
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&foreignTenantSlice).Error, "invalid billing shared estimated charge slice")

	wrongShape := validSharedTenantChargeSlice(fixture, "pod", fixture.claimAt, fixture.releaseAt)
	wrongShape.AllocationKind = "platform-idle"
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&wrongShape).Error, "invalid billing shared estimated charge slice")

	mismatchedTarget := fixture.target
	mismatchedTarget.ID = uuid.New()
	mismatchedTarget.Name = "other-shared-allocation-target"
	if err := fixture.db.Create(&mismatchedTarget).Error; err != nil {
		t.Fatalf("create mismatched shared target: %v", err)
	}
	mismatchedTargetRun := fixture.run
	mismatchedTargetRun.ID = uuid.New()
	mismatchedTargetRun.ExecutionTargetID = mismatchedTarget.ID
	mismatchedTargetRun.BillingPeriodStartAt = mismatchedTargetRun.BillingPeriodStartAt.Add(-2 * time.Hour)
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&mismatchedTargetRun).Error, "invalid billing shared cost allocation run")

	mismatchedWorkerRun := fixture.run
	mismatchedWorkerRun.ID = uuid.New()
	mismatchedWorkerRun.WorkerID = uuid.New()
	mismatchedWorkerRun.BillingPeriodStartAt = mismatchedWorkerRun.BillingPeriodStartAt.Add(-time.Hour)
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&mismatchedWorkerRun).Error, "invalid billing shared cost allocation run")

	tenantTarget := fixture.target
	tenantTarget.ID = uuid.New()
	tenantTarget.TenantID = &fixture.tenantID
	tenantTarget.Name = "tenant-owned-allocation-target"
	if err := fixture.db.Create(&tenantTarget).Error; err != nil {
		t.Fatalf("create tenant-owned target: %v", err)
	}
	invalidCoverage := fixture.coverage
	invalidCoverage.ID = uuid.New()
	invalidCoverage.ExecutionTargetID = tenantTarget.ID
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&invalidCoverage).Error, "invalid billing shared Target ledger coverage")

	duplicateRun := fixture.run
	duplicateRun.ID = uuid.New()
	if err := fixture.db.Create(&duplicateRun).Error; err == nil || !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("shared allocation accepted duplicate deterministic identity or returned wrong error: %v", err)
	}

	overlappingRun := fixture.run
	overlappingRun.ID = uuid.New()
	overlappingRun.BillingPeriodStartAt = overlappingRun.BillingPeriodStartAt.Add(-time.Minute)
	overlappingRun.BillingPeriodEndAt = overlappingRun.BillingPeriodEndAt.Add(-time.Minute)
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&overlappingRun).Error, "invalid billing shared cost allocation run")

	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Model(&persistence.BillingSharedTargetLedgerCoverage{}).
			Where("id = ?", fixture.coverage.ID).Update("minimum_writer_version", "v2").Error,
		"immutable",
	)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).
			Where("id = ?", fixture.run.ID).Update("platform_idle_seconds", 0).Error,
		"immutable",
	)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Delete(&persistence.BillingSharedEstimatedChargeSlice{}, "id = ?", tenantSlice.ID).Error,
		"immutable",
	)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Model(&persistence.BillingSharedEstimatedChargeSlice{}).
			Where("id = ?", idleSlice.ID).Update("amount_micros", 0).Error,
		"immutable",
	)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Delete(&persistence.BillingSharedCostAllocationRun{}, "id = ?", fixture.run.ID).Error,
		"immutable",
	)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Delete(&persistence.BillingSharedTargetLedgerCoverage{}, "id = ?", fixture.coverage.ID).Error,
		"immutable",
	)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Model(&persistence.ExecutionTarget{}).
			Where("id = ?", fixture.target.ID).UpdateColumn("kind", "docker").Error,
		"billing shared allocation parent is retained",
	)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Delete(&persistence.ExecutionTarget{}, "id = ?", fixture.target.ID).Error,
		"billing shared allocation parent is retained",
	)
}

func TestSQLiteSharedCostAllocationRejectsClaimWithoutUniqueRelease(t *testing.T) {
	fixture := seedSQLiteSharedCostAllocationFixture(t, time.Minute, 3*time.Minute, 5*time.Minute, false)
	slice := validSharedTenantChargeSlice(fixture, "cpu", fixture.claimAt, fixture.releaseAt)
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&slice).Error, "invalid billing shared estimated charge slice")
}

func TestSQLiteSharedActualAllocationRequiresCompleteConservedGraphBeforeSeal(t *testing.T) {
	fixture := seedSQLiteSharedCostAllocationFixture(t, time.Minute, 3*time.Minute, 5*time.Minute, true)
	estimateSlice := validSharedTenantChargeSlice(fixture, "cpu", fixture.claimAt, fixture.releaseAt)
	if err := fixture.db.Create(&estimateSlice).Error; err != nil {
		t.Fatalf("create shared estimate basis: %v", err)
	}
	for _, name := range []string{
		"trg_billing_shared_actual_allocation_runs_insert",
		"trg_billing_shared_actual_allocation_lines_insert",
		"trg_billing_shared_actual_charge_slices_insert",
		"trg_billing_shared_actual_allocation_runs_seal",
		"trg_billing_shared_actual_allocation_runs_delete",
		"trg_billing_shared_actual_allocation_lines_update",
		"trg_billing_shared_actual_allocation_lines_delete",
		"trg_billing_shared_actual_charge_slices_update",
		"trg_billing_shared_actual_charge_slices_delete",
		"trg_billing_actual_invoice_lines_shared_allocation_fence",
		"trg_billing_shared_estimated_charge_slices_actual_allocation_fence",
		"uq_billing_shared_actual_allocation_lines_actual",
		"uq_billing_shared_actual_charge_slices_estimate",
	} {
		var count int64
		if err := fixture.db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite shared actual safety object %s count = %d, want 1", name, count)
		}
	}

	var operatorTenant persistence.Tenant
	if err := fixture.db.Where("id = ?", fixture.tenantID).Take(&operatorTenant).Error; err != nil {
		t.Fatal(err)
	}
	createdAt := fixture.run.BillingPeriodEndAt.Add(time.Minute)
	invoiceImport := persistence.BillingActualInvoiceImport{
		ID: uuid.New(), TenantID: fixture.tenantID, Provider: fixture.run.Provider,
		ExternalImportID:     "sqlite-shared-actual-" + uuid.NewString(),
		BillingPeriodStartAt: fixture.run.BillingPeriodStartAt,
		BillingPeriodEndAt:   fixture.run.BillingPeriodEndAt,
		CurrencyCode:         fixture.run.CurrencyCode, SourceChecksum: strings.Repeat("c", 64),
		ImportedAt: createdAt, CreatedAt: createdAt,
	}
	if err := fixture.db.Create(&invoiceImport).Error; err != nil {
		t.Fatal(err)
	}
	actualLine := persistence.BillingActualInvoiceLine{
		ID: uuid.New(), TenantID: fixture.tenantID, InvoiceImportID: invoiceImport.ID,
		ExternalLineID: "shared-cpu", Provider: fixture.run.Provider, CurrencyCode: fixture.run.CurrencyCode,
		ChargeKind: estimateSlice.ChargeKind, ResourceCorrelationKey: estimateSlice.ResourceCorrelationKey,
		BillingPeriodStartAt: fixture.run.BillingPeriodStartAt,
		BillingPeriodEndAt:   fixture.run.BillingPeriodEndAt,
		AmountMicros:         7, ReconciliationState: "pending", CreatedAt: createdAt,
	}
	if err := fixture.db.Create(&actualLine).Error; err != nil {
		t.Fatal(err)
	}
	run := persistence.BillingSharedActualAllocationRun{
		ID: uuid.New(), OperatorTenantID: fixture.tenantID, InvoiceImportID: invoiceImport.ID,
		ExecutionTargetID: fixture.target.ID, LedgerCoverageID: fixture.coverage.ID,
		Provider: fixture.run.Provider, CurrencyCode: fixture.run.CurrencyCode,
		BillingPeriodStartAt:         fixture.run.BillingPeriodStartAt,
		BillingPeriodEndAt:           fixture.run.BillingPeriodEndAt,
		AlgorithmVersion:             "proportional-shared-estimate-v1",
		SourceChecksum:               invoiceImport.SourceChecksum,
		SourceScopeAttestationSHA256: strings.Repeat("d", 64),
		SourceLineSetSHA256:          strings.Repeat("e", 64),
		ImportLineCount:              1, ImportAmountMicros: 7,
		SourceLineCount: 1, SourceAmountMicros: 7,
		UnallocatedLineCount: 0, UnallocatedAmountMicros: 0,
		AllocationLineCount: 1, AllocationSliceCount: 1, AllocatedAmountMicros: 7,
		State: "building", CreatedBy: operatorTenant.CreatedBy, CreatedAt: createdAt,
	}
	if err := fixture.db.Create(&run).Error; err != nil {
		t.Fatalf("create building shared actual run: %v", err)
	}
	allocationLine := persistence.BillingSharedActualAllocationLine{
		ID: uuid.New(), RunID: run.ID, ActualInvoiceLineID: actualLine.ID,
		SourceAmountMicros: 7, EstimatedAmountMicros: estimateSlice.AmountMicros,
		AllocatedAmountMicros: 7, EstimatedSliceCount: 1,
		EstimatedSliceSetSHA256: strings.Repeat("f", 64), CreatedAt: createdAt,
	}
	if err := fixture.db.Create(&allocationLine).Error; err != nil {
		t.Fatalf("create shared actual allocation line: %v", err)
	}
	actualSlice := persistence.BillingSharedActualChargeSlice{
		ID: uuid.New(), AllocationLineID: allocationLine.ID, EstimatedSliceID: estimateSlice.ID,
		TenantID: estimateSlice.TenantID, AllocationKind: estimateSlice.AllocationKind,
		EstimateWeightMicros: estimateSlice.AmountMicros, AmountMicros: 7, CreatedAt: createdAt,
	}
	if err := fixture.db.Create(&actualSlice).Error; err != nil {
		t.Fatalf("create shared actual charge slice: %v", err)
	}
	sealedAt := createdAt.Add(time.Second)
	if err := fixture.db.Model(&persistence.BillingSharedActualAllocationRun{}).
		Where("id = ?", run.ID).
		Updates(map[string]any{"state": "sealed", "sealed_at": sealedAt}).Error; err != nil {
		t.Fatalf("seal conserved shared actual graph: %v", err)
	}
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Model(&persistence.BillingSharedActualAllocationRun{}).
			Where("id = ?", run.ID).UpdateColumn("source_amount_micros", 8).Error,
		"transition is invalid",
	)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Delete(&persistence.BillingSharedActualChargeSlice{}, "id = ?", actualSlice.ID).Error,
		"immutable",
	)
	lateActualLine := actualLine
	lateActualLine.ID = uuid.New()
	lateActualLine.ExternalLineID = "late-line"
	lateActualLine.ResourceCorrelationKey = "late-resource/" + uuid.NewString()
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Create(&lateActualLine).Error,
		"invoice import is sealed by shared allocation",
	)
	lateEstimateSlice := estimateSlice
	lateEstimateSlice.ID = uuid.New()
	lateEstimateSlice.UsageStartAt = lateEstimateSlice.UsageStartAt.Add(time.Second)
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Create(&lateEstimateSlice).Error,
		"estimate scope is sealed by actual allocation",
	)

	incompleteImport := invoiceImport
	incompleteImport.ID = uuid.New()
	incompleteImport.ExternalImportID = "sqlite-shared-actual-incomplete-" + uuid.NewString()
	if err := fixture.db.Create(&incompleteImport).Error; err != nil {
		t.Fatal(err)
	}
	incompleteLine := actualLine
	incompleteLine.ID = uuid.New()
	incompleteLine.InvoiceImportID = incompleteImport.ID
	if err := fixture.db.Create(&incompleteLine).Error; err != nil {
		t.Fatal(err)
	}
	incompleteRun := run
	incompleteRun.ID = uuid.New()
	incompleteRun.InvoiceImportID = incompleteImport.ID
	incompleteRun.State = "building"
	incompleteRun.SealedAt = nil
	if err := fixture.db.Create(&incompleteRun).Error; err != nil {
		t.Fatal(err)
	}
	assertSharedAllocationSQLiteRejected(
		t,
		fixture.db.Model(&persistence.BillingSharedActualAllocationRun{}).
			Where("id = ?", incompleteRun.ID).
			Updates(map[string]any{"state": "sealed", "sealed_at": sealedAt}).Error,
		"conservation is invalid",
	)
}

func TestSQLiteSharedRequestSliceAllowsExactFinalRunBoundaryAndGlobalTariff(t *testing.T) {
	fixture := seedSQLiteSharedCostAllocationFixture(t, 5*time.Minute, 5*time.Minute, 5*time.Minute, true)
	slice := validSharedTenantChargeSlice(
		fixture,
		"request",
		fixture.terminatedAt.Add(-time.Minute),
		fixture.terminatedAt,
	)
	slice.BillableSeconds = 0
	if err := fixture.db.Create(&slice).Error; err != nil {
		t.Fatalf("exact final-boundary request slice with global fallback tariff was rejected: %v", err)
	}

	nonFinalWindow := slice
	nonFinalWindow.ID = uuid.New()
	nonFinalWindow.UsageStartAt = nonFinalWindow.UsageStartAt.Add(-time.Minute)
	nonFinalWindow.UsageEndAt = nonFinalWindow.UsageEndAt.Add(-time.Second)
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&nonFinalWindow).Error, "invalid billing shared estimated charge slice")
}

func TestSQLiteSharedRequestSliceRejectsNonTerminalRunBoundary(t *testing.T) {
	fixture := seedSQLiteSharedCostAllocationFixtureWithRunEnd(
		t,
		5*time.Minute,
		5*time.Minute,
		10*time.Minute,
		5*time.Minute,
		true,
	)
	slice := validSharedTenantChargeSlice(
		fixture,
		"request",
		fixture.run.UsageEndAt.Add(-time.Minute),
		fixture.run.UsageEndAt,
	)
	slice.BillableSeconds = 0
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&slice).Error, "invalid billing shared estimated charge slice")
}

func TestSQLiteSharedSliceRejectsGlobalTariffWhenRegionalTariffIsEffective(t *testing.T) {
	fixture := seedSQLiteSharedCostAllocationFixture(t, time.Minute, 3*time.Minute, 5*time.Minute, true)
	regional := fixture.tariff
	regional.ID = uuid.New()
	regional.Region = fixture.workerFact.Region
	if err := fixture.db.Create(&regional).Error; err != nil {
		t.Fatal(err)
	}
	slice := validSharedTenantChargeSlice(fixture, "cpu", fixture.claimAt, fixture.releaseAt)
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&slice).Error, "invalid billing shared estimated charge slice")
}

func TestSQLiteSharedRequestSliceRequiresExactTariffRateAndAmount(t *testing.T) {
	fixture := seedSQLiteSharedCostAllocationFixture(t, time.Minute, 3*time.Minute, 5*time.Minute, true)
	slice := validSharedTenantChargeSlice(fixture, "request", fixture.claimAt, fixture.releaseAt)
	slice.BillableSeconds = 0
	slice.AmountMicros++
	assertSharedAllocationSQLiteRejected(t, fixture.db.Create(&slice).Error, "invalid billing shared estimated charge slice")
}

func seedSQLiteSharedCostAllocationFixture(
	t *testing.T,
	claimOffset time.Duration,
	releaseOffset time.Duration,
	terminationOffset time.Duration,
	withRelease bool,
) sharedCostAllocationFixture {
	return seedSQLiteSharedCostAllocationFixtureWithRunEnd(
		t,
		claimOffset,
		releaseOffset,
		terminationOffset,
		terminationOffset,
		withRelease,
	)
}

func seedSQLiteSharedCostAllocationFixtureWithRunEnd(
	t *testing.T,
	claimOffset time.Duration,
	releaseOffset time.Duration,
	terminationOffset time.Duration,
	runEndOffset time.Duration,
	withRelease bool,
) sharedCostAllocationFixture {
	t.Helper()
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Truncate(time.Second)
	claimAt := base.Add(claimOffset)
	releaseAt := base.Add(releaseOffset)
	terminatedAt := base.Add(terminationOffset)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "shared-allocation-target-" + uuid.NewString()[:8], Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{}, CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatalf("create shared allocation target: %v", err)
	}
	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: target.ID,
		TargetKind: target.Kind, WorkerMode: "general-pool", RegistrationTrustMode: "kubernetes-pod-bound-v1",
		ClusterID: "kubernetes", Namespace: "default", PodName: "shared-allocation-worker",
		Version: "test", ProtocolVersion: 2, Capabilities: map[string]any{}, LeaseSupported: true,
		FencingSupported: true, AuthTokenHash: []byte("hash"), Status: "online", AdministrativeStatus: "active",
		RegisteredAt: base, LastHeartbeatAt: base,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatalf("create shared allocation worker: %v", err)
	}
	workerFact := persistence.WorkerIncarnationFact{
		WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, ExecutionTargetID: target.ID,
		TargetKind: target.Kind, WorkerMode: worker.WorkerMode, ClusterID: worker.ClusterID, Region: "us-east-1",
		Namespace: worker.Namespace, PodName: worker.PodName, InstanceUID: worker.InstanceUID, RegisteredAt: base,
		CurrentState: "active", StateChangedAt: base, ClaimCount: 1, CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&workerFact).Error; err != nil {
		t.Fatalf("create shared allocation worker fact: %v", err)
	}

	project := persistence.Project{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "shared allocation project", DefaultBranch: "main", Visibility: "private",
		CreatedBy: domain.UserID, CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&project).Error; err != nil {
		t.Fatalf("create shared allocation project: %v", err)
	}
	session := persistence.AgentSession{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: project.ID,
		CreatedBy: domain.UserID, Title: "shared allocation session", Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: target.ID, CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatalf("create shared allocation session: %v", err)
	}
	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: session.ID, CreatedBy: domain.UserID,
		Status: "running", InputText: "shared allocation", StartedAt: &base, CreatedAt: base,
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("create shared allocation turn: %v", err)
	}
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: session.ID, TurnID: turn.ID, Attempt: 1,
		Status: "leased", ExecutionTargetID: target.ID, TargetKind: target.Kind, WorkerID: &worker.ID,
		ProviderResumeStrategySnapshot: "authoritative-history", Generation: 1, RequestedBy: domain.UserID,
		QueuedAt: base, StartedAt: &base,
	}
	if err := db.Create(&execution).Error; err != nil {
		t.Fatalf("create shared allocation execution: %v", err)
	}
	lease := persistence.WorkerLease{
		ExecutionID: execution.ID, TenantID: domain.TenantID, WorkerID: worker.ID,
		WorkerIncarnation: worker.Incarnation, WorkerInstanceUID: worker.InstanceUID, Generation: 1,
		LeaseTokenHash: []byte("shared-allocation-lease"), AcquiredAt: base, HeartbeatAt: base,
		ExpiresAt: terminatedAt.Add(time.Hour),
	}
	if err := db.Create(&lease).Error; err != nil {
		t.Fatalf("create shared allocation lease: %v", err)
	}
	generation := int64(1)
	claim := persistence.WorkerClaimFact{
		ID: uuid.New(), WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, TenantID: domain.TenantID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, ClaimKind: "execution",
		RequestID: "shared-allocation-claim", ClaimedAt: claimAt, ExecutionID: &execution.ID,
		ExecutionGeneration: &generation, CreatedAt: claimAt,
	}
	if err := db.Create(&claim).Error; err != nil {
		t.Fatalf("create shared allocation claim: %v", err)
	}
	if withRelease {
		release := persistence.WorkerClaimReleaseFact{
			ClaimFactID: claim.ID, ReleasedAt: releaseAt, RecordedAt: releaseAt.Add(time.Second),
			ReleaseReason: "execution_completed", AuthorityKind: "control-plane", Metadata: map[string]any{},
		}
		if err := db.Create(&release).Error; err != nil {
			t.Fatalf("create shared allocation claim release: %v", err)
		}
	}
	if err := db.Delete(&persistence.WorkerLease{}, "execution_id = ?", execution.ID).Error; err != nil {
		t.Fatalf("remove shared allocation lease: %v", err)
	}
	if err := db.Model(&persistence.WorkerIncarnationFact{}).
		Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
		Updates(map[string]any{
			"current_state": "terminated", "state_changed_at": terminatedAt, "terminated_at": terminatedAt,
			"terminal_reason": "test-completed", "updated_at": terminatedAt,
		}).Error; err != nil {
		t.Fatalf("terminate shared allocation worker fact: %v", err)
	}
	workerFact.CurrentState = "terminated"
	workerFact.StateChangedAt = terminatedAt
	workerFact.TerminatedAt = &terminatedAt
	workerFact.UpdatedAt = terminatedAt

	coverage := persistence.BillingSharedTargetLedgerCoverage{
		ID: uuid.New(), ExecutionTargetID: target.ID, CompleteFromAt: base.Add(-time.Hour),
		MinimumWriterVersion: "control-plane-v1.0.0", DeploymentAttestationSHA256: strings.Repeat("a", 64),
		SealedAt: base.Add(-time.Minute), SealedBy: domain.UserID,
	}
	if err := db.Create(&coverage).Error; err != nil {
		t.Fatalf("create shared allocation ledger coverage: %v", err)
	}
	tariff := persistence.BillingProviderTariff{
		ID: uuid.New(), Provider: "aws", Region: "", CurrencyCode: "USD", Version: 1,
		EffectiveStartAt: base.Add(-time.Hour), EffectiveEndAt: &terminatedAt,
		CPUCoreHourRateMicros: 1, MemoryGiBHourRateMicros: 1,
		EphemeralGiBHourRateMicros: 1, RequestRateMicros: 1, PodHourRateMicros: 1, CreatedAt: base,
	}
	if err := db.Create(&tariff).Error; err != nil {
		t.Fatalf("create shared allocation global fallback tariff: %v", err)
	}
	periodEnd := base.Add(10 * time.Minute)
	runUsageEnd := base.Add(runEndOffset)
	run := persistence.BillingSharedCostAllocationRun{
		ID: uuid.New(), ExecutionTargetID: target.ID, WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
		LedgerCoverageID: coverage.ID, Provider: "aws", Region: "us-east-1", CurrencyCode: "USD",
		BillingPeriodStartAt: base, BillingPeriodEndAt: periodEnd, AlgorithmVersion: "closed-claim-interval-v1",
		UsageStartAt: base, UsageEndAt: runUsageEnd, ClaimCount: 1, ReleaseCount: 1,
		LedgerSHA256: strings.Repeat("b", 64), TenantAllocatedSeconds: int64(releaseAt.Sub(claimAt).Seconds()),
		PlatformIdleSeconds: int64(runUsageEnd.Sub(base).Seconds()) - int64(releaseAt.Sub(claimAt).Seconds()),
		CreatedAt:           terminatedAt.Add(time.Minute),
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatalf("create shared cost allocation run: %v", err)
	}
	return sharedCostAllocationFixture{
		db: db, tenantID: domain.TenantID, target: target, workerFact: workerFact, claim: claim,
		coverage: coverage, tariff: tariff, run: run, claimAt: claimAt, releaseAt: releaseAt,
		terminatedAt: terminatedAt,
	}
}

func validSharedTenantChargeSlice(
	fixture sharedCostAllocationFixture,
	chargeKind string,
	usageStart time.Time,
	usageEnd time.Time,
) persistence.BillingSharedEstimatedChargeSlice {
	return persistence.BillingSharedEstimatedChargeSlice{
		ID: uuid.New(), RunID: fixture.run.ID, TenantID: &fixture.tenantID, ClaimFactID: &fixture.claim.ID,
		TariffID: fixture.tariff.ID, ChargeKind: chargeKind, AllocationKind: "tenant-claim",
		ResourceCorrelationKey: "shared-worker/" + fixture.run.WorkerID.String(),
		BillingPeriodStartAt:   fixture.run.BillingPeriodStartAt, BillingPeriodEndAt: fixture.run.BillingPeriodEndAt,
		UsageStartAt: usageStart, UsageEndAt: usageEnd, BillableSeconds: int64(usageEnd.Sub(usageStart).Seconds()),
		ClaimCount: 1, RateMicros: 1, AmountMicros: 1, CreatedAt: fixture.run.CreatedAt,
	}
}

func sharedAllocationInt64Pointer(value int64) *int64 {
	return &value
}

func validSharedIdleChargeSlice(
	fixture sharedCostAllocationFixture,
	usageStart time.Time,
	usageEnd time.Time,
) persistence.BillingSharedEstimatedChargeSlice {
	return persistence.BillingSharedEstimatedChargeSlice{
		ID: uuid.New(), RunID: fixture.run.ID, TariffID: fixture.tariff.ID,
		ChargeKind: "pod", AllocationKind: "platform-idle",
		ResourceCorrelationKey: "shared-worker/" + fixture.run.WorkerID.String(),
		BillingPeriodStartAt:   fixture.run.BillingPeriodStartAt, BillingPeriodEndAt: fixture.run.BillingPeriodEndAt,
		UsageStartAt: usageStart, UsageEndAt: usageEnd, BillableSeconds: int64(usageEnd.Sub(usageStart).Seconds()),
		ClaimCount: 0, RateMicros: 1, AmountMicros: 1, CreatedAt: fixture.run.CreatedAt,
	}
}

func assertSharedAllocationSQLiteRejected(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), expected) {
		t.Fatalf("SQLite accepted invalid shared allocation state or returned wrong error: %v (want containing %q)", err, expected)
	}
}

func uuidPointer(value uuid.UUID) *uuid.UUID { return &value }
