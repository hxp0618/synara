package billing_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/billing"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

const (
	migratedClaimKindExecution        = "execution"
	migratedClaimKindWorkspaceCleanup = "workspace-cleanup"
)

func TestEstimateUsageChargesWithMigratedSQLiteClaimLedger(t *testing.T) {
	t.Run("complete claim ledger splits request charges across tariffs", func(t *testing.T) {
		ctx := context.Background()
		fixture := newMigratedSQLiteBillingFixture(t)
		seedMigratedBillingTariffs(t, ctx, fixture.service, fixture.base)

		scenario := seedMigratedWorkerClaimScenario(t, ctx, fixture, migratedWorkerClaimScenarioSeed{
			workerName:        "complete-ledger",
			registeredAt:      fixture.base,
			lifetime:          2 * time.Hour,
			claimCount:        2,
			executionClaimAt:  fixture.base.Add(15 * time.Minute),
			cleanupClaimAt:    fixture.base.Add(75 * time.Minute),
			executionTargetID: fixture.target.ID,
		})

		estimated, err := fixture.service.EstimateUsageCharges(ctx, billing.EstimateUsageChargesInput{
			Provider:             "aws",
			CurrencyCode:         "USD",
			WorkerID:             scenario.worker.ID,
			WorkerIncarnation:    scenario.worker.Incarnation,
			BillingPeriodStartAt: fixture.base,
			BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(estimated) != 10 {
			t.Fatalf("estimated row count = %d, want 10", len(estimated))
		}

		requestRows := filterChargesByKind(estimated, billing.ChargeKindRequest)
		if len(requestRows) != 2 {
			t.Fatalf("request row count = %d, want 2", len(requestRows))
		}
		if !requestRows[0].UsageStartAt.Equal(fixture.base) ||
			!requestRows[0].UsageEndAt.Equal(fixture.base.Add(time.Hour)) ||
			requestRows[0].ClaimCount != 1 ||
			requestRows[0].AmountMicros != 200_000 {
			t.Fatalf("first request row = %#v, want first tariff segment with one claim and 200000 micros", requestRows[0])
		}
		if !requestRows[1].UsageStartAt.Equal(fixture.base.Add(time.Hour)) ||
			!requestRows[1].UsageEndAt.Equal(fixture.base.Add(2*time.Hour)) ||
			requestRows[1].ClaimCount != 1 ||
			requestRows[1].AmountMicros != 300_000 {
			t.Fatalf("second request row = %#v, want second tariff segment with one claim and 300000 micros", requestRows[1])
		}

		totals := sumChargesByKind(estimated)
		if totals[billing.ChargeKindCPU] != 10_800_000 {
			t.Fatalf("cpu total = %d, want 10800000", totals[billing.ChargeKindCPU])
		}
		if totals[billing.ChargeKindMemory] != 6_000_000 {
			t.Fatalf("memory total = %d, want 6000000", totals[billing.ChargeKindMemory])
		}
		if totals[billing.ChargeKindEphemeralStorage] != 6_000_000 {
			t.Fatalf("ephemeral total = %d, want 6000000", totals[billing.ChargeKindEphemeralStorage])
		}
		if totals[billing.ChargeKindPod] != 10_800_000 {
			t.Fatalf("pod total = %d, want 10800000", totals[billing.ChargeKindPod])
		}
		if totals[billing.ChargeKindRequest] != 500_000 {
			t.Fatalf("request total = %d, want 500000", totals[billing.ChargeKindRequest])
		}

		var persisted int64
		if err := fixture.db.WithContext(ctx).Model(&persistence.BillingEstimatedUsageCharge{}).Count(&persisted).Error; err != nil {
			t.Fatal(err)
		}
		if persisted != 10 {
			t.Fatalf("persisted estimated row count = %d, want 10", persisted)
		}
	})

	t.Run("incomplete claim ledger fails closed across tariff boundary", func(t *testing.T) {
		ctx := context.Background()
		fixture := newMigratedSQLiteBillingFixture(t)
		seedMigratedBillingTariffs(t, ctx, fixture.service, fixture.base)

		scenario := seedMigratedWorkerClaimScenario(t, ctx, fixture, migratedWorkerClaimScenarioSeed{
			workerName:        "incomplete-ledger",
			registeredAt:      fixture.base,
			lifetime:          2 * time.Hour,
			claimCount:        3,
			executionClaimAt:  fixture.base.Add(15 * time.Minute),
			cleanupClaimAt:    fixture.base.Add(75 * time.Minute),
			executionTargetID: fixture.target.ID,
		})

		_, err := fixture.service.EstimateUsageCharges(ctx, billing.EstimateUsageChargesInput{
			Provider:             "aws",
			CurrencyCode:         "USD",
			WorkerID:             scenario.worker.ID,
			WorkerIncarnation:    scenario.worker.Incarnation,
			BillingPeriodStartAt: fixture.base,
			BillingPeriodEndAt:   fixture.base.Add(4 * time.Hour),
		})
		assertProblemCode(t, err, "cost_accounting_request_charge_delta_unavailable")

		var persisted int64
		if err := fixture.db.WithContext(ctx).Model(&persistence.BillingEstimatedUsageCharge{}).Count(&persisted).Error; err != nil {
			t.Fatal(err)
		}
		if persisted != 0 {
			t.Fatalf("persisted estimated row count after fail-closed path = %d, want 0", persisted)
		}
	})
}

type migratedSQLiteBillingFixture struct {
	db      *gorm.DB
	service *billing.Service
	base    time.Time
	domain  bootstrap.Result
	target  persistence.ExecutionTarget
	project persistence.Project
}

func newMigratedSQLiteBillingFixture(t *testing.T) migratedSQLiteBillingFixture {
	t.Helper()

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

	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	target := persistence.ExecutionTarget{
		ID:                     uuid.New(),
		TenantID:               &domain.TenantID,
		OrganizationID:         &domain.OrganizationID,
		Kind:                   "kubernetes",
		Name:                   "billing-k8s-target-" + uuid.NewString()[:8],
		Status:                 "active",
		ConfigurationEncrypted: []byte("{}"),
		Capabilities:           map[string]any{},
		CreatedAt:              base,
		UpdatedAt:              base,
	}
	if err := store.DB().WithContext(ctx).Create(&target).Error; err != nil {
		t.Fatalf("create migrated billing target: %v", err)
	}

	project := persistence.Project{
		ID:             uuid.New(),
		TenantID:       domain.TenantID,
		OrganizationID: domain.OrganizationID,
		Name:           "billing-claim-ledger",
		DefaultBranch:  "main",
		Visibility:     "private",
		CreatedBy:      domain.UserID,
		CreatedAt:      base,
		UpdatedAt:      base,
	}
	if err := store.DB().WithContext(ctx).Create(&project).Error; err != nil {
		t.Fatalf("create migrated billing project: %v", err)
	}

	return migratedSQLiteBillingFixture{
		db:      store.DB(),
		service: billing.NewService(store.DB(), nil),
		base:    base,
		domain:  domain,
		target:  target,
		project: project,
	}
}

func seedMigratedBillingTariffs(
	t *testing.T,
	ctx context.Context,
	service *billing.Service,
	base time.Time,
) {
	t.Helper()

	inputs := []billing.CreateTariffInput{
		{
			Provider:                   "aws",
			Region:                     "us-east-1",
			CurrencyCode:               "USD",
			Version:                    1,
			EffectiveStartAt:           base,
			EffectiveEndAt:             timePtr(base.Add(time.Hour)),
			CPUCoreHourRateMicros:      3_600_000,
			MemoryGiBHourRateMicros:    1_000_000,
			EphemeralGiBHourRateMicros: 500_000,
			RequestRateMicros:          200_000,
			PodHourRateMicros:          3_600_000,
		},
		{
			Provider:                   "aws",
			Region:                     "us-east-1",
			CurrencyCode:               "USD",
			Version:                    2,
			EffectiveStartAt:           base.Add(time.Hour),
			EffectiveEndAt:             timePtr(base.Add(4 * time.Hour)),
			CPUCoreHourRateMicros:      7_200_000,
			MemoryGiBHourRateMicros:    2_000_000,
			EphemeralGiBHourRateMicros: 1_000_000,
			RequestRateMicros:          300_000,
			PodHourRateMicros:          7_200_000,
		},
	}
	for _, input := range inputs {
		if _, err := service.CreateTariff(ctx, input); err != nil {
			t.Fatalf("create migrated billing tariff %#v: %v", input, err)
		}
	}
}

type migratedWorkerClaimScenarioSeed struct {
	workerName        string
	registeredAt      time.Time
	lifetime          time.Duration
	claimCount        int64
	executionClaimAt  time.Time
	cleanupClaimAt    time.Time
	executionTargetID uuid.UUID
}

type migratedWorkerClaimScenario struct {
	worker  persistence.WorkerInstance
	fact    persistence.WorkerIncarnationFact
	session persistence.AgentSession
	turn    persistence.AgentTurn
}

func seedMigratedWorkerClaimScenario(
	t *testing.T,
	ctx context.Context,
	fixture migratedSQLiteBillingFixture,
	seed migratedWorkerClaimScenarioSeed,
) migratedWorkerClaimScenario {
	t.Helper()

	worker := persistence.WorkerInstance{
		ID:                    uuid.New(),
		Incarnation:           1,
		InstanceUID:           uuid.NewString(),
		ExecutionTargetID:     seed.executionTargetID,
		TargetKind:            fixture.target.Kind,
		WorkerMode:            "general-pool",
		RegistrationTrustMode: "kubernetes-pod-bound-v1",
		ClusterID:             "kubernetes",
		Namespace:             "default",
		PodName:               seed.workerName + "-pod",
		Version:               "test",
		ProtocolVersion:       2,
		Capabilities:          map[string]any{},
		LeaseSupported:        true,
		FencingSupported:      true,
		AuthTokenHash:         []byte("worker-claim-ledger-auth"),
		Status:                "online",
		AdministrativeStatus:  "active",
		RegisteredAt:          seed.registeredAt,
		LastHeartbeatAt:       seed.registeredAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&worker).Error; err != nil {
		t.Fatalf("create migrated billing worker: %v", err)
	}

	cpuMillicores := int64(1000)
	memoryBytes := int64(2 * 1024 * 1024 * 1024)
	ephemeralBytes := int64(4 * 1024 * 1024 * 1024)
	terminatedAt := seed.registeredAt.Add(seed.lifetime)
	fact := persistence.WorkerIncarnationFact{
		WorkerID:                       worker.ID,
		WorkerIncarnation:              worker.Incarnation,
		TenantID:                       &fixture.domain.TenantID,
		ExecutionTargetID:              seed.executionTargetID,
		TargetKind:                     fixture.target.Kind,
		WorkerMode:                     worker.WorkerMode,
		ClusterID:                      worker.ClusterID,
		Region:                         "us-east-1",
		Namespace:                      worker.Namespace,
		PodName:                        worker.PodName,
		InstanceUID:                    worker.InstanceUID,
		RegisteredAt:                   seed.registeredAt,
		CurrentState:                   "terminated",
		StateChangedAt:                 terminatedAt,
		TerminatedAt:                   &terminatedAt,
		TerminalReason:                 stringPtr("completed"),
		AccumulatedActiveSeconds:       3600,
		AccumulatedIdleSeconds:         3600,
		ClaimCount:                     seed.claimCount,
		RequestedCPUMillicores:         &cpuMillicores,
		RequestedMemoryBytes:           &memoryBytes,
		RequestedEphemeralStorageBytes: &ephemeralBytes,
		CreatedAt:                      seed.registeredAt,
		UpdatedAt:                      terminatedAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&fact).Error; err != nil {
		t.Fatalf("create migrated billing worker fact: %v", err)
	}

	session := persistence.AgentSession{
		ID:                         uuid.New(),
		TenantID:                   fixture.domain.TenantID,
		OrganizationID:             fixture.domain.OrganizationID,
		ProjectID:                  fixture.project.ID,
		CreatedBy:                  fixture.domain.UserID,
		Title:                      "billing claim ledger " + seed.workerName,
		Status:                     "active",
		Visibility:                 "private",
		Provider:                   "codex",
		ExecutionTargetID:          seed.executionTargetID,
		RequestedExecutionTargetID: seed.executionTargetID,
		ProviderResumeCursorState:  "absent",
		ResourceState:              "active",
		MeaningfulActivityAt:       seed.registeredAt,
		CreatedAt:                  seed.registeredAt,
		UpdatedAt:                  seed.registeredAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&session).Error; err != nil {
		t.Fatalf("create migrated billing session: %v", err)
	}

	turn := persistence.AgentTurn{
		ID:        uuid.New(),
		TenantID:  fixture.domain.TenantID,
		SessionID: session.ID,
		CreatedBy: fixture.domain.UserID,
		Status:    "running",
		InputText: "Continue",
		StartedAt: &seed.registeredAt,
		CreatedAt: seed.registeredAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&turn).Error; err != nil {
		t.Fatalf("create migrated billing turn: %v", err)
	}

	execution := persistence.AgentExecution{
		ID:                             uuid.New(),
		TenantID:                       fixture.domain.TenantID,
		SessionID:                      session.ID,
		TurnID:                         turn.ID,
		Attempt:                        1,
		Status:                         "leased",
		ExecutionTargetID:              seed.executionTargetID,
		TargetKind:                     fixture.target.Kind,
		WorkerID:                       &worker.ID,
		ProviderResumeStrategySnapshot: "authoritative-history",
		Generation:                     1,
		RequestedBy:                    fixture.domain.UserID,
		QueuedAt:                       seed.registeredAt,
		StartedAt:                      &seed.registeredAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&execution).Error; err != nil {
		t.Fatalf("create migrated billing execution: %v", err)
	}

	leaseAcquiredAt := seed.registeredAt.Add(15 * time.Second)
	lease := persistence.WorkerLease{
		ExecutionID:       execution.ID,
		TenantID:          fixture.domain.TenantID,
		WorkerID:          worker.ID,
		WorkerIncarnation: worker.Incarnation,
		WorkerInstanceUID: worker.InstanceUID,
		Generation:        execution.Generation,
		LeaseTokenHash:    []byte("worker-claim-ledger-lease"),
		AcquiredAt:        leaseAcquiredAt,
		HeartbeatAt:       leaseAcquiredAt,
		ExpiresAt:         leaseAcquiredAt.Add(10 * time.Minute),
	}
	if err := fixture.db.WithContext(ctx).Create(&lease).Error; err != nil {
		t.Fatalf("create migrated billing worker lease: %v", err)
	}

	workspace := persistence.RemoteWorkspace{
		ID:                uuid.New(),
		TenantID:          fixture.domain.TenantID,
		OrganizationID:    fixture.domain.OrganizationID,
		ProjectID:         fixture.project.ID,
		SessionID:         session.ID,
		ExecutionTargetID: seed.executionTargetID,
		WorkspaceMode:     "clone",
		State:             "cleanup-pending",
		DefaultBranch:     "main",
		CreatedAt:         seed.registeredAt,
		UpdatedAt:         seed.cleanupClaimAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&workspace).Error; err != nil {
		t.Fatalf("create migrated billing workspace: %v", err)
	}

	cleanupRequestedAt := seed.cleanupClaimAt.Add(-2 * time.Minute)
	materialization := persistence.WorkspaceMaterialization{
		ID:                 uuid.New(),
		TenantID:           fixture.domain.TenantID,
		WorkspaceID:        workspace.ID,
		OrganizationID:     fixture.domain.OrganizationID,
		ProjectID:          fixture.project.ID,
		SessionID:          session.ID,
		ExecutionTargetID:  seed.executionTargetID,
		TargetKind:         fixture.target.Kind,
		StorageScope:       "target",
		LayoutVersion:      3,
		IncarnationID:      uuid.New(),
		State:              "cleanup-pending",
		CleanupReason:      stringPtr("retention-expired"),
		CleanupRequestedAt: &cleanupRequestedAt,
		CreatedAt:          seed.registeredAt,
		UpdatedAt:          cleanupRequestedAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&materialization).Error; err != nil {
		t.Fatalf("create migrated billing materialization: %v", err)
	}
	if err := fixture.db.WithContext(ctx).
		Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", fixture.domain.TenantID, workspace.ID).
		Update("current_materialization_id", materialization.ID).Error; err != nil {
		t.Fatalf("bind migrated billing materialization: %v", err)
	}

	leaseExpiresAt := seed.cleanupClaimAt.Add(2 * time.Minute)
	cleanup := persistence.WorkspaceCleanupCommand{
		ID:                           uuid.New(),
		TenantID:                     fixture.domain.TenantID,
		MaterializationID:            materialization.ID,
		MaterializationIncarnationID: materialization.IncarnationID,
		WorkspaceID:                  workspace.ID,
		ExecutionTargetID:            seed.executionTargetID,
		TargetKind:                   fixture.target.Kind,
		StorageScope:                 materialization.StorageScope,
		LayoutVersion:                materialization.LayoutVersion,
		Reason:                       "retention-expired",
		Status:                       "leased",
		LeaseTokenHash:               []byte("worker-claim-ledger-cleanup"),
		DispatchGeneration:           1,
		DeliveryWorkerID:             &worker.ID,
		DeliveryWorkerIncarnation:    &worker.Incarnation,
		DeliveryAttempts:             1,
		DeliveryAvailableAt:          cleanupRequestedAt,
		LeaseExpiresAt:               &leaseExpiresAt,
		RequestedAt:                  cleanupRequestedAt,
		LeasedAt:                     &seed.cleanupClaimAt,
		CreatedAt:                    cleanupRequestedAt,
		UpdatedAt:                    seed.cleanupClaimAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&cleanup).Error; err != nil {
		t.Fatalf("create migrated billing cleanup command: %v", err)
	}

	executionGeneration := execution.Generation
	executionClaim := persistence.WorkerClaimFact{
		ID:                  uuid.New(),
		WorkerID:            worker.ID,
		WorkerIncarnation:   worker.Incarnation,
		TenantID:            fixture.domain.TenantID,
		ExecutionTargetID:   seed.executionTargetID,
		TargetKind:          fixture.target.Kind,
		ClaimKind:           migratedClaimKindExecution,
		RequestID:           seed.workerName + "-execution-claim",
		ClaimedAt:           seed.executionClaimAt,
		ExecutionID:         &execution.ID,
		ExecutionGeneration: &executionGeneration,
		CreatedAt:           seed.executionClaimAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&executionClaim).Error; err != nil {
		t.Fatalf("create migrated billing execution claim fact: %v", err)
	}

	cleanupDispatchGeneration := cleanup.DispatchGeneration
	cleanupClaim := persistence.WorkerClaimFact{
		ID:                        uuid.New(),
		WorkerID:                  worker.ID,
		WorkerIncarnation:         worker.Incarnation,
		TenantID:                  fixture.domain.TenantID,
		ExecutionTargetID:         seed.executionTargetID,
		TargetKind:                fixture.target.Kind,
		ClaimKind:                 migratedClaimKindWorkspaceCleanup,
		RequestID:                 seed.workerName + "-cleanup-claim",
		ClaimedAt:                 seed.cleanupClaimAt,
		CleanupCommandID:          &cleanup.ID,
		CleanupDispatchGeneration: &cleanupDispatchGeneration,
		CreatedAt:                 seed.cleanupClaimAt,
	}
	if err := fixture.db.WithContext(ctx).Create(&cleanupClaim).Error; err != nil {
		t.Fatalf("create migrated billing cleanup claim fact: %v", err)
	}

	return migratedWorkerClaimScenario{
		worker:  worker,
		fact:    fact,
		session: session,
		turn:    turn,
	}
}

func filterChargesByKind(charges []persistence.BillingEstimatedUsageCharge, kind string) []persistence.BillingEstimatedUsageCharge {
	filtered := make([]persistence.BillingEstimatedUsageCharge, 0)
	for _, charge := range charges {
		if charge.ChargeKind == kind {
			filtered = append(filtered, charge)
		}
	}
	return filtered
}

func sumChargesByKind(charges []persistence.BillingEstimatedUsageCharge) map[string]int64 {
	totals := make(map[string]int64, len(charges))
	for _, charge := range charges {
		totals[charge.ChargeKind] += charge.AmountMicros
	}
	return totals
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()

	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}

func timePtr(value time.Time) *time.Time {
	return &value
}

func stringPtr(value string) *string {
	return &value
}
