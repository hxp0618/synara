package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteBillingSharedAllocationSchedulePeriodSafety(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-shared-schedule-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"idx_billing_shared_allocation_schedule_due",
		"trg_billing_shared_allocation_schedule_periods_insert",
		"trg_billing_shared_allocation_schedule_periods_update",
		"trg_billing_shared_allocation_schedule_periods_delete",
		"trg_execution_targets_shared_schedule_restrict_delete",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite shared allocation schedule safety object %s count = %d, want 1", name, count)
		}
	}

	periodStart := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	createdAt := periodEnd.Add(2 * time.Hour)
	state := persistence.BillingSharedAllocationSchedulePeriod{
		ScheduleConfigSHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BillingPeriodStartAt:    periodStart,
		BillingPeriodEndAt:      periodEnd,
		ExecutionTargetID:       domain.ExecutionTargetID,
		Provider:                "aws",
		CurrencyCode:            "USD",
		ScheduleKind:            "monthly-utc",
		SettlementDelaySeconds:  3600,
		ScheduleIntervalSeconds: 21600,
		NextAttemptAt:           periodEnd.Add(time.Hour),
		LastOutcome:             "never",
		CreatedAt:               createdAt,
		UpdatedAt:               createdAt,
	}
	if err := store.DB().Create(&state).Error; err != nil {
		t.Fatalf("create valid monthly shared schedule period: %v", err)
	}

	startedAt := createdAt
	nextAttemptAt := startedAt.Add(6 * time.Hour)
	if err := store.DB().Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
		Where(
			"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
			state.ScheduleConfigSHA256,
			state.BillingPeriodStartAt,
			state.BillingPeriodEndAt,
		).
		Updates(map[string]any{
			"attempt_count": 1, "last_started_at": startedAt, "last_outcome": "running",
			"next_attempt_at": nextAttemptAt, "updated_at": startedAt,
		}).Error; err != nil {
		t.Fatalf("claim valid monthly shared schedule period: %v", err)
	}
	finishedAt := startedAt.Add(time.Minute)
	if err := store.DB().Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
		Where(
			"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
			state.ScheduleConfigSHA256,
			state.BillingPeriodStartAt,
			state.BillingPeriodEndAt,
		).
		Updates(map[string]any{
			"last_finished_at": finishedAt, "last_success_at": finishedAt,
			"last_outcome": "completed", "updated_at": finishedAt,
		}).Error; err != nil {
		t.Fatalf("complete valid monthly shared schedule period: %v", err)
	}

	invalidPeriod := state
	invalidPeriod.ScheduleConfigSHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	invalidPeriod.BillingPeriodEndAt = periodEnd.AddDate(0, 1, 0)
	invalidPeriod.NextAttemptAt = invalidPeriod.BillingPeriodEndAt.Add(time.Hour)
	if err := store.DB().Create(&invalidPeriod).Error; err == nil {
		t.Fatal("SQLite accepted a monthly schedule row spanning two periods")
	}
	if err := store.DB().Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
		Where(
			"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
			state.ScheduleConfigSHA256,
			state.BillingPeriodStartAt,
			state.BillingPeriodEndAt,
		).
		Update("attempt_count", 0).Error; err == nil {
		t.Fatal("SQLite accepted shared schedule attempt-count regression")
	}
	if err := store.DB().Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
		Where(
			"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
			state.ScheduleConfigSHA256,
			state.BillingPeriodStartAt,
			state.BillingPeriodEndAt,
		).
		Updates(map[string]any{"last_outcome": "failed", "last_error_code": "rewritten"}).Error; err == nil {
		t.Fatal("SQLite accepted outcome rewrite without a new claim")
	}
	if err := store.DB().Delete(
		&persistence.BillingSharedAllocationSchedulePeriod{},
		"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
		state.ScheduleConfigSHA256,
		state.BillingPeriodStartAt,
		state.BillingPeriodEndAt,
	).Error; err == nil {
		t.Fatal("SQLite accepted shared schedule period deletion")
	}
	if err := store.DB().Delete(&persistence.ExecutionTarget{}, "id = ?", domain.ExecutionTargetID).Error; err == nil {
		t.Fatal("SQLite accepted deletion of a Target retained by shared schedule state")
	}

	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatalf("repeat SQLite migration with durable schedule state: %v", err)
	}
	var retained persistence.BillingSharedAllocationSchedulePeriod
	if err := store.DB().Where(
		"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
		state.ScheduleConfigSHA256,
		state.BillingPeriodStartAt,
		state.BillingPeriodEndAt,
	).Take(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if retained.AttemptCount != 1 || retained.LastOutcome != "completed" || retained.LastSuccessAt == nil {
		t.Fatalf("repeated SQLite migration changed shared schedule state: %#v", retained)
	}
}
