package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateWorkerReleaseSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP INDEX IF EXISTS uq_worker_release_revision_tenant_id`,
		`CREATE UNIQUE INDEX uq_worker_release_revision_tenant_id
		 ON worker_release_revisions (tenant_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_release_revision_target_id
		 ON worker_release_revisions (execution_target_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_release_revision_target_number
		 ON worker_release_revisions (execution_target_id, revision)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_release_revision_target_manifest
		 ON worker_release_revisions (execution_target_id, worker_manifest_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_release_transition_target_version
		 ON worker_release_transitions (execution_target_id, policy_version)`,
		`CREATE INDEX IF NOT EXISTS idx_worker_release_revisions_tenant_target
		 ON worker_release_revisions (tenant_id, execution_target_id, revision DESC, id)`,
		`CREATE INDEX IF NOT EXISTS idx_worker_release_revisions_manifest
		 ON worker_release_revisions (worker_manifest_id, execution_target_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_worker_release_transitions_tenant_target
		 ON worker_release_transitions (tenant_id, execution_target_id, policy_version DESC, id)`,
		`CREATE INDEX IF NOT EXISTS idx_worker_release_auto_rollback_pending
		 ON worker_release_auto_rollback_windows (status, expires_at, execution_target_id, policy_version)
		 WHERE status IN ('monitoring', 'rollback-pending')`,
		`CREATE INDEX IF NOT EXISTS idx_worker_instances_release_claimability
		 ON worker_instances (
		   execution_target_id, worker_release_status, worker_release_revision_id,
		   worker_release_channel, administrative_status, compatibility_status,
		   status, last_heartbeat_at, id
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_agent_executions_release_claimable
		 ON agent_executions (
		   execution_target_id, worker_release_revision_id, worker_release_channel, queued_at, id
		 )
		 WHERE status IN ('queued', 'recovering')`,
		`DROP TRIGGER IF EXISTS trg_worker_release_revisions_insert_shape`,
		`CREATE TRIGGER trg_worker_release_revisions_insert_shape
		 BEFORE INSERT ON worker_release_revisions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker release revision')
		   WHERE NEW.revision <= 0
		      OR length(NEW.description) > 2000
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id
		          AND target.tenant_id = NEW.tenant_id
		      )
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_manifests AS manifest
		        WHERE manifest.id = NEW.worker_manifest_id
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_revisions_immutable_update`,
		`CREATE TRIGGER trg_worker_release_revisions_immutable_update
		 BEFORE UPDATE ON worker_release_revisions
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker release revisions are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_revisions_immutable_delete`,
		`CREATE TRIGGER trg_worker_release_revisions_immutable_delete
		 BEFORE DELETE ON worker_release_revisions
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker release revisions are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_policies_insert_shape`,
		`CREATE TRIGGER trg_worker_release_policies_insert_shape
		 BEFORE INSERT ON worker_release_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker release policy')
		   WHERE NEW.policy_version <= 0
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id
		          AND target.tenant_id = NEW.tenant_id
		      )
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_release_revisions AS revision
		        WHERE revision.id = NEW.promoted_revision_id
		          AND revision.execution_target_id = NEW.execution_target_id
		          AND revision.tenant_id = NEW.tenant_id
		      )
		      OR (
		        NEW.canary_revision_id IS NULL
		        AND NEW.canary_percent <> 0
		      )
		      OR (
		        NEW.canary_revision_id IS NOT NULL
		        AND (
		          NEW.canary_revision_id = NEW.promoted_revision_id
		          OR NEW.canary_percent NOT BETWEEN 1 AND 100
		          OR NOT EXISTS (
		            SELECT 1 FROM worker_release_revisions AS revision
		            WHERE revision.id = NEW.canary_revision_id
		              AND revision.execution_target_id = NEW.execution_target_id
		              AND revision.tenant_id = NEW.tenant_id
		          )
		        )
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_policies_update_cas`,
		`CREATE TRIGGER trg_worker_release_policies_update_cas
		 BEFORE UPDATE ON worker_release_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker release policy ownership is immutable')
		   WHERE NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id;

		   SELECT RAISE(ABORT, 'Worker release policy version must advance exactly once')
		   WHERE NEW.policy_version <> OLD.policy_version + 1;

		   SELECT RAISE(ABORT, 'invalid Worker release policy')
		   WHERE NOT EXISTS (
		        SELECT 1 FROM worker_release_revisions AS revision
		        WHERE revision.id = NEW.promoted_revision_id
		          AND revision.execution_target_id = NEW.execution_target_id
		          AND revision.tenant_id = NEW.tenant_id
		      )
		      OR (NEW.canary_revision_id IS NULL AND NEW.canary_percent <> 0)
		      OR (
		        NEW.canary_revision_id IS NOT NULL
		        AND (
		          NEW.canary_revision_id = NEW.promoted_revision_id
		          OR NEW.canary_percent NOT BETWEEN 1 AND 100
		          OR NOT EXISTS (
		            SELECT 1 FROM worker_release_revisions AS revision
		            WHERE revision.id = NEW.canary_revision_id
		              AND revision.execution_target_id = NEW.execution_target_id
		              AND revision.tenant_id = NEW.tenant_id
		          )
		        )
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_policies_no_delete`,
		`CREATE TRIGGER trg_worker_release_policies_no_delete
		 BEFORE DELETE ON worker_release_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker release policies cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_transitions_insert_shape`,
		`CREATE TRIGGER trg_worker_release_transitions_insert_shape
		 BEFORE INSERT ON worker_release_transitions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker release transition')
		   WHERE NEW.policy_version <= 0
		      OR NEW.action NOT IN ('promote', 'canary', 'rollback')
		      OR length(trim(NEW.reason)) NOT BETWEEN 1 AND 2000
		      OR (NEW.request_id IS NOT NULL AND length(NEW.request_id) NOT BETWEEN 1 AND 160)
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_release_policies AS policy
		        WHERE policy.execution_target_id = NEW.execution_target_id
		          AND policy.tenant_id = NEW.tenant_id
		          AND policy.policy_version = NEW.policy_version
		          AND policy.promoted_revision_id = NEW.to_promoted_revision_id
		          AND policy.canary_revision_id IS NEW.to_canary_revision_id
		          AND policy.canary_percent = NEW.canary_percent
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_transitions_immutable_update`,
		`CREATE TRIGGER trg_worker_release_transitions_immutable_update
		 BEFORE UPDATE ON worker_release_transitions
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker release transitions are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_transitions_immutable_delete`,
		`CREATE TRIGGER trg_worker_release_transitions_immutable_delete
		 BEFORE DELETE ON worker_release_transitions
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker release transitions are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_auto_rollback_window_insert_shape`,
		`CREATE TRIGGER trg_worker_release_auto_rollback_window_insert_shape
		 BEFORE INSERT ON worker_release_auto_rollback_windows
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker release auto-rollback window')
		   WHERE NEW.policy_version <= 0
		      OR NEW.candidate_channel NOT IN ('promoted', 'canary')
		      OR NEW.candidate_revision_id = NEW.fallback_revision_id
		      OR NEW.expires_at <= NEW.started_at
		      OR NEW.minimum_executions NOT BETWEEN 1 AND 10000
		      OR NEW.failure_threshold NOT BETWEEN 1 AND NEW.minimum_executions
		      OR NEW.failure_rate_percent NOT BETWEEN 1 AND 100
		      OR NEW.status <> 'monitoring'
		      OR NEW.decision_reason IS NOT NULL
		      OR NEW.decision_at IS NOT NULL
		      OR NOT json_valid(NEW.evidence)
		      OR json_type(NEW.evidence) <> 'object'
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id
		          AND target.tenant_id = NEW.tenant_id
		      )
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_release_transitions AS transition
		        WHERE transition.execution_target_id = NEW.execution_target_id
		          AND transition.policy_version = NEW.policy_version
		          AND transition.tenant_id = NEW.tenant_id
		          AND (
		            (
		              NEW.candidate_channel = 'canary'
		              AND transition.action = 'canary'
		              AND transition.to_canary_revision_id = NEW.candidate_revision_id
		              AND transition.to_promoted_revision_id = NEW.fallback_revision_id
		            )
		            OR
		            (
		              NEW.candidate_channel = 'promoted'
		              AND transition.action = 'promote'
		              AND transition.to_promoted_revision_id = NEW.candidate_revision_id
		              AND transition.from_promoted_revision_id = NEW.fallback_revision_id
		            )
		          )
		      )
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_release_revisions AS revision
		        WHERE revision.execution_target_id = NEW.execution_target_id
		          AND revision.id = NEW.candidate_revision_id
		      )
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_release_revisions AS revision
		        WHERE revision.execution_target_id = NEW.execution_target_id
		          AND revision.id = NEW.fallback_revision_id
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_auto_rollback_window_update_shape`,
		`CREATE TRIGGER trg_worker_release_auto_rollback_window_update_shape
		 BEFORE UPDATE ON worker_release_auto_rollback_windows
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker release auto-rollback window identity and policy are immutable')
		   WHERE NEW.id IS NOT OLD.id
		      OR NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.policy_version IS NOT OLD.policy_version
		      OR NEW.candidate_revision_id IS NOT OLD.candidate_revision_id
		      OR NEW.candidate_channel IS NOT OLD.candidate_channel
		      OR NEW.fallback_revision_id IS NOT OLD.fallback_revision_id
		      OR NEW.started_at IS NOT OLD.started_at
		      OR NEW.expires_at IS NOT OLD.expires_at
		      OR NEW.minimum_executions IS NOT OLD.minimum_executions
		      OR NEW.failure_threshold IS NOT OLD.failure_threshold
		      OR NEW.failure_rate_percent IS NOT OLD.failure_rate_percent
		      OR NEW.enabled_by IS NOT OLD.enabled_by
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'Worker release auto-rollback window status is terminal or regressed')
		   WHERE NOT (
		     (OLD.status = 'monitoring' AND NEW.status IN ('monitoring', 'rollback-pending', 'expired', 'superseded'))
		     OR
		     (OLD.status = 'rollback-pending' AND NEW.status IN ('rollback-pending', 'triggered', 'superseded'))
		   );

		   SELECT RAISE(ABORT, 'invalid Worker release auto-rollback decision')
		   WHERE NOT json_valid(NEW.evidence)
		      OR json_type(NEW.evidence) <> 'object'
		      OR (NEW.decision_reason IS NOT NULL AND length(trim(NEW.decision_reason)) NOT BETWEEN 1 AND 2000)
		      OR (NEW.status IN ('monitoring', 'expired') AND (NEW.decision_reason IS NOT NULL OR NEW.decision_at IS NOT NULL))
		      OR (NEW.status IN ('rollback-pending', 'triggered') AND (NEW.decision_reason IS NULL OR NEW.decision_at IS NULL));
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_release_auto_rollback_window_delete`,
		`CREATE TRIGGER trg_worker_release_auto_rollback_window_delete
		 BEFORE DELETE ON worker_release_auto_rollback_windows
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker release auto-rollback windows are durable release evidence');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_release_shape_insert`,
		`CREATE TRIGGER trg_worker_instances_release_shape_insert
		 BEFORE INSERT ON worker_instances
		 WHEN NOT (
		   (
		     NEW.worker_release_status = 'unmanaged'
		     AND NEW.worker_release_revision_id IS NULL
		     AND NEW.worker_release_channel IS NULL
		     AND NEW.worker_release_reason IS NULL
		     AND NEW.worker_release_checked_at IS NULL
		   )
		   OR
		   (
		     NEW.worker_release_status = 'active'
		     AND NEW.worker_release_revision_id IS NOT NULL
		     AND NEW.worker_release_channel IN ('promoted', 'canary')
		     AND NEW.worker_release_reason IS NULL
		     AND NEW.worker_release_checked_at IS NOT NULL
		   )
		   OR
		   (
		     NEW.worker_release_status = 'inactive'
		     AND NEW.worker_release_channel IS NULL
		     AND length(trim(NEW.worker_release_reason)) BETWEEN 1 AND 2000
		     AND NEW.worker_release_checked_at IS NOT NULL
		   )
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker release state');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_release_shape_update`,
		`CREATE TRIGGER trg_worker_instances_release_shape_update
		 BEFORE UPDATE OF worker_release_revision_id, worker_release_channel,
		   worker_release_status, worker_release_reason, worker_release_checked_at
		 ON worker_instances
		 WHEN NOT (
		   (
		     NEW.worker_release_status = 'unmanaged'
		     AND NEW.worker_release_revision_id IS NULL
		     AND NEW.worker_release_channel IS NULL
		     AND NEW.worker_release_reason IS NULL
		     AND NEW.worker_release_checked_at IS NULL
		   )
		   OR
		   (
		     NEW.worker_release_status = 'active'
		     AND NEW.worker_release_revision_id IS NOT NULL
		     AND NEW.worker_release_channel IN ('promoted', 'canary')
		     AND NEW.worker_release_reason IS NULL
		     AND NEW.worker_release_checked_at IS NOT NULL
		   )
		   OR
		   (
		     NEW.worker_release_status = 'inactive'
		     AND NEW.worker_release_channel IS NULL
		     AND length(trim(NEW.worker_release_reason)) BETWEEN 1 AND 2000
		     AND NEW.worker_release_checked_at IS NOT NULL
		   )
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker release state');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_release_shape_insert`,
		`CREATE TRIGGER trg_agent_executions_release_shape_insert
		 BEFORE INSERT ON agent_executions
		 WHEN NOT (
		   (NEW.worker_release_revision_id IS NULL AND NEW.worker_release_channel IS NULL)
		   OR
		   (
		     NEW.worker_release_revision_id IS NOT NULL
		     AND NEW.worker_release_channel IN ('promoted', 'canary')
		     AND EXISTS (
		       SELECT 1 FROM worker_release_revisions AS revision
		       WHERE revision.id = NEW.worker_release_revision_id
		         AND revision.execution_target_id = NEW.execution_target_id
		         AND revision.tenant_id = NEW.tenant_id
		     )
		   )
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Worker release selection');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_release_shape_update`,
		`CREATE TRIGGER trg_agent_executions_release_shape_update
		 BEFORE UPDATE OF worker_release_revision_id, worker_release_channel ON agent_executions
		 WHEN NOT (
		   (NEW.worker_release_revision_id IS NULL AND NEW.worker_release_channel IS NULL)
		   OR
		   (
		     NEW.worker_release_revision_id IS NOT NULL
		     AND NEW.worker_release_channel IN ('promoted', 'canary')
		     AND EXISTS (
		       SELECT 1 FROM worker_release_revisions AS revision
		       WHERE revision.id = NEW.worker_release_revision_id
		         AND revision.execution_target_id = NEW.execution_target_id
		         AND revision.tenant_id = NEW.tenant_id
		     )
		   )
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Worker release selection');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Worker release safety migration: %w", err)
		}
	}
	return nil
}
