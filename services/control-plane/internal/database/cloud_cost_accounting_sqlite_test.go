package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteCloudCostTariffsAreImmutable(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
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
	if err := store.DB().Create(&tariff).Error; err != nil {
		t.Fatal(err)
	}
	overlapping := tariff
	overlapping.ID = uuid.New()
	overlapping.Version = 2
	overlapping.EffectiveStartAt = now.Add(time.Hour)
	overlappingEndAt := endAt.Add(time.Hour)
	overlapping.EffectiveEndAt = &overlappingEndAt
	assertSQLiteStage4Rejected(
		t,
		store.DB().Create(&overlapping).Error,
		"billing provider tariff effective interval overlaps another tariff",
	)
	adjacent := tariff
	adjacent.ID = uuid.New()
	adjacent.Version = 3
	adjacent.EffectiveStartAt = endAt
	adjacentEndAt := endAt.Add(time.Hour)
	adjacent.EffectiveEndAt = &adjacentEndAt
	if err := store.DB().Create(&adjacent).Error; err != nil {
		t.Fatalf("SQLite rejected adjacent billing tariff interval: %v", err)
	}

	assertSQLiteStage4Rejected(
		t,
		store.DB().Model(&persistence.BillingProviderTariff{}).
			Where("id = ?", tariff.ID).
			Update("cpu_core_hour_rate_micros", int64(4_200_000)).Error,
		"billing provider tariffs are immutable",
	)
	assertSQLiteStage4Rejected(
		t,
		store.DB().Delete(&persistence.BillingProviderTariff{}, "id = ?", tariff.ID).Error,
		"billing provider tariffs are immutable",
	)
}
