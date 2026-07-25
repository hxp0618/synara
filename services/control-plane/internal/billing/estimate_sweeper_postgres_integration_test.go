package billing

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestBuiltInEstimateSweeperPostgresPagesAcrossUUIDKeysetAndStaysIdempotent(t *testing.T) {
	const expectedChargesPerWorker = 9

	ctx := context.Background()
	db := openBillingPostgresIntegrationDB(t)
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	targetID := uuid.New()
	if err := db.WithContext(ctx).Create(&persistence.ExecutionTarget{
		ID:                     targetID,
		TenantID:               &domain.TenantID,
		OrganizationID:         &domain.OrganizationID,
		Kind:                   "kubernetes",
		Name:                   "billing-pg-target-" + uuid.NewString()[:8],
		Status:                 "active",
		ConfigurationEncrypted: []byte{},
		Capabilities:           map[string]any{},
		CreatedAt:              base,
		UpdatedAt:              base,
	}).Error; err != nil {
		t.Fatal(err)
	}

	service := NewService(db, nil, WithBuiltInEstimateSweeper())
	service.now = func() time.Time { return base.Add(5 * time.Hour) }
	fixture := billingFixture{
		db:       db,
		service:  service,
		base:     base,
		tenantID: domain.TenantID,
	}
	seedTwoTariffs(t, ctx, fixture)

	workerIDPrefix := uuid.NewString()[:24]
	instanceUIDPrefix := uuid.NewString()[:24]
	workerTotal := estimateSweepPageSize + 5
	inserted := make([]persistence.WorkerIncarnationFact, 0, workerTotal)
	for i := 1; i <= workerTotal; i++ {
		inserted = append(inserted, insertBillingPostgresWorker(
			t,
			db,
			base,
			domain.TenantID,
			targetID,
			orderedUUID(workerIDPrefix, i),
			orderedUUIDString(instanceUIDPrefix, i),
			i,
		))
	}

	request := ScheduledEstimateSweepRequest{
		Import: persistence.BillingActualInvoiceImport{
			TenantID:             domain.TenantID,
			Provider:             "aws",
			CurrencyCode:         "USD",
			BillingPeriodStartAt: base,
			BillingPeriodEndAt:   base.Add(4 * time.Hour),
		},
		TenantID:           domain.TenantID,
		Provider:           "aws",
		ExecutionTargetIDs: []uuid.UUID{targetID},
		RequestedBy:        "test",
	}

	first, err := service.SweepImportedInvoice(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkerCount != workerTotal || first.FailedWorkerCount != 0 ||
		first.EstimateCount != workerTotal*expectedChargesPerWorker {
		t.Fatalf("first PostgreSQL estimate sweep = %#v", first)
	}

	chargeScope := func() *gorm.DB {
		return db.WithContext(ctx).Model(&persistence.BillingEstimatedUsageCharge{}).
			Where(
				"tenant_id = ? AND execution_target_id = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
				domain.TenantID,
				targetID,
				request.Import.BillingPeriodStartAt,
				request.Import.BillingPeriodEndAt,
			)
	}

	var firstRowCount int64
	if err := chargeScope().Count(&firstRowCount).Error; err != nil {
		t.Fatal(err)
	}
	if firstRowCount != int64(workerTotal*expectedChargesPerWorker) {
		t.Fatalf("first PostgreSQL estimate row count = %d, want %d", firstRowCount, workerTotal*expectedChargesPerWorker)
	}

	type workerChargeCount struct {
		WorkerID          uuid.UUID
		WorkerIncarnation int64
		ChargeCount       int64
	}

	var grouped []workerChargeCount
	if err := chargeScope().
		Select("worker_id, worker_incarnation, COUNT(*) AS charge_count").
		Group("worker_id, worker_incarnation").
		Order("worker_id, worker_incarnation").
		Scan(&grouped).Error; err != nil {
		t.Fatal(err)
	}
	if len(grouped) != workerTotal {
		t.Fatalf("grouped PostgreSQL estimate workers = %d, want %d", len(grouped), workerTotal)
	}

	expected := make(map[string]struct{}, len(inserted))
	for _, fact := range inserted {
		expected[workerFactKey(fact.WorkerID, fact.WorkerIncarnation)] = struct{}{}
	}
	for _, item := range grouped {
		key := workerFactKey(item.WorkerID, item.WorkerIncarnation)
		if _, ok := expected[key]; !ok {
			t.Fatalf("unexpected PostgreSQL estimate worker %s", key)
		}
		if item.ChargeCount != expectedChargesPerWorker {
			t.Fatalf("PostgreSQL estimate worker %s has %d charges, want %d", key, item.ChargeCount, expectedChargesPerWorker)
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		t.Fatalf("PostgreSQL estimate sweep missed %d worker facts", len(expected))
	}

	replayed, err := service.SweepImportedInvoice(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first {
		t.Fatalf("replayed PostgreSQL estimate sweep = %#v, want %#v", replayed, first)
	}

	var replayedRowCount int64
	if err := chargeScope().Count(&replayedRowCount).Error; err != nil {
		t.Fatal(err)
	}
	if replayedRowCount != firstRowCount {
		t.Fatalf("replayed PostgreSQL estimate row count = %d, want %d", replayedRowCount, firstRowCount)
	}
}

func openBillingPostgresIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()

	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertBillingPostgresWorker(
	t *testing.T,
	db *gorm.DB,
	base time.Time,
	tenantID uuid.UUID,
	targetID uuid.UUID,
	workerID uuid.UUID,
	instanceUID string,
	index int,
) persistence.WorkerIncarnationFact {
	t.Helper()

	cpu := int64(1000)
	memory := int64(2 * 1024 * 1024 * 1024)
	ephemeral := int64(4 * 1024 * 1024 * 1024)
	terminatedAt := base.Add(2 * time.Hour)
	worker := persistence.WorkerInstance{
		ID:                    workerID,
		Incarnation:           1,
		InstanceUID:           instanceUID,
		ExecutionTargetID:     targetID,
		TargetKind:            "kubernetes",
		WorkerMode:            "general-pool",
		RegistrationTrustMode: "shared-token",
		ClusterID:             "billing-sweeper-cluster",
		Namespace:             "default",
		PodName:               fmt.Sprintf("billing-sweeper-%03d", index),
		Version:               "legacy",
		ProtocolVersion:       2,
		Capabilities:          map[string]any{},
		CompatibilityStatus:   "unknown",
		WorkerReleaseStatus:   "unmanaged",
		LeaseSupported:        true,
		FencingSupported:      true,
		AuthTokenHash:         secret.HashToken(fmt.Sprintf("billing-sweeper-token-%03d", index)),
		Status:                "terminated",
		AdministrativeStatus:  "active",
		RegisteredAt:          base,
		LastHeartbeatAt:       terminatedAt,
		TerminatedAt:          &terminatedAt,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}

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
		TerminalReason:                 stringPointer("completed"),
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

func orderedUUID(prefix string, seq int) uuid.UUID {
	return uuid.MustParse(orderedUUIDString(prefix, seq))
}

func orderedUUIDString(prefix string, seq int) string {
	return fmt.Sprintf("%s%012x", prefix, seq)
}

func workerFactKey(workerID uuid.UUID, incarnation int64) string {
	return fmt.Sprintf("%s/%d", workerID.String(), incarnation)
}
