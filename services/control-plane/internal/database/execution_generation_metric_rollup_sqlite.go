package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateExecutionGenerationMetricRollupSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_execution_generation_metric_rollups_window
		 ON execution_generation_metric_rollups (bucket_day, metric_kind, target_kind)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_generation_metric_rollup_entries_pending
		 ON execution_generation_metric_rollup_entries (terminal_at, tenant_id, execution_id, generation)
		 WHERE rolled_up_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_execution_generation_pod_failure_metric_rollup_entries_pending
		 ON execution_generation_pod_failure_metric_rollup_entries (
		   first_observed_at, tenant_id, execution_id, generation, failure_class
		 ) WHERE rolled_up_at IS NULL`,
		`INSERT OR IGNORE INTO execution_generation_metric_rollup_entries (
		   tenant_id, execution_id, generation, dispatch_requested_at, terminal_at,
		   bucket_day, rolled_up_at, created_at, updated_at
		 )
		 SELECT
		   tenant_id, execution_id, generation, dispatch_requested_at, terminal_at,
		   date(dispatch_requested_at), NULL, terminal_at, terminal_at
		 FROM execution_generation_facts
		 WHERE dispatch_requested_at IS NOT NULL
		   AND terminal_at IS NOT NULL
		   AND terminal_outcome IS NOT NULL`,
		`INSERT OR IGNORE INTO execution_generation_pod_failure_metric_rollup_entries (
		   tenant_id, execution_id, generation, failure_class, first_observed_at,
		   bucket_day, rolled_up_at, created_at, updated_at
		 )
		 SELECT
		   tenant_id, execution_id, generation, failure_class, first_observed_at,
		   date(first_observed_at), NULL, first_observed_at, first_observed_at
		 FROM execution_generation_pod_failure_facts`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollups_insert`,
		`CREATE TRIGGER trg_execution_generation_metric_rollups_insert
		 BEFORE INSERT ON execution_generation_metric_rollups
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution generation metric rollup')
		   WHERE NEW.bucket_day IS NULL
		      OR NEW.sample_count <= 0
		      OR julianday(NEW.created_at) > julianday(NEW.updated_at)
		      OR NEW.target_kind NOT IN ('local', 'ssh', 'docker', 'kubernetes', 'other')
		      OR NOT (
		        (
		          NEW.metric_kind = 'outcome'
		          AND NEW.recovery_reason IN ('initial-claim', 'legacy-adoption', 'execution-recovery', 'suspend-resume', 'other')
		          AND NEW.outcome IN ('completed', 'failed', 'cancelled', 'interrupted', 'recovering', 'other')
		          AND NEW.warm_pool_mode = 'none' AND NEW.warm_pool_result = 'none'
		          AND NEW.failure_class = 'none' AND NEW.histogram_bucket = -1
		        )
		        OR (
		          NEW.metric_kind = 'warm-acquisition'
		          AND NEW.recovery_reason = 'none' AND NEW.outcome = 'none'
		          AND NEW.warm_pool_mode IN ('balanced', 'low-latency')
		          AND NEW.warm_pool_result IN ('pending', 'not-requested', 'hit', 'fallback')
		          AND NEW.failure_class = 'none' AND NEW.histogram_bucket = -1
		        )
		        OR (
		          NEW.metric_kind = 'cold-start-duration'
		          AND NEW.recovery_reason IN ('initial-claim', 'legacy-adoption', 'execution-recovery', 'suspend-resume', 'other')
		          AND NEW.outcome = 'none'
		          AND NEW.warm_pool_mode IN ('disabled', 'balanced', 'low-latency')
		          AND NEW.warm_pool_result IN ('pending', 'not-requested', 'hit', 'fallback')
		          AND NEW.failure_class = 'none' AND NEW.histogram_bucket BETWEEN 0 AND 4095
		        )
		        OR (
		          NEW.metric_kind IN ('pod-queue-duration', 'pod-provisioning-duration')
		          AND NEW.recovery_reason IN ('initial-claim', 'legacy-adoption', 'execution-recovery', 'suspend-resume', 'other')
		          AND NEW.outcome = 'none' AND NEW.warm_pool_mode = 'none' AND NEW.warm_pool_result = 'none'
		          AND NEW.failure_class = 'none' AND NEW.histogram_bucket BETWEEN 0 AND 4095
		        )
		        OR (
		          NEW.metric_kind = 'pod-failure'
		          AND NEW.recovery_reason = 'none' AND NEW.outcome = 'none'
		          AND NEW.warm_pool_mode = 'none' AND NEW.warm_pool_result = 'none'
		          AND NEW.failure_class IN (
		            'pod-apply-failed', 'pending-timeout', 'unschedulable', 'image-pull',
		            'container-start', 'evicted', 'oom-killed', 'pod-failed', 'other'
		          )
		          AND NEW.histogram_bucket = -1
		        )
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollups_update`,
		`CREATE TRIGGER trg_execution_generation_metric_rollups_update
		 BEFORE UPDATE ON execution_generation_metric_rollups
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation metric rollup identity is immutable')
		   WHERE NEW.bucket_day IS NOT OLD.bucket_day
		      OR NEW.metric_kind IS NOT OLD.metric_kind
		      OR NEW.target_kind IS NOT OLD.target_kind
		      OR NEW.recovery_reason IS NOT OLD.recovery_reason
		      OR NEW.outcome IS NOT OLD.outcome
		      OR NEW.warm_pool_mode IS NOT OLD.warm_pool_mode
		      OR NEW.warm_pool_result IS NOT OLD.warm_pool_result
		      OR NEW.failure_class IS NOT OLD.failure_class
		      OR NEW.histogram_bucket IS NOT OLD.histogram_bucket
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'Execution generation metric rollup totals cannot regress')
		   WHERE NEW.sample_count < OLD.sample_count
		      OR julianday(NEW.updated_at) < julianday(OLD.updated_at);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollups_delete`,
		`CREATE TRIGGER trg_execution_generation_metric_rollups_delete
		 BEFORE DELETE ON execution_generation_metric_rollups
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation metric rollups cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollup_entries_insert`,
		`CREATE TRIGGER trg_execution_generation_metric_rollup_entries_insert
		 BEFORE INSERT ON execution_generation_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution generation metric rollup entry')
		   WHERE NEW.generation <= 0
		      OR NEW.dispatch_requested_at IS NULL
		      OR NEW.terminal_at IS NULL
		      OR date(NEW.bucket_day) <> date(NEW.dispatch_requested_at)
		      OR julianday(NEW.terminal_at) < julianday(NEW.dispatch_requested_at)
		      OR julianday(NEW.created_at) > julianday(NEW.updated_at)
		      OR (NEW.rolled_up_at IS NOT NULL AND julianday(NEW.rolled_up_at) < julianday(NEW.created_at))
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_generation_facts AS fact
		        WHERE fact.tenant_id = NEW.tenant_id
		          AND fact.execution_id = NEW.execution_id
		          AND fact.generation = NEW.generation
		          AND fact.dispatch_requested_at IS NEW.dispatch_requested_at
		          AND fact.terminal_at IS NEW.terminal_at
		          AND fact.terminal_outcome IS NOT NULL
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollup_entries_update`,
		`CREATE TRIGGER trg_execution_generation_metric_rollup_entries_update
		 BEFORE UPDATE ON execution_generation_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation metric rollup entry identity is immutable')
		   WHERE NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.execution_id IS NOT OLD.execution_id
		      OR NEW.generation IS NOT OLD.generation
		      OR NEW.dispatch_requested_at IS NOT OLD.dispatch_requested_at
		      OR NEW.terminal_at IS NOT OLD.terminal_at
		      OR NEW.bucket_day IS NOT OLD.bucket_day
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'Execution generation metric rollup entry completion is immutable')
		   WHERE OLD.rolled_up_at IS NOT NULL AND NEW.rolled_up_at IS NOT OLD.rolled_up_at;

		   SELECT RAISE(ABORT, 'Execution generation metric rollup entry timeline cannot regress')
		   WHERE julianday(NEW.updated_at) < julianday(OLD.updated_at)
		      OR (NEW.rolled_up_at IS NOT NULL AND julianday(NEW.rolled_up_at) < julianday(NEW.created_at));
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollup_entries_delete`,
		`CREATE TRIGGER trg_execution_generation_metric_rollup_entries_delete
		 BEFORE DELETE ON execution_generation_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation metric rollup entries cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollup_enqueue_insert`,
		`CREATE TRIGGER trg_execution_generation_metric_rollup_enqueue_insert
		 AFTER INSERT ON execution_generation_facts
		 WHEN NEW.dispatch_requested_at IS NOT NULL
		   AND NEW.terminal_at IS NOT NULL
		   AND NEW.terminal_outcome IS NOT NULL
		 BEGIN
		   INSERT OR IGNORE INTO execution_generation_metric_rollup_entries (
		     tenant_id, execution_id, generation, dispatch_requested_at, terminal_at,
		     bucket_day, rolled_up_at, created_at, updated_at
		   ) VALUES (
		     NEW.tenant_id, NEW.execution_id, NEW.generation, NEW.dispatch_requested_at, NEW.terminal_at,
		     date(NEW.dispatch_requested_at), NULL, NEW.terminal_at, NEW.terminal_at
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollup_enqueue_update`,
		`CREATE TRIGGER trg_execution_generation_metric_rollup_enqueue_update
		 AFTER UPDATE OF dispatch_requested_at, terminal_at, terminal_outcome ON execution_generation_facts
		 WHEN NEW.dispatch_requested_at IS NOT NULL
		   AND NEW.terminal_at IS NOT NULL
		   AND NEW.terminal_outcome IS NOT NULL
		 BEGIN
		   INSERT OR IGNORE INTO execution_generation_metric_rollup_entries (
		     tenant_id, execution_id, generation, dispatch_requested_at, terminal_at,
		     bucket_day, rolled_up_at, created_at, updated_at
		   ) VALUES (
		     NEW.tenant_id, NEW.execution_id, NEW.generation, NEW.dispatch_requested_at, NEW.terminal_at,
		     date(NEW.dispatch_requested_at), NULL, NEW.terminal_at, NEW.terminal_at
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_metric_source_sealed`,
		`CREATE TRIGGER trg_execution_generation_metric_source_sealed
		 BEFORE UPDATE ON execution_generation_facts
		 WHEN EXISTS (
		   SELECT 1 FROM execution_generation_metric_rollup_entries AS entry
		   WHERE entry.tenant_id = OLD.tenant_id
		     AND entry.execution_id = OLD.execution_id
		     AND entry.generation = OLD.generation
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation metric source is sealed')
		   WHERE NEW.target_kind IS NOT OLD.target_kind
		      OR NEW.recovery_reason IS NOT OLD.recovery_reason
		      OR NEW.warm_pool_mode IS NOT OLD.warm_pool_mode
		      OR NEW.warm_pool_result IS NOT OLD.warm_pool_result
		      OR NEW.dispatch_requested_at IS NOT OLD.dispatch_requested_at
		      OR NEW.provider_ready_at IS NOT OLD.provider_ready_at
		      OR NEW.pod_provisioning_started_at IS NOT OLD.pod_provisioning_started_at
		      OR NEW.pod_running_at IS NOT OLD.pod_running_at
		      OR NEW.terminal_at IS NOT OLD.terminal_at
		      OR NEW.terminal_outcome IS NOT OLD.terminal_outcome;
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_metric_rollup_entries_insert`,
		`CREATE TRIGGER trg_execution_generation_pod_failure_metric_rollup_entries_insert
		 BEFORE INSERT ON execution_generation_pod_failure_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution generation Pod failure metric rollup entry')
		   WHERE NEW.generation <= 0
		      OR NEW.first_observed_at IS NULL
		      OR date(NEW.bucket_day) <> date(NEW.first_observed_at)
		      OR julianday(NEW.created_at) > julianday(NEW.updated_at)
		      OR (NEW.rolled_up_at IS NOT NULL AND julianday(NEW.rolled_up_at) < julianday(NEW.created_at))
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_generation_pod_failure_facts AS failure
		        WHERE failure.tenant_id = NEW.tenant_id
		          AND failure.execution_id = NEW.execution_id
		          AND failure.generation = NEW.generation
		          AND failure.failure_class = NEW.failure_class
		          AND failure.first_observed_at IS NEW.first_observed_at
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_metric_rollup_entries_update`,
		`CREATE TRIGGER trg_execution_generation_pod_failure_metric_rollup_entries_update
		 BEFORE UPDATE ON execution_generation_pod_failure_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation Pod failure metric rollup entry identity is immutable')
		   WHERE NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.execution_id IS NOT OLD.execution_id
		      OR NEW.generation IS NOT OLD.generation
		      OR NEW.failure_class IS NOT OLD.failure_class
		      OR NEW.first_observed_at IS NOT OLD.first_observed_at
		      OR NEW.bucket_day IS NOT OLD.bucket_day
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'Execution generation Pod failure metric rollup entry completion is immutable')
		   WHERE OLD.rolled_up_at IS NOT NULL AND NEW.rolled_up_at IS NOT OLD.rolled_up_at;

		   SELECT RAISE(ABORT, 'Execution generation Pod failure metric rollup entry timeline cannot regress')
		   WHERE julianday(NEW.updated_at) < julianday(OLD.updated_at)
		      OR (NEW.rolled_up_at IS NOT NULL AND julianday(NEW.rolled_up_at) < julianday(NEW.created_at));
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_metric_rollup_entries_delete`,
		`CREATE TRIGGER trg_execution_generation_pod_failure_metric_rollup_entries_delete
		 BEFORE DELETE ON execution_generation_pod_failure_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation Pod failure metric rollup entries cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_metric_rollup_enqueue`,
		`CREATE TRIGGER trg_execution_generation_pod_failure_metric_rollup_enqueue
		 AFTER INSERT ON execution_generation_pod_failure_facts
		 BEGIN
		   INSERT OR IGNORE INTO execution_generation_pod_failure_metric_rollup_entries (
		     tenant_id, execution_id, generation, failure_class, first_observed_at,
		     bucket_day, rolled_up_at, created_at, updated_at
		   ) VALUES (
		     NEW.tenant_id, NEW.execution_id, NEW.generation, NEW.failure_class, NEW.first_observed_at,
		     date(NEW.first_observed_at), NULL, NEW.first_observed_at, NEW.first_observed_at
		   );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Execution generation metric rollup safety: %w", err)
		}
	}
	return nil
}
