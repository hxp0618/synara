package database

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresCloudCostActualInvoiceTenantConstraints(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	secondTenant := persistence.Tenant{
		ID: uuid.New(), Slug: "billing-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Name: "Billing migration second tenant", Status: "active", PlanCode: "free", Region: "default",
		Settings: map[string]any{}, CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := persistence.InTransaction(ctx, db, func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&secondTenant).Error; err != nil {
			return err
		}
		return tx.WithContext(ctx).Create(&persistence.TenantMembership{
			TenantID: secondTenant.ID, UserID: domain.UserID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}

	periodStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	newImport := func(tenantID uuid.UUID) persistence.BillingActualInvoiceImport {
		return persistence.BillingActualInvoiceImport{
			ID: uuid.New(), TenantID: tenantID, Provider: "aws", ExternalImportID: "shared-cur-2026-07",
			BillingPeriodStartAt: periodStart, BillingPeriodEndAt: periodEnd, CurrencyCode: "USD",
			SourceChecksum: strings.Repeat("a", 64), ImportedAt: now, CreatedAt: now,
		}
	}
	firstImport := newImport(domain.TenantID)
	secondImport := newImport(secondTenant.ID)
	if err := db.Create(&firstImport).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&secondImport).Error; err != nil {
		t.Fatalf("same provider external ID in another tenant: %v", err)
	}
	duplicate := newImport(domain.TenantID)
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("PostgreSQL accepted a duplicate actual invoice external ID in one tenant")
	}

	newLine := func(tenantID, importID uuid.UUID, externalLineID string) persistence.BillingActualInvoiceLine {
		return persistence.BillingActualInvoiceLine{
			ID: uuid.New(), TenantID: tenantID, InvoiceImportID: importID, ExternalLineID: externalLineID,
			Provider: "aws", CurrencyCode: "USD", ChargeKind: "cpu",
			ResourceCorrelationKey: "kubernetes:cluster-a:us-east-1:synara:pod-a:instance-a",
			BillingPeriodStartAt:   periodStart, BillingPeriodEndAt: periodEnd, AmountMicros: 2_000_000,
			ReconciliationState: "pending", CreatedAt: now,
		}
	}
	firstLine := newLine(domain.TenantID, firstImport.ID, "line-a")
	secondLine := newLine(secondTenant.ID, secondImport.ID, "line-a")
	if err := db.Create(&firstLine).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&secondLine).Error; err != nil {
		t.Fatalf("same normalized line identity in another tenant: %v", err)
	}
	crossTenantLine := newLine(domain.TenantID, secondImport.ID, "cross-tenant-line")
	if err := db.Create(&crossTenantLine).Error; err == nil {
		t.Fatal("PostgreSQL accepted an actual invoice line referencing another tenant's import")
	}
	if err := db.Model(&persistence.BillingActualInvoiceLine{}).
		Where("tenant_id = ? AND id = ?", domain.TenantID, firstLine.ID).
		Update("tenant_id", secondTenant.ID).Error; err == nil {
		t.Fatal("PostgreSQL accepted mutation of an actual invoice line tenant identity")
	}
}

func TestPostgresCloudCostTariffsAreImmutable(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	db.Config.TranslateError = false
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	endAt := now.Add(2 * time.Hour)
	tariff := persistence.BillingProviderTariff{
		ID:                         uuid.New(),
		Provider:                   "aws",
		Region:                     "us-east-1",
		CurrencyCode:               "USD",
		Version:                    1,
		EffectiveStartAt:           now,
		EffectiveEndAt:             &endAt,
		CPUCoreHourRateMicros:      3_600_000,
		MemoryGiBHourRateMicros:    1_000_000,
		EphemeralGiBHourRateMicros: 500_000,
		RequestRateMicros:          300_000,
		PodHourRateMicros:          3_600_000,
		CreatedAt:                  now,
	}
	if err := db.Create(&tariff).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.Model(&persistence.BillingProviderTariff{}).
		Where("id = ?", tariff.ID).
		Update("cpu_core_hour_rate_micros", int64(4_200_000)).Error; err == nil {
		t.Fatal("PostgreSQL accepted mutation of an immutable billing tariff")
	} else if !strings.Contains(err.Error(), "billing provider tariffs are immutable") {
		t.Fatalf("billing tariff update error = %v", err)
	}

	if err := db.Delete(&persistence.BillingProviderTariff{}, "id = ?", tariff.ID).Error; err == nil {
		t.Fatal("PostgreSQL accepted deletion of an immutable billing tariff")
	} else if !strings.Contains(err.Error(), "billing provider tariffs are immutable") {
		t.Fatalf("billing tariff delete error = %v", err)
	}
}

func TestPostgresCloudCostTariffOverlapIsSerialized(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	db.Config.TranslateError = false
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	startAt := time.Now().UTC().Truncate(time.Microsecond)
	endAt := startAt.Add(2 * time.Hour)
	provider := "test-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	region := "concurrency-" + uuid.NewString()
	newTariff := func(version int64) persistence.BillingProviderTariff {
		return persistence.BillingProviderTariff{
			ID: uuid.New(), Provider: provider, Region: region, CurrencyCode: "USD", Version: version,
			EffectiveStartAt: startAt, EffectiveEndAt: &endAt,
			CPUCoreHourRateMicros: 1, CreatedAt: startAt,
		}
	}

	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for version := int64(1); version <= 2; version++ {
		tariff := newTariff(version)
		workers.Add(1)
		go func() {
			defer workers.Done()
			results <- db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				ready <- struct{}{}
				<-start
				return tx.Create(&tariff).Error
			})
		}()
	}
	<-ready
	<-ready
	close(start)
	workers.Wait()
	close(results)

	succeeded := 0
	rejectedOverlap := 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if strings.Contains(err.Error(), "billing provider tariff effective interval overlaps another tariff") {
			rejectedOverlap++
			continue
		}
		t.Fatalf("unexpected concurrent tariff insert error: %v", err)
	}
	if succeeded != 1 || rejectedOverlap != 1 {
		t.Fatalf("concurrent tariff results: succeeded=%d overlapRejected=%d", succeeded, rejectedOverlap)
	}
	var count int64
	if err := db.Model(&persistence.BillingProviderTariff{}).
		Where("provider = ? AND region = ? AND currency_code = ?", provider, region, "USD").
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted overlapping tariff count = %d, want 1", count)
	}
}
