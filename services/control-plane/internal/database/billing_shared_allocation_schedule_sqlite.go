package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateBillingSharedAllocationScheduleSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_billing_shared_allocation_schedule_due
		 ON billing_shared_allocation_schedule_periods (
		   next_attempt_at, execution_target_id, billing_period_start_at
		 )`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_allocation_schedule_periods_insert`,
		`CREATE TRIGGER trg_billing_shared_allocation_schedule_periods_insert
		 BEFORE INSERT ON billing_shared_allocation_schedule_periods
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid billing shared allocation schedule period')
		   WHERE length(NEW.schedule_config_sha256) <> 64
		      OR NEW.schedule_config_sha256 GLOB '*[^0-9a-f]*'
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id
		      )
		      OR NEW.provider NOT IN ('aws', 'gcp', 'azure')
		      OR length(NEW.currency_code) <> 3
		      OR NEW.currency_code <> upper(NEW.currency_code)
		      OR NEW.schedule_kind NOT IN ('static', 'monthly-utc')
		      OR julianday(NEW.billing_period_end_at) <= julianday(NEW.billing_period_start_at)
		      OR julianday(NEW.billing_period_end_at) > julianday(NEW.billing_period_start_at) + 366
		      OR (
		        NEW.schedule_kind = 'monthly-utc'
		        AND (
		          datetime(NEW.billing_period_start_at) <> datetime(date(NEW.billing_period_start_at, 'start of month'))
		          OR datetime(NEW.billing_period_end_at) <> datetime(NEW.billing_period_start_at, '+1 month')
		        )
		      )
		      OR NEW.settlement_delay_seconds NOT BETWEEN 60 AND 7776000
		      OR NEW.schedule_interval_seconds < 60
		      OR julianday(NEW.next_attempt_at) <
		         julianday(NEW.billing_period_end_at, '+' || NEW.settlement_delay_seconds || ' seconds')
		      OR NEW.attempt_count < 0
		      OR julianday(NEW.created_at) > julianday(NEW.updated_at)
		      OR NOT (
		        (
		          NEW.last_outcome = 'never'
		          AND NEW.attempt_count = 0
		          AND NEW.last_started_at IS NULL
		          AND NEW.last_finished_at IS NULL
		          AND NEW.last_success_at IS NULL
		          AND NEW.last_error_code IS NULL
		        )
		        OR (
		          NEW.last_outcome = 'running'
		          AND NEW.attempt_count > 0
		          AND NEW.last_started_at IS NOT NULL
		          AND julianday(NEW.next_attempt_at) > julianday(NEW.last_started_at)
		          AND NEW.last_error_code IS NULL
		        )
		        OR (
		          NEW.last_outcome = 'completed'
		          AND NEW.attempt_count > 0
		          AND NEW.last_started_at IS NOT NULL
		          AND julianday(NEW.last_finished_at) >= julianday(NEW.last_started_at)
		          AND NEW.last_success_at IS NEW.last_finished_at
		          AND NEW.last_error_code IS NULL
		        )
		        OR (
		          NEW.last_outcome = 'failed'
		          AND NEW.attempt_count > 0
		          AND NEW.last_started_at IS NOT NULL
		          AND julianday(NEW.last_finished_at) >= julianday(NEW.last_started_at)
		          AND length(NEW.last_error_code) BETWEEN 1 AND 120
		          AND NEW.last_error_code NOT GLOB '*[^a-z0-9_-]*'
		          AND (NEW.last_success_at IS NULL OR julianday(NEW.last_success_at) <= julianday(NEW.last_finished_at))
		        )
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_allocation_schedule_periods_update`,
		`CREATE TRIGGER trg_billing_shared_allocation_schedule_periods_update
		 BEFORE UPDATE ON billing_shared_allocation_schedule_periods
		 BEGIN
		   SELECT RAISE(ABORT, 'billing shared allocation schedule period identity is immutable')
		   WHERE NEW.schedule_config_sha256 IS NOT OLD.schedule_config_sha256
		      OR NEW.billing_period_start_at IS NOT OLD.billing_period_start_at
		      OR NEW.billing_period_end_at IS NOT OLD.billing_period_end_at
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.provider IS NOT OLD.provider
		      OR NEW.currency_code IS NOT OLD.currency_code
		      OR NEW.schedule_kind IS NOT OLD.schedule_kind
		      OR NEW.settlement_delay_seconds IS NOT OLD.settlement_delay_seconds
		      OR NEW.schedule_interval_seconds IS NOT OLD.schedule_interval_seconds
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'billing shared allocation schedule period timeline cannot regress')
		   WHERE NEW.attempt_count < OLD.attempt_count
		      OR julianday(NEW.next_attempt_at) < julianday(OLD.next_attempt_at)
		      OR julianday(NEW.updated_at) < julianday(OLD.updated_at)
		      OR (OLD.last_started_at IS NOT NULL AND (
		        NEW.last_started_at IS NULL OR julianday(NEW.last_started_at) < julianday(OLD.last_started_at)
		      ))
		      OR (OLD.last_finished_at IS NOT NULL AND (
		        NEW.last_finished_at IS NULL OR julianday(NEW.last_finished_at) < julianday(OLD.last_finished_at)
		      ))
		      OR (OLD.last_success_at IS NOT NULL AND (
		        NEW.last_success_at IS NULL OR julianday(NEW.last_success_at) < julianday(OLD.last_success_at)
		      ));

		   SELECT RAISE(ABORT, 'billing shared allocation schedule period claim is invalid')
		   WHERE NEW.last_started_at IS NOT OLD.last_started_at
		     AND (
		       NEW.last_started_at IS NULL
		       OR (OLD.last_started_at IS NOT NULL AND julianday(NEW.last_started_at) <= julianday(OLD.last_started_at))
		       OR NEW.attempt_count <> OLD.attempt_count + 1
		       OR NEW.last_outcome <> 'running'
		       OR NEW.last_error_code IS NOT NULL
		       OR julianday(NEW.next_attempt_at) <= julianday(NEW.last_started_at)
		     );

		   SELECT RAISE(ABORT, 'billing shared allocation schedule attempt count requires a new claim')
		   WHERE NEW.last_started_at IS OLD.last_started_at
		     AND NEW.attempt_count <> OLD.attempt_count;

		   SELECT RAISE(ABORT, 'billing shared allocation schedule period completion is invalid')
		   WHERE NEW.last_finished_at IS NOT OLD.last_finished_at
		     AND (
		       OLD.last_outcome <> 'running'
		       OR NEW.last_finished_at IS NULL
		       OR NEW.last_started_at IS NULL
		       OR julianday(NEW.last_finished_at) < julianday(NEW.last_started_at)
		       OR NEW.last_outcome NOT IN ('completed', 'failed')
		     );

		   SELECT RAISE(ABORT, 'billing shared allocation schedule period mutation lacks a claim or completion')
		   WHERE NEW.last_finished_at IS OLD.last_finished_at
		     AND NEW.last_started_at IS OLD.last_started_at
		     AND (
		       NEW.next_attempt_at IS NOT OLD.next_attempt_at
		       OR NEW.last_outcome IS NOT OLD.last_outcome
		       OR NEW.last_error_code IS NOT OLD.last_error_code
		       OR NEW.last_success_at IS NOT OLD.last_success_at
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_allocation_schedule_periods_delete`,
		`CREATE TRIGGER trg_billing_shared_allocation_schedule_periods_delete
		 BEFORE DELETE ON billing_shared_allocation_schedule_periods
		 BEGIN
		   SELECT RAISE(ABORT, 'billing shared allocation schedule periods cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_targets_shared_schedule_restrict_delete`,
		`CREATE TRIGGER trg_execution_targets_shared_schedule_restrict_delete
		 BEFORE DELETE ON execution_targets
		 WHEN EXISTS (
		   SELECT 1 FROM billing_shared_allocation_schedule_periods
		   WHERE execution_target_id = OLD.id
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'billing shared allocation schedule parent is retained');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite billing shared allocation schedule safety: %w", err)
		}
	}
	return nil
}
