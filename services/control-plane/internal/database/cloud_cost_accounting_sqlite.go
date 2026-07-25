package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateCloudCostAccountingSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_billing_provider_tariffs_non_overlapping_insert`,
		`CREATE TRIGGER trg_billing_provider_tariffs_non_overlapping_insert
		 BEFORE INSERT ON billing_provider_tariffs
		 BEGIN
		   SELECT RAISE(ABORT, 'billing provider tariff effective interval overlaps another tariff')
		   WHERE EXISTS (
		     SELECT 1
		     FROM billing_provider_tariffs AS existing
		     WHERE existing.provider = NEW.provider
		       AND existing.region = NEW.region
		       AND existing.currency_code = NEW.currency_code
		       AND (NEW.effective_end_at IS NULL OR existing.effective_start_at < NEW.effective_end_at)
		       AND (existing.effective_end_at IS NULL OR existing.effective_end_at > NEW.effective_start_at)
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_provider_tariffs_immutable_update`,
		`CREATE TRIGGER trg_billing_provider_tariffs_immutable_update
		 BEFORE UPDATE ON billing_provider_tariffs
		 BEGIN
		   SELECT RAISE(ABORT, 'billing provider tariffs are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_provider_tariffs_immutable_delete`,
		`CREATE TRIGGER trg_billing_provider_tariffs_immutable_delete
		 BEFORE DELETE ON billing_provider_tariffs
		 BEGIN
		   SELECT RAISE(ABORT, 'billing provider tariffs are immutable');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite cloud cost accounting safety migration: %w", err)
		}
	}
	return nil
}
