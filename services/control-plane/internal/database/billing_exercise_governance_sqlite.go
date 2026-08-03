package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateBillingExerciseGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_billing_exercises_insert`,
		`DROP TRIGGER IF EXISTS trg_stage6_billing_exercises_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_billing_exercises_guard`,
		`DROP TRIGGER IF EXISTS trg_stage6_billing_exercises_no_delete`,
		`DROP TRIGGER IF EXISTS trg_stage6_billing_exercise_approvals_insert`,
		`DROP TRIGGER IF EXISTS trg_stage6_billing_exercise_approvals_no_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_billing_exercise_approvals_no_delete`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_billing_exercise_approval_gate`,
		`DROP TRIGGER IF EXISTS trg_legacy_billing_exercises_no_insert`,
		`CREATE TRIGGER trg_legacy_billing_exercises_no_insert BEFORE INSERT ON stage6_billing_exercises
		 BEGIN SELECT RAISE(ABORT, 'historical Billing exercise records are read-only'); END`,
		`DROP TRIGGER IF EXISTS trg_legacy_billing_exercises_no_update`,
		`CREATE TRIGGER trg_legacy_billing_exercises_no_update BEFORE UPDATE ON stage6_billing_exercises
		 BEGIN SELECT RAISE(ABORT, 'historical Billing exercise records are read-only'); END`,
		`DROP TRIGGER IF EXISTS trg_legacy_billing_exercises_no_delete`,
		`CREATE TRIGGER trg_legacy_billing_exercises_no_delete BEFORE DELETE ON stage6_billing_exercises
		 BEGIN SELECT RAISE(ABORT, 'historical Billing exercise records are read-only'); END`,
		`DROP TRIGGER IF EXISTS trg_legacy_billing_exercise_approvals_no_insert`,
		`CREATE TRIGGER trg_legacy_billing_exercise_approvals_no_insert BEFORE INSERT ON stage6_billing_exercise_approvals
		 BEGIN SELECT RAISE(ABORT, 'historical Billing exercise approvals are read-only'); END`,
		`DROP TRIGGER IF EXISTS trg_legacy_billing_exercise_approvals_no_update`,
		`CREATE TRIGGER trg_legacy_billing_exercise_approvals_no_update BEFORE UPDATE ON stage6_billing_exercise_approvals
		 BEGIN SELECT RAISE(ABORT, 'historical Billing exercise approvals are read-only'); END`,
		`DROP TRIGGER IF EXISTS trg_legacy_billing_exercise_approvals_no_delete`,
		`CREATE TRIGGER trg_legacy_billing_exercise_approvals_no_delete BEFORE DELETE ON stage6_billing_exercise_approvals
		 BEGIN SELECT RAISE(ABORT, 'historical Billing exercise approvals are read-only'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("lock historical Billing exercise SQLite state: %w", err)
		}
	}
	return nil
}
