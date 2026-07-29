package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateWorkerStorageScrubSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_storage_scrubs_active
		 ON worker_storage_scrubs (worker_id, worker_incarnation)
		 WHERE status IN ('pending', 'failed')`,
		`DROP TRIGGER IF EXISTS trg_worker_storage_scrubs_insert`,
		`CREATE TRIGGER trg_worker_storage_scrubs_insert
		 BEFORE INSERT ON worker_storage_scrubs
		 FOR EACH ROW
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker storage scrub')
		   WHERE NEW.worker_incarnation <= 0
		      OR NEW.worker_instance_uid = ''
		      OR NEW.scope_kind NOT IN ('execution', 'workspace-cleanup')
		      OR NEW.scope_generation <= 0
		      OR NEW.scrub_generation <= 0
		      OR NEW.status NOT IN ('pending', 'acknowledged', 'failed')
		      OR NOT (
		        (NEW.status = 'pending' AND NEW.acknowledged_at IS NULL AND NEW.failed_at IS NULL
		          AND NEW.failure_code IS NULL AND NEW.failure_message IS NULL)
		        OR
		        (NEW.status = 'acknowledged' AND NEW.acknowledged_at IS NOT NULL AND NEW.failed_at IS NULL
		          AND NEW.failure_code IS NULL AND NEW.failure_message IS NULL)
		        OR
		        (NEW.status = 'failed' AND NEW.acknowledged_at IS NULL AND NEW.failed_at IS NOT NULL
		          AND NEW.failure_code IS NOT NULL)
		      )
		      OR (NEW.failure_code IS NOT NULL AND (length(NEW.failure_code) < 1 OR length(NEW.failure_code) > 160))
		      OR (NEW.failure_message IS NOT NULL AND length(NEW.failure_message) > 10000);

		   SELECT RAISE(ABORT, 'Worker storage scrub physical identity is invalid')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM worker_instances AS worker
		     WHERE worker.id = NEW.worker_id
		       AND worker.incarnation = NEW.worker_incarnation
		       AND worker.instance_uid = NEW.worker_instance_uid
		       AND worker.execution_target_id = NEW.execution_target_id
		       AND worker.worker_mode = 'general-pool'
		   );

		   SELECT RAISE(ABORT, 'Worker storage scrub generation is not monotonic')
		   WHERE NEW.scrub_generation <> COALESCE((
		     SELECT MAX(existing.scrub_generation) + 1
		     FROM worker_storage_scrubs AS existing
		     WHERE existing.worker_id = NEW.worker_id
		       AND existing.worker_incarnation = NEW.worker_incarnation
		   ), 1);
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_storage_scrubs_update`,
		`CREATE TRIGGER trg_worker_storage_scrubs_update
		 BEFORE UPDATE ON worker_storage_scrubs
		 FOR EACH ROW
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker storage scrub identity is immutable')
		   WHERE NEW.worker_id IS NOT OLD.worker_id
		      OR NEW.worker_instance_uid IS NOT OLD.worker_instance_uid
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.scope_kind IS NOT OLD.scope_kind
		      OR NEW.scope_id IS NOT OLD.scope_id
		      OR NEW.scope_generation IS NOT OLD.scope_generation
		      OR NEW.scrub_generation IS NOT OLD.scrub_generation
		      OR NEW.created_at IS NOT OLD.created_at
		      OR (
		        NEW.worker_incarnation IS NOT OLD.worker_incarnation
		        AND NOT (
		          OLD.status = 'pending'
		          AND NEW.status = 'pending'
		          AND NEW.worker_incarnation = OLD.worker_incarnation + 1
		          AND EXISTS (
		            SELECT 1 FROM worker_instances AS worker
		            WHERE worker.id = NEW.worker_id
		              AND worker.incarnation = NEW.worker_incarnation
		              AND worker.instance_uid = NEW.worker_instance_uid
		          )
		        )
		      );

		   SELECT RAISE(ABORT, 'Worker storage scrub terminal receipt is immutable')
		   WHERE OLD.status IN ('acknowledged', 'failed') AND (
		      NEW.status IS NOT OLD.status
		      OR NEW.acknowledged_at IS NOT OLD.acknowledged_at
		      OR NEW.failed_at IS NOT OLD.failed_at
		      OR NEW.failure_code IS NOT OLD.failure_code
		      OR NEW.failure_message IS NOT OLD.failure_message
		   );

		   SELECT RAISE(ABORT, 'invalid Worker storage scrub transition')
		   WHERE OLD.status = 'pending'
		     AND NOT (
		       (
		        NEW.status IN ('acknowledged', 'failed')
		        AND (
		        (NEW.status = 'acknowledged' AND NEW.acknowledged_at IS NOT NULL AND NEW.failed_at IS NULL
		          AND NEW.failure_code IS NULL AND NEW.failure_message IS NULL)
		        OR
		        (NEW.status = 'failed' AND NEW.acknowledged_at IS NULL AND NEW.failed_at IS NOT NULL
		          AND NEW.failure_code IS NOT NULL)
		        )
		       )
		       OR
		       (
		        NEW.status = 'pending'
		        AND NEW.worker_incarnation = OLD.worker_incarnation + 1
		        AND EXISTS (
		          SELECT 1 FROM worker_instances AS worker
		          WHERE worker.id = NEW.worker_id
		            AND worker.incarnation = NEW.worker_incarnation
		            AND worker.instance_uid = NEW.worker_instance_uid
		        )
		       )
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_storage_scrubs_delete`,
		`CREATE TRIGGER trg_worker_storage_scrubs_delete
		 BEFORE DELETE ON worker_storage_scrubs
		 FOR EACH ROW
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker storage scrubs are append-only');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Worker storage scrub safety: %w", err)
		}
	}
	return nil
}
