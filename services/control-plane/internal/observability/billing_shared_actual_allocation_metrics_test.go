package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestBillingSharedActualAllocationMetricsExposeBoundedConservationGap(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.BillingSharedActualAllocationRun{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	sealedAt := now
	for _, run := range []persistence.BillingSharedActualAllocationRun{
		{
			ID: uuid.New(), OperatorTenantID: uuid.New(), InvoiceImportID: uuid.New(), ExecutionTargetID: uuid.New(),
			LedgerCoverageID: uuid.New(), Provider: "aws", CurrencyCode: "USD",
			BillingPeriodStartAt: now.Add(-time.Hour), BillingPeriodEndAt: now,
			AlgorithmVersion: "proportional-shared-estimate-v1",
			SourceChecksum:   strings.Repeat("a", 64), SourceScopeAttestationSHA256: strings.Repeat("b", 64),
			SourceLineSetSHA256: strings.Repeat("c", 64), ImportLineCount: 3, ImportAmountMicros: 90,
			SourceLineCount: 2, SourceAmountMicros: 100, UnallocatedLineCount: 1, UnallocatedAmountMicros: -10,
			AllocationLineCount: 2, AllocationSliceCount: 4, AllocatedAmountMicros: 100,
			State: "sealed", CreatedBy: uuid.New(), CreatedAt: now, SealedAt: &sealedAt,
		},
		{
			ID: uuid.New(), OperatorTenantID: uuid.New(), InvoiceImportID: uuid.New(), ExecutionTargetID: uuid.New(),
			LedgerCoverageID: uuid.New(), Provider: "aws", CurrencyCode: "USD",
			BillingPeriodStartAt: now.Add(-2 * time.Hour), BillingPeriodEndAt: now.Add(-time.Hour),
			AlgorithmVersion: "proportional-shared-estimate-v1",
			SourceChecksum:   strings.Repeat("d", 64), SourceScopeAttestationSHA256: strings.Repeat("e", 64),
			SourceLineSetSHA256: strings.Repeat("f", 64), ImportLineCount: 1, ImportAmountMicros: 25,
			SourceLineCount: 1, SourceAmountMicros: 25, AllocationLineCount: 1, AllocationSliceCount: 1,
			AllocatedAmountMicros: 25, State: "sealed", CreatedBy: uuid.New(), CreatedAt: now, SealedAt: &sealedAt,
		},
	} {
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := New(db).writeBillingSharedActualAllocationMetrics(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	metrics := output.String()
	for _, expected := range []string{
		`synara_billing_shared_actual_allocation_runs{currency="USD",provider="aws",state="sealed"} 2`,
		`synara_billing_shared_actual_allocation_lines{currency="USD",kind="selected",provider="aws",state="sealed"} 3`,
		`synara_billing_shared_actual_allocation_lines{currency="USD",kind="unallocated",provider="aws",state="sealed"} 1`,
		`synara_billing_shared_actual_allocation_amount_micros{currency="USD",kind="selected",provider="aws",state="sealed"} 125`,
		`synara_billing_shared_actual_allocation_amount_micros{currency="USD",kind="unallocated",provider="aws",state="sealed"} -10`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("shared actual metrics omitted %q:\n%s", expected, metrics)
		}
	}
}
