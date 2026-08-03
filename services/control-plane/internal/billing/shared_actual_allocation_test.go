package billing

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestAllocateSharedActualInvoiceConservesSignedMicrosAndReplaysConcurrentFirstWrite(t *testing.T) {
	fixture := newSharedActualAllocationFixture(t, 11_000_003, 250_000)
	input := fixture.input(strings.Repeat("b", 64))

	type outcome struct {
		result SharedActualInvoiceAllocationResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			result, err := fixture.service.AllocateSharedActualInvoiceAuthorized(
				context.Background(),
				fixture.principal,
				fixture.operatorTenantID,
				input,
				"shared-actual-concurrent-"+uuid.NewString(),
				"127.0.0.1",
			)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	first := <-outcomes
	second := <-outcomes
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent shared actual allocations: first=%v second=%v", first.err, second.err)
	}
	if first.result.Run.ID == uuid.Nil || first.result.Run.ID != second.result.Run.ID || first.result.Created == second.result.Created {
		t.Fatalf("concurrent shared actual identities: first=%#v second=%#v", first.result, second.result)
	}
	result := first.result
	if !result.Created {
		result = second.result
	}
	if result.Run.State != SharedActualAllocationStateSealed || result.Run.SealedAt == nil ||
		result.Run.AlgorithmVersion != SharedActualAllocationAlgorithmProportionalEstimateV1 ||
		result.Run.ImportLineCount != 2 || result.Run.SourceLineCount != 1 ||
		result.Run.UnallocatedLineCount != 1 || result.Run.SourceAmountMicros != 11_000_003 ||
		result.Run.AllocatedAmountMicros != result.Run.SourceAmountMicros ||
		result.Run.ImportAmountMicros != 11_250_003 || result.Run.UnallocatedAmountMicros != 250_000 ||
		len(result.Run.SourceLineSetSHA256) != 64 {
		t.Fatalf("unexpected shared actual run: %#v", result.Run)
	}

	var lines []persistence.BillingSharedActualAllocationLine
	if err := fixture.db.Where("run_id = ?", result.Run.ID).Find(&lines).Error; err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].SourceAmountMicros != 11_000_003 ||
		lines[0].AllocatedAmountMicros != lines[0].SourceAmountMicros || lines[0].EstimatedSliceCount <= 1 {
		t.Fatalf("unexpected shared actual lines: %#v", lines)
	}
	var slices []persistence.BillingSharedActualChargeSlice
	if err := fixture.db.Where("allocation_line_id = ?", lines[0].ID).
		Order("estimated_slice_id").Find(&slices).Error; err != nil {
		t.Fatal(err)
	}
	if int64(len(slices)) != lines[0].EstimatedSliceCount || int64(len(slices)) != result.Run.AllocationSliceCount {
		t.Fatalf("actual slice count = %d, line=%d run=%d", len(slices), lines[0].EstimatedSliceCount, result.Run.AllocationSliceCount)
	}
	var allocatedTotal, estimateTotal int64
	kinds := map[string]bool{}
	for _, slice := range slices {
		var err error
		allocatedTotal, err = addInt64Checked(allocatedTotal, slice.AmountMicros)
		if err != nil {
			t.Fatal(err)
		}
		estimateTotal, err = addInt64Checked(estimateTotal, slice.EstimateWeightMicros)
		if err != nil {
			t.Fatal(err)
		}
		kinds[slice.AllocationKind] = true
	}
	if allocatedTotal != result.Run.SourceAmountMicros || estimateTotal != lines[0].EstimatedAmountMicros ||
		!kinds[SharedAllocationKindTenantClaim] || !kinds[SharedAllocationKindPlatformIdle] {
		t.Fatalf("shared actual conservation = actual:%d estimate:%d kinds:%v", allocatedTotal, estimateTotal, kinds)
	}

	var runCount, lineCount, sliceCount, auditCount int64
	for model, count := range map[any]*int64{
		&persistence.BillingSharedActualAllocationRun{}:  &runCount,
		&persistence.BillingSharedActualAllocationLine{}: &lineCount,
		&persistence.BillingSharedActualChargeSlice{}:    &sliceCount,
	} {
		if err := fixture.db.Model(model).Count(count).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ?", fixture.operatorTenantID, "cost_accounting.shared_actual_invoice_allocated").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || lineCount != 1 || sliceCount != int64(len(slices)) || auditCount != 1 {
		t.Fatalf("shared actual retained counts = run:%d line:%d slice:%d audit:%d", runCount, lineCount, sliceCount, auditCount)
	}

	conflicting := input
	conflicting.SourceScopeAttestationSHA256 = strings.Repeat("c", 64)
	_, err := fixture.service.AllocateSharedActualInvoiceAuthorized(
		context.Background(), fixture.principal, fixture.operatorTenantID, conflicting,
		"shared-actual-conflict", "127.0.0.1",
	)
	assertProblemCode(t, err, "cost_accounting_shared_actual_allocation_conflict")
}

func TestAllocateSharedActualInvoicePreservesNegativeInvoiceLineConservation(t *testing.T) {
	fixture := newSharedActualAllocationFixture(t, -7, 0)
	result, err := fixture.service.AllocateSharedActualInvoiceAuthorized(
		context.Background(), fixture.principal, fixture.operatorTenantID,
		fixture.input(strings.Repeat("d", 64)), "shared-actual-negative", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	var slices []persistence.BillingSharedActualChargeSlice
	if err := fixture.db.Table("billing_shared_actual_charge_slices AS actual_slice").
		Select("actual_slice.*").
		Joins("JOIN billing_shared_actual_allocation_lines AS allocation_line ON allocation_line.id = actual_slice.allocation_line_id").
		Where("allocation_line.run_id = ?", result.Run.ID).
		Find(&slices).Error; err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, slice := range slices {
		if slice.AmountMicros > 0 {
			t.Fatalf("negative source produced positive slice: %#v", slice)
		}
		var addErr error
		sum, addErr = addInt64Checked(sum, slice.AmountMicros)
		if addErr != nil {
			t.Fatal(addErr)
		}
	}
	if sum != -7 || result.Run.SourceAmountMicros != -7 || result.Run.AllocatedAmountMicros != -7 {
		t.Fatalf("negative actual conservation = sum:%d run:%#v", sum, result.Run)
	}
}

func TestAllocateSharedActualInvoiceFailsClosedOnCrossTargetAmbiguity(t *testing.T) {
	fixture := newSharedActualAllocationFixture(t, 1_000_000, 0)
	var sourceEstimate persistence.BillingSharedEstimatedChargeSlice
	if err := fixture.db.
		Where("charge_kind = ? AND resource_correlation_key = ?", fixture.chargeKind, fixture.resourceKey).
		First(&sourceEstimate).Error; err != nil {
		t.Fatal(err)
	}
	var sourceRun persistence.BillingSharedCostAllocationRun
	if err := fixture.db.Where("id = ?", sourceEstimate.RunID).Take(&sourceRun).Error; err != nil {
		t.Fatal(err)
	}
	otherTarget := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "shared-actual-ambiguous", Status: "active",
		Capabilities: map[string]any{}, CreatedAt: fixture.base.Add(-time.Hour), UpdatedAt: fixture.base.Add(-time.Hour),
	}
	if err := fixture.db.Create(&otherTarget).Error; err != nil {
		t.Fatal(err)
	}
	otherRun := sourceRun
	otherRun.ID = uuid.New()
	otherRun.ExecutionTargetID = otherTarget.ID
	otherRun.WorkerID = uuid.New()
	if err := fixture.db.Omit(clause.Associations).Create(&otherRun).Error; err != nil {
		t.Fatal(err)
	}
	otherSlice := sourceEstimate
	otherSlice.ID = uuid.New()
	otherSlice.RunID = otherRun.ID
	if err := fixture.db.Omit(clause.Associations).Create(&otherSlice).Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.AllocateSharedActualInvoiceAuthorized(
		context.Background(), fixture.principal, fixture.operatorTenantID,
		fixture.input(strings.Repeat("e", 64)), "shared-actual-ambiguous", "127.0.0.1",
	)
	assertProblemCode(t, err, "cost_accounting_shared_actual_allocation_target_ambiguous")
	var runCount int64
	if err := fixture.db.Model(&persistence.BillingSharedActualAllocationRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 0 {
		t.Fatalf("ambiguous allocation retained %d runs", runCount)
	}
}

func TestAllocateSharedActualMicrosUsesExactCumulativeIntegerConservation(t *testing.T) {
	amounts, estimated, err := allocateSharedActualMicros(-7, []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if estimated != 6 || len(amounts) != 3 || amounts[0] != -1 || amounts[1] != -2 || amounts[2] != -4 {
		t.Fatalf("cumulative allocation = amounts:%v estimated:%d", amounts, estimated)
	}
	amounts, estimated, err = allocateSharedActualMicros(math.MinInt64, []int64{1})
	if err != nil || estimated != 1 || len(amounts) != 1 || amounts[0] != math.MinInt64 {
		t.Fatalf("min-int allocation = amounts:%v estimated:%d err:%v", amounts, estimated, err)
	}
	_, _, err = allocateSharedActualMicros(1, []int64{0, 0})
	assertProblemCode(t, err, "cost_accounting_shared_actual_allocation_basis_zero")
}

type sharedActualFixture struct {
	db               *gorm.DB
	service          *Service
	base             time.Time
	operatorTenantID uuid.UUID
	principal        identity.Principal
	targetID         uuid.UUID
	invoiceImportID  uuid.UUID
	resourceKey      string
	chargeKind       string
}

func newSharedActualAllocationFixture(t *testing.T, matchedAmount, unmatchedAmount int64) sharedActualFixture {
	t.Helper()
	shared := newSharedAllocationFixture(t, 2)
	shared.seedClaim(t, shared.tenantA, shared.base.Add(10*time.Minute), shared.base.Add(50*time.Minute), 1)
	shared.seedClaim(t, shared.tenantB, shared.base.Add(70*time.Minute), shared.base.Add(100*time.Minute), 2)
	estimate, err := shared.service.AllocateSharedUsageCharges(context.Background(), shared.input())
	if err != nil {
		t.Fatal(err)
	}
	resourceKey := ""
	for _, slice := range estimate.Slices {
		if slice.ChargeKind == ChargeKindPod {
			resourceKey = slice.ResourceCorrelationKey
			break
		}
	}
	if resourceKey == "" {
		t.Fatal("shared estimate fixture produced no Pod resource key")
	}

	operatorUserID := uuid.New()
	joinedAt := shared.base.Add(-2 * time.Hour)
	if err := shared.db.Create(&persistence.User{
		ID:              operatorUserID,
		Email:           "shared-actual-" + uuid.NewString() + "@example.com",
		DisplayName:     "Shared actual billing operator",
		Status:          "active",
		EmailVerifiedAt: &joinedAt,
		CreatedAt:       joinedAt,
		UpdatedAt:       joinedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := shared.db.Create(&persistence.TenantMembership{
		TenantID:  shared.tenantA,
		UserID:    operatorUserID,
		Role:      "owner",
		Status:    "active",
		JoinedAt:  &joinedAt,
		CreatedAt: joinedAt,
		UpdatedAt: joinedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}

	importID := uuid.New()
	importedAt := shared.base.Add(2*time.Hour + time.Minute)
	invoiceImport := persistence.BillingActualInvoiceImport{
		ID:                   importID,
		TenantID:             shared.tenantA,
		Provider:             "aws",
		ExternalImportID:     "shared-account-" + uuid.NewString(),
		BillingPeriodStartAt: shared.base,
		BillingPeriodEndAt:   shared.base.Add(2 * time.Hour),
		CurrencyCode:         "USD",
		SourceChecksum:       strings.Repeat("a", 64),
		ImportedAt:           importedAt,
		CreatedAt:            importedAt,
	}
	if err := shared.db.Omit(clause.Associations).Create(&invoiceImport).Error; err != nil {
		t.Fatal(err)
	}
	lines := []persistence.BillingActualInvoiceLine{
		{
			ID: uuid.New(), TenantID: shared.tenantA, InvoiceImportID: importID,
			ExternalLineID: "shared-pod", Provider: "aws", CurrencyCode: "USD",
			ChargeKind: ChargeKindPod, ResourceCorrelationKey: resourceKey,
			BillingPeriodStartAt: shared.base, BillingPeriodEndAt: shared.base.Add(2 * time.Hour),
			AmountMicros: matchedAmount, ReconciliationState: reconciliationStatePending,
			CreatedAt: importedAt,
		},
		{
			ID: uuid.New(), TenantID: shared.tenantA, InvoiceImportID: importID,
			ExternalLineID: "unallocated-cpu", Provider: "aws", CurrencyCode: "USD",
			ChargeKind: ChargeKindCPU, ResourceCorrelationKey: "aws:unallocated:" + uuid.NewString(),
			BillingPeriodStartAt: shared.base, BillingPeriodEndAt: shared.base.Add(2 * time.Hour),
			AmountMicros: unmatchedAmount, ReconciliationState: reconciliationStatePending,
			CreatedAt: importedAt,
		},
	}
	if err := shared.db.Omit(clause.Associations).Create(&lines).Error; err != nil {
		t.Fatal(err)
	}

	service := NewService(shared.db, nil, WithPlatformBillingOperatorTenant(shared.tenantA))
	service.now = func() time.Time { return shared.base.Add(3 * time.Hour) }
	activeTenantID := shared.tenantA
	return sharedActualFixture{
		db:               shared.db,
		service:          service,
		base:             shared.base,
		operatorTenantID: shared.tenantA,
		principal:        identity.Principal{UserID: operatorUserID, ActiveTenantID: &activeTenantID},
		targetID:         shared.target.ID,
		invoiceImportID:  importID,
		resourceKey:      resourceKey,
		chargeKind:       ChargeKindPod,
	}
}

func (fixture sharedActualFixture) input(attestation string) AllocateSharedActualInvoiceInput {
	return AllocateSharedActualInvoiceInput{
		ExecutionTargetID:            fixture.targetID,
		InvoiceImportID:              fixture.invoiceImportID,
		SourceScopeAttestationSHA256: attestation,
	}
}
