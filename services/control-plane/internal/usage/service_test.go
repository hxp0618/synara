package usage

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/billing"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestTenantUsageRejectsInactiveTenantBeforeStorageAccess(t *testing.T) {
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	_, err := NewService(nil).GetTenantUsage(context.Background(), identity.Principal{
		UserID: uuid.New(), ActiveTenantID: &activeTenantID,
	}, requestedTenantID)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "tenant_not_found" {
		t.Fatalf("inactive Tenant Usage read error = %v", err)
	}
}

func TestInternalCostAllocationRejectsInactiveTenantBeforeStorageAccess(t *testing.T) {
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	service := NewService(nil)
	assertTenantNotFound := func(err error) {
		t.Helper()
		var apiError *problem.Error
		if !errors.As(err, &apiError) || apiError.Code != "tenant_not_found" {
			t.Fatalf("inactive internal cost allocation error = %v", err)
		}
	}
	_, err := service.GetInternalCostAllocationReport(context.Background(), principal, requestedTenantID)
	assertTenantNotFound(err)
	_, err = service.PutProjectCostAllocation(
		context.Background(), principal, requestedTenantID, uuid.New(),
		PutProjectCostAllocationInput{CostCenterCode: "CC-ENG", DepartmentCode: "platform"},
		"inactive-cost-allocation", "127.0.0.1",
	)
	assertTenantNotFound(err)
	err = service.RecordInternalCostAllocationExport(
		context.Background(), principal, InternalCostAllocationReport{TenantID: requestedTenantID},
		"inactive-cost-export", "127.0.0.1",
	)
	assertTenantNotFound(err)
}

func TestSessionUsageRejectsCrossTenantSessionSubstitution(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "usage-isolation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "usage-isolation-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	otherTenant, err := tenancy.NewService(store.DB()).CreateTenant(ctx, principal, tenancy.CreateTenantInput{
		Slug: "usage-other-" + uuid.NewString()[:8], Name: "Usage Other", PlanCode: "free", Status: "active",
	}, "usage-other-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	projectID, sessionID := uuid.New(), uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Usage isolation", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Usage isolation",
		Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	principal.ActiveTenantID = &otherTenant.ID
	_, err = NewService(store.DB()).GetSessionUsage(ctx, principal, sessionID)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "session_not_found" {
		t.Fatalf("cross-Tenant Session Usage read error = %v", err)
	}
}

func TestSessionUsageExplainsProviderAndAllocatedPlatformCost(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "usage-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	assertInternalCostCenterProjection(t, ctx, store.DB(), domain, "SQLite")
}

func assertInternalCostCenterProjection(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	domain bootstrap.Result,
	databaseName string,
) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second).Add(-20 * time.Minute)
	claimAt := now.Add(time.Minute)
	releaseAt := now.Add(2 * time.Minute)
	terminatedAt := now.Add(3 * time.Minute)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "local", Name: "usage-cost-target-" + uuid.NewString()[:8], Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatalf("seed global execution target: %v", err)
	}
	projectID, sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	workerID := uuid.New()
	worker := persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(),
		ExecutionTargetID: target.ID, TargetKind: target.Kind, WorkerMode: "general-pool",
		RegistrationTrustMode: "shared-token", ClusterID: "sqlite", Namespace: "default",
		PodName: "usage-cost-worker", Version: "test", ProtocolVersion: 2,
		Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: []byte("usage-cost-token"), Status: "online", AdministrativeStatus: "active",
		RegisteredAt: now, LastHeartbeatAt: now,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	if err := db.Create(&persistence.WorkerIncarnationFact{
		WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
		ExecutionTargetID: target.ID, TargetKind: worker.TargetKind, WorkerMode: worker.WorkerMode,
		ClusterID: worker.ClusterID, Region: "local", Namespace: worker.Namespace, PodName: worker.PodName,
		InstanceUID: worker.InstanceUID, RegisteredAt: now, CurrentState: "active", StateChangedAt: now,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed worker incarnation fact: %v", err)
	}
	provider := "codex"
	for _, model := range []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Usage", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Usage", Status: "active", Visibility: "private", Provider: provider, ExecutionTargetID: target.ID},
		&persistence.AgentTurn{ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "completed", InputText: "Explain cost", RuntimeMode: "approval-required", InteractionMode: "default"},
		&persistence.AgentExecution{ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "leased", ExecutionTargetID: target.ID, TargetKind: target.Kind, WorkerID: &workerID, Generation: 1, Provider: &provider, RequestedBy: domain.UserID, QueuedAt: now, StartedAt: &now},
		&persistence.ExecutionUsageSummary{TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1, SessionID: sessionID, TurnID: turnID, Provider: provider, Model: stringPointer("gpt-5.6-sol"), InputTokens: 120, OutputTokens: 30, TotalTokens: 150, NetworkIngressBytes: 1024, NetworkEgressBytes: 2048, DurationMillis: 3000, ProviderCostMicros: 15000, ProviderCostReported: true, CurrencyCode: "USD", Final: true, LatestEventSequence: 3},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	if err := db.Create(&persistence.WorkerLease{
		ExecutionID: executionID, TenantID: domain.TenantID, WorkerID: workerID,
		WorkerIncarnation: 1, WorkerInstanceUID: worker.InstanceUID, Generation: 1,
		LeaseTokenHash: []byte("usage-cost-lease"), AcquiredAt: now, HeartbeatAt: now,
		ExpiresAt: now.Add(time.Hour),
	}).Error; err != nil {
		t.Fatalf("seed worker lease: %v", err)
	}
	claimID := uuid.New()
	for _, model := range []any{
		&persistence.WorkerClaimFact{ID: claimID, WorkerID: workerID, WorkerIncarnation: 1, TenantID: domain.TenantID, ExecutionTargetID: target.ID, TargetKind: target.Kind, ClaimKind: "execution", RequestID: "usage-claim", ClaimedAt: claimAt, ExecutionID: &executionID, ExecutionGeneration: int64Pointer(1), CreatedAt: claimAt},
		&persistence.WorkerClaimReleaseFact{ClaimFactID: claimID, ReleasedAt: releaseAt, RecordedAt: releaseAt.Add(time.Second), ReleaseReason: "execution_completed", AuthorityKind: "control-plane", Metadata: map[string]any{}},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	if err := db.Delete(&persistence.WorkerLease{}, "execution_id = ?", executionID).Error; err != nil {
		t.Fatalf("remove worker lease: %v", err)
	}
	// Recovery advances the mutable execution row, but generation 1 remains part
	// of the immutable usage and cost history for the reporting period.
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ? AND generation = ?", domain.TenantID, executionID, 1).
		Update("generation", 2).Error; err != nil {
		t.Fatalf("advance execution generation: %v", err)
	}
	if err := db.Model(&persistence.WorkerIncarnationFact{}).
		Where("worker_id = ? AND worker_incarnation = ?", workerID, 1).
		Updates(map[string]any{
			"current_state": "terminated", "state_changed_at": terminatedAt, "terminated_at": terminatedAt,
			"terminal_reason": "test-completed", "updated_at": terminatedAt,
		}).Error; err != nil {
		t.Fatalf("terminate worker fact: %v", err)
	}
	coverageID, tariffID, runID, estimatedSliceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	periodEnd := now.Add(10 * time.Minute)
	runCreatedAt := terminatedAt.Add(time.Minute)
	for _, model := range []any{
		&persistence.BillingSharedTargetLedgerCoverage{ID: coverageID, ExecutionTargetID: target.ID, CompleteFromAt: now.Add(-time.Hour), MinimumWriterVersion: "control-plane-v1.0.0", DeploymentAttestationSHA256: strings.Repeat("a", 64), SealedAt: now, SealedBy: domain.UserID},
		&persistence.BillingProviderTariff{ID: tariffID, Provider: "local", Region: "local", CurrencyCode: "USD", Version: 1, EffectiveStartAt: now.Add(-time.Hour), CPUCoreHourRateMicros: 100, CreatedAt: now},
		&persistence.BillingSharedCostAllocationRun{ID: runID, ExecutionTargetID: target.ID, WorkerID: workerID, WorkerIncarnation: 1, LedgerCoverageID: coverageID, Provider: "local", Region: "local", CurrencyCode: "USD", BillingPeriodStartAt: now, BillingPeriodEndAt: periodEnd, AlgorithmVersion: "closed-claim-interval-v1", UsageStartAt: now, UsageEndAt: terminatedAt, ClaimCount: 1, ReleaseCount: 1, LedgerSHA256: strings.Repeat("b", 64), TenantAllocatedSeconds: 60, PlatformIdleSeconds: 120, CreatedAt: runCreatedAt},
		&persistence.BillingSharedEstimatedChargeSlice{ID: estimatedSliceID, RunID: runID, TenantID: &domain.TenantID, ClaimFactID: &claimID, TariffID: tariffID, ChargeKind: "cpu", AllocationKind: "tenant-claim", ResourceCorrelationKey: "usage-test", BillingPeriodStartAt: now, BillingPeriodEndAt: periodEnd, UsageStartAt: claimAt, UsageEndAt: releaseAt, BillableSeconds: 60, ClaimCount: 1, RateMicros: 100, AmountMicros: 6000, CreatedAt: runCreatedAt},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	invoiceImportID, invoiceLineID := uuid.New(), uuid.New()
	invoiceChecksum := strings.Repeat("c", 64)
	for _, model := range []any{
		&persistence.BillingActualInvoiceImport{
			ID: invoiceImportID, TenantID: domain.TenantID, Provider: "local",
			ExternalImportID: "usage-actual-invoice", BillingPeriodStartAt: now, BillingPeriodEndAt: periodEnd,
			CurrencyCode: "USD", SourceChecksum: invoiceChecksum, ImportedAt: periodEnd.Add(time.Minute),
			CreatedAt: periodEnd.Add(time.Minute),
		},
		&persistence.BillingActualInvoiceLine{
			ID: invoiceLineID, TenantID: domain.TenantID, InvoiceImportID: invoiceImportID,
			ExternalLineID: "usage-actual-cpu", Provider: "local", CurrencyCode: "USD", ChargeKind: "cpu",
			ResourceCorrelationKey: "usage-test", BillingPeriodStartAt: now, BillingPeriodEndAt: periodEnd,
			AmountMicros: 9000, ReconciliationState: "pending", CreatedAt: periodEnd.Add(time.Minute),
		},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	if _, err := billing.NewService(
		db,
		nil,
		billing.WithPlatformBillingOperatorTenant(domain.TenantID),
	).AllocateSharedActualInvoiceAuthorized(
		ctx,
		identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		domain.TenantID,
		billing.AllocateSharedActualInvoiceInput{
			ExecutionTargetID: target.ID, InvoiceImportID: invoiceImportID,
			SourceScopeAttestationSHA256: strings.Repeat("d", 64),
		},
		"usage-actual-allocation",
		"127.0.0.1",
	); err != nil {
		t.Fatalf("allocate actual platform cost: %v", err)
	}
	result, err := NewService(db).GetSessionUsage(ctx, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
	}, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("Session Usage items = %#v", result.Items)
	}
	item := result.Items[0]
	if item.TotalTokens != 150 || item.NetworkEgressBytes != 2048 || item.ProviderCostMicros != 15000 || !item.ProviderCostReported ||
		len(item.PlatformCharges) != 1 || item.PlatformCharges[0].Kind != "cpu" ||
		item.PlatformCharges[0].AmountMicros != 9000 || item.PlatformCharges[0].Source != "actual" ||
		item.TotalCostByCurrency["USD"] != 24000 ||
		item.CostCoverage != "provider-and-allocated-platform" {
		t.Fatalf("unexpected Session Usage explanation: %#v", item)
	}
	serviceAccountID := uuid.New()
	_, err = NewService(db).GetSessionUsage(ctx, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &domain.TenantID, ServiceAccountID: &serviceAccountID,
	}, sessionID)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "session_not_found" {
		t.Fatalf("Service Account private Session Usage error = %v", err)
	}
	if err := db.Model(&persistence.TenantSubscription{}).
		Where("tenant_id = ?", domain.TenantID).
		Updates(map[string]any{
			"current_period_start": now.Add(-time.Minute),
			"current_period_end":   periodEnd.Add(20 * time.Minute),
		}).Error; err != nil {
		t.Fatalf("set internal cost-center period: %v", err)
	}
	tenantUsage, err := NewService(db).GetTenantUsage(ctx, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
	}, domain.TenantID)
	if err != nil {
		t.Fatalf("load Tenant cost-center usage: %v", err)
	}
	if len(tenantUsage.Usage.PlatformCharges) != 1 ||
		tenantUsage.Usage.PlatformCharges[0].Source != "actual" ||
		tenantUsage.Usage.PlatformCostByCurrency["USD"] != 9000 ||
		tenantUsage.Usage.KnownCostByCurrency["USD"] != 24000 {
		t.Fatalf("unexpected Tenant cost-center projection: %#v", tenantUsage.Usage)
	}
	allocationService := NewService(db)
	allocationService.now = func() time.Time { return periodEnd.Add(10 * time.Minute) }
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	report, err := allocationService.GetInternalCostAllocationReport(ctx, principal, domain.TenantID)
	if err != nil {
		t.Fatalf("load unallocated internal cost report: %v", err)
	}
	if len(report.Rows) != 1 || report.Rows[0].ProjectID != projectID ||
		report.Rows[0].CostCenterCode != unallocatedDimension || report.Rows[0].DepartmentCode != unallocatedDimension ||
		report.Rows[0].TotalTokens != 150 || report.Rows[0].KnownCostByCurrency["USD"] != 24000 ||
		report.UnallocatedProjectCount != 1 {
		t.Fatalf("unexpected unallocated internal cost report: %#v", report)
	}
	allocation, err := allocationService.PutProjectCostAllocation(ctx, principal, domain.TenantID, projectID,
		PutProjectCostAllocationInput{CostCenterCode: "CC-ENG", DepartmentCode: "platform", ExpectedVersion: 0},
		"assign-project-cost", "127.0.0.1")
	if err != nil {
		t.Fatalf("assign Project cost allocation: %v", err)
	}
	if allocation.Version != 1 || allocation.CostCenterCode != "CC-ENG" || allocation.DepartmentCode != "platform" {
		t.Fatalf("assigned Project cost allocation = %#v", allocation)
	}
	if _, err := allocationService.PutProjectCostAllocation(ctx, principal, domain.TenantID, projectID,
		PutProjectCostAllocationInput{CostCenterCode: "CC-STALE", DepartmentCode: "stale", ExpectedVersion: 0},
		"stale-project-cost", "127.0.0.1"); err == nil {
		t.Fatal("stale Project cost allocation update was accepted")
	}
	report, err = allocationService.GetInternalCostAllocationReport(ctx, principal, domain.TenantID)
	if err != nil {
		t.Fatalf("load allocated internal cost report: %v", err)
	}
	if len(report.Rows) != 1 || report.Rows[0].CostCenterCode != "CC-ENG" ||
		report.Rows[0].DepartmentCode != "platform" || report.Rows[0].Version != 1 ||
		report.Rows[0].ProviderCostByCurrency["USD"] != 15000 ||
		report.Rows[0].PlatformCostByCurrency["USD"] != 9000 || report.UnallocatedProjectCount != 0 {
		t.Fatalf("unexpected allocated internal cost report: %#v", report)
	}
	if err := db.Model(&persistence.ExecutionUsageSummary{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 1).
		Updates(map[string]any{"provider_cost_micros": 0, "provider_cost_reported": false}).Error; err == nil {
		t.Fatalf("%s allowed reported Provider cost coverage to regress", databaseName)
	}
}

func TestTenantUsageProjectsSoftQuotaAlertsWithoutHardStop(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "soft-quota-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	periodStart, periodEnd := now.Add(-time.Hour), now.Add(time.Hour)
	if err := store.DB().Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", domain.TenantID).
		Updates(map[string]any{"current_period_start": periodStart, "current_period_end": periodEnd}).Error; err != nil {
		t.Fatal(err)
	}
	limit, warning := int64(10), int64(80)
	for _, entitlement := range []persistence.PlanEntitlement{
		{PlanCode: "personal", Key: executionSecondsEntitlement, ValueKind: "integer", IntegerValue: &limit},
		{PlanCode: "personal", Key: softWarningEntitlement, ValueKind: "integer", IntegerValue: &warning},
	} {
		if err := store.DB().Create(&entitlement).Error; err != nil {
			t.Fatal(err)
		}
	}
	projectID, sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	provider := "codex"
	for _, model := range []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Soft quota", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Soft quota", Status: "active", Visibility: "private", Provider: provider, ExecutionTargetID: domain.ExecutionTargetID},
		&persistence.AgentTurn{ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "completed", InputText: "Use quota", RuntimeMode: "approval-required", InteractionMode: "default"},
		&persistence.AgentExecution{ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "completed", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", Generation: 1, Provider: &provider, RequestedBy: domain.UserID, QueuedAt: now},
		&persistence.ExecutionUsageSummary{TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1, SessionID: sessionID, TurnID: turnID, Provider: provider, TotalTokens: 50, DurationMillis: 8500, CurrencyCode: "USD", Final: true, LatestEventSequence: 1},
	} {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	service := NewService(store.DB())
	service.now = func() time.Time { return now }
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	result, err := service.GetTenantUsage(ctx, principal, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftQuota.State != "approaching_limit" || result.SoftQuota.HardStop ||
		result.Usage.ExecutionSeconds != 9 || len(result.Alerts) != 1 || result.Alerts[0].ThresholdPercent != 80 {
		t.Fatalf("unexpected approaching-limit result: %#v", result)
	}
	if err := store.DB().Model(&persistence.ExecutionUsageSummary{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 1).
		Update("duration_millis", 10001).Error; err != nil {
		t.Fatal(err)
	}
	result, err = service.GetTenantUsage(ctx, principal, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftQuota.State != "limit_reached" || result.SoftQuota.HardStop ||
		result.Usage.ExecutionSeconds != 11 || len(result.Alerts) != 2 || result.Alerts[1].ThresholdPercent != 100 {
		t.Fatalf("unexpected limit-reached result: %#v", result)
	}
	if _, err := service.GetTenantUsage(ctx, principal, domain.TenantID); err != nil {
		t.Fatal(err)
	}
	var alertCount int64
	if err := store.DB().Model(&persistence.TenantUsageQuotaAlert{}).
		Where("tenant_id = ?", domain.TenantID).Count(&alertCount).Error; err != nil {
		t.Fatal(err)
	}
	if alertCount != 2 {
		t.Fatalf("soft quota alert rows = %d, want 2", alertCount)
	}
}

func TestThresholdReachedAvoidsIntegerOverflow(t *testing.T) {
	if thresholdReached(math.MaxInt64-1, math.MaxInt64, 100) {
		t.Fatal("threshold reached before the exact limit")
	}
	if !thresholdReached(math.MaxInt64, math.MaxInt64, 100) {
		t.Fatal("exact maximum limit was not reached")
	}
	if !thresholdReached(8, 10, 80) || thresholdReached(7, 10, 80) {
		t.Fatal("80 percent threshold boundary is incorrect")
	}
}

func stringPointer(value string) *string { return &value }
func int64Pointer(value int64) *int64    { return &value }
