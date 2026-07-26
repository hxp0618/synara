package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migratePlatformRoutingAuthoritySQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_platform_routing_publications_target_received
		 ON platform_routing_publications (execution_target_id, received_at DESC, publisher_identity, nonce)`,
		`DROP TRIGGER IF EXISTS trg_platform_routing_publications_insert`,
		`CREATE TRIGGER trg_platform_routing_publications_insert
		 BEFORE INSERT ON platform_routing_publications
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Platform routing publication receipt')
		   WHERE length(trim(NEW.publisher_identity)) NOT BETWEEN 1 AND 160
		      OR length(trim(NEW.key_id)) NOT BETWEEN 1 AND 160
		      OR length(NEW.nonce) <> 36
		      OR length(NEW.public_key_sha256) <> 64
		      OR NEW.public_key_sha256 GLOB '*[^0-9a-f]*'
		      OR length(NEW.request_sha256) <> 64
		      OR NEW.request_sha256 GLOB '*[^0-9a-f]*'
		      OR length(NEW.response_sha256) <> 64
		      OR NEW.response_sha256 GLOB '*[^0-9a-f]*'
		      OR NOT json_valid(NEW.response)
		      OR json_type(NEW.response) <> 'object'
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_platform_routing_publications_update`,
		`CREATE TRIGGER trg_platform_routing_publications_update
		 BEFORE UPDATE ON platform_routing_publications
		 BEGIN
		   SELECT RAISE(ABORT, 'Platform routing publication receipts are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_platform_routing_publications_delete`,
		`CREATE TRIGGER trg_platform_routing_publications_delete
		 BEFORE DELETE ON platform_routing_publications
		 BEGIN
		   SELECT RAISE(ABORT, 'Platform routing publication receipts cannot be deleted');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Platform routing authority safety migration: %w", err)
		}
	}
	return nil
}
