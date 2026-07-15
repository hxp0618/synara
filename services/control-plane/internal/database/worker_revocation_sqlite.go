package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateWorkerRevocationSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`UPDATE worker_instances
		 SET administrative_status = 'revoked',
		     revoked_at = COALESCE(compatibility_checked_at, terminated_at, last_heartbeat_at, registered_at, CURRENT_TIMESTAMP),
		     revoked_by = NULL,
		     revocation_reason = 'legacy-compatibility-revoked'
		 WHERE compatibility_status = 'revoked'
		   AND NOT EXISTS (
		     SELECT 1 FROM sqlite_master
		     WHERE type = 'trigger' AND name = 'trg_worker_instances_revocation_shape_insert'
		   )`,
		`UPDATE worker_instances
		 SET compatibility_status = 'incompatible',
		     compatibility_reason = COALESCE(
		       NULLIF(trim(compatibility_reason), ''),
		       'Worker compatibility revocation was migrated to administrative revocation.'
		     ),
		     compatibility_checked_at = COALESCE(compatibility_checked_at, revoked_at)
		 WHERE compatibility_status = 'revoked'
		   AND NOT EXISTS (
		     SELECT 1 FROM sqlite_master
		     WHERE type = 'trigger' AND name = 'trg_worker_instances_revocation_shape_insert'
		   )`,
		`UPDATE worker_instances
		 SET administrative_status = 'draining'
		 WHERE status = 'draining'
		   AND administrative_status = 'active'
		   AND revoked_at IS NULL
		   AND revoked_by IS NULL
		   AND revocation_reason IS NULL
		   AND NOT EXISTS (
		     SELECT 1 FROM sqlite_master
		     WHERE type = 'trigger' AND name = 'trg_worker_instances_revocation_shape_insert'
		   )`,
		`UPDATE worker_instances
		 SET administrative_status = 'active'
		 WHERE (administrative_status IS NULL OR administrative_status = '')
		   AND NOT EXISTS (
		     SELECT 1 FROM sqlite_master
		     WHERE type = 'trigger' AND name = 'trg_worker_instances_revocation_shape_insert'
		   )`,
		`INSERT OR IGNORE INTO worker_identity_tombstones (
		   execution_target_id,
		   cluster_id,
		   namespace,
		   pod_name,
		   worker_id,
		   worker_incarnation,
		   revoked_at,
		   revoked_by,
		   revocation_reason
		 )
		 SELECT
		   revoked.execution_target_id,
		   revoked.cluster_id,
		   revoked.namespace,
		   revoked.pod_name,
		   revoked.id,
		   revoked.incarnation,
		   revoked.revoked_at,
		   revoked.revoked_by,
		   revoked.revocation_reason
		 FROM (
		   SELECT
		     execution_target_id,
		     cluster_id,
		     namespace,
		     pod_name,
		     id,
		     incarnation,
		     revoked_at,
		     revoked_by,
		     revocation_reason,
		     row_number() OVER (
		       PARTITION BY execution_target_id, cluster_id, namespace, pod_name
		       ORDER BY incarnation DESC, revoked_at DESC, id DESC
		     ) AS identity_rank
		   FROM worker_instances
		   WHERE administrative_status = 'revoked'
		 ) AS revoked
		 WHERE revoked.identity_rank = 1`,
		`DROP INDEX IF EXISTS idx_worker_instances_compatibility`,
		`DROP INDEX IF EXISTS uq_worker_instances_active_pod`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_instances_active_logical_identity
		 ON worker_instances (execution_target_id, cluster_id, namespace, pod_name)
		 WHERE administrative_status <> 'revoked' AND status <> 'terminated'`,
		`CREATE INDEX IF NOT EXISTS idx_worker_instances_claimability
		 ON worker_instances (
		   execution_target_id,
		   administrative_status,
		   compatibility_status,
		   status,
		   last_heartbeat_at,
		   id
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_worker_instances_logical_identity
		 ON worker_instances (
		   execution_target_id,
		   cluster_id,
		   namespace,
		   pod_name,
		   administrative_status,
		   id
		 )`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_revocation_shape_insert`,
		`CREATE TRIGGER trg_worker_instances_revocation_shape_insert
		 BEFORE INSERT ON worker_instances
		 WHEN NEW.administrative_status IS NULL
		   OR NEW.administrative_status NOT IN ('active', 'draining', 'revoked')
		   OR NEW.compatibility_status IS NULL
		   OR NEW.compatibility_status NOT IN ('unknown', 'compatible', 'incompatible')
		   OR (
		     NEW.compatibility_status = 'unknown'
		     AND NEW.compatibility_checked_at IS NOT NULL
		   )
		   OR (
		     NEW.compatibility_status <> 'unknown'
		     AND NEW.compatibility_checked_at IS NULL
		   )
		   OR (
		     NEW.administrative_status = 'revoked'
		     AND (
		       NEW.revoked_at IS NULL
		       OR NEW.revoked_by IS NULL
		       OR NEW.revocation_reason IS NULL
		       OR length(trim(NEW.revocation_reason)) NOT BETWEEN 1 AND 2000
		     )
		   )
		   OR (
		     NEW.administrative_status <> 'revoked'
		     AND (
		       NEW.revoked_at IS NOT NULL
		       OR NEW.revoked_by IS NOT NULL
		       OR NEW.revocation_reason IS NOT NULL
		     )
		   )
		   OR (
		     NEW.revoked_by IS NOT NULL
		     AND NOT EXISTS (SELECT 1 FROM users AS actor WHERE actor.id = NEW.revoked_by)
		   )
		   OR (
		     NEW.administrative_status <> 'revoked'
		     AND EXISTS (
		       SELECT 1
		       FROM worker_identity_tombstones AS tombstone
		       WHERE tombstone.execution_target_id = NEW.execution_target_id
		         AND tombstone.cluster_id = NEW.cluster_id
		         AND tombstone.namespace = NEW.namespace
		         AND tombstone.pod_name = NEW.pod_name
		     )
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid or revoked Worker administrative identity');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_revocation_shape_update`,
		`CREATE TRIGGER trg_worker_instances_revocation_shape_update
		 BEFORE UPDATE OF administrative_status, revoked_at, revoked_by, revocation_reason, compatibility_status, compatibility_checked_at
		 ON worker_instances
		 WHEN NEW.administrative_status IS NULL
		   OR NEW.administrative_status NOT IN ('active', 'draining', 'revoked')
		   OR NEW.compatibility_status IS NULL
		   OR NEW.compatibility_status NOT IN ('unknown', 'compatible', 'incompatible')
		   OR (
		     NEW.compatibility_status = 'unknown'
		     AND NEW.compatibility_checked_at IS NOT NULL
		   )
		   OR (
		     NEW.compatibility_status <> 'unknown'
		     AND NEW.compatibility_checked_at IS NULL
		   )
		   OR (
		     NEW.administrative_status = 'revoked'
		     AND (
		       NEW.revoked_at IS NULL
		       OR NEW.revocation_reason IS NULL
		       OR length(trim(NEW.revocation_reason)) NOT BETWEEN 1 AND 2000
		       OR (OLD.administrative_status <> 'revoked' AND NEW.revoked_by IS NULL)
		       OR (NEW.revoked_by IS NULL AND NEW.revocation_reason <> 'legacy-compatibility-revoked')
		     )
		   )
		   OR (
		     NEW.administrative_status <> 'revoked'
		     AND (
		       NEW.revoked_at IS NOT NULL
		       OR NEW.revoked_by IS NOT NULL
		       OR NEW.revocation_reason IS NOT NULL
		     )
		   )
		   OR (
		     NEW.revoked_by IS NOT NULL
		     AND NOT EXISTS (SELECT 1 FROM users AS actor WHERE actor.id = NEW.revoked_by)
		   )
		   OR (
		     NEW.administrative_status <> 'revoked'
		     AND EXISTS (
		       SELECT 1
		       FROM worker_identity_tombstones AS tombstone
		       WHERE tombstone.execution_target_id = NEW.execution_target_id
		         AND tombstone.cluster_id = NEW.cluster_id
		         AND tombstone.namespace = NEW.namespace
		         AND tombstone.pod_name = NEW.pod_name
		     )
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid or revoked Worker administrative identity');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_identity_immutable`,
		`CREATE TRIGGER trg_worker_instances_identity_immutable
		 BEFORE UPDATE OF execution_target_id, cluster_id, namespace, pod_name
		 ON worker_instances
		 WHEN NEW.execution_target_id IS NOT OLD.execution_target_id
		   OR NEW.cluster_id IS NOT OLD.cluster_id
		   OR NEW.namespace IS NOT OLD.namespace
		   OR NEW.pod_name IS NOT OLD.pod_name
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker logical identity is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_revoked_immutable_update`,
		`CREATE TRIGGER trg_worker_instances_revoked_immutable_update
		 BEFORE UPDATE ON worker_instances
		 WHEN OLD.administrative_status = 'revoked'
		 BEGIN
		   SELECT RAISE(ABORT, 'revoked Worker registration is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_revoked_immutable_delete`,
		`CREATE TRIGGER trg_worker_instances_revoked_immutable_delete
		 BEFORE DELETE ON worker_instances
		 WHEN OLD.administrative_status = 'revoked'
		 BEGIN
		   SELECT RAISE(ABORT, 'revoked Worker registration is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_identity_tombstones_source`,
		`CREATE TRIGGER trg_worker_identity_tombstones_source
		 BEFORE INSERT ON worker_identity_tombstones
		 WHEN NOT EXISTS (
		   SELECT 1
		   FROM worker_instances AS worker
		   WHERE worker.id = NEW.worker_id
		     AND worker.execution_target_id = NEW.execution_target_id
		     AND worker.cluster_id = NEW.cluster_id
		     AND worker.namespace = NEW.namespace
		     AND worker.pod_name = NEW.pod_name
		     AND worker.incarnation = NEW.worker_incarnation
		     AND worker.administrative_status = 'revoked'
		     AND worker.revoked_at IS NEW.revoked_at
		     AND worker.revoked_by IS NEW.revoked_by
		     AND worker.revocation_reason = NEW.revocation_reason
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker identity tombstone does not match its revoked Worker source');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_tombstone_insert`,
		`CREATE TRIGGER trg_worker_instances_tombstone_insert
		 AFTER INSERT ON worker_instances
		 WHEN NEW.administrative_status = 'revoked'
		 BEGIN
		   INSERT INTO worker_identity_tombstones (
		     execution_target_id,
		     cluster_id,
		     namespace,
		     pod_name,
		     worker_id,
		     worker_incarnation,
		     revoked_at,
		     revoked_by,
		     revocation_reason
		   ) VALUES (
		     NEW.execution_target_id,
		     NEW.cluster_id,
		     NEW.namespace,
		     NEW.pod_name,
		     NEW.id,
		     NEW.incarnation,
		     NEW.revoked_at,
		     NEW.revoked_by,
		     NEW.revocation_reason
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_tombstone_update`,
		`CREATE TRIGGER trg_worker_instances_tombstone_update
		 AFTER UPDATE OF administrative_status ON worker_instances
		 WHEN OLD.administrative_status <> 'revoked'
		   AND NEW.administrative_status = 'revoked'
		 BEGIN
		   INSERT INTO worker_identity_tombstones (
		     execution_target_id,
		     cluster_id,
		     namespace,
		     pod_name,
		     worker_id,
		     worker_incarnation,
		     revoked_at,
		     revoked_by,
		     revocation_reason
		   ) VALUES (
		     NEW.execution_target_id,
		     NEW.cluster_id,
		     NEW.namespace,
		     NEW.pod_name,
		     NEW.id,
		     NEW.incarnation,
		     NEW.revoked_at,
		     NEW.revoked_by,
		     NEW.revocation_reason
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_identity_tombstones_immutable_update`,
		`CREATE TRIGGER trg_worker_identity_tombstones_immutable_update
		 BEFORE UPDATE ON worker_identity_tombstones
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker identity tombstone is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_identity_tombstones_immutable_delete`,
		`CREATE TRIGGER trg_worker_identity_tombstones_immutable_delete
		 BEFORE DELETE ON worker_identity_tombstones
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker identity tombstone is immutable');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Worker revocation safety migration: %w", err)
		}
	}
	return nil
}
