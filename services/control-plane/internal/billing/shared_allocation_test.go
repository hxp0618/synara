package billing

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestAllocateSharedUsageChargesConservesSequentialClaimsIdleTariffsAndReplay(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 3)
	claimA := fixture.seedClaim(
		t,
		fixture.tenantA,
		fixture.base.Add(15*time.Minute+400*time.Millisecond),
		fixture.base.Add(45*time.Minute+600*time.Millisecond),
		1,
	)
	claimB := fixture.seedClaim(
		t,
		fixture.tenantB,
		fixture.base.Add(75*time.Minute+200*time.Millisecond),
		fixture.base.Add(105*time.Minute+900*time.Millisecond),
		2,
	)
	boundaryClaim := fixture.seedClaim(t, fixture.tenantB, fixture.base.Add(2*time.Hour), fixture.base.Add(2*time.Hour), 3)

	result, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	if result.Run == nil {
		t.Fatal("shared allocation did not persist a run")
	}
	if result.Run.AlgorithmVersion != SharedAllocationAlgorithmClosedClaimIntervalV1 ||
		result.Run.ClaimCount != 3 || result.Run.ReleaseCount != 3 ||
		result.Run.TenantAllocatedSeconds != 3600 || result.Run.PlatformIdleSeconds != 3600 ||
		len(result.Run.LedgerSHA256) != 64 {
		t.Fatalf("unexpected shared allocation run: %#v", result.Run)
	}
	if len(result.Slices) != 27 {
		t.Fatalf("slice count = %d, want 27", len(result.Slices))
	}

	requestClaims := map[uuid.UUID]bool{}
	for _, row := range result.Slices {
		switch row.AllocationKind {
		case SharedAllocationKindTenantClaim:
			if row.TenantID == nil || row.ClaimFactID == nil || row.ClaimCount != 1 {
				t.Fatalf("invalid tenant-claim slice: %#v", row)
			}
			if row.ChargeKind == ChargeKindRequest {
				requestClaims[*row.ClaimFactID] = true
				if row.BillableSeconds != 0 {
					t.Fatalf("request billable seconds = %d, want 0", row.BillableSeconds)
				}
			}
		case SharedAllocationKindPlatformIdle:
			if row.TenantID != nil || row.ClaimFactID != nil || row.ClaimCount != 0 || row.ChargeKind == ChargeKindRequest {
				t.Fatalf("invalid platform-idle slice: %#v", row)
			}
		default:
			t.Fatalf("unexpected allocation kind %q", row.AllocationKind)
		}
	}
	for _, claimID := range []uuid.UUID{claimA.ID, claimB.ID, boundaryClaim.ID} {
		if !requestClaims[claimID] {
			t.Fatalf("request slice missing for claim %s", claimID)
		}
	}

	totals := sharedAllocationAmountsByKind(result.Slices)
	wantTotals := map[string]int64{
		ChargeKindCPU:              10_800_000,
		ChargeKindMemory:           6_000_000,
		ChargeKindEphemeralStorage: 6_000_000,
		ChargeKindRequest:          1_500_000,
		ChargeKindPod:              10_800_000,
	}
	for kind, want := range wantTotals {
		if totals[kind] != want {
			t.Fatalf("%s amount = %d, want %d (all=%#v)", kind, totals[kind], want, totals)
		}
	}

	replayed, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Run == nil || replayed.Run.ID != result.Run.ID || !sameSharedAllocationSlices(replayed.Slices, result.Slices) {
		t.Fatalf("shared allocation replay changed identity: first=%#v replay=%#v", result.Run, replayed.Run)
	}
	var runCount, sliceCount int64
	if err := fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.BillingSharedEstimatedChargeSlice{}).Count(&sliceCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || sliceCount != int64(len(result.Slices)) {
		t.Fatalf("persisted counts = runs:%d slices:%d", runCount, sliceCount)
	}
}

func TestAllocateSharedUsageChargesFailsClosedOnIncompleteOrAmbiguousAuthority(t *testing.T) {
	t.Run("missing coverage", func(t *testing.T) {
		fixture := newSharedAllocationFixture(t, 0)
		if err := fixture.db.Delete(&persistence.BillingSharedTargetLedgerCoverage{}, "id = ?", fixture.coverage.ID).Error; err != nil {
			t.Fatal(err)
		}
		_, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
		assertProblemCode(t, err, "billing_shared_ledger_coverage_missing")
		fixture.assertNoRuns(t)
	})

	t.Run("worker predates sealed complete boundary", func(t *testing.T) {
		fixture := newSharedAllocationFixture(t, 0)
		completeFromAt := fixture.base.Add(time.Minute)
		sealedAt := fixture.base.Add(2 * time.Minute)
		if err := fixture.db.Model(&persistence.BillingSharedTargetLedgerCoverage{}).
			Where("id = ?", fixture.coverage.ID).
			Updates(map[string]any{"complete_from_at": completeFromAt, "sealed_at": sealedAt}).Error; err != nil {
			t.Fatal(err)
		}
		_, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
		assertProblemCode(t, err, "billing_shared_ledger_coverage_incomplete")
		fixture.assertNoRuns(t)
	})

	t.Run("claim count drift", func(t *testing.T) {
		fixture := newSharedAllocationFixture(t, 2)
		fixture.seedClaim(t, fixture.tenantA, fixture.base.Add(10*time.Minute), fixture.base.Add(20*time.Minute), 1)
		_, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
		assertProblemCode(t, err, "billing_shared_claim_ledger_incomplete")
		fixture.assertNoRuns(t)
	})

	t.Run("missing release", func(t *testing.T) {
		fixture := newSharedAllocationFixture(t, 1)
		fixture.seedClaimWithoutRelease(t, fixture.tenantA, fixture.base.Add(10*time.Minute), 1)
		_, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
		assertProblemCode(t, err, "billing_shared_release_ledger_incomplete")
		fixture.assertNoRuns(t)
	})

	t.Run("overlapping tenant intervals", func(t *testing.T) {
		fixture := newSharedAllocationFixture(t, 2)
		fixture.seedClaim(t, fixture.tenantA, fixture.base.Add(10*time.Minute), fixture.base.Add(40*time.Minute), 1)
		fixture.seedClaim(t, fixture.tenantB, fixture.base.Add(30*time.Minute), fixture.base.Add(50*time.Minute), 2)
		_, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
		assertProblemCode(t, err, "billing_shared_claim_intervals_overlap")
		fixture.assertNoRuns(t)
	})

	t.Run("non terminal worker", func(t *testing.T) {
		fixture := newSharedAllocationFixture(t, 0)
		if err := fixture.db.Model(&persistence.WorkerIncarnationFact{}).
			Where("worker_id = ? AND worker_incarnation = ?", fixture.worker.WorkerID, fixture.worker.WorkerIncarnation).
			Updates(map[string]any{"current_state": "idle", "terminated_at": nil, "terminal_reason": nil}).Error; err != nil {
			t.Fatal(err)
		}
		_, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
		assertProblemCode(t, err, "billing_shared_worker_not_terminal")
		fixture.assertNoRuns(t)
	})
}

func TestAllocateSharedUsageChargesRejectsPersistedReplayDrift(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 1)
	fixture.seedClaim(t, fixture.tenantA, fixture.base.Add(10*time.Minute), fixture.base.Add(20*time.Minute), 1)
	result, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	if result.Run == nil {
		t.Fatal("missing first allocation run")
	}
	if err := fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).
		Where("id = ?", result.Run.ID).
		UpdateColumn("ledger_sha256", strings.Repeat("f", 64)).Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
	assertProblemCode(t, err, "billing_shared_allocation_conflict")
}

func TestAllocateSharedUsageChargesSQLiteConcurrentFirstWriteReplaysWinner(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 1)
	fixture.seedClaim(t, fixture.tenantA, fixture.base.Add(10*time.Minute), fixture.base.Add(20*time.Minute), 1)
	type outcome struct {
		result SharedUsageAllocationResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			result, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	first := <-outcomes
	second := <-outcomes
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent SQLite allocation errors: first=%v second=%v", first.err, second.err)
	}
	if first.result.Run == nil || second.result.Run == nil || first.result.Run.ID != second.result.Run.ID ||
		!sameSharedAllocationSlices(first.result.Slices, second.result.Slices) {
		t.Fatalf("concurrent SQLite allocation identities: first=%#v second=%#v", first.result.Run, second.result.Run)
	}
	var runCount int64
	if err := fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Fatalf("concurrent SQLite allocation run count = %d, want 1", runCount)
	}
}

func TestAllocateSharedUsageChargesFailsClosedWhenRatedResourceFactIsMissing(t *testing.T) {
	for _, column := range []string{
		"requested_cpu_millicores",
		"requested_memory_bytes",
		"requested_ephemeral_storage_bytes",
	} {
		t.Run(column, func(t *testing.T) {
			fixture := newSharedAllocationFixture(t, 0)
			if err := fixture.db.Model(&persistence.WorkerIncarnationFact{}).
				Where("worker_id = ? AND worker_incarnation = ?", fixture.worker.WorkerID, fixture.worker.WorkerIncarnation).
				UpdateColumn(column, nil).Error; err != nil {
				t.Fatal(err)
			}
			_, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
			assertProblemCode(t, err, "billing_shared_requested_resource_invalid")
			fixture.assertNoRuns(t)
		})
	}
}

func TestAllocateSharedUsageChargesRejectsOverlappingImmutablePeriod(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 0)
	first, err := fixture.service.AllocateSharedUsageCharges(context.Background(), fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	if first.Run == nil {
		t.Fatal("missing first shared allocation run")
	}
	overlap := fixture.input()
	overlap.BillingPeriodStartAt = fixture.base.Add(30 * time.Minute)
	overlap.BillingPeriodEndAt = fixture.base.Add(90 * time.Minute)
	_, err = fixture.service.AllocateSharedUsageCharges(context.Background(), overlap)
	assertProblemCode(t, err, "billing_shared_allocation_period_overlap")
	var runCount int64
	if err := fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Fatalf("overlap rejection retained %d runs, want 1", runCount)
	}
}

func TestSharedClaimLedgerSHA256BindsImmutableReleaseMetadata(t *testing.T) {
	fixture := newSharedAllocationFixture(t, 1)
	claim := fixture.seedClaim(t, fixture.tenantA, fixture.base.Add(10*time.Minute), fixture.base.Add(20*time.Minute), 1)
	fact := fixture.worker
	release := persistence.WorkerClaimReleaseFact{
		ClaimFactID: claim.ID, ReleasedAt: fixture.base.Add(20 * time.Minute),
		RecordedAt: fixture.base.Add(20*time.Minute + time.Second), ReleaseReason: "worker_released",
		AuthorityKind: "control-plane", Metadata: map[string]any{"proof": "a", "sequence": float64(1)},
	}
	interval := sharedClaimInterval{
		Claim: claim, Release: release, StartAt: claim.ClaimedAt, EndAt: release.ReleasedAt,
	}
	first, err := sharedClaimLedgerSHA256(fact, []sharedClaimInterval{interval})
	if err != nil {
		t.Fatal(err)
	}
	interval.Release.Metadata = map[string]any{"sequence": float64(1), "proof": "b"}
	second, err := sharedClaimLedgerSHA256(fact, []sharedClaimInterval{interval})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("release metadata change did not change the canonical ledger digest")
	}
}

func TestMergeAdjacentSharedChargeSegmentsIgnoresHiddenFallbackBoundary(t *testing.T) {
	base := time.Date(2026, time.July, 26, 8, 0, 0, 0, time.UTC)
	tariff := persistence.BillingProviderTariff{
		ID: uuid.New(), Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", PodHourRateMicros: 1,
	}
	segments := mergeAdjacentSharedChargeSegments([]chargeSegment{
		{StartAt: base, EndAt: base.Add(30 * time.Minute), Tariff: tariff},
		{StartAt: base.Add(30 * time.Minute), EndAt: base.Add(time.Hour), Tariff: tariff},
	})
	if len(segments) != 1 || !segments[0].StartAt.Equal(base) || !segments[0].EndAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("merged shared charge segments = %#v", segments)
	}
	candidate := sharedTimeChargeCandidate{Kind: ChargeKindPod, RateMicros: 1, Denominator: 3600}
	full, err := sharedTimeChargeAmountAt(candidate, 3600)
	if err != nil {
		t.Fatal(err)
	}
	half, err := sharedTimeChargeAmountAt(candidate, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if full != 1 || half+half != 2 {
		t.Fatalf("rounding fixture = full:%d split:%d", full, half+half)
	}
	terminatedAt := base.Add(time.Hour)
	fact := persistence.WorkerIncarnationFact{
		WorkerID: uuid.New(), WorkerIncarnation: 1, ExecutionTargetID: uuid.New(), TargetKind: "kubernetes",
		Region: "us-east-1", ClusterID: "cluster-a", Namespace: "default", PodName: "worker-a",
		InstanceUID: uuid.NewString(), RegisteredAt: base, TerminatedAt: &terminatedAt,
	}
	_, slices, err := buildSharedAllocationRows(
		fact,
		persistence.BillingSharedTargetLedgerCoverage{ID: uuid.New()},
		"aws",
		"USD",
		base,
		terminatedAt,
		base,
		terminatedAt,
		nil,
		nil,
		nil,
		strings.Repeat("a", 64),
		segments,
		terminatedAt.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(slices) != 1 || slices[0].ChargeKind != ChargeKindPod || slices[0].AmountMicros != 1 {
		t.Fatalf("merged shared allocation slices = %#v", slices)
	}
}

type sharedAllocationFixture struct {
	db       *gorm.DB
	service  *Service
	base     time.Time
	target   persistence.ExecutionTarget
	worker   persistence.WorkerIncarnationFact
	coverage persistence.BillingSharedTargetLedgerCoverage
	tenantA  uuid.UUID
	tenantB  uuid.UUID
}

func newSharedAllocationFixture(t *testing.T, claimCount int64) sharedAllocationFixture {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(filepath.Join(t.TempDir(), "shared-allocation.sqlite")),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.Tenant{},
		&persistence.ExecutionTarget{},
		&persistence.WorkerIncarnationFact{},
		&persistence.WorkerClaimFact{},
		&persistence.WorkerClaimReleaseFact{},
		&persistence.BillingProviderTariff{},
		&persistence.BillingSharedTargetLedgerCoverage{},
		&persistence.BillingSharedCostAllocationRun{},
		&persistence.BillingSharedEstimatedChargeSlice{},
	); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, time.July, 26, 8, 0, 0, 0, time.UTC)
	tenantA := uuid.New()
	tenantB := uuid.New()
	for index, tenantID := range []uuid.UUID{tenantA, tenantB} {
		if err := db.Create(&persistence.Tenant{
			ID: tenantID, Slug: "shared-allocation-" + uuid.NewString(), Name: "Shared allocation tenant",
			Status: "active", PlanCode: "test", Region: "us-east-1", Settings: map[string]any{},
			CreatedBy: uuid.New(), CreatedAt: base.Add(-time.Hour), UpdatedAt: base.Add(-time.Hour).Add(time.Duration(index) * time.Second),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "platform-shared-billing", Status: "active",
		Capabilities: map[string]any{}, CreatedAt: base.Add(-time.Hour), UpdatedAt: base.Add(-time.Hour),
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	cpu := int64(1000)
	memory := int64(2 * (1 << 30))
	ephemeral := int64(4 * (1 << 30))
	terminatedAt := base.Add(2 * time.Hour)
	terminalReason := "test-completed"
	worker := persistence.WorkerIncarnationFact{
		WorkerID: uuid.New(), WorkerIncarnation: 1,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, WorkerMode: "general-pool",
		ClusterID: "cluster-a", Region: "us-east-1", Namespace: "default", PodName: "shared-worker",
		InstanceUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RegisteredAt: base,
		CurrentState: "terminated", StateChangedAt: terminatedAt, TerminatedAt: &terminatedAt, TerminalReason: &terminalReason,
		ClaimCount: claimCount, RequestedCPUMillicores: &cpu, RequestedMemoryBytes: &memory,
		RequestedEphemeralStorageBytes: &ephemeral, CreatedAt: base, UpdatedAt: terminatedAt,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	coverage := persistence.BillingSharedTargetLedgerCoverage{
		ID: uuid.New(), ExecutionTargetID: target.ID, CompleteFromAt: base.Add(-time.Minute),
		MinimumWriterVersion: "stage4-test", DeploymentAttestationSHA256: strings.Repeat("a", 64),
		SealedAt: base.Add(-30 * time.Second), SealedBy: uuid.New(),
	}
	if err := db.Omit(clause.Associations).Create(&coverage).Error; err != nil {
		t.Fatal(err)
	}

	service := NewService(db, nil)
	service.now = func() time.Time { return base.Add(3 * time.Hour) }
	for _, input := range []CreateTariffInput{
		{
			Provider: "aws", Region: "us-east-1", CurrencyCode: "USD", Version: 1,
			EffectiveStartAt: base, EffectiveEndAt: sharedTimePointer(base.Add(time.Hour)),
			CPUCoreHourRateMicros: 3_600_000, MemoryGiBHourRateMicros: 1_000_000,
			EphemeralGiBHourRateMicros: 500_000, RequestRateMicros: 300_000, PodHourRateMicros: 3_600_000,
		},
		{
			Provider: "aws", Region: "", CurrencyCode: "USD", Version: 2,
			EffectiveStartAt: base.Add(time.Hour), EffectiveEndAt: sharedTimePointer(base.Add(2 * time.Hour)),
			CPUCoreHourRateMicros: 7_200_000, MemoryGiBHourRateMicros: 2_000_000,
			EphemeralGiBHourRateMicros: 1_000_000, RequestRateMicros: 600_000, PodHourRateMicros: 7_200_000,
		},
	} {
		if _, err := service.CreateTariff(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	return sharedAllocationFixture{
		db: db, service: service, base: base, target: target, worker: worker, coverage: coverage,
		tenantA: tenantA, tenantB: tenantB,
	}
}

func (fixture sharedAllocationFixture) input() AllocateSharedUsageChargesInput {
	return AllocateSharedUsageChargesInput{
		Provider: "aws", CurrencyCode: "USD", WorkerID: fixture.worker.WorkerID,
		WorkerIncarnation:    fixture.worker.WorkerIncarnation,
		BillingPeriodStartAt: fixture.base, BillingPeriodEndAt: fixture.base.Add(2 * time.Hour),
	}
}

func (fixture sharedAllocationFixture) seedClaim(
	t *testing.T,
	tenantID uuid.UUID,
	claimedAt time.Time,
	releasedAt time.Time,
	generation int64,
) persistence.WorkerClaimFact {
	t.Helper()
	claim := fixture.seedClaimWithoutRelease(t, tenantID, claimedAt, generation)
	release := persistence.WorkerClaimReleaseFact{
		ClaimFactID: claim.ID, ReleasedAt: releasedAt, RecordedAt: releasedAt.Add(time.Second),
		ReleaseReason: "worker_released", AuthorityKind: "control-plane", Metadata: map[string]any{},
	}
	if err := fixture.db.Omit(clause.Associations).Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	return claim
}

func (fixture sharedAllocationFixture) seedClaimWithoutRelease(
	t *testing.T,
	tenantID uuid.UUID,
	claimedAt time.Time,
	generation int64,
) persistence.WorkerClaimFact {
	t.Helper()
	executionID := uuid.New()
	claim := persistence.WorkerClaimFact{
		ID: uuid.New(), WorkerID: fixture.worker.WorkerID, WorkerIncarnation: fixture.worker.WorkerIncarnation,
		TenantID: tenantID, ExecutionTargetID: fixture.target.ID, TargetKind: fixture.target.Kind,
		ClaimKind: "execution", RequestID: "shared-request-" + uuid.NewString(), ClaimedAt: claimedAt,
		ExecutionID: &executionID, ExecutionGeneration: &generation, CreatedAt: claimedAt,
	}
	if err := fixture.db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}
	return claim
}

func (fixture sharedAllocationFixture) assertNoRuns(t *testing.T) {
	t.Helper()
	var count int64
	if err := fixture.db.Model(&persistence.BillingSharedCostAllocationRun{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("shared allocation run count = %d, want 0", count)
	}
}

func sharedAllocationAmountsByKind(slices []persistence.BillingSharedEstimatedChargeSlice) map[string]int64 {
	totals := make(map[string]int64)
	for _, row := range slices {
		totals[row.ChargeKind] += row.AmountMicros
	}
	return totals
}

func sharedTimePointer(value time.Time) *time.Time {
	return &value
}
