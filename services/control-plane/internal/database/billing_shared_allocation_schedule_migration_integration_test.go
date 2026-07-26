package database

import (
	"context"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresBillingSharedAllocationSchedulePeriodMigration(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000075_worker_claim_release_facts.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "shared-calendar-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, billingSharedAllocationScheduleMigrationOnly(t)); err != nil {
		t.Fatal(err)
	}

	periodStart := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	createdAt := periodEnd.Add(2 * time.Hour)
	state := persistence.BillingSharedAllocationSchedulePeriod{
		ScheduleConfigSHA256:    "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
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
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}

	startedAt := createdAt
	nextAttemptAt := startedAt.Add(6 * time.Hour)
	if err := db.Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
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
		t.Fatalf("claim PostgreSQL shared schedule period: %v", err)
	}
	finishedAt := startedAt.Add(time.Minute)
	if err := db.Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
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
		t.Fatalf("complete PostgreSQL shared schedule period: %v", err)
	}

	var retained persistence.BillingSharedAllocationSchedulePeriod
	if err := db.Where(
		"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
		state.ScheduleConfigSHA256,
		state.BillingPeriodStartAt,
		state.BillingPeriodEndAt,
	).Take(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if retained.AttemptCount != 1 || retained.LastOutcome != "completed" ||
		retained.LastStartedAt == nil || retained.LastSuccessAt == nil {
		t.Fatalf("PostgreSQL shared schedule state = %#v", retained)
	}

	invalid := state
	invalid.ScheduleConfigSHA256 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	invalid.BillingPeriodEndAt = periodEnd.AddDate(0, 1, 0)
	invalid.NextAttemptAt = invalid.BillingPeriodEndAt.Add(time.Hour)
	assertStage4MigrationRejected(
		t,
		db.Create(&invalid).Error,
		"chk_billing_shared_allocation_schedule_period",
	)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
			Where(
				"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
				state.ScheduleConfigSHA256,
				state.BillingPeriodStartAt,
				state.BillingPeriodEndAt,
			).
			Update("attempt_count", 0).Error,
		"chk_billing_shared_allocation_schedule_timeline_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
			Where(
				"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
				state.ScheduleConfigSHA256,
				state.BillingPeriodStartAt,
				state.BillingPeriodEndAt,
			).
			Updates(map[string]any{"last_outcome": "failed", "last_error_code": "rewritten"}).Error,
		"chk_billing_shared_allocation_schedule_outcome_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Delete(
			&persistence.BillingSharedAllocationSchedulePeriod{},
			"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
			state.ScheduleConfigSHA256,
			state.BillingPeriodStartAt,
			state.BillingPeriodEndAt,
		).Error,
		"chk_billing_shared_allocation_schedule_delete_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Delete(&persistence.ExecutionTarget{}, "id = ?", domain.ExecutionTargetID).Error,
		"billing_shared_allocation_schedule_periods_execution_target_id_fkey",
	)

	if err := Migrate(ctx, db, billingSharedAllocationScheduleMigrationOnly(t)); err != nil {
		t.Fatalf("replay Migration 080: %v", err)
	}
	var applied int64
	if err := db.Table("control_plane_schema_migrations").Where("version = ?", 80).Count(&applied).Error; err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("Migration 080 records = %d, want 1", applied)
	}
}

func billingSharedAllocationScheduleMigrationOnly(t *testing.T) fs.FS {
	t.Helper()
	const name = "000080_billing_shared_allocation_calendar_schedules.sql"
	payload, err := fs.ReadFile(migrations.Files, name)
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{name: &fstest.MapFile{Data: payload}}
}
