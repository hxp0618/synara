package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateUsageSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`UPDATE execution_usage_summaries
		 SET provider_cost_reported = 1
		 WHERE provider_cost_micros > 0 AND provider_cost_reported = 0`,
		`DROP TRIGGER IF EXISTS trg_execution_usage_provider_cost_reporting_insert`,
		`CREATE TRIGGER trg_execution_usage_provider_cost_reporting_insert
		 BEFORE INSERT ON execution_usage_summaries
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Usage Provider cost reporting')
		   WHERE NEW.provider_cost_reported NOT IN (0, 1)
		      OR (NEW.provider_cost_reported = 0 AND NEW.provider_cost_micros <> 0);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_usage_provider_cost_reporting_update`,
		`CREATE TRIGGER trg_execution_usage_provider_cost_reporting_update
		 BEFORE UPDATE ON execution_usage_summaries
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Usage Provider cost reporting is monotonic')
		   WHERE NEW.provider_cost_reported NOT IN (0, 1)
		      OR (NEW.provider_cost_reported = 0 AND NEW.provider_cost_micros <> 0)
		      OR (OLD.provider_cost_reported = 1 AND NEW.provider_cost_reported = 0)
		      OR NEW.provider_cost_micros < OLD.provider_cost_micros
		      OR (OLD.provider_cost_reported = 1 AND NEW.currency_code <> OLD.currency_code);
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Execution Usage Provider cost reporting safety migration: %w", err)
		}
	}
	return nil
}
