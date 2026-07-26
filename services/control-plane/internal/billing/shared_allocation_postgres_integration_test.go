package billing

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

func TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db := openBillingPostgresIntegrationDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(8)

	domainA, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	tenantB, organizationB := createPostgresSharedAllocationTenant(t, ctx, db, domainA.UserID, base)

	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "shared-allocation-pg-" + uuid.NewString()[:8], Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{}, CreatedAt: base, UpdatedAt: base,
	}
	if err := db.WithContext(ctx).Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: target.ID,
		TargetKind: target.Kind, WorkerMode: "general-pool", RegistrationTrustMode: "shared-token",
		ClusterID: "shared-allocation-pg", Namespace: "default", PodName: "shared-allocation-pg-worker",
		Version: "test", ProtocolVersion: 2, Capabilities: map[string]any{}, CompatibilityStatus: "unknown",
		WorkerReleaseStatus: "unmanaged", LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: secret.HashToken("shared-allocation-pg-" + uuid.NewString()),
		Status:        "online", AdministrativeStatus: "active", RegisteredAt: base, LastHeartbeatAt: base,
	}
	if err := db.WithContext(ctx).Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	cpu := int64(1000)
	memory := int64(2 * (1 << 30))
	ephemeral := int64(4 * (1 << 30))
	workerFact := persistence.WorkerIncarnationFact{
		WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, ExecutionTargetID: target.ID,
		TargetKind: target.Kind, WorkerMode: worker.WorkerMode, ClusterID: worker.ClusterID, Region: "us-east-1",
		Namespace: worker.Namespace, PodName: worker.PodName, InstanceUID: worker.InstanceUID, RegisteredAt: base,
		CurrentState: "active", StateChangedAt: base, ClaimCount: 3,
		RequestedCPUMillicores: &cpu, RequestedMemoryBytes: &memory,
		RequestedEphemeralStorageBytes: &ephemeral, CreatedAt: base, UpdatedAt: base,
	}
	if err := db.WithContext(ctx).Create(&workerFact).Error; err != nil {
		t.Fatal(err)
	}

	seedPostgresSharedAllocationClaim(
		t, ctx, db, worker, target, domainA.TenantID, domainA.OrganizationID, domainA.UserID,
		base.Add(15*time.Minute+400*time.Millisecond), base.Add(45*time.Minute+600*time.Millisecond), 1,
	)
	seedPostgresSharedAllocationClaim(
		t, ctx, db, worker, target, tenantB, organizationB, domainA.UserID,
		base.Add(75*time.Minute+200*time.Millisecond), base.Add(105*time.Minute+900*time.Millisecond), 2,
	)
	seedPostgresSharedAllocationClaim(
		t, ctx, db, worker, target, tenantB, organizationB, domainA.UserID,
		base.Add(2*time.Hour), base.Add(2*time.Hour), 3,
	)

	terminatedAt := base.Add(2 * time.Hour)
	if err := db.WithContext(ctx).Model(&persistence.WorkerIncarnationFact{}).
		Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
		Updates(map[string]any{
			"current_state": "terminated", "state_changed_at": terminatedAt, "terminated_at": terminatedAt,
			"terminal_reason": "test-completed", "updated_at": terminatedAt,
		}).Error; err != nil {
		t.Fatal(err)
	}

	coverage := persistence.BillingSharedTargetLedgerCoverage{
		ID: uuid.New(), ExecutionTargetID: target.ID, CompleteFromAt: base.Add(-time.Minute),
		MinimumWriterVersion: "stage4-pg-test", DeploymentAttestationSHA256: strings.Repeat("a", 64),
		SealedAt: base.Add(-30 * time.Second), SealedBy: domainA.UserID,
	}
	if err := db.WithContext(ctx).Create(&coverage).Error; err != nil {
		t.Fatal(err)
	}
	provider := "shared-pg-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	service := NewService(db, nil, WithPlatformBillingOperatorTenant(domainA.TenantID))
	service.now = func() time.Time { return base.Add(3 * time.Hour) }
	for _, input := range []CreateTariffInput{
		{
			Provider: provider, Region: "us-east-1", CurrencyCode: "USD", Version: 1,
			EffectiveStartAt: base, EffectiveEndAt: sharedTimePointer(base.Add(time.Hour)),
			CPUCoreHourRateMicros: 3_600_000, MemoryGiBHourRateMicros: 1_000_000,
			EphemeralGiBHourRateMicros: 500_000, RequestRateMicros: 300_000, PodHourRateMicros: 3_600_000,
		},
		{
			Provider: provider, Region: "", CurrencyCode: "USD", Version: 2,
			EffectiveStartAt: base.Add(time.Hour), EffectiveEndAt: sharedTimePointer(base.Add(2 * time.Hour)),
			CPUCoreHourRateMicros: 7_200_000, MemoryGiBHourRateMicros: 2_000_000,
			EphemeralGiBHourRateMicros: 1_000_000, RequestRateMicros: 600_000, PodHourRateMicros: 7_200_000,
		},
	} {
		if _, err := service.CreateTariff(ctx, input); err != nil {
			t.Fatal(err)
		}
	}

	input := AllocateSharedUsageChargesInput{
		Provider: provider, CurrencyCode: "USD", WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
		BillingPeriodStartAt: base, BillingPeriodEndAt: terminatedAt,
	}
	type outcome struct {
		result SharedUsageAllocationResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			result, allocationErr := service.AllocateSharedUsageCharges(ctx, input)
			outcomes <- outcome{result: result, err: allocationErr}
		}()
	}
	close(start)
	first := <-outcomes
	second := <-outcomes
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent shared allocation errors: first=%v second=%v", first.err, second.err)
	}
	if first.result.Run == nil || second.result.Run == nil || first.result.Run.ID != second.result.Run.ID {
		t.Fatalf("concurrent shared allocation identities: first=%#v second=%#v", first.result.Run, second.result.Run)
	}
	if first.result.Run.TenantAllocatedSeconds != 3600 || first.result.Run.PlatformIdleSeconds != 3600 ||
		len(first.result.Slices) != 27 || !sameSharedAllocationSlices(first.result.Slices, second.result.Slices) {
		t.Fatalf("unexpected concurrent shared allocation: first=%#v slices=%d secondSlices=%d",
			first.result.Run, len(first.result.Slices), len(second.result.Slices))
	}

	var runCount, sliceCount int64
	if err := db.WithContext(ctx).Model(&persistence.BillingSharedCostAllocationRun{}).
		Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
		Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&persistence.BillingSharedEstimatedChargeSlice{}).
		Where("run_id = ?", first.result.Run.ID).
		Count(&sliceCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || sliceCount != 27 {
		t.Fatalf("PostgreSQL shared allocation counts = runs:%d slices:%d", runCount, sliceCount)
	}
	sweep, err := service.SweepSharedUsageChargesAuthorized(ctx, identity.Principal{
		UserID: domainA.UserID, ActiveTenantID: &domainA.TenantID,
	}, domainA.TenantID, SweepSharedUsageChargesInput{
		ExecutionTargetID: target.ID, Provider: provider, CurrencyCode: "USD",
		BillingPeriodStartAt: base, BillingPeriodEndAt: terminatedAt,
	}, "shared-allocation-pg-sweep", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if sweep.Status != sharedAllocationSweepStatusComplete || sweep.WorkerCount != 1 ||
		sweep.AllocationRunCount != 1 || sweep.AllocationSliceCount != 27 || sweep.FailedWorkerCount != 0 {
		t.Fatalf("unexpected PostgreSQL shared allocation sweep replay: %#v", sweep)
	}
	var sweepAuditCount int64
	if err := db.WithContext(ctx).Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND resource_id = ?", domainA.TenantID,
			"billing.shared_cost_allocation_sweep_requested", target.ID).
		Count(&sweepAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if sweepAuditCount != 1 {
		t.Fatalf("PostgreSQL shared allocation sweep audit count = %d, want 1", sweepAuditCount)
	}

	overlap := input
	overlap.BillingPeriodStartAt = base.Add(30 * time.Minute)
	overlap.BillingPeriodEndAt = base.Add(90 * time.Minute)
	_, err = service.AllocateSharedUsageCharges(ctx, overlap)
	assertProblemCode(t, err, "billing_shared_allocation_period_overlap")

	directOverlap := *first.result.Run
	directOverlap.ID = uuid.New()
	directOverlap.BillingPeriodStartAt = base.Add(-time.Minute)
	if err := db.WithContext(ctx).Create(&directOverlap).Error; err == nil {
		t.Fatal("PostgreSQL accepted a directly inserted overlapping shared allocation period")
	}

	duplicateSlice := first.result.Slices[0]
	duplicateSlice.ID = uuid.New()
	if err := db.WithContext(ctx).Create(&duplicateSlice).Error; err == nil {
		t.Fatal("PostgreSQL accepted a duplicate semantic shared allocation slice")
	}

	wrongRate := first.result.Slices[0]
	wrongRate.ID = uuid.New()
	wrongRate.UsageStartAt = wrongRate.UsageStartAt.Add(time.Microsecond)
	wrongRate.RateMicros++
	if err := db.WithContext(ctx).Create(&wrongRate).Error; err == nil {
		t.Fatal("PostgreSQL accepted a shared allocation slice with a forged tariff rate")
	}

	partialProvider := "shared-partial-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	partialTariff, err := service.CreateTariff(ctx, CreateTariffInput{
		Provider: partialProvider, Region: "", CurrencyCode: "USD", Version: 1,
		EffectiveStartAt: base, EffectiveEndAt: sharedTimePointer(terminatedAt),
		RequestRateMicros: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	var boundaryClaim persistence.WorkerClaimFact
	if err := db.WithContext(ctx).
		Where("worker_id = ? AND worker_incarnation = ? AND claimed_at = ?", worker.ID, worker.Incarnation, base.Add(75*time.Minute+200*time.Millisecond)).
		Take(&boundaryClaim).Error; err != nil {
		t.Fatal(err)
	}
	partialTx := db.WithContext(ctx).Begin()
	if partialTx.Error != nil {
		t.Fatal(partialTx.Error)
	}
	partialRun := persistence.BillingSharedCostAllocationRun{
		ID: uuid.New(), ExecutionTargetID: target.ID, WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
		LedgerCoverageID: coverage.ID, Provider: partialProvider, Region: workerFact.Region, CurrencyCode: "USD",
		BillingPeriodStartAt: base, BillingPeriodEndAt: boundaryClaim.ClaimedAt,
		AlgorithmVersion: SharedAllocationAlgorithmClosedClaimIntervalV1,
		UsageStartAt:     base, UsageEndAt: boundaryClaim.ClaimedAt, ClaimCount: 3, ReleaseCount: 3,
		LedgerSHA256: strings.Repeat("f", 64), PlatformIdleSeconds: int64(boundaryClaim.ClaimedAt.Sub(base) / time.Second),
		CreatedAt: base.Add(3 * time.Hour),
	}
	if err := partialTx.Create(&partialRun).Error; err != nil {
		_ = partialTx.Rollback().Error
		t.Fatal(err)
	}
	tenantID := boundaryClaim.TenantID
	claimID := boundaryClaim.ID
	partialRequest := persistence.BillingSharedEstimatedChargeSlice{
		ID: uuid.New(), RunID: partialRun.ID, TenantID: &tenantID, ClaimFactID: &claimID, TariffID: partialTariff.ID,
		ChargeKind: ChargeKindRequest, AllocationKind: SharedAllocationKindTenantClaim,
		ResourceCorrelationKey: workerResourceCorrelationKey(workerFact),
		BillingPeriodStartAt:   partialRun.BillingPeriodStartAt, BillingPeriodEndAt: partialRun.BillingPeriodEndAt,
		UsageStartAt: boundaryClaim.ClaimedAt.Add(-time.Minute), UsageEndAt: boundaryClaim.ClaimedAt,
		ClaimCount: 1, RequestedCPUMillicores: &cpu, RequestedMemoryBytes: &memory,
		RequestedEphemeralStorageBytes: &ephemeral, RateMicros: 100, AmountMicros: 100,
		CreatedAt: partialRun.CreatedAt,
	}
	if err := partialTx.Create(&partialRequest).Error; err == nil {
		_ = partialTx.Rollback().Error
		t.Fatal("PostgreSQL accepted a final-end request on a non-terminal partial allocation run")
	}
	_ = partialTx.Rollback().Error
}

func createPostgresSharedAllocationTenant(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	userID uuid.UUID,
	now time.Time,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	tenantID := uuid.New()
	organizationID := uuid.New()
	err := persistence.InTransaction(ctx, db, func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: tenantID, Slug: "shared-pg-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
			Name: "Shared allocation PostgreSQL tenant", Status: "active", PlanCode: "test", Region: "us-east-1",
			Settings: map[string]any{}, CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.Organization{
			ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root organization",
			Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID,
			CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.OrganizationMembership{
			TenantID: tenantID, OrganizationID: organizationID, UserID: userID,
			Role: "owner", Status: "active", CreatedAt: now, UpdatedAt: now,
		}).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	return tenantID, organizationID
}

func seedPostgresSharedAllocationClaim(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	worker persistence.WorkerInstance,
	target persistence.ExecutionTarget,
	tenantID uuid.UUID,
	organizationID uuid.UUID,
	userID uuid.UUID,
	claimedAt time.Time,
	releasedAt time.Time,
	generation int64,
) {
	t.Helper()
	project := persistence.Project{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID,
		Name: "shared allocation " + fmt.Sprintf("%d", generation), DefaultBranch: "main", Visibility: "private",
		CreatedBy: userID, CreatedAt: claimedAt, UpdatedAt: claimedAt,
	}
	if err := db.WithContext(ctx).Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	session := persistence.AgentSession{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID, ProjectID: project.ID,
		CreatedBy: userID, Title: "shared allocation", Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: target.ID, CreatedAt: claimedAt, UpdatedAt: claimedAt,
	}
	if err := db.WithContext(ctx).Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: tenantID, SessionID: session.ID, CreatedBy: userID,
		Status: "running", InputText: "shared allocation", StartedAt: &claimedAt, CreatedAt: claimedAt,
	}
	if err := db.WithContext(ctx).Create(&turn).Error; err != nil {
		t.Fatal(err)
	}
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: tenantID, SessionID: session.ID, TurnID: turn.ID, Attempt: 1,
		Status: "leased", ExecutionTargetID: target.ID, TargetKind: target.Kind, WorkerID: &worker.ID,
		ProviderResumeStrategySnapshot: "authoritative-history", Generation: generation, RequestedBy: userID,
		QueuedAt: claimedAt, StartedAt: &claimedAt,
	}
	if err := db.WithContext(ctx).Create(&execution).Error; err != nil {
		t.Fatal(err)
	}
	lease := persistence.WorkerLease{
		ExecutionID: execution.ID, TenantID: tenantID, WorkerID: worker.ID,
		WorkerIncarnation: worker.Incarnation, WorkerInstanceUID: worker.InstanceUID, Generation: generation,
		LeaseTokenHash: []byte("shared-pg-lease-" + uuid.NewString()), AcquiredAt: claimedAt,
		HeartbeatAt: claimedAt, ExpiresAt: releasedAt.Add(time.Hour),
	}
	if err := db.WithContext(ctx).Create(&lease).Error; err != nil {
		t.Fatal(err)
	}
	claim := persistence.WorkerClaimFact{
		ID: uuid.New(), WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, TenantID: tenantID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, ClaimKind: "execution",
		RequestID: "shared-pg-claim-" + uuid.NewString(), ClaimedAt: claimedAt,
		ExecutionID: &execution.ID, ExecutionGeneration: &generation, CreatedAt: claimedAt,
	}
	if err := db.WithContext(ctx).Create(&claim).Error; err != nil {
		t.Fatal(err)
	}
	release := persistence.WorkerClaimReleaseFact{
		ClaimFactID: claim.ID, ReleasedAt: releasedAt, RecordedAt: releasedAt.Add(time.Second),
		ReleaseReason: "execution_completed", AuthorityKind: "control-plane", Metadata: map[string]any{},
	}
	if err := db.WithContext(ctx).Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Delete(&persistence.WorkerLease{}, "execution_id = ?", execution.ID).Error; err != nil {
		t.Fatal(err)
	}
}
