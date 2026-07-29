package database

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

type MetadataStore interface {
	DB() *gorm.DB
	Kind() platform.MetadataStore
	Migrate(context.Context, fs.FS) error
	Close() error
}

type store struct {
	db                   *gorm.DB
	kind                 platform.MetadataStore
	migrationLockTimeout time.Duration
}

func OpenMetadataStore(ctx context.Context, config platform.Config, databaseURL, sqlitePath string, values ...Options) (MetadataStore, error) {
	options := resolveOptions(values)
	switch config.MetadataStore {
	case platform.MetadataPostgres:
		db, err := Open(ctx, databaseURL, options)
		if err != nil {
			return nil, err
		}
		return &store{db: db, kind: platform.MetadataPostgres, migrationLockTimeout: options.MigrationLockTimeout}, nil
	case platform.MetadataSQLite:
		if err := os.MkdirAll(filepath.Dir(sqlitePath), 0o700); err != nil {
			return nil, fmt.Errorf("create sqlite metadata directory: %w", err)
		}
		db, err := gorm.Open(sqlite.Open(sqlitePath), &gorm.Config{
			TranslateError: true, SkipDefaultTransaction: true,
			Logger: gormLogger(),
		})
		if err != nil {
			return nil, fmt.Errorf("open sqlite metadata store: %w", err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			return nil, fmt.Errorf("resolve sqlite pool: %w", err)
		}
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		sqlDB.SetConnMaxLifetime(0)
		if err := db.WithContext(ctx).Exec("PRAGMA foreign_keys = ON").Error; err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("enable sqlite foreign keys: %w", err)
		}
		if err := db.WithContext(ctx).Exec("PRAGMA busy_timeout = 5000").Error; err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("configure sqlite busy timeout: %w", err)
		}
		if err := db.WithContext(ctx).Exec("PRAGMA journal_mode = WAL").Error; err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("configure sqlite journal mode: %w", err)
		}
		if err := db.WithContext(ctx).Exec("PRAGMA synchronous = NORMAL").Error; err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("configure sqlite synchronous mode: %w", err)
		}
		return &store{db: db, kind: platform.MetadataSQLite, migrationLockTimeout: options.MigrationLockTimeout}, nil
	default:
		return nil, fmt.Errorf("unsupported metadata store %q", config.MetadataStore)
	}
}

func (s *store) DB() *gorm.DB { return s.db }

func (s *store) Kind() platform.MetadataStore { return s.kind }

func (s *store) Migrate(ctx context.Context, files fs.FS) error {
	if s.kind == platform.MetadataPostgres {
		return Migrate(ctx, s.db, files, s.migrationLockTimeout)
	}
	if err := s.db.WithContext(ctx).AutoMigrate(persistence.AllModels()...); err != nil {
		return fmt.Errorf("auto-migrate sqlite metadata schema: %w", err)
	}
	if err := migrateSQLiteSafety(ctx, s.db); err != nil {
		return err
	}
	return nil
}

func migrateSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`UPDATE agent_sessions
		 SET resource_state = CASE
		       WHEN EXISTS (
		         SELECT 1 FROM agent_executions AS execution
		         WHERE execution.tenant_id = agent_sessions.tenant_id
		           AND execution.session_id = agent_sessions.id
		           AND execution.status = 'suspended'
		       ) THEN 'suspended'
		       WHEN EXISTS (
		         SELECT 1 FROM agent_executions AS execution
		         WHERE execution.tenant_id = agent_sessions.tenant_id
		           AND execution.session_id = agent_sessions.id
		           AND execution.status = 'recovering'
		       ) THEN 'restoring'
		       WHEN EXISTS (
		         SELECT 1 FROM agent_executions AS execution
		         WHERE execution.tenant_id = agent_sessions.tenant_id
		           AND execution.session_id = agent_sessions.id
		           AND execution.status = 'queued'
		       ) THEN 'provisioning'
		       WHEN EXISTS (
		         SELECT 1 FROM agent_executions AS execution
		         WHERE execution.tenant_id = agent_sessions.tenant_id
		           AND execution.session_id = agent_sessions.id
		           AND execution.status = 'waiting-for-approval'
		       ) THEN 'waiting'
		       WHEN EXISTS (
		         SELECT 1 FROM agent_executions AS execution
		         WHERE execution.tenant_id = agent_sessions.tenant_id
		           AND execution.session_id = agent_sessions.id
		           AND execution.status IN ('leased', 'running')
		       ) THEN 'active'
		       ELSE 'idle'
		     END
		 WHERE resource_state IS NULL OR resource_state = ''`,
		`UPDATE agent_sessions
		 SET resource_idle_since = CASE
		       WHEN EXISTS (
		         SELECT 1 FROM agent_executions AS execution
		         WHERE execution.tenant_id = agent_sessions.tenant_id
		           AND execution.session_id = agent_sessions.id
		           AND execution.status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering')
		       ) THEN NULL
		       ELSE COALESCE(updated_at, created_at, CURRENT_TIMESTAMP)
		     END,
		     meaningful_activity_at = COALESCE(updated_at, created_at, CURRENT_TIMESTAMP)
		 WHERE meaningful_activity_at IS NULL`,
		`UPDATE agent_sessions
		 SET meaningful_activity_sequence = last_event_sequence
		 WHERE meaningful_activity_sequence IS NULL
		    OR meaningful_activity_sequence < 0
		    OR meaningful_activity_sequence > last_event_sequence`,
		`UPDATE agent_sessions
		 SET provider_resume_cursor_encrypted = NULL,
		     provider_resume_cursor_state = 'absent',
		     provider_resume_cursor_source_execution_id = NULL,
		     provider_resume_cursor_source_generation = NULL,
		     provider_resume_cursor_history_sequence = NULL
		 WHERE provider_resume_cursor_encrypted IS NULL
		    OR length(provider_resume_cursor_encrypted) = 0`,
		`UPDATE agent_sessions
		 SET provider_resume_cursor_state = 'quarantined',
		     provider_resume_cursor_source_execution_id = NULL,
		     provider_resume_cursor_source_generation = NULL,
		     provider_resume_cursor_history_sequence = NULL
		 WHERE length(provider_resume_cursor_encrypted) > 0
		   AND (provider_resume_cursor_state IS NULL
		     OR provider_resume_cursor_state = ''
		     OR provider_resume_cursor_state = 'absent')`,
		`DROP INDEX IF EXISTS uq_agent_executions_session_active`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_executions_session_active
		 ON agent_executions (tenant_id, session_id)
		 WHERE status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering', 'suspended')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_recovery_bundle_generation
		 ON execution_recovery_bundles (tenant_id, execution_id, generation)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_recovery_bundles_session_created
		 ON execution_recovery_bundles (tenant_id, session_id, created_at DESC, id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_events_execution_started_lookup
		 ON session_events (tenant_id, session_id, execution_id, generation, occurred_at)
		 WHERE event_type = 'execution.started'
		   AND execution_id IS NOT NULL
		   AND generation IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_storage_scrubs_active
		 ON worker_storage_scrubs (worker_id, worker_incarnation)
		 WHERE status IN ('pending', 'failed')`,
		`CREATE INDEX IF NOT EXISTS idx_session_events_execution_generation_lifecycle
		 ON session_events (tenant_id, execution_id, generation, event_type, occurred_at, event_id)
		 WHERE execution_id IS NOT NULL
		   AND generation IS NOT NULL
		   AND event_type IN (
		     'execution.started',
		     'session.started',
		     'execution.completed',
		     'execution.failed',
		     'execution.cancelled',
		     'execution.interrupted'
		   )`,
		`CREATE INDEX IF NOT EXISTS idx_session_events_provider_resume_claim_decision
		 ON session_events (occurred_at, tenant_id, event_id)
		 WHERE event_type = 'execution.leased'`,
		`CREATE INDEX IF NOT EXISTS idx_session_events_provider_resume_runtime_fallback
		 ON session_events (occurred_at, tenant_id, event_id)
		 WHERE event_type = 'runtime.warning'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_suspend_attempts_active
		 ON execution_suspend_attempts (tenant_id, execution_id, generation)
		 WHERE status = 'checkpointing'`,
		`CREATE INDEX IF NOT EXISTS idx_execution_suspend_attempts_execution
		 ON execution_suspend_attempts (tenant_id, execution_id, requested_at DESC, id)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_suspend_attempts_deadline
		 ON execution_suspend_attempts (checkpoint_deadline_at, tenant_id, execution_id, id)
		 WHERE status = 'checkpointing'`,
		`UPDATE execution_suspend_attempts
		 SET completion_mode = 'worker-attested-v1'
		 WHERE completion_mode IS NULL
		    OR trim(completion_mode) = ''`,
		`UPDATE execution_suspend_attempts AS attempt
		 SET execution_target_id = execution.execution_target_id
		 FROM agent_executions AS execution
		 WHERE attempt.execution_target_id IS NULL
		   AND execution.tenant_id = attempt.tenant_id
		   AND execution.id = attempt.execution_id`,
		`CREATE INDEX IF NOT EXISTS idx_agent_sessions_resource_idle
		 ON agent_sessions (tenant_id, resource_idle_since, id)
		 WHERE status = 'active' AND resource_idle_since IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_agent_sessions_absolute_expiry
		 ON agent_sessions (absolute_expires_at, tenant_id, id)
		 WHERE status IN ('active', 'suspended') AND absolute_expires_at IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_worker_leases_provider_credential_access_expiry
		 ON worker_leases (provider_credential_access_expires_at, tenant_id, execution_id)
		 WHERE provider_credential_grant_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_worker_leases_provider_credential_refresh_deadline
		 ON worker_leases (provider_credential_refresh_deadline_at, tenant_id, execution_id)
		 WHERE provider_credential_grant_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_memory_heads_user_key
		 ON agent_memory_heads (tenant_id, scope_user_id, memory_key)
		 WHERE scope_type = 'user'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_memory_heads_project_key
		 ON agent_memory_heads (tenant_id, scope_project_id, memory_key)
		 WHERE scope_type = 'project'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_memory_heads_session_key
		 ON agent_memory_heads (tenant_id, scope_session_id, memory_key)
		 WHERE scope_type = 'session'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_memory_revision_number
		 ON agent_memory_revisions (tenant_id, memory_head_id, revision_number)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_memory_revisions_artifact
		 ON agent_memory_revisions (tenant_id, artifact_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_project_resource_lifecycle_policies_tenant
		 ON project_resource_lifecycle_policies (tenant_id, project_id)`,
		`UPDATE worker_instances
		 SET registration_trust_mode = 'shared-token'
		 WHERE registration_trust_mode IS NULL
		    OR trim(registration_trust_mode) = ''`,
		`UPDATE worker_instances
		 SET worker_mode = CASE
		       WHEN target_kind = 'kubernetes' THEN 'execution-pinned'
		       ELSE 'general-pool'
		     END
		 WHERE worker_mode IS NULL
		    OR trim(worker_mode) = ''`,
		`UPDATE worker_pools
		 SET tenant_isolation = 'pinned'
		 WHERE tenant_isolation IS NULL
		    OR trim(tenant_isolation) = ''`,
		`UPDATE agent_executions
		 SET warm_pool_mode_snapshot = COALESCE((
		       SELECT CASE
		         WHEN lower(trim(session.warm_pool_mode)) IN ('disabled', 'balanced', 'low-latency')
		           THEN lower(trim(session.warm_pool_mode))
		         ELSE 'disabled'
		       END
		       FROM agent_sessions AS session
		       WHERE session.tenant_id = agent_executions.tenant_id
		         AND session.id = agent_executions.session_id
		     ), 'disabled')
		 WHERE warm_pool_mode_snapshot IS NULL
		    OR trim(warm_pool_mode_snapshot) = ''`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_worker_mode_insert`,
		`CREATE TRIGGER trg_worker_instances_worker_mode_insert
		 BEFORE INSERT ON worker_instances
		 WHEN NEW.worker_mode IS NULL
		   OR NEW.worker_mode NOT IN ('execution-pinned', 'warm-pool', 'general-pool')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker mode');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_worker_mode_update`,
		`CREATE TRIGGER trg_worker_instances_worker_mode_update
		 BEFORE UPDATE OF worker_mode ON worker_instances
		 WHEN NEW.worker_mode IS NULL
		   OR NEW.worker_mode NOT IN ('execution-pinned', 'warm-pool', 'general-pool')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker mode');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_identity_insert`,
		`CREATE TRIGGER trg_worker_instances_identity_insert
		 BEFORE INSERT ON worker_instances
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker identity snapshot')
		   WHERE NOT (
		     (NEW.worker_mode = 'execution-pinned'
		       AND NEW.assigned_execution_id IS NOT NULL
		       AND NEW.worker_pool_id IS NULL
		       AND NEW.worker_pool_version IS NULL
		       AND NEW.capacity_class IS NULL)
		     OR
		     (NEW.worker_mode = 'warm-pool'
		       AND NEW.assigned_execution_id IS NULL
		       AND NEW.worker_pool_id IS NOT NULL
		       AND NEW.worker_pool_version > 0
		       AND NEW.capacity_class IN ('standard', 'interactive'))
		     OR
		     (NEW.worker_mode = 'general-pool'
		       AND NEW.assigned_execution_id IS NULL
		       AND NEW.worker_pool_id IS NULL
		       AND NEW.worker_pool_version IS NULL
		       AND NEW.capacity_class IS NULL)
		   );

		   SELECT RAISE(ABORT, 'Worker assigned Execution scope mismatch')
		   WHERE NEW.assigned_execution_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1
		     FROM agent_executions AS execution
		     WHERE execution.id = NEW.assigned_execution_id
		       AND execution.execution_target_id = NEW.execution_target_id
		       AND execution.target_kind = NEW.target_kind
		   );

		   SELECT RAISE(ABORT, 'Worker pool identity mismatch')
		   WHERE NEW.worker_pool_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1
		     FROM worker_pools AS pool
		     WHERE pool.id = NEW.worker_pool_id
		       AND pool.execution_target_id = NEW.execution_target_id
		       AND pool.mode = 'warm'
		       AND pool.status = 'active'
		       AND pool.version = NEW.worker_pool_version
		       AND pool.capacity_class = NEW.capacity_class
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_identity_update`,
		`CREATE TRIGGER trg_worker_instances_identity_update
		 BEFORE UPDATE OF worker_mode, assigned_execution_id, worker_pool_id, worker_pool_version, capacity_class, execution_target_id, target_kind
		 ON worker_instances
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker identity snapshot')
		   WHERE NOT (
		     (NEW.worker_mode = 'execution-pinned'
		       AND NEW.assigned_execution_id IS NOT NULL
		       AND NEW.worker_pool_id IS NULL
		       AND NEW.worker_pool_version IS NULL
		       AND NEW.capacity_class IS NULL)
		     OR
		     (NEW.worker_mode = 'warm-pool'
		       AND NEW.assigned_execution_id IS NULL
		       AND NEW.worker_pool_id IS NOT NULL
		       AND NEW.worker_pool_version > 0
		       AND NEW.capacity_class IN ('standard', 'interactive'))
		     OR
		     (NEW.worker_mode = 'general-pool'
		       AND NEW.assigned_execution_id IS NULL
		       AND NEW.worker_pool_id IS NULL
		       AND NEW.worker_pool_version IS NULL
		       AND NEW.capacity_class IS NULL)
		   );

		   SELECT RAISE(ABORT, 'Worker assigned Execution scope mismatch')
		   WHERE NEW.assigned_execution_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1
		     FROM agent_executions AS execution
		     WHERE execution.id = NEW.assigned_execution_id
		       AND execution.execution_target_id = NEW.execution_target_id
		       AND execution.target_kind = NEW.target_kind
		   );

		   SELECT RAISE(ABORT, 'Worker pool identity mismatch')
		   WHERE NEW.worker_pool_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1
		     FROM worker_pools AS pool
		     WHERE pool.id = NEW.worker_pool_id
		       AND pool.execution_target_id = NEW.execution_target_id
		       AND pool.mode = 'warm'
		       AND pool.status = 'active'
		       AND pool.version = NEW.worker_pool_version
		       AND pool.capacity_class = NEW.capacity_class
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_tenant_binding_insert`,
		`CREATE TRIGGER trg_worker_instances_tenant_binding_insert
		 BEFORE INSERT ON worker_instances
		 WHEN NEW.tenant_binding_id IS NOT NULL
		 BEGIN
		   SELECT RAISE(ABORT, 'Only general-pool Workers may carry a Tenant binding')
		   WHERE NEW.worker_mode <> 'general-pool';

		   SELECT RAISE(ABORT, 'Worker Tenant binding is outside its Execution Target')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM execution_targets AS target
		     WHERE target.id = NEW.execution_target_id
		       AND (target.tenant_id IS NULL OR target.tenant_id IS NEW.tenant_binding_id)
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_tenant_binding_update`,
		`CREATE TRIGGER trg_worker_instances_tenant_binding_update
		 BEFORE UPDATE OF tenant_binding_id, worker_mode, execution_target_id ON worker_instances
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker Tenant binding is immutable')
		   WHERE OLD.tenant_binding_id IS NOT NULL
		     AND NEW.tenant_binding_id IS NOT OLD.tenant_binding_id;

		   SELECT RAISE(ABORT, 'Only general-pool Workers may carry a Tenant binding')
		   WHERE NEW.tenant_binding_id IS NOT NULL AND NEW.worker_mode <> 'general-pool';

		   SELECT RAISE(ABORT, 'Worker Tenant binding is outside its Execution Target')
		   WHERE NEW.tenant_binding_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1 FROM execution_targets AS target
		     WHERE target.id = NEW.execution_target_id
		       AND (target.tenant_id IS NULL OR target.tenant_id IS NEW.tenant_binding_id)
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_warm_pool_mode_snapshot_insert`,
		`CREATE TRIGGER trg_agent_executions_warm_pool_mode_snapshot_insert
		 BEFORE INSERT ON agent_executions
		 WHEN NEW.warm_pool_mode_snapshot IS NULL
		   OR NEW.warm_pool_mode_snapshot NOT IN ('disabled', 'balanced', 'low-latency')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid warm_pool_mode_snapshot');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_warm_pool_mode_snapshot_update`,
		`CREATE TRIGGER trg_agent_executions_warm_pool_mode_snapshot_update
		 BEFORE UPDATE OF warm_pool_mode_snapshot ON agent_executions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid warm_pool_mode_snapshot')
		   WHERE NEW.warm_pool_mode_snapshot IS NULL
		      OR NEW.warm_pool_mode_snapshot NOT IN ('disabled', 'balanced', 'low-latency');
		   SELECT RAISE(ABORT, 'Execution warm-pool demand snapshot is immutable')
		   WHERE NEW.warm_pool_mode_snapshot IS NOT OLD.warm_pool_mode_snapshot;
		 END`,
		`CREATE INDEX IF NOT EXISTS idx_worker_instances_assigned_execution
		 ON worker_instances (execution_target_id, assigned_execution_id, id)
		 WHERE assigned_execution_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_worker_instances_pool_identity
		 ON worker_instances (execution_target_id, worker_pool_id, worker_pool_version, capacity_class, id)
		 WHERE worker_pool_id IS NOT NULL`,
		`DROP INDEX IF EXISTS idx_worker_instances_tenant_binding`,
		`CREATE INDEX idx_worker_instances_tenant_binding
		 ON worker_instances (execution_target_id, tenant_binding_id, status, id)
		 WHERE tenant_binding_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_pool_target_name
		 ON worker_pools (execution_target_id, name)`,
		`CREATE INDEX IF NOT EXISTS idx_worker_pool_target_status
		 ON worker_pools (execution_target_id, status, capacity_class, name, id)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_executions_placement_claim
		 ON agent_executions (execution_target_id, worker_pool_id, status, queued_at, id)
		 WHERE status IN ('queued', 'recovering')`,
		`CREATE INDEX IF NOT EXISTS idx_agent_executions_fair_share_active
		 ON agent_executions (execution_target_id, target_kind, tenant_id)
		 WHERE status IN ('leased', 'running', 'waiting-for-approval')`,
		`CREATE INDEX IF NOT EXISTS idx_agent_executions_target_nonterminal
		 ON agent_executions (execution_target_id, target_kind, status, queued_at, id)
		 WHERE status IN ('queued', 'recovering', 'leased', 'running', 'waiting-for-approval')`,
		`INSERT INTO worker_pools (
		   id, tenant_id, execution_target_id, name, mode, capacity_class, tenant_isolation,
		   cluster_id, region, namespace, desired_idle_units, min_idle_units, max_active_units,
		   scheduling_template, status, version, created_at, updated_at
		 )
		 SELECT target.id, target.tenant_id, target.id, 'default',
		   CASE WHEN target.kind = 'kubernetes' THEN 'per-execution' ELSE 'resident' END,
		   'standard', 'pinned', '', '', '', 0, 0, 1, '{}', 'active', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		 FROM execution_targets AS target
		 WHERE NOT EXISTS (
		   SELECT 1 FROM worker_pools AS pool
		   WHERE pool.execution_target_id = target.id AND lower(pool.name) = 'default'
		 )`,
		`INSERT INTO execution_placement_policies (
		   execution_target_id, tenant_id, version, default_pool_id, updated_at
		 )
		 SELECT target.id, target.tenant_id, 1, pool.id, CURRENT_TIMESTAMP
		 FROM execution_targets AS target
		 JOIN worker_pools AS pool
		   ON pool.execution_target_id = target.id AND lower(pool.name) = 'default'
		 WHERE NOT EXISTS (
		   SELECT 1 FROM execution_placement_policies AS policy
		   WHERE policy.execution_target_id = target.id
		 )`,
		`DROP TRIGGER IF EXISTS trg_worker_pools_shape_insert`,
		`CREATE TRIGGER trg_worker_pools_shape_insert
		 BEFORE INSERT ON worker_pools
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker pool shape')
		   WHERE NEW.mode NOT IN ('resident', 'per-execution', 'warm')
		      OR NEW.capacity_class NOT IN ('standard', 'interactive')
		      OR NEW.tenant_isolation NOT IN ('pinned', 'shared')
		      OR NEW.status NOT IN ('active', 'draining', 'disabled')
		      OR length(trim(NEW.name)) NOT BETWEEN 1 AND 160
		      OR NEW.desired_idle_units < 0
		      OR NEW.min_idle_units < 0
		      OR NEW.min_idle_units > NEW.desired_idle_units
		      OR NEW.max_active_units < NEW.desired_idle_units
		      OR NEW.version <= 0
		      OR json_valid(NEW.scheduling_template) = 0
		      OR json_type(NEW.scheduling_template) <> 'object';

		   SELECT RAISE(ABORT, 'Worker pool target scope mismatch')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM execution_targets AS target
		     WHERE target.id = NEW.execution_target_id
		       AND target.tenant_id IS NEW.tenant_id
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_pools_shape_update`,
		`CREATE TRIGGER trg_worker_pools_shape_update
		 BEFORE UPDATE ON worker_pools
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker pool ownership is immutable')
		   WHERE NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.tenant_id IS NOT OLD.tenant_id;

		   SELECT RAISE(ABORT, 'Worker pool mode, capacity class, and Tenant isolation are immutable')
		   WHERE NEW.mode IS NOT OLD.mode
		      OR NEW.capacity_class IS NOT OLD.capacity_class
		      OR NEW.tenant_isolation IS NOT OLD.tenant_isolation;

		   SELECT RAISE(ABORT, 'invalid Worker pool update')
		   WHERE NEW.tenant_isolation NOT IN ('pinned', 'shared')
		      OR NEW.status NOT IN ('active', 'draining', 'disabled')
		      OR length(trim(NEW.name)) NOT BETWEEN 1 AND 160
		      OR NEW.desired_idle_units < 0
		      OR NEW.min_idle_units < 0
		      OR NEW.min_idle_units > NEW.desired_idle_units
		      OR NEW.max_active_units < NEW.desired_idle_units
		      OR NEW.version <> OLD.version + 1
		      OR json_valid(NEW.scheduling_template) = 0
		      OR json_type(NEW.scheduling_template) <> 'object';

		   SELECT RAISE(ABORT, 'Execution placement default pool must remain active')
		   WHERE NEW.status <> 'active' AND EXISTS (
		     SELECT 1 FROM execution_placement_policies AS policy
		     WHERE policy.default_pool_id = NEW.id
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_pools_delete`,
		`CREATE TRIGGER trg_worker_pools_delete
		 BEFORE DELETE ON worker_pools
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker pools cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_pool_warm_capacity_scope_insert`,
		`CREATE TRIGGER trg_worker_pool_warm_capacity_scope_insert
		 BEFORE INSERT ON worker_pool_warm_capacity
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker pool warm capacity scope is invalid')
		   WHERE NOT EXISTS (
		     SELECT 1
		     FROM worker_pools AS pool
		     JOIN execution_targets AS target ON target.id = pool.execution_target_id
		     WHERE pool.id = NEW.worker_pool_id
		       AND pool.execution_target_id = NEW.execution_target_id
		       AND pool.tenant_id IS NEW.tenant_id
		       AND pool.version = NEW.worker_pool_version
		       AND pool.mode = 'warm'
		       AND pool.status <> 'disabled'
		       AND pool.capacity_class = NEW.capacity_class
		       AND pool.desired_idle_units = NEW.desired_idle_units
		       AND pool.min_idle_units = NEW.min_idle_units
		       AND pool.max_active_units = NEW.max_active_units
		       AND target.tenant_id IS NOT NULL
		       AND target.tenant_id IS NEW.tenant_id
		       AND target.kind = 'kubernetes'
		       AND target.status = 'active'
		   )
		   OR (NEW.worker_release_revision_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1
		     FROM worker_release_revisions AS release
		     WHERE release.id = NEW.worker_release_revision_id
		       AND release.execution_target_id = NEW.execution_target_id
		       AND release.tenant_id = NEW.tenant_id
		   ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_pool_warm_capacity_scope_update`,
		`CREATE TRIGGER trg_worker_pool_warm_capacity_scope_update
		 BEFORE UPDATE ON worker_pool_warm_capacity
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker pool warm capacity scope is invalid')
		   WHERE NOT EXISTS (
		     SELECT 1
		     FROM worker_pools AS pool
		     JOIN execution_targets AS target ON target.id = pool.execution_target_id
		     WHERE pool.id = NEW.worker_pool_id
		       AND pool.execution_target_id = NEW.execution_target_id
		       AND pool.tenant_id IS NEW.tenant_id
		       AND pool.version = NEW.worker_pool_version
		       AND pool.mode = 'warm'
		       AND pool.status <> 'disabled'
		       AND pool.capacity_class = NEW.capacity_class
		       AND pool.desired_idle_units = NEW.desired_idle_units
		       AND pool.min_idle_units = NEW.min_idle_units
		       AND pool.max_active_units = NEW.max_active_units
		       AND target.tenant_id IS NOT NULL
		       AND target.tenant_id IS NEW.tenant_id
		       AND target.kind = 'kubernetes'
		       AND target.status = 'active'
		   )
		   OR (NEW.worker_release_revision_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1
		     FROM worker_release_revisions AS release
		     WHERE release.id = NEW.worker_release_revision_id
		       AND release.execution_target_id = NEW.execution_target_id
		       AND release.tenant_id = NEW.tenant_id
		   ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_placement_policy_insert`,
		`CREATE TRIGGER trg_execution_placement_policy_insert
		 BEFORE INSERT ON execution_placement_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution placement policy')
		   WHERE NEW.version <= 0
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id AND target.tenant_id IS NEW.tenant_id
		      )
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_pools AS pool
		        WHERE pool.id = NEW.default_pool_id
		          AND pool.execution_target_id = NEW.execution_target_id
		          AND pool.tenant_id IS NEW.tenant_id
		          AND pool.status = 'active'
		      )
		      OR (NEW.balanced_pool_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1 FROM worker_pools AS pool
		        WHERE pool.id = NEW.balanced_pool_id
		          AND pool.execution_target_id = NEW.execution_target_id
		          AND pool.tenant_id IS NEW.tenant_id
		      ))
		      OR (NEW.low_latency_pool_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1 FROM worker_pools AS pool
		        WHERE pool.id = NEW.low_latency_pool_id
		          AND pool.execution_target_id = NEW.execution_target_id
		          AND pool.tenant_id IS NEW.tenant_id
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_placement_policy_update`,
		`CREATE TRIGGER trg_execution_placement_policy_update
		 BEFORE UPDATE ON execution_placement_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution placement policy ownership is immutable')
		   WHERE NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.tenant_id IS NOT OLD.tenant_id;

		   SELECT RAISE(ABORT, 'invalid Execution placement policy update')
		   WHERE NEW.version <> OLD.version + 1
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_pools AS pool
		        WHERE pool.id = NEW.default_pool_id
		          AND pool.execution_target_id = NEW.execution_target_id
		          AND pool.tenant_id IS NEW.tenant_id
		          AND pool.status = 'active'
		      )
		      OR (NEW.balanced_pool_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1 FROM worker_pools AS pool
		        WHERE pool.id = NEW.balanced_pool_id
		          AND pool.execution_target_id = NEW.execution_target_id
		          AND pool.tenant_id IS NEW.tenant_id
		      ))
		      OR (NEW.low_latency_pool_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1 FROM worker_pools AS pool
		        WHERE pool.id = NEW.low_latency_pool_id
		          AND pool.execution_target_id = NEW.execution_target_id
		          AND pool.tenant_id IS NEW.tenant_id
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_placement_policy_delete`,
		`CREATE TRIGGER trg_execution_placement_policy_delete
		 BEFORE DELETE ON execution_placement_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution placement policies cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_placement_insert`,
		`CREATE TRIGGER trg_agent_executions_placement_insert
		 BEFORE INSERT ON agent_executions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution placement snapshot')
		   WHERE NOT (
		     (NEW.worker_pool_id IS NULL AND NEW.worker_pool_version IS NULL
		       AND NEW.capacity_class IS NULL AND NEW.placement_policy_version IS NULL)
		     OR
		     (NEW.worker_pool_id IS NOT NULL AND NEW.worker_pool_version > 0
		       AND NEW.capacity_class IN ('standard', 'interactive')
		       AND NEW.placement_policy_version > 0
		       AND EXISTS (
		         SELECT 1 FROM worker_pools AS pool
		         WHERE pool.id = NEW.worker_pool_id
		           AND pool.version = NEW.worker_pool_version
		           AND pool.execution_target_id = NEW.execution_target_id
		           AND pool.tenant_id IS NEW.tenant_id
		           AND pool.capacity_class = NEW.capacity_class
		       ))
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_placement_update`,
		`CREATE TRIGGER trg_agent_executions_placement_update
		 BEFORE UPDATE OF worker_pool_id, worker_pool_version, capacity_class, placement_policy_version
		 ON agent_executions
		 WHEN OLD.worker_pool_id IS NOT NULL AND (
		   NEW.worker_pool_id IS NOT OLD.worker_pool_id
		   OR NEW.worker_pool_version IS NOT OLD.worker_pool_version
		   OR NEW.capacity_class IS NOT OLD.capacity_class
		   OR NEW.placement_policy_version IS NOT OLD.placement_policy_version
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution placement snapshot is immutable');
		 END`,
		`WITH lifecycle AS (
		   SELECT
		     tenant_id,
		     execution_id,
		     generation,
		     MIN(CASE WHEN event_type = 'execution.started' THEN occurred_at END) AS execution_started_at,
		     MIN(CASE WHEN event_type = 'session.started' THEN occurred_at END) AS provider_ready_at
		   FROM session_events
		   WHERE execution_id IS NOT NULL
		     AND generation IS NOT NULL
		     AND event_type IN ('execution.started', 'session.started')
		   GROUP BY tenant_id, execution_id, generation
		 )
		 INSERT OR IGNORE INTO execution_generation_facts (
		   tenant_id, execution_id, generation, session_id, turn_id,
		   execution_target_id, target_kind, provider, recovery_reason,
		   warm_pool_mode, warm_pool_result, dispatch_requested_at,
		   bundle_created_at, leased_at, execution_started_at, provider_ready_at,
		   provider_resume_strategy, created_at, updated_at
		 )
		 SELECT
		   execution.tenant_id,
		   execution.id,
		   execution.generation,
		   execution.session_id,
		   execution.turn_id,
		   execution.execution_target_id,
		   execution.target_kind,
		   COALESCE(NULLIF(execution.provider, ''), session.provider),
		   CASE
		     WHEN bundle.recovery_reason IS NOT NULL THEN bundle.recovery_reason
		     WHEN EXISTS (
		       SELECT 1 FROM execution_recovery_bundles AS previous
		       WHERE previous.tenant_id = execution.tenant_id
		         AND previous.execution_id = execution.id
		         AND previous.generation < execution.generation
		     ) THEN CASE
		     WHEN execution.next_recovery_reason = 'suspend-resume' THEN 'suspend-resume'
		     WHEN execution.next_recovery_reason = 'disaster-recovery' THEN 'disaster-recovery'
		       ELSE 'execution-recovery'
		     END
		     WHEN execution.generation > 1 THEN 'legacy-adoption'
		     ELSE 'initial-claim'
		   END,
		   session.warm_pool_mode,
		   CASE
		     WHEN session.warm_pool_mode = 'disabled' THEN 'not-requested'
		     WHEN worker.worker_mode = 'warm-pool' THEN 'hit'
		     WHEN worker.id IS NOT NULL THEN 'fallback'
		     ELSE 'pending'
		   END,
		   CASE
		     WHEN execution.generation = 1 THEN execution.queued_at
		     ELSE COALESCE(bundle.created_at, lease.acquired_at, execution.queued_at)
		   END,
		   bundle.created_at,
		   CASE
		     WHEN lease.acquired_at IS NULL THEN NULL
		     ELSE MAX(
		       lease.acquired_at,
		       CASE
		         WHEN execution.generation = 1 THEN execution.queued_at
		         ELSE COALESCE(bundle.created_at, lease.acquired_at, execution.queued_at)
		       END
		     )
		   END,
		   CASE
		     WHEN lifecycle.execution_started_at IS NULL THEN NULL
		     ELSE MAX(
		       lifecycle.execution_started_at,
		       COALESCE(lease.acquired_at, bundle.created_at, execution.queued_at),
		       CASE
		         WHEN execution.generation = 1 THEN execution.queued_at
		         ELSE COALESCE(bundle.created_at, lease.acquired_at, execution.queued_at)
		       END
		     )
		   END,
		   CASE
		     WHEN lifecycle.provider_ready_at IS NULL THEN NULL
		     ELSE MAX(
		       lifecycle.provider_ready_at,
		       COALESCE(lifecycle.execution_started_at, lease.acquired_at, bundle.created_at, execution.queued_at),
		       COALESCE(lease.acquired_at, bundle.created_at, execution.queued_at),
		       CASE
		         WHEN execution.generation = 1 THEN execution.queued_at
		         ELSE COALESCE(bundle.created_at, lease.acquired_at, execution.queued_at)
		       END
		     )
		   END,
		   COALESCE(NULLIF(execution.provider_resume_strategy_snapshot, ''), 'authoritative-history'),
		   CASE
		     WHEN execution.generation = 1 THEN execution.queued_at
		     ELSE COALESCE(bundle.created_at, lease.acquired_at, execution.queued_at)
		   END,
		   MAX(
		     CASE
		       WHEN execution.generation = 1 THEN execution.queued_at
		       ELSE COALESCE(bundle.created_at, lease.acquired_at, execution.queued_at)
		     END,
		     COALESCE(bundle.created_at, lease.acquired_at, execution.queued_at),
		     COALESCE(lease.acquired_at, bundle.created_at, execution.queued_at),
		     COALESCE(lifecycle.execution_started_at, lease.acquired_at, bundle.created_at, execution.queued_at),
		     COALESCE(lifecycle.provider_ready_at, lifecycle.execution_started_at, lease.acquired_at, bundle.created_at, execution.queued_at)
		   )
		 FROM agent_executions AS execution
		 JOIN agent_sessions AS session
		   ON session.tenant_id = execution.tenant_id AND session.id = execution.session_id
		 LEFT JOIN execution_recovery_bundles AS bundle
		   ON bundle.tenant_id = execution.tenant_id
		  AND bundle.execution_id = execution.id
		  AND bundle.generation = execution.generation
		 LEFT JOIN worker_leases AS lease
		   ON lease.tenant_id = execution.tenant_id
		  AND lease.execution_id = execution.id
		  AND lease.generation = execution.generation
		 LEFT JOIN worker_instances AS worker ON worker.id = execution.worker_id
		 LEFT JOIN lifecycle
		   ON lifecycle.tenant_id = execution.tenant_id
		  AND lifecycle.execution_id = execution.id
		  AND lifecycle.generation = execution.generation
		 WHERE execution.generation > 0`,
		`CREATE INDEX IF NOT EXISTS idx_execution_generation_facts_metrics
		 ON execution_generation_facts (target_kind, recovery_reason, warm_pool_mode, warm_pool_result)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_generation_facts_terminal
		 ON execution_generation_facts (terminal_outcome, terminal_at)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_generation_facts_pod_provisioning
		 ON execution_generation_facts (
		   target_kind, pod_provisioning_started_at, pod_running_at, pod_pending_since_at
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_execution_generation_pod_failure_metrics
		 ON execution_generation_pod_failure_facts (
		   failure_class, first_observed_at, execution_target_id
		 )`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_facts_insert`,
		`CREATE TRIGGER trg_execution_generation_facts_insert
		 BEFORE INSERT ON execution_generation_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution generation fact')
		   WHERE NEW.generation <= 0
		      OR NEW.recovery_reason NOT IN ('initial-claim', 'legacy-adoption', 'execution-recovery', 'suspend-resume', 'disaster-recovery')
		      OR NEW.warm_pool_mode NOT IN ('disabled', 'balanced', 'low-latency')
		      OR NEW.warm_pool_result NOT IN ('pending', 'not-requested', 'hit', 'fallback')
		      OR (NEW.terminal_outcome IS NOT NULL AND NEW.terminal_outcome NOT IN (
		        'completed', 'failed', 'cancelled', 'interrupted', 'recovering'
		      ))
		      OR (NEW.pod_provisioning_started_at IS NOT NULL AND NEW.dispatch_requested_at IS NOT NULL
		        AND NEW.pod_provisioning_started_at < NEW.dispatch_requested_at)
		      OR (NEW.pod_pending_since_at IS NOT NULL AND NEW.pod_provisioning_started_at IS NOT NULL
		        AND NEW.pod_pending_since_at < NEW.pod_provisioning_started_at)
		      OR (NEW.pod_running_at IS NOT NULL AND NEW.pod_provisioning_started_at IS NOT NULL
		        AND NEW.pod_running_at < NEW.pod_provisioning_started_at)
		      OR (NEW.pod_last_observed_at IS NOT NULL AND NEW.pod_provisioning_started_at IS NOT NULL
		        AND NEW.pod_last_observed_at < NEW.pod_provisioning_started_at)
		      OR (NEW.pod_last_observed_at IS NOT NULL AND NEW.pod_pending_since_at IS NOT NULL
		        AND NEW.pod_last_observed_at < NEW.pod_pending_since_at)
		      OR (NEW.pod_last_observed_at IS NOT NULL AND NEW.pod_running_at IS NOT NULL
		        AND NEW.pod_last_observed_at < NEW.pod_running_at)
		      OR NOT EXISTS (
		        SELECT 1 FROM agent_executions AS execution
		        WHERE execution.tenant_id = NEW.tenant_id AND execution.id = NEW.execution_id
		          AND execution.session_id = NEW.session_id AND execution.turn_id = NEW.turn_id
		          AND execution.execution_target_id = NEW.execution_target_id
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_facts_update`,
		`CREATE TRIGGER trg_execution_generation_facts_update
		 BEFORE UPDATE ON execution_generation_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation fact identity is immutable')
		   WHERE NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.execution_id IS NOT OLD.execution_id
		      OR NEW.generation IS NOT OLD.generation
		      OR NEW.session_id IS NOT OLD.session_id
		      OR NEW.turn_id IS NOT OLD.turn_id
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.target_kind IS NOT OLD.target_kind
		      OR NEW.provider IS NOT OLD.provider
		      OR NEW.recovery_reason IS NOT OLD.recovery_reason
		      OR NEW.warm_pool_mode IS NOT OLD.warm_pool_mode;

		   SELECT RAISE(ABORT, 'Execution generation warm-pool result is immutable after decision')
		   WHERE OLD.warm_pool_result <> 'pending'
		     AND NEW.warm_pool_result IS NOT OLD.warm_pool_result;

		   SELECT RAISE(ABORT, 'Execution generation terminal outcome is immutable')
		   WHERE OLD.terminal_outcome IS NOT NULL AND (
		     NEW.terminal_outcome IS NOT OLD.terminal_outcome
		     OR NEW.terminal_at IS NOT OLD.terminal_at
		   );

		   SELECT RAISE(ABORT, 'Execution generation Pod provisioning start is immutable')
		   WHERE OLD.pod_provisioning_started_at IS NOT NULL
		     AND NEW.pod_provisioning_started_at IS NOT OLD.pod_provisioning_started_at;

		   SELECT RAISE(ABORT, 'Execution generation Pod Pending start is immutable')
		   WHERE OLD.pod_pending_since_at IS NOT NULL
		     AND NEW.pod_pending_since_at IS NOT OLD.pod_pending_since_at;

		   SELECT RAISE(ABORT, 'Execution generation Pod Running time is immutable')
		   WHERE OLD.pod_running_at IS NOT NULL
		     AND NEW.pod_running_at IS NOT OLD.pod_running_at;

		   SELECT RAISE(ABORT, 'Execution generation Pod observation time cannot move backwards')
		   WHERE OLD.pod_last_observed_at IS NOT NULL AND (
		     NEW.pod_last_observed_at IS NULL
		     OR NEW.pod_last_observed_at < OLD.pod_last_observed_at
		   );

		   SELECT RAISE(ABORT, 'Execution generation fact timeline is invalid')
		   WHERE (NEW.dispatch_requested_at IS NOT NULL AND NEW.leased_at IS NOT NULL
		       AND NEW.dispatch_requested_at > NEW.leased_at)
		      OR (NEW.leased_at IS NOT NULL AND NEW.execution_started_at IS NOT NULL
		       AND NEW.leased_at > NEW.execution_started_at)
		      OR (NEW.execution_started_at IS NOT NULL AND NEW.provider_ready_at IS NOT NULL
		       AND NEW.execution_started_at > NEW.provider_ready_at)
		      OR (NEW.dispatch_requested_at IS NOT NULL AND NEW.terminal_at IS NOT NULL
		       AND NEW.dispatch_requested_at > NEW.terminal_at)
		      OR (NEW.pod_provisioning_started_at IS NOT NULL AND NEW.dispatch_requested_at IS NOT NULL
		       AND NEW.pod_provisioning_started_at < NEW.dispatch_requested_at)
		      OR (NEW.pod_pending_since_at IS NOT NULL AND NEW.pod_provisioning_started_at IS NOT NULL
		       AND NEW.pod_pending_since_at < NEW.pod_provisioning_started_at)
		      OR (NEW.pod_running_at IS NOT NULL AND NEW.pod_provisioning_started_at IS NOT NULL
		       AND NEW.pod_running_at < NEW.pod_provisioning_started_at)
		      OR (NEW.pod_last_observed_at IS NOT NULL AND NEW.pod_provisioning_started_at IS NOT NULL
		       AND NEW.pod_last_observed_at < NEW.pod_provisioning_started_at)
		      OR (NEW.pod_last_observed_at IS NOT NULL AND NEW.pod_pending_since_at IS NOT NULL
		       AND NEW.pod_last_observed_at < NEW.pod_pending_since_at)
		      OR (NEW.pod_last_observed_at IS NOT NULL AND NEW.pod_running_at IS NOT NULL
		       AND NEW.pod_last_observed_at < NEW.pod_running_at);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_facts_delete`,
		`CREATE TRIGGER trg_execution_generation_facts_delete
		 BEFORE DELETE ON execution_generation_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation facts cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_facts_insert`,
		`CREATE TRIGGER trg_execution_generation_pod_failure_facts_insert
		 BEFORE INSERT ON execution_generation_pod_failure_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution generation Pod failure fact')
		   WHERE NEW.generation <= 0
		      OR NEW.failure_class IS NULL
		      OR NEW.failure_class NOT IN (
		        'pod-apply-failed', 'pending-timeout', 'unschedulable', 'image-pull',
		        'container-start', 'evicted', 'oom-killed', 'pod-failed'
		      )
		      OR NEW.namespace IS NULL OR length(NEW.namespace) NOT BETWEEN 1 AND 253
		      OR NEW.pod_name IS NULL OR length(NEW.pod_name) NOT BETWEEN 1 AND 253
		      OR (NEW.pod_uid IS NOT NULL AND length(NEW.pod_uid) NOT BETWEEN 1 AND 160)
		      OR NEW.reason_code IS NULL OR length(NEW.reason_code) NOT BETWEEN 1 AND 160
		      OR (NEW.failure_class = 'pod-apply-failed' AND NEW.pod_uid IS NOT NULL)
		      OR (NEW.failure_class <> 'pod-apply-failed' AND NEW.pod_uid IS NULL)
		      OR NEW.first_observed_at IS NULL OR NEW.last_observed_at IS NULL
		      OR NEW.last_observed_at < NEW.first_observed_at
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_generation_facts AS generation
		        WHERE generation.tenant_id = NEW.tenant_id
		          AND generation.execution_id = NEW.execution_id
		          AND generation.generation = NEW.generation
		          AND generation.execution_target_id = NEW.execution_target_id
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_facts_update`,
		`CREATE TRIGGER trg_execution_generation_pod_failure_facts_update
		 BEFORE UPDATE ON execution_generation_pod_failure_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation Pod failure fact identity is immutable')
		   WHERE NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.execution_id IS NOT OLD.execution_id
		      OR NEW.generation IS NOT OLD.generation
		      OR NEW.failure_class IS NOT OLD.failure_class
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.namespace IS NOT OLD.namespace
		      OR NEW.pod_name IS NOT OLD.pod_name
		      OR NEW.pod_uid IS NOT OLD.pod_uid
		      OR NEW.reason_code IS NOT OLD.reason_code
		      OR NEW.first_observed_at IS NOT OLD.first_observed_at;

		   SELECT RAISE(ABORT, 'Execution generation Pod failure observation cannot move backwards')
		   WHERE NEW.last_observed_at IS NULL OR NEW.last_observed_at < OLD.last_observed_at;
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_facts_delete`,
		`CREATE TRIGGER trg_execution_generation_pod_failure_facts_delete
		 BEFORE DELETE ON execution_generation_pod_failure_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution generation Pod failure facts cannot be deleted');
		 END`,
		`INSERT OR IGNORE INTO worker_incarnation_facts (
		   worker_id, worker_incarnation, tenant_id, execution_target_id,
		   target_kind, worker_mode, worker_pool_id, worker_pool_version,
		   pool_mode, capacity_class, cluster_id, region, namespace, pod_name,
		   instance_uid, registered_at, current_state, state_changed_at,
		   terminated_at, terminal_reason, accumulated_active_seconds,
		   accumulated_idle_seconds, claim_count, created_at, updated_at
		 )
		 SELECT
		   worker.id,
		   worker.incarnation,
		   target.tenant_id,
		   worker.execution_target_id,
		   worker.target_kind,
		   worker.worker_mode,
		   worker.worker_pool_id,
		   worker.worker_pool_version,
		   pool.mode,
		   worker.capacity_class,
		   worker.cluster_id,
		   COALESCE(pool.region, ''),
		   worker.namespace,
		   worker.pod_name,
		   worker.instance_uid,
		   worker.registered_at,
		   CASE
		     WHEN worker.status = 'terminated' THEN 'terminated'
		     WHEN worker.status = 'draining' THEN 'draining'
		     WHEN worker.status = 'offline' THEN 'offline'
		     WHEN lease.execution_id IS NOT NULL THEN 'active'
		     ELSE 'idle'
		   END,
		   CASE
		     WHEN worker.status = 'terminated' THEN COALESCE(worker.terminated_at, worker.last_heartbeat_at, worker.registered_at)
		     WHEN worker.status = 'draining' THEN COALESCE(worker.draining_at, worker.last_heartbeat_at, worker.registered_at)
		     ELSE COALESCE(worker.last_heartbeat_at, worker.registered_at)
		   END,
		   CASE WHEN worker.status = 'terminated' THEN COALESCE(worker.terminated_at, worker.last_heartbeat_at, worker.registered_at) END,
		   CASE WHEN worker.status = 'terminated' THEN 'legacy-observed-terminal' END,
		   0,
		   0,
		   CASE WHEN lease.execution_id IS NULL THEN 0 ELSE 1 END,
		   worker.registered_at,
		   CASE
		     WHEN worker.status = 'terminated' THEN COALESCE(worker.terminated_at, worker.last_heartbeat_at, worker.registered_at)
		     ELSE COALESCE(worker.last_heartbeat_at, worker.registered_at)
		   END
		 FROM worker_instances AS worker
		 JOIN execution_targets AS target ON target.id = worker.execution_target_id
		 LEFT JOIN worker_pools AS pool ON pool.id = worker.worker_pool_id
		 LEFT JOIN worker_leases AS lease
		   ON lease.worker_id = worker.id AND lease.worker_incarnation = worker.incarnation`,
		`CREATE INDEX IF NOT EXISTS idx_worker_incarnation_facts_metrics
		 ON worker_incarnation_facts (target_kind, pool_mode, capacity_class, current_state)`,
		`CREATE INDEX IF NOT EXISTS idx_worker_incarnation_facts_target_state
		 ON worker_incarnation_facts (execution_target_id, current_state, worker_id, worker_incarnation)`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_facts_insert`,
		`CREATE TRIGGER trg_worker_incarnation_facts_insert
		 BEFORE INSERT ON worker_incarnation_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker incarnation fact')
		   WHERE NEW.worker_incarnation <= 0
		      OR NEW.target_kind NOT IN ('local', 'ssh', 'docker', 'kubernetes')
		      OR NEW.worker_mode NOT IN ('execution-pinned', 'warm-pool', 'general-pool')
		      OR NEW.current_state NOT IN ('idle', 'active', 'draining', 'offline', 'terminated')
		      OR (
		        (NEW.worker_pool_id IS NULL AND (
		          NEW.worker_pool_version IS NOT NULL OR NEW.pool_mode IS NOT NULL OR NEW.capacity_class IS NOT NULL
		        ))
		        OR
		        (NEW.worker_pool_id IS NOT NULL AND (
		          NEW.worker_pool_version IS NULL OR NEW.worker_pool_version <= 0
		          OR NEW.pool_mode NOT IN ('resident', 'per-execution', 'warm')
		          OR NEW.capacity_class NOT IN ('standard', 'interactive')
		        ))
		      )
		      OR length(trim(ifnull(NEW.cluster_id, ''))) NOT BETWEEN 1 AND 160
		      OR length(ifnull(NEW.region, '')) > 160
		      OR length(trim(ifnull(NEW.namespace, ''))) NOT BETWEEN 1 AND 253
		      OR length(trim(ifnull(NEW.pod_name, ''))) NOT BETWEEN 1 AND 253
		      OR length(ifnull(NEW.instance_uid, '')) <> 36
		      OR NEW.instance_uid <> trim(NEW.instance_uid)
		      OR NEW.instance_uid <> lower(NEW.instance_uid)
		      OR substr(NEW.instance_uid, 9, 1) <> '-'
		      OR substr(NEW.instance_uid, 14, 1) <> '-'
		      OR substr(NEW.instance_uid, 19, 1) <> '-'
		      OR substr(NEW.instance_uid, 24, 1) <> '-'
		      OR length(replace(NEW.instance_uid, '-', '')) <> 32
		      OR replace(NEW.instance_uid, '-', '') GLOB '*[^0-9a-f]*'
		      OR (NEW.terminal_reason IS NOT NULL AND length(trim(NEW.terminal_reason)) NOT BETWEEN 1 AND 160)
		      OR (NEW.requested_cpu_millicores IS NOT NULL AND NEW.requested_cpu_millicores <= 0)
		      OR (NEW.requested_memory_bytes IS NOT NULL AND NEW.requested_memory_bytes <= 0)
		      OR (NEW.requested_ephemeral_storage_bytes IS NOT NULL AND NEW.requested_ephemeral_storage_bytes <= 0)
		      OR NEW.accumulated_active_seconds < 0
		      OR NEW.accumulated_idle_seconds < 0
		      OR NEW.claim_count < 0
		      OR NEW.state_changed_at < NEW.registered_at
		      OR NEW.created_at > NEW.updated_at
		      OR (
		        (NEW.current_state = 'terminated' AND (
		          NEW.terminated_at IS NULL OR NEW.terminated_at < NEW.state_changed_at
		        ))
		        OR
		        (NEW.current_state <> 'terminated' AND (
		          NEW.terminated_at IS NOT NULL OR NEW.terminal_reason IS NOT NULL
		        ))
		      )
		      OR NOT EXISTS (
		        SELECT 1
		        FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id
		          AND target.tenant_id IS NEW.tenant_id
		      )
		      OR NOT EXISTS (
		        SELECT 1
		        FROM worker_instances AS worker
		        WHERE worker.id = NEW.worker_id
		          AND worker.incarnation = NEW.worker_incarnation
		          AND worker.execution_target_id = NEW.execution_target_id
		          AND worker.target_kind = NEW.target_kind
		          AND worker.worker_mode = NEW.worker_mode
		          AND worker.worker_pool_id IS NEW.worker_pool_id
		          AND worker.worker_pool_version IS NEW.worker_pool_version
		          AND worker.capacity_class IS NEW.capacity_class
		          AND worker.cluster_id = NEW.cluster_id
		          AND worker.namespace = NEW.namespace
		          AND worker.pod_name = NEW.pod_name
		          AND worker.instance_uid = NEW.instance_uid
		      )
		      OR (
		        NEW.worker_pool_id IS NOT NULL AND NOT EXISTS (
		          SELECT 1
		          FROM worker_pools AS pool
		          WHERE pool.id = NEW.worker_pool_id
		            AND pool.execution_target_id = NEW.execution_target_id
		            AND pool.tenant_id IS NEW.tenant_id
		            AND pool.mode = NEW.pool_mode
		            AND pool.capacity_class = NEW.capacity_class
		            AND pool.version >= NEW.worker_pool_version
		        )
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_facts_update`,
		`CREATE TRIGGER trg_worker_incarnation_facts_update
		 BEFORE UPDATE ON worker_incarnation_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker incarnation fact identity is immutable')
		   WHERE NEW.worker_id IS NOT OLD.worker_id
		      OR NEW.worker_incarnation IS NOT OLD.worker_incarnation
		      OR NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.target_kind IS NOT OLD.target_kind
		      OR NEW.worker_mode IS NOT OLD.worker_mode
		      OR NEW.worker_pool_id IS NOT OLD.worker_pool_id
		      OR NEW.worker_pool_version IS NOT OLD.worker_pool_version
		      OR NEW.pool_mode IS NOT OLD.pool_mode
		      OR NEW.capacity_class IS NOT OLD.capacity_class
		      OR NEW.cluster_id IS NOT OLD.cluster_id
		      OR NEW.region IS NOT OLD.region
		      OR NEW.namespace IS NOT OLD.namespace
		      OR NEW.pod_name IS NOT OLD.pod_name
		      OR NEW.instance_uid IS NOT OLD.instance_uid
		      OR NEW.registered_at IS NOT OLD.registered_at
		      OR NEW.requested_cpu_millicores IS NOT OLD.requested_cpu_millicores
		      OR NEW.requested_memory_bytes IS NOT OLD.requested_memory_bytes
		      OR NEW.requested_ephemeral_storage_bytes IS NOT OLD.requested_ephemeral_storage_bytes;

		   SELECT RAISE(ABORT, 'Worker incarnation terminal state is immutable')
		   WHERE OLD.current_state = 'terminated' AND (
		     NEW.current_state IS NOT OLD.current_state
		     OR NEW.state_changed_at IS NOT OLD.state_changed_at
		     OR NEW.terminated_at IS NOT OLD.terminated_at
		     OR NEW.terminal_reason IS NOT OLD.terminal_reason
		     OR NEW.accumulated_active_seconds <> OLD.accumulated_active_seconds
		     OR NEW.accumulated_idle_seconds <> OLD.accumulated_idle_seconds
		     OR NEW.claim_count <> OLD.claim_count
		     OR NEW.updated_at IS NOT OLD.updated_at
		   );

		   SELECT RAISE(ABORT, 'Worker incarnation fact timeline or counters regressed')
		   WHERE NEW.state_changed_at < OLD.state_changed_at
		      OR NEW.updated_at < OLD.updated_at
		      OR NEW.accumulated_active_seconds < OLD.accumulated_active_seconds
		      OR NEW.accumulated_idle_seconds < OLD.accumulated_idle_seconds
		      OR NEW.claim_count < OLD.claim_count
		      OR (OLD.terminated_at IS NOT NULL AND NEW.terminated_at IS NOT OLD.terminated_at)
		      OR (OLD.terminal_reason IS NOT NULL AND NEW.terminal_reason IS NOT OLD.terminal_reason);

		   SELECT RAISE(ABORT, 'invalid Worker incarnation fact')
		   WHERE (
		        (NEW.worker_pool_id IS NULL AND (
		          NEW.worker_pool_version IS NOT NULL OR NEW.pool_mode IS NOT NULL OR NEW.capacity_class IS NOT NULL
		        ))
		        OR
		        (NEW.worker_pool_id IS NOT NULL AND (
		          NEW.worker_pool_version IS NULL OR NEW.worker_pool_version <= 0
		          OR NEW.pool_mode NOT IN ('resident', 'per-execution', 'warm')
		          OR NEW.capacity_class NOT IN ('standard', 'interactive')
		        ))
		      )
		      OR NEW.current_state NOT IN ('idle', 'active', 'draining', 'offline', 'terminated')
		      OR (NEW.terminal_reason IS NOT NULL AND length(trim(NEW.terminal_reason)) NOT BETWEEN 1 AND 160)
		      OR NEW.state_changed_at < NEW.registered_at
		      OR NEW.created_at > NEW.updated_at
		      OR (
		        (NEW.current_state = 'terminated' AND (
		          NEW.terminated_at IS NULL OR NEW.terminated_at < NEW.state_changed_at
		        ))
		        OR
		        (NEW.current_state <> 'terminated' AND (
		          NEW.terminated_at IS NOT NULL OR NEW.terminal_reason IS NOT NULL
		        ))
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_facts_delete`,
		`CREATE TRIGGER trg_worker_incarnation_facts_delete
		 BEFORE DELETE ON worker_incarnation_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker incarnation facts cannot be deleted');
		 END`,
		`CREATE INDEX IF NOT EXISTS idx_worker_incarnation_metric_rollups_day
		 ON worker_incarnation_metric_rollups (bucket_day, target_kind)`,
		`CREATE INDEX IF NOT EXISTS idx_worker_incarnation_metric_rollup_entries_pending
		 ON worker_incarnation_metric_rollup_entries (terminal_at, worker_id, worker_incarnation)
		 WHERE rolled_up_at IS NULL`,
		`INSERT OR IGNORE INTO worker_incarnation_metric_rollup_entries (
		   worker_id, worker_incarnation, terminal_at, bucket_day,
		   rolled_up_at, created_at, updated_at
		 )
		 SELECT
		   worker_id, worker_incarnation, terminated_at, date(terminated_at),
		   NULL, terminated_at, terminated_at
		 FROM worker_incarnation_facts
		 WHERE current_state = 'terminated'
		   AND terminated_at IS NOT NULL`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollups_insert`,
		`CREATE TRIGGER trg_worker_incarnation_metric_rollups_insert
		 BEFORE INSERT ON worker_incarnation_metric_rollups
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker incarnation metric rollup')
		   WHERE NEW.bucket_day IS NULL
		      OR NEW.target_kind NOT IN ('local', 'ssh', 'docker', 'kubernetes')
		      OR NEW.pool_mode NOT IN ('resident', 'per-execution', 'warm', 'unassigned')
		      OR NEW.capacity_class NOT IN ('standard', 'interactive', 'unassigned')
		      OR NEW.fact_count <= 0
		      OR NEW.run_seconds < 0
		      OR NEW.active_seconds < 0
		      OR NEW.idle_seconds < 0
		      OR NEW.requested_cpu_seconds < 0
		      OR NEW.requested_memory_byte_seconds < 0
		      OR NEW.requested_ephemeral_storage_byte_seconds < 0
		      OR NEW.run_seconds >= 1e308
		      OR NEW.active_seconds >= 1e308
		      OR NEW.idle_seconds >= 1e308
		      OR NEW.requested_cpu_seconds >= 1e308
		      OR NEW.requested_memory_byte_seconds >= 1e308
		      OR NEW.requested_ephemeral_storage_byte_seconds >= 1e308
		      OR NEW.created_at > NEW.updated_at;
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollups_update`,
		`CREATE TRIGGER trg_worker_incarnation_metric_rollups_update
		 BEFORE UPDATE ON worker_incarnation_metric_rollups
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker incarnation metric rollup identity is immutable')
		   WHERE NEW.bucket_day IS NOT OLD.bucket_day
		      OR NEW.target_kind IS NOT OLD.target_kind
		      OR NEW.pool_mode IS NOT OLD.pool_mode
		      OR NEW.capacity_class IS NOT OLD.capacity_class
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'Worker incarnation metric rollup totals cannot regress')
		   WHERE NEW.fact_count < OLD.fact_count
		      OR NEW.run_seconds < OLD.run_seconds
		      OR NEW.active_seconds < OLD.active_seconds
		      OR NEW.idle_seconds < OLD.idle_seconds
		      OR NEW.requested_cpu_seconds < OLD.requested_cpu_seconds
		      OR NEW.requested_memory_byte_seconds < OLD.requested_memory_byte_seconds
		      OR NEW.requested_ephemeral_storage_byte_seconds < OLD.requested_ephemeral_storage_byte_seconds
		      OR NEW.run_seconds >= 1e308
		      OR NEW.active_seconds >= 1e308
		      OR NEW.idle_seconds >= 1e308
		      OR NEW.requested_cpu_seconds >= 1e308
		      OR NEW.requested_memory_byte_seconds >= 1e308
		      OR NEW.requested_ephemeral_storage_byte_seconds >= 1e308
		      OR NEW.updated_at < OLD.updated_at;
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollups_delete`,
		`CREATE TRIGGER trg_worker_incarnation_metric_rollups_delete
		 BEFORE DELETE ON worker_incarnation_metric_rollups
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker incarnation metric rollups cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollup_entries_insert`,
		`CREATE TRIGGER trg_worker_incarnation_metric_rollup_entries_insert
		 BEFORE INSERT ON worker_incarnation_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker incarnation metric rollup entry')
		   WHERE NEW.worker_incarnation <= 0
		      OR NEW.terminal_at IS NULL
		      OR NEW.bucket_day IS NULL
		      OR date(NEW.bucket_day) <> date(NEW.terminal_at)
		      OR NEW.created_at > NEW.updated_at
		      OR (NEW.rolled_up_at IS NOT NULL AND NEW.rolled_up_at < NEW.created_at)
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_incarnation_facts AS fact
		        WHERE fact.worker_id = NEW.worker_id
		          AND fact.worker_incarnation = NEW.worker_incarnation
		          AND fact.current_state = 'terminated'
		          AND fact.terminated_at IS NEW.terminal_at
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollup_entries_update`,
		`CREATE TRIGGER trg_worker_incarnation_metric_rollup_entries_update
		 BEFORE UPDATE ON worker_incarnation_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker incarnation metric rollup entry identity is immutable')
		   WHERE NEW.worker_id IS NOT OLD.worker_id
		      OR NEW.worker_incarnation IS NOT OLD.worker_incarnation
		      OR NEW.terminal_at IS NOT OLD.terminal_at
		      OR NEW.bucket_day IS NOT OLD.bucket_day
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'Worker incarnation metric rollup entry completion is immutable')
		   WHERE OLD.rolled_up_at IS NOT NULL
		     AND NEW.rolled_up_at IS NOT OLD.rolled_up_at;

		   SELECT RAISE(ABORT, 'Worker incarnation metric rollup entry timeline cannot regress')
		   WHERE NEW.updated_at < OLD.updated_at
		      OR (NEW.rolled_up_at IS NOT NULL AND NEW.rolled_up_at < NEW.created_at);
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollup_entries_delete`,
		`CREATE TRIGGER trg_worker_incarnation_metric_rollup_entries_delete
		 BEFORE DELETE ON worker_incarnation_metric_rollup_entries
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker incarnation metric rollup entries cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollup_enqueue_insert`,
		`CREATE TRIGGER trg_worker_incarnation_metric_rollup_enqueue_insert
		 AFTER INSERT ON worker_incarnation_facts
		 WHEN NEW.current_state = 'terminated' AND NEW.terminated_at IS NOT NULL
		 BEGIN
		   INSERT OR IGNORE INTO worker_incarnation_metric_rollup_entries (
		     worker_id, worker_incarnation, terminal_at, bucket_day,
		     rolled_up_at, created_at, updated_at
		   ) VALUES (
		     NEW.worker_id, NEW.worker_incarnation, NEW.terminated_at, date(NEW.terminated_at),
		     NULL, NEW.terminated_at, NEW.terminated_at
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollup_enqueue_update`,
		`CREATE TRIGGER trg_worker_incarnation_metric_rollup_enqueue_update
		 AFTER UPDATE OF current_state, terminated_at ON worker_incarnation_facts
		 WHEN NEW.current_state = 'terminated'
		   AND NEW.terminated_at IS NOT NULL
		   AND OLD.current_state <> 'terminated'
		 BEGIN
		   INSERT OR IGNORE INTO worker_incarnation_metric_rollup_entries (
		     worker_id, worker_incarnation, terminal_at, bucket_day,
		     rolled_up_at, created_at, updated_at
		   ) VALUES (
		     NEW.worker_id, NEW.worker_incarnation, NEW.terminated_at, date(NEW.terminated_at),
		     NULL, NEW.terminated_at, NEW.terminated_at
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_registration_trust_insert`,
		`CREATE TRIGGER trg_worker_instances_registration_trust_insert
		 BEFORE INSERT ON worker_instances
		 WHEN NEW.registration_trust_mode IS NULL
		   OR NEW.registration_trust_mode NOT IN ('shared-token', 'kubernetes-pod-bound-v1')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker registration trust mode');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_registration_trust_update`,
		`CREATE TRIGGER trg_worker_instances_registration_trust_update
		 BEFORE UPDATE OF registration_trust_mode ON worker_instances
		 WHEN NEW.registration_trust_mode IS NULL
		   OR NEW.registration_trust_mode NOT IN ('shared-token', 'kubernetes-pod-bound-v1')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker registration trust mode');
		 END`,
		`UPDATE worker_manifests
		 SET process_containment_trust_mode = 'legacy-untrusted'
		 WHERE process_containment_mode <> 'none'
		   AND process_containment_trust_mode = 'none'
		   AND process_containment_attestation_key_id IS NULL
		   AND process_containment_attestation_key_sha256 IS NULL`,
		`DROP TRIGGER IF EXISTS trg_worker_manifest_process_containment_insert`,
		`CREATE TRIGGER trg_worker_manifest_process_containment_insert
		 BEFORE INSERT ON worker_manifests
		 WHEN NOT (
		   (
		     NEW.process_containment_mode = 'none'
		     AND NEW.process_containment_supervisor_version IS NULL
		     AND NEW.process_containment_probe_version IS NULL
		     AND NEW.process_containment_probe_sha256 IS NULL
		     AND NEW.process_containment_supervisor_identity IS NULL
		     AND NEW.process_containment_provider_identity IS NULL
		     AND NEW.process_containment_trust_mode = 'none'
		     AND NEW.process_containment_attestation_key_id IS NULL
		     AND NEW.process_containment_attestation_key_sha256 IS NULL
		   )
		   OR
		   (
		     NEW.process_containment_mode IN ('cgroup-v2', 'job-object')
		     AND length(trim(NEW.process_containment_supervisor_version)) BETWEEN 1 AND 160
		     AND NEW.process_containment_probe_version > 0
		     AND length(NEW.process_containment_probe_sha256) = 64
		     AND NEW.process_containment_probe_sha256 NOT GLOB '*[^0-9a-f]*'
		     AND length(trim(NEW.process_containment_supervisor_identity)) BETWEEN 1 AND 160
		     AND length(trim(NEW.process_containment_provider_identity)) BETWEEN 1 AND 160
		     AND NEW.process_containment_supervisor_identity <> NEW.process_containment_provider_identity
		     AND (
		       (
		         NEW.process_containment_trust_mode = 'signed-v1'
		         AND length(trim(NEW.process_containment_attestation_key_id)) BETWEEN 1 AND 160
		         AND length(NEW.process_containment_attestation_key_sha256) = 64
		         AND NEW.process_containment_attestation_key_sha256 NOT GLOB '*[^0-9a-f]*'
		       ) OR (
		         NEW.process_containment_trust_mode = 'legacy-untrusted'
		         AND NEW.process_containment_attestation_key_id IS NULL
		         AND NEW.process_containment_attestation_key_sha256 IS NULL
		       )
		     )
		     AND (
		       (NEW.process_containment_mode = 'cgroup-v2' AND NEW.operating_system = 'linux')
		       OR (NEW.process_containment_mode = 'job-object' AND NEW.operating_system = 'windows')
		     )
		   )
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker Manifest process containment evidence');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_manifest_process_containment_immutable`,
		`CREATE TRIGGER trg_worker_manifest_process_containment_immutable
		 BEFORE UPDATE OF process_containment_mode, process_containment_supervisor_version,
		   process_containment_probe_version, process_containment_probe_sha256,
		   process_containment_supervisor_identity, process_containment_provider_identity,
		   process_containment_trust_mode, process_containment_attestation_key_id,
		   process_containment_attestation_key_sha256
		 ON worker_manifests
		 WHEN NEW.process_containment_mode IS NOT OLD.process_containment_mode
		   OR NEW.process_containment_supervisor_version IS NOT OLD.process_containment_supervisor_version
		   OR NEW.process_containment_probe_version IS NOT OLD.process_containment_probe_version
		   OR NEW.process_containment_probe_sha256 IS NOT OLD.process_containment_probe_sha256
		   OR NEW.process_containment_supervisor_identity IS NOT OLD.process_containment_supervisor_identity
		   OR NEW.process_containment_provider_identity IS NOT OLD.process_containment_provider_identity
		   OR NEW.process_containment_trust_mode IS NOT OLD.process_containment_trust_mode
		   OR NEW.process_containment_attestation_key_id IS NOT OLD.process_containment_attestation_key_id
		   OR NEW.process_containment_attestation_key_sha256 IS NOT OLD.process_containment_attestation_key_sha256
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker Manifest process containment evidence is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_recovery_bundles_insert`,
		`CREATE TRIGGER trg_execution_recovery_bundles_insert
		 BEFORE INSERT ON execution_recovery_bundles
		 BEGIN
		   SELECT RAISE(ABORT, 'Recovery Bundle shape is invalid')
		   WHERE NEW.generation IS NULL
		      OR NEW.schema_version IS NULL
		      OR NEW.authoritative_history_sequence IS NULL
		      OR NEW.recovery_reason IS NULL
		      OR NEW.payload IS NULL
		      OR NEW.generation <= 0
		      OR NEW.schema_version <= 0
		      OR NEW.authoritative_history_sequence < 0
		      OR NEW.recovery_reason NOT IN ('initial-claim', 'execution-recovery', 'suspend-resume', 'legacy-adoption', 'disaster-recovery')
		      OR NEW.payload_sha256 IS NULL
		      OR length(NEW.payload_sha256) <> 64
		      OR NEW.payload_sha256 GLOB '*[^0-9a-f]*'
		      OR NOT json_valid(NEW.payload)
		      OR json_type(NEW.payload) <> 'object';

		   SELECT RAISE(ABORT, 'Recovery Bundle must reference an existing Tenant Session Turn')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_turns AS turn
		     WHERE turn.tenant_id = NEW.tenant_id
		       AND turn.session_id = NEW.session_id
		       AND turn.id = NEW.turn_id
		   );

		   SELECT RAISE(ABORT, 'Recovery Bundle does not match the current leased Execution generation')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_executions AS execution
		     WHERE execution.tenant_id = NEW.tenant_id
		       AND execution.id = NEW.execution_id
		       AND execution.session_id = NEW.session_id
		       AND execution.turn_id = NEW.turn_id
		       AND execution.generation = NEW.generation
		       AND execution.status IN ('leased', 'running', 'waiting-for-approval')
		   );

		   SELECT RAISE(ABORT, 'Recovery Bundle predecessor must belong to an earlier Generation or declared predecessor Execution')
		   WHERE NEW.previous_bundle_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1
		       FROM execution_recovery_bundles AS previous
		       JOIN agent_executions AS current
		         ON current.tenant_id = NEW.tenant_id AND current.id = NEW.execution_id
		       JOIN agent_executions AS predecessor
		         ON predecessor.tenant_id = previous.tenant_id AND predecessor.id = previous.execution_id
		       WHERE previous.tenant_id = NEW.tenant_id
		         AND previous.id = NEW.previous_bundle_id
		         AND previous.session_id = NEW.session_id
		         AND previous.turn_id = NEW.turn_id
		         AND (
		           (previous.execution_id = NEW.execution_id AND previous.generation < NEW.generation)
		           OR
		           (current.predecessor_execution_id = previous.execution_id AND predecessor.attempt < current.attempt)
		         )
		     );

		   SELECT RAISE(ABORT, 'Initial Recovery Bundle must be Generation 1 without a predecessor')
		   WHERE NEW.recovery_reason = 'initial-claim'
		     AND (NEW.generation <> 1 OR NEW.previous_bundle_id IS NOT NULL);

		   SELECT RAISE(ABORT, 'Recovered Execution generations require a predecessor Recovery Bundle')
		   WHERE NEW.recovery_reason IN ('execution-recovery', 'suspend-resume', 'disaster-recovery')
		     AND NEW.previous_bundle_id IS NULL;

		   SELECT RAISE(ABORT, 'Disaster Recovery requires a predecessor from another Execution attempt')
		   WHERE NEW.recovery_reason = 'disaster-recovery'
		     AND EXISTS (
		       SELECT 1 FROM execution_recovery_bundles AS previous
		       WHERE previous.tenant_id = NEW.tenant_id
		         AND previous.id = NEW.previous_bundle_id
		         AND previous.execution_id = NEW.execution_id
		     );

		   SELECT RAISE(ABORT, 'Legacy Recovery Bundle adoption is allowed only once for an existing Generation without history')
		   WHERE NEW.recovery_reason = 'legacy-adoption'
		     AND (
		       NEW.generation <= 1
		       OR NEW.previous_bundle_id IS NOT NULL
		       OR EXISTS (
		         SELECT 1 FROM execution_recovery_bundles AS existing
		         WHERE existing.tenant_id = NEW.tenant_id
		           AND existing.execution_id = NEW.execution_id
		       )
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_recovery_bundles_immutable_update`,
		`CREATE TRIGGER trg_execution_recovery_bundles_immutable_update
		 BEFORE UPDATE ON execution_recovery_bundles
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Recovery Bundles are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_recovery_bundles_immutable_delete`,
		`CREATE TRIGGER trg_execution_recovery_bundles_immutable_delete
		 BEFORE DELETE ON execution_recovery_bundles
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Recovery Bundles are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_memory_revisions_insert`,
		`CREATE TRIGGER trg_agent_memory_revisions_insert
		 BEFORE INSERT ON agent_memory_revisions
		 BEGIN
		   SELECT RAISE(ABORT, 'Memory Revision shape is invalid')
		   WHERE NEW.revision_number IS NULL
		      OR NEW.revision_number <= 0
		      OR NEW.sha256 IS NULL
		      OR NEW.media_type IS NULL
		      OR NEW.media_type NOT IN ('text/plain', 'text/markdown', 'application/json')
		      OR NEW.size_bytes IS NULL
		      OR NEW.size_bytes < 0
		      OR NEW.size_bytes > 262144
		      OR length(NEW.sha256) <> 64
		      OR NEW.sha256 GLOB '*[^0-9a-f]*';

		   SELECT RAISE(ABORT, 'Memory Revision creator must reference an existing User')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM users AS user
		     WHERE user.id = NEW.created_by
		   );

		   SELECT RAISE(ABORT, 'Memory Revision must advance the locked Head by exactly one')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_memory_heads AS head
		     WHERE head.tenant_id = NEW.tenant_id
		       AND head.id = NEW.memory_head_id
		       AND NEW.revision_number = head.version + 1
		   );

		   SELECT RAISE(ABORT, 'Memory Revision requires an available ready Memory Artifact with the exact SHA-256, size, and base media type')
		   WHERE NOT EXISTS (
		     SELECT 1
		     FROM artifacts AS artifact
		     JOIN agent_memory_heads AS head
		       ON head.tenant_id = NEW.tenant_id
		      AND head.id = NEW.memory_head_id
		     WHERE artifact.tenant_id = NEW.tenant_id
		       AND artifact.id = NEW.artifact_id
		       AND artifact.kind = 'memory'
		       AND artifact.status = 'ready'
		       AND artifact.deleted_at IS NULL
		       AND artifact.sha256 = NEW.sha256
		       AND artifact.size_bytes = NEW.size_bytes
		       AND lower(trim(substr(ifnull(artifact.content_type, ''), 1, instr(ifnull(artifact.content_type, '') || ';', ';') - 1))) = NEW.media_type
		       AND (
		         (head.scope_type = 'user' AND artifact.created_by_type = 'user' AND artifact.created_by_id = head.scope_user_id)
		         OR (head.scope_type = 'project' AND artifact.project_id = head.scope_project_id)
		         OR (head.scope_type = 'session' AND artifact.session_id = head.scope_session_id)
		       )
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_memory_heads_validate_insert`,
		`CREATE TRIGGER trg_agent_memory_heads_validate_insert
		 BEFORE INSERT ON agent_memory_heads
		 BEGIN
		   SELECT RAISE(ABORT, 'Memory Head scope, key, or version shape is invalid')
		   WHERE NEW.scope_type IS NULL
		      OR NEW.version IS NULL
		      OR NEW.enabled IS NULL
		      OR NEW.scope_type NOT IN ('user', 'project', 'session')
		      OR NEW.memory_key IS NULL
		      OR length(NEW.memory_key) < 1
		      OR length(NEW.memory_key) > 160
		      OR substr(NEW.memory_key, 1, 1) NOT GLOB '[a-z]'
		      OR NEW.memory_key GLOB '*[^a-z0-9._-]*'
		      OR (
		        (NEW.scope_type = 'user' AND (NEW.scope_user_id IS NULL OR NEW.scope_project_id IS NOT NULL OR NEW.scope_session_id IS NOT NULL))
		        OR (NEW.scope_type = 'project' AND (NEW.scope_user_id IS NOT NULL OR NEW.scope_project_id IS NULL OR NEW.scope_session_id IS NOT NULL))
		        OR (NEW.scope_type = 'session' AND (NEW.scope_user_id IS NOT NULL OR NEW.scope_project_id IS NOT NULL OR NEW.scope_session_id IS NULL))
		      )
		      OR NEW.version < 0
		      OR (NEW.version = 0 AND (NEW.current_revision_id IS NOT NULL OR NEW.enabled <> 0))
		      OR (NEW.version > 0 AND NEW.current_revision_id IS NULL);

		   SELECT RAISE(ABORT, 'User Memory scope must reference an existing Tenant Membership')
		   WHERE NEW.scope_type = 'user'
		     AND NOT EXISTS (
		       SELECT 1 FROM tenant_memberships AS membership
		       WHERE membership.tenant_id = NEW.tenant_id
		         AND membership.user_id = NEW.scope_user_id
		     );

		   SELECT RAISE(ABORT, 'Project Memory scope must reference an existing Tenant Project')
		   WHERE NEW.scope_type = 'project'
		     AND NOT EXISTS (
		       SELECT 1 FROM projects AS project
		       WHERE project.tenant_id = NEW.tenant_id
		         AND project.id = NEW.scope_project_id
		     );

		   SELECT RAISE(ABORT, 'Session Memory scope must reference an existing Tenant Session')
		   WHERE NEW.scope_type = 'session'
		     AND NOT EXISTS (
		       SELECT 1 FROM agent_sessions AS session
		       WHERE session.tenant_id = NEW.tenant_id
		         AND session.id = NEW.scope_session_id
		     );

		   SELECT RAISE(ABORT, 'Memory Head current Revision must belong to the same Head and version')
		   WHERE NEW.current_revision_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1 FROM agent_memory_revisions AS revision
		       WHERE revision.tenant_id = NEW.tenant_id
		         AND revision.id = NEW.current_revision_id
		         AND revision.memory_head_id = NEW.id
		         AND revision.revision_number = NEW.version
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_memory_heads_validate_update`,
		`CREATE TRIGGER trg_agent_memory_heads_validate_update
		 BEFORE UPDATE ON agent_memory_heads
		 BEGIN
		   SELECT RAISE(ABORT, 'Memory Head scope, key, or version shape is invalid')
		   WHERE NEW.scope_type IS NULL
		      OR NEW.version IS NULL
		      OR NEW.enabled IS NULL
		      OR NEW.scope_type NOT IN ('user', 'project', 'session')
		      OR NEW.memory_key IS NULL
		      OR length(NEW.memory_key) < 1
		      OR length(NEW.memory_key) > 160
		      OR substr(NEW.memory_key, 1, 1) NOT GLOB '[a-z]'
		      OR NEW.memory_key GLOB '*[^a-z0-9._-]*'
		      OR NEW.version < 0
		      OR (NEW.version = 0 AND (NEW.current_revision_id IS NOT NULL OR NEW.enabled <> 0))
		      OR (NEW.version > 0 AND NEW.current_revision_id IS NULL);

		   SELECT RAISE(ABORT, 'Memory Head identity and scope are immutable')
		   WHERE NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.scope_type IS NOT OLD.scope_type
		      OR NEW.scope_user_id IS NOT OLD.scope_user_id
		      OR NEW.scope_project_id IS NOT OLD.scope_project_id
		      OR NEW.scope_session_id IS NOT OLD.scope_session_id
		      OR NEW.memory_key IS NOT OLD.memory_key
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'Memory Head publication must advance exactly one Revision')
		   WHERE NEW.current_revision_id IS NOT OLD.current_revision_id
		     AND (
		       NEW.current_revision_id IS NULL
		       OR NEW.version <> OLD.version + 1
		       OR NOT EXISTS (
		         SELECT 1 FROM agent_memory_revisions AS revision
		         WHERE revision.tenant_id = NEW.tenant_id
		           AND revision.id = NEW.current_revision_id
		           AND revision.memory_head_id = NEW.id
		           AND revision.revision_number = NEW.version
		       )
		     );

		   SELECT RAISE(ABORT, 'Memory Head version cannot change without publishing a Revision')
		   WHERE NEW.current_revision_id IS OLD.current_revision_id
		     AND NEW.version <> OLD.version;

		   SELECT RAISE(ABORT, 'Enabled Memory Head requires a current Revision')
		   WHERE NEW.enabled = 1 AND NEW.current_revision_id IS NULL;
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_memory_revisions_immutable_update`,
		`CREATE TRIGGER trg_agent_memory_revisions_immutable_update
		 BEFORE UPDATE ON agent_memory_revisions
		 BEGIN
		   SELECT RAISE(ABORT, 'Agent Memory Revisions are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_memory_revisions_immutable_delete`,
		`CREATE TRIGGER trg_agent_memory_revisions_immutable_delete
		 BEFORE DELETE ON agent_memory_revisions
		 BEGIN
		   SELECT RAISE(ABORT, 'Agent Memory Revisions are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_artifacts_agent_memory_protected`,
		`CREATE TRIGGER trg_artifacts_agent_memory_protected
		 BEFORE UPDATE OF kind, status, deleted_at, sha256, content_type, size_bytes, object_key ON artifacts
		 WHEN EXISTS (
		   SELECT 1 FROM agent_memory_revisions AS revision
		   WHERE revision.tenant_id = OLD.tenant_id
		     AND revision.artifact_id = OLD.id
		 ) AND (
		   NEW.kind <> 'memory'
		   OR NEW.status <> 'ready'
		   OR NEW.deleted_at IS NOT NULL
		   OR NEW.sha256 IS NOT OLD.sha256
		   OR NEW.content_type IS NOT OLD.content_type
		   OR NEW.size_bytes IS NOT OLD.size_bytes
		   OR NEW.object_key IS NOT OLD.object_key
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Artifact is pinned by an immutable Agent Memory Revision');
		 END`,
		`DROP TRIGGER IF EXISTS trg_artifacts_agent_memory_protected_delete`,
		`CREATE TRIGGER trg_artifacts_agent_memory_protected_delete
		 BEFORE DELETE ON artifacts
		 WHEN EXISTS (
		   SELECT 1 FROM agent_memory_revisions AS revision
		   WHERE revision.tenant_id = OLD.tenant_id
		     AND revision.artifact_id = OLD.id
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Artifact is pinned by an immutable Agent Memory Revision');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_lifecycle_policy_immutable`,
		`CREATE TRIGGER trg_agent_sessions_lifecycle_policy_immutable
		 BEFORE UPDATE OF waiting_keep_alive_seconds, suspend_after_idle_seconds,
		   absolute_session_lifetime_seconds, workspace_retention_days, warm_pool_mode
		 ON agent_sessions
		 WHEN NEW.waiting_keep_alive_seconds IS NOT OLD.waiting_keep_alive_seconds
		   OR NEW.suspend_after_idle_seconds IS NOT OLD.suspend_after_idle_seconds
		   OR NEW.absolute_session_lifetime_seconds IS NOT OLD.absolute_session_lifetime_seconds
		   OR NEW.workspace_retention_days IS NOT OLD.workspace_retention_days
		   OR NEW.warm_pool_mode IS NOT OLD.warm_pool_mode
		 BEGIN
		   SELECT RAISE(ABORT, 'Session Resource Lifecycle Policy snapshot is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_resource_lifecycle_validate_insert`,
		`CREATE TRIGGER trg_agent_sessions_resource_lifecycle_validate_insert
		 BEFORE INSERT ON agent_sessions
		 WHEN NEW.created_at IS NULL
		   OR NEW.last_event_sequence IS NULL
		   OR NEW.meaningful_activity_sequence IS NULL
		   OR NEW.resource_state IS NULL
		   OR NEW.meaningful_activity_at IS NULL
		   OR julianday(NEW.created_at) IS NULL
		   OR julianday(NEW.meaningful_activity_at) IS NULL
		   OR NEW.waiting_keep_alive_seconds IS NULL
		   OR NEW.suspend_after_idle_seconds IS NULL
		   OR NEW.workspace_retention_days IS NULL
		   OR NEW.warm_pool_mode IS NULL
		   OR NEW.resource_state NOT IN ('idle', 'provisioning', 'active', 'waiting', 'checkpointing', 'suspended', 'restoring', 'terminating')
		   OR NEW.waiting_keep_alive_seconds NOT BETWEEN 60 AND 86400
		   OR NEW.suspend_after_idle_seconds NOT BETWEEN 60 AND 604800
		   OR (NEW.absolute_session_lifetime_seconds IS NOT NULL AND NEW.absolute_session_lifetime_seconds NOT BETWEEN 3600 AND 31536000)
		   OR NEW.workspace_retention_days NOT BETWEEN 1 AND 3650
		   OR NEW.warm_pool_mode NOT IN ('disabled', 'balanced', 'low-latency')
		   OR NEW.last_event_sequence < 0
		   OR NEW.meaningful_activity_sequence < 0
		   OR NEW.meaningful_activity_sequence > NEW.last_event_sequence
		   OR (NEW.resource_idle_since IS NOT NULL AND (
		     julianday(NEW.resource_idle_since) IS NULL
		     OR julianday(NEW.resource_idle_since) < julianday(NEW.created_at)
		   ))
		   OR (NEW.absolute_expires_at IS NOT NULL AND (
		     julianday(NEW.absolute_expires_at) IS NULL
		     OR julianday(NEW.absolute_expires_at) <= julianday(NEW.created_at)
		   ))
		 BEGIN
		   SELECT RAISE(ABORT, 'Session Resource Lifecycle fields are invalid');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_resource_lifecycle_validate_update`,
		`CREATE TRIGGER trg_agent_sessions_resource_lifecycle_validate_update
		 BEFORE UPDATE OF resource_state, meaningful_activity_at, meaningful_activity_sequence,
		   resource_idle_since, absolute_expires_at, created_at, last_event_sequence,
		   waiting_keep_alive_seconds, suspend_after_idle_seconds, absolute_session_lifetime_seconds,
		   workspace_retention_days, warm_pool_mode ON agent_sessions
		 WHEN NEW.created_at IS NULL
		   OR NEW.last_event_sequence IS NULL
		   OR NEW.meaningful_activity_sequence IS NULL
		   OR NEW.resource_state IS NULL
		   OR NEW.meaningful_activity_at IS NULL
		   OR julianday(NEW.created_at) IS NULL
		   OR julianday(NEW.meaningful_activity_at) IS NULL
		   OR NEW.waiting_keep_alive_seconds IS NULL
		   OR NEW.suspend_after_idle_seconds IS NULL
		   OR NEW.workspace_retention_days IS NULL
		   OR NEW.warm_pool_mode IS NULL
		   OR NEW.resource_state NOT IN ('idle', 'provisioning', 'active', 'waiting', 'checkpointing', 'suspended', 'restoring', 'terminating')
		   OR NEW.waiting_keep_alive_seconds NOT BETWEEN 60 AND 86400
		   OR NEW.suspend_after_idle_seconds NOT BETWEEN 60 AND 604800
		   OR (NEW.absolute_session_lifetime_seconds IS NOT NULL AND NEW.absolute_session_lifetime_seconds NOT BETWEEN 3600 AND 31536000)
		   OR NEW.workspace_retention_days NOT BETWEEN 1 AND 3650
		   OR NEW.warm_pool_mode NOT IN ('disabled', 'balanced', 'low-latency')
		   OR NEW.last_event_sequence < 0
		   OR NEW.meaningful_activity_sequence < 0
		   OR NEW.meaningful_activity_sequence > NEW.last_event_sequence
		   OR (NEW.resource_idle_since IS NOT NULL AND (
		     julianday(NEW.resource_idle_since) IS NULL
		     OR julianday(NEW.resource_idle_since) < julianday(NEW.created_at)
		   ))
		   OR (NEW.absolute_expires_at IS NOT NULL AND (
		     julianday(NEW.absolute_expires_at) IS NULL
		     OR julianday(NEW.absolute_expires_at) <= julianday(NEW.created_at)
		   ))
		 BEGIN
		   SELECT RAISE(ABORT, 'Session Resource Lifecycle fields are invalid');
		 END`,
		`DROP TRIGGER IF EXISTS trg_tenant_resource_lifecycle_policy_validate_insert`,
		`CREATE TRIGGER trg_tenant_resource_lifecycle_policy_validate_insert
		 BEFORE INSERT ON tenant_resource_lifecycle_policies
		 WHEN NEW.version IS NULL
		   OR (NEW.waiting_keep_alive_seconds IS NOT NULL AND NEW.waiting_keep_alive_seconds NOT BETWEEN 60 AND 86400)
		   OR (NEW.suspend_after_idle_seconds IS NOT NULL AND NEW.suspend_after_idle_seconds NOT BETWEEN 60 AND 604800)
		   OR (NEW.absolute_session_lifetime_seconds IS NOT NULL AND NEW.absolute_session_lifetime_seconds NOT BETWEEN 3600 AND 31536000)
		   OR (NEW.workspace_retention_days IS NOT NULL AND NEW.workspace_retention_days NOT BETWEEN 1 AND 3650)
		   OR (NEW.warm_pool_mode IS NOT NULL AND NEW.warm_pool_mode NOT IN ('disabled', 'balanced', 'low-latency'))
		   OR NEW.version <= 0
		 BEGIN
		   SELECT RAISE(ABORT, 'Tenant Resource Lifecycle Policy is invalid');
		 END`,
		`DROP TRIGGER IF EXISTS trg_tenant_resource_lifecycle_policy_parent_insert`,
		`CREATE TRIGGER trg_tenant_resource_lifecycle_policy_parent_insert
		 BEFORE INSERT ON tenant_resource_lifecycle_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'Tenant Resource Lifecycle Policy must reference an existing Tenant')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM tenants AS tenant
		     WHERE tenant.id = NEW.tenant_id
		   );

		   SELECT RAISE(ABORT, 'Tenant Resource Lifecycle Policy updater must reference an existing User')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM users AS user
		     WHERE user.id = NEW.updated_by
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_tenant_resource_lifecycle_policy_validate_update`,
		`CREATE TRIGGER trg_tenant_resource_lifecycle_policy_validate_update
		 BEFORE UPDATE ON tenant_resource_lifecycle_policies
		 WHEN NEW.version IS NULL
		   OR (NEW.waiting_keep_alive_seconds IS NOT NULL AND NEW.waiting_keep_alive_seconds NOT BETWEEN 60 AND 86400)
		   OR (NEW.suspend_after_idle_seconds IS NOT NULL AND NEW.suspend_after_idle_seconds NOT BETWEEN 60 AND 604800)
		   OR (NEW.absolute_session_lifetime_seconds IS NOT NULL AND NEW.absolute_session_lifetime_seconds NOT BETWEEN 3600 AND 31536000)
		   OR (NEW.workspace_retention_days IS NOT NULL AND NEW.workspace_retention_days NOT BETWEEN 1 AND 3650)
		   OR (NEW.warm_pool_mode IS NOT NULL AND NEW.warm_pool_mode NOT IN ('disabled', 'balanced', 'low-latency'))
		   OR NEW.version <= 0
		 BEGIN
		   SELECT RAISE(ABORT, 'Tenant Resource Lifecycle Policy is invalid');
		 END`,
		`DROP TRIGGER IF EXISTS trg_tenant_resource_lifecycle_policy_parent_update`,
		`CREATE TRIGGER trg_tenant_resource_lifecycle_policy_parent_update
		 BEFORE UPDATE ON tenant_resource_lifecycle_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'Tenant Resource Lifecycle Policy must reference an existing Tenant')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM tenants AS tenant
		     WHERE tenant.id = NEW.tenant_id
		   );

		   SELECT RAISE(ABORT, 'Tenant Resource Lifecycle Policy updater must reference an existing User')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM users AS user
		     WHERE user.id = NEW.updated_by
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_project_resource_lifecycle_policy_validate_insert`,
		`CREATE TRIGGER trg_project_resource_lifecycle_policy_validate_insert
		 BEFORE INSERT ON project_resource_lifecycle_policies
		 WHEN NEW.version IS NULL
		   OR (NEW.waiting_keep_alive_seconds IS NOT NULL AND NEW.waiting_keep_alive_seconds NOT BETWEEN 60 AND 86400)
		   OR (NEW.suspend_after_idle_seconds IS NOT NULL AND NEW.suspend_after_idle_seconds NOT BETWEEN 60 AND 604800)
		   OR (NEW.absolute_session_lifetime_seconds IS NOT NULL AND NEW.absolute_session_lifetime_seconds NOT BETWEEN 3600 AND 31536000)
		   OR (NEW.workspace_retention_days IS NOT NULL AND NEW.workspace_retention_days NOT BETWEEN 1 AND 3650)
		   OR (NEW.warm_pool_mode IS NOT NULL AND NEW.warm_pool_mode NOT IN ('disabled', 'balanced', 'low-latency'))
		   OR NEW.version <= 0
		 BEGIN
		   SELECT RAISE(ABORT, 'Project Resource Lifecycle Policy is invalid');
		 END`,
		`DROP TRIGGER IF EXISTS trg_project_resource_lifecycle_policy_parent_insert`,
		`CREATE TRIGGER trg_project_resource_lifecycle_policy_parent_insert
		 BEFORE INSERT ON project_resource_lifecycle_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'Project Resource Lifecycle Policy must reference an existing Tenant Project')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM projects AS project
		     WHERE project.tenant_id = NEW.tenant_id
		       AND project.id = NEW.project_id
		   );

		   SELECT RAISE(ABORT, 'Project Resource Lifecycle Policy updater must reference an existing User')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM users AS user
		     WHERE user.id = NEW.updated_by
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_project_resource_lifecycle_policy_validate_update`,
		`CREATE TRIGGER trg_project_resource_lifecycle_policy_validate_update
		 BEFORE UPDATE ON project_resource_lifecycle_policies
		 WHEN NEW.version IS NULL
		   OR (NEW.waiting_keep_alive_seconds IS NOT NULL AND NEW.waiting_keep_alive_seconds NOT BETWEEN 60 AND 86400)
		   OR (NEW.suspend_after_idle_seconds IS NOT NULL AND NEW.suspend_after_idle_seconds NOT BETWEEN 60 AND 604800)
		   OR (NEW.absolute_session_lifetime_seconds IS NOT NULL AND NEW.absolute_session_lifetime_seconds NOT BETWEEN 3600 AND 31536000)
		   OR (NEW.workspace_retention_days IS NOT NULL AND NEW.workspace_retention_days NOT BETWEEN 1 AND 3650)
		   OR (NEW.warm_pool_mode IS NOT NULL AND NEW.warm_pool_mode NOT IN ('disabled', 'balanced', 'low-latency'))
		   OR NEW.version <= 0
		 BEGIN
		   SELECT RAISE(ABORT, 'Project Resource Lifecycle Policy is invalid');
		 END`,
		`DROP TRIGGER IF EXISTS trg_project_resource_lifecycle_policy_parent_update`,
		`CREATE TRIGGER trg_project_resource_lifecycle_policy_parent_update
		 BEFORE UPDATE ON project_resource_lifecycle_policies
		 BEGIN
		   SELECT RAISE(ABORT, 'Project Resource Lifecycle Policy must reference an existing Tenant Project')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM projects AS project
		     WHERE project.tenant_id = NEW.tenant_id
		       AND project.id = NEW.project_id
		   );

		   SELECT RAISE(ABORT, 'Project Resource Lifecycle Policy updater must reference an existing User')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM users AS user
		     WHERE user.id = NEW.updated_by
		   );
		 END`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_interactions_request
		 ON execution_interactions (tenant_id, execution_id, request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_interactions_resume_unbound
		 ON execution_interactions (tenant_id, execution_id, resolved_at, id)
		 WHERE status = 'resolved' AND delivery_status = 'resume-recorded'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_control_commands_primary_operation
		 ON execution_control_commands (tenant_id, execution_id)
		 WHERE command_type IN ('CompactSession', 'RollbackSession', 'ForkSession', 'StartReview')`,
		`DROP TRIGGER IF EXISTS trg_agent_turns_advanced_shape_insert`,
		`CREATE TRIGGER trg_agent_turns_advanced_shape_insert
		 BEFORE INSERT ON agent_turns
		 WHEN NEW.turn_kind IS NULL
		   OR NEW.turn_kind NOT IN ('message', 'compact', 'review', 'rollback', 'fork')
		   OR NEW.input_text IS NULL
		   OR (NEW.turn_kind = 'message' AND length(NEW.input_text) NOT BETWEEN 1 AND 1000000)
		   OR (NEW.turn_kind <> 'message' AND NEW.input_text <> '')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid advanced Turn kind or input shape');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_turns_advanced_shape_update`,
		`CREATE TRIGGER trg_agent_turns_advanced_shape_update
		 BEFORE UPDATE OF turn_kind, input_text ON agent_turns
		 WHEN NEW.turn_kind IS NOT OLD.turn_kind
		   OR NEW.turn_kind IS NULL
		   OR NEW.turn_kind NOT IN ('message', 'compact', 'review', 'rollback', 'fork')
		   OR NEW.input_text IS NULL
		   OR (NEW.turn_kind = 'message' AND length(NEW.input_text) NOT BETWEEN 1 AND 1000000)
		   OR (NEW.turn_kind <> 'message' AND NEW.input_text <> '')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid or mutable advanced Turn kind');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_fork_lineage_insert`,
		`CREATE TRIGGER trg_agent_sessions_fork_lineage_insert
		 BEFORE INSERT ON agent_sessions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Fork lineage shape')
		   WHERE NOT (
		     (
		       NEW.fork_source_session_id IS NULL
		       AND NEW.fork_source_turn_id IS NULL
		       AND NEW.fork_source_event_sequence IS NULL
		       AND NEW.fork_strategy IS NULL
		     )
		     OR
		     (
		       NEW.fork_source_session_id IS NOT NULL
		       AND NEW.fork_source_event_sequence IS NOT NULL
		       AND NEW.fork_source_event_sequence >= 0
		       AND NEW.fork_strategy IS NOT NULL
		       AND NEW.fork_strategy IN ('emulated', 'native')
		       AND NEW.last_event_sequence IS NOT NULL
		       AND NEW.last_event_sequence >= NEW.fork_source_event_sequence
		       AND (NEW.fork_source_event_sequence > 0 OR NEW.fork_source_turn_id IS NULL)
		       AND NEW.fork_source_session_id <> NEW.id
		     )
		   );

		   SELECT RAISE(ABORT, 'Fork source Session is missing or outside the tenant Project')
		   WHERE NEW.fork_source_session_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1
		       FROM agent_sessions AS source
		       WHERE source.tenant_id = NEW.tenant_id
		         AND source.project_id = NEW.project_id
		         AND source.id = NEW.fork_source_session_id
		         AND NEW.fork_source_event_sequence <= source.last_event_sequence
		     );

		   SELECT RAISE(ABORT, 'Fork lineage cannot contain a Session cycle')
		   WHERE NEW.fork_source_session_id IS NOT NULL
		     AND EXISTS (
		       WITH RECURSIVE source_lineage(session_id, source_session_id, path, cycle) AS (
		         SELECT
		           source.id,
		           source.fork_source_session_id,
		           ',' || NEW.id || ',' || source.id || ',',
		           source.id = NEW.id
		         FROM agent_sessions AS source
		         WHERE source.tenant_id = NEW.tenant_id
		           AND source.project_id = NEW.project_id
		           AND source.id = NEW.fork_source_session_id

		         UNION ALL

		         SELECT
		           parent.id,
		           parent.fork_source_session_id,
		           lineage.path || parent.id || ',',
		           instr(lineage.path, ',' || parent.id || ',') > 0
		         FROM source_lineage AS lineage
		         JOIN agent_sessions AS parent
		           ON parent.tenant_id = NEW.tenant_id
		          AND parent.project_id = NEW.project_id
		          AND parent.id = lineage.source_session_id
		         WHERE NOT lineage.cycle
		       )
		       SELECT 1 FROM source_lineage WHERE cycle LIMIT 1
		     );

		   SELECT RAISE(ABORT, 'Fork source Turn is outside the selected logical history prefix')
		   WHERE NEW.fork_source_turn_id IS NOT NULL
		     AND NOT EXISTS (
		       WITH RECURSIVE source_lineage(session_id, through_sequence, depth, path) AS (
		         SELECT
		           source.id,
		           NEW.fork_source_event_sequence,
		           0,
		           ',' || source.id || ','
		         FROM agent_sessions AS source
		         WHERE source.tenant_id = NEW.tenant_id
		           AND source.id = NEW.fork_source_session_id

		         UNION ALL

		         SELECT
		           parent.id,
		           min(lineage.through_sequence, child.fork_source_event_sequence),
		           lineage.depth + 1,
		           lineage.path || parent.id || ','
		         FROM source_lineage AS lineage
		         JOIN agent_sessions AS child
		           ON child.tenant_id = NEW.tenant_id
		          AND child.id = lineage.session_id
		         JOIN agent_sessions AS parent
		           ON parent.tenant_id = NEW.tenant_id
		          AND parent.id = child.fork_source_session_id
		         WHERE child.fork_source_session_id IS NOT NULL
		           AND child.fork_source_event_sequence IS NOT NULL
		           AND lineage.depth + 1 < 32
		           AND instr(lineage.path, ',' || parent.id || ',') = 0
		       )
		       SELECT 1
		       FROM source_lineage AS lineage
		       JOIN session_events AS source_event
		         ON source_event.tenant_id = NEW.tenant_id
		        AND source_event.session_id = lineage.session_id
		        AND source_event.sequence <= lineage.through_sequence
		       WHERE source_event.event_type = 'turn.created'
		         AND json_valid(source_event.payload)
		         AND json_extract(source_event.payload, '$.turnId') = NEW.fork_source_turn_id
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_fork_lineage_immutable`,
		`CREATE TRIGGER trg_agent_sessions_fork_lineage_immutable
		 BEFORE UPDATE OF fork_source_session_id, fork_source_turn_id, fork_source_event_sequence, fork_strategy
		 ON agent_sessions
		 WHEN NEW.fork_source_session_id IS NOT OLD.fork_source_session_id
		   OR NEW.fork_source_turn_id IS NOT OLD.fork_source_turn_id
		   OR NEW.fork_source_event_sequence IS NOT OLD.fork_source_event_sequence
		   OR NEW.fork_strategy IS NOT OLD.fork_strategy
		 BEGIN
		   SELECT RAISE(ABORT, 'Fork lineage is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_control_commands_primary_kind_insert`,
		`CREATE TRIGGER trg_execution_control_commands_primary_kind_insert
		 BEFORE INSERT ON execution_control_commands
		 WHEN NEW.command_type IN ('CompactSession', 'RollbackSession', 'ForkSession', 'StartReview')
		   AND NOT EXISTS (
		     SELECT 1
		     FROM agent_executions AS execution
		     JOIN agent_turns AS turn
		       ON turn.tenant_id = execution.tenant_id
		      AND turn.session_id = execution.session_id
		      AND turn.id = execution.turn_id
		     WHERE execution.tenant_id = NEW.tenant_id
		       AND execution.id = NEW.execution_id
		       AND execution.session_id = NEW.session_id
		       AND execution.turn_id = NEW.turn_id
		       AND turn.turn_kind = CASE NEW.command_type
		         WHEN 'CompactSession' THEN 'compact'
		         WHEN 'RollbackSession' THEN 'rollback'
		         WHEN 'ForkSession' THEN 'fork'
		         WHEN 'StartReview' THEN 'review'
		       END
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Primary Control command does not match the Execution Turn kind');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_control_commands_primary_kind_update`,
		`CREATE TRIGGER trg_execution_control_commands_primary_kind_update
		 BEFORE UPDATE OF tenant_id, execution_id, session_id, turn_id, command_type
		 ON execution_control_commands
		 WHEN NEW.command_type IN ('CompactSession', 'RollbackSession', 'ForkSession', 'StartReview')
		   AND NOT EXISTS (
		     SELECT 1
		     FROM agent_executions AS execution
		     JOIN agent_turns AS turn
		       ON turn.tenant_id = execution.tenant_id
		      AND turn.session_id = execution.session_id
		      AND turn.id = execution.turn_id
		     WHERE execution.tenant_id = NEW.tenant_id
		       AND execution.id = NEW.execution_id
		       AND execution.session_id = NEW.session_id
		       AND execution.turn_id = NEW.turn_id
		       AND turn.turn_kind = CASE NEW.command_type
		         WHEN 'CompactSession' THEN 'compact'
		         WHEN 'RollbackSession' THEN 'rollback'
		         WHEN 'ForkSession' THEN 'fork'
		         WHEN 'StartReview' THEN 'review'
		       END
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Primary Control command does not match the Execution Turn kind');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_control_commands_primary_preserved_update`,
		`CREATE TRIGGER trg_execution_control_commands_primary_preserved_update
		 BEFORE UPDATE OF tenant_id, execution_id, session_id, turn_id, command_type
		 ON execution_control_commands
		 WHEN OLD.command_type IN ('CompactSession', 'RollbackSession', 'ForkSession', 'StartReview')
		   AND (
		     NEW.tenant_id IS NOT OLD.tenant_id
		     OR NEW.execution_id IS NOT OLD.execution_id
		     OR NEW.session_id IS NOT OLD.session_id
		     OR NEW.turn_id IS NOT OLD.turn_id
		     OR NEW.command_type IS NOT OLD.command_type
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Special Turn Execution must retain its matching primary Control command');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_control_commands_primary_preserved_delete`,
		`CREATE TRIGGER trg_execution_control_commands_primary_preserved_delete
		 BEFORE DELETE ON execution_control_commands
		 WHEN OLD.command_type IN ('CompactSession', 'RollbackSession', 'ForkSession', 'StartReview')
		   AND EXISTS (
		     SELECT 1
		     FROM agent_executions AS execution
		     JOIN agent_turns AS turn
		       ON turn.tenant_id = execution.tenant_id
		      AND turn.session_id = execution.session_id
		      AND turn.id = execution.turn_id
		     WHERE execution.tenant_id = OLD.tenant_id
		       AND execution.id = OLD.execution_id
		       AND execution.session_id = OLD.session_id
		       AND execution.turn_id = OLD.turn_id
		       AND turn.turn_kind = CASE OLD.command_type
		         WHEN 'CompactSession' THEN 'compact'
		         WHEN 'RollbackSession' THEN 'rollback'
		         WHEN 'ForkSession' THEN 'fork'
		         WHEN 'StartReview' THEN 'review'
		       END
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Special Turn Execution must retain its matching primary Control command');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_control_commands_cascade`,
		`CREATE TRIGGER trg_agent_executions_control_commands_cascade
		 AFTER DELETE ON agent_executions
		 BEGIN
		   DELETE FROM execution_control_commands
		   WHERE tenant_id = OLD.tenant_id AND execution_id = OLD.id;
		 END`,
		`UPDATE worker_leases
		 SET worker_incarnation = (
		       SELECT worker.incarnation FROM worker_instances AS worker WHERE worker.id = worker_leases.worker_id
		     ),
		     worker_instance_uid = (
		       SELECT worker.instance_uid FROM worker_instances AS worker WHERE worker.id = worker_leases.worker_id
		     )
		 WHERE (worker_incarnation <= 0 OR worker_instance_uid = '')
		   AND EXISTS (
		   SELECT 1 FROM worker_instances AS worker
		   WHERE worker.id = worker_leases.worker_id
		 )`,
		`DROP TRIGGER IF EXISTS trg_worker_leases_matches_execution_insert`,
		`CREATE TRIGGER trg_worker_leases_matches_execution_insert
		 BEFORE INSERT ON worker_leases
		 BEGIN
		   SELECT RAISE(ABORT, 'worker lease does not match the current execution generation')
		   WHERE NOT EXISTS (
		     SELECT 1
		     FROM agent_executions AS execution
		     WHERE execution.tenant_id = NEW.tenant_id
		       AND execution.id = NEW.execution_id
		       AND execution.worker_id = NEW.worker_id
		       AND execution.generation = NEW.generation
		       AND execution.status IN ('leased', 'running', 'waiting-for-approval')
		       AND NEW.worker_incarnation > 0
		       AND length(NEW.worker_instance_uid) = 36
		       AND EXISTS (
		         SELECT 1 FROM worker_instances AS worker
		         WHERE worker.id = NEW.worker_id
		           AND worker.incarnation = NEW.worker_incarnation
		           AND worker.instance_uid = NEW.worker_instance_uid
		       )
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_leases_matches_execution_update`,
		`CREATE TRIGGER trg_worker_leases_matches_execution_update
		 BEFORE UPDATE OF tenant_id, execution_id, worker_id, worker_incarnation, worker_instance_uid, generation ON worker_leases
		 BEGIN
		   SELECT RAISE(ABORT, 'worker lease does not match the current execution generation')
		   WHERE NOT EXISTS (
		     SELECT 1
		     FROM agent_executions AS execution
		     WHERE execution.tenant_id = NEW.tenant_id
		       AND execution.id = NEW.execution_id
		       AND execution.worker_id = NEW.worker_id
		       AND execution.generation = NEW.generation
		       AND execution.status IN ('leased', 'running', 'waiting-for-approval')
		       AND NEW.worker_incarnation > 0
		       AND length(NEW.worker_instance_uid) = 36
		       AND EXISTS (
		         SELECT 1 FROM worker_instances AS worker
		         WHERE worker.id = NEW.worker_id
		           AND worker.incarnation = NEW.worker_incarnation
		           AND worker.instance_uid = NEW.worker_instance_uid
		       )
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_leases_incarnation_immutable`,
		`CREATE TRIGGER trg_worker_leases_incarnation_immutable
		 BEFORE UPDATE OF worker_id, worker_incarnation, worker_instance_uid ON worker_leases
		 WHEN NEW.worker_id IS NOT OLD.worker_id
		   OR NEW.worker_incarnation IS NOT OLD.worker_incarnation
		   OR NEW.worker_instance_uid IS NOT OLD.worker_instance_uid
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker lease incarnation lineage is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_leases_provider_credential_access_insert`,
		`CREATE TRIGGER trg_worker_leases_provider_credential_access_insert
		 BEFORE INSERT ON worker_leases
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential access fields are invalid')
		   WHERE NOT (
		     (
		       NEW.provider_credential_grant_id IS NULL
		       AND NEW.provider_credential_access_serial IS NULL
		       AND NEW.provider_credential_activity_sequence IS NULL
		       AND NEW.provider_credential_activity_at IS NULL
		       AND NEW.provider_credential_access_issued_at IS NULL
		       AND NEW.provider_credential_access_renewed_at IS NULL
		       AND NEW.provider_credential_access_expires_at IS NULL
		       AND NEW.provider_credential_refresh_deadline_at IS NULL
		       AND NEW.provider_credential_hard_expires_at IS NULL
		     ) OR (
		       NEW.provider_credential_grant_id IS NOT NULL
		       AND NEW.provider_credential_access_serial IS NOT NULL
		       AND NEW.provider_credential_activity_sequence IS NOT NULL
		       AND NEW.provider_credential_activity_at IS NOT NULL
		       AND NEW.provider_credential_access_issued_at IS NOT NULL
		       AND NEW.provider_credential_access_renewed_at IS NOT NULL
		       AND NEW.provider_credential_access_expires_at IS NOT NULL
		       AND NEW.provider_credential_refresh_deadline_at IS NOT NULL
		       AND NEW.provider_credential_access_serial > 0
		       AND NEW.provider_credential_activity_sequence >= 0
		       AND julianday(NEW.provider_credential_activity_at) IS NOT NULL
		       AND julianday(NEW.provider_credential_access_issued_at) IS NOT NULL
		       AND julianday(NEW.provider_credential_access_renewed_at) IS NOT NULL
		       AND julianday(NEW.provider_credential_access_expires_at) IS NOT NULL
		       AND julianday(NEW.provider_credential_refresh_deadline_at) IS NOT NULL
		       AND (NEW.provider_credential_hard_expires_at IS NULL
		         OR julianday(NEW.provider_credential_hard_expires_at) IS NOT NULL)
		       AND julianday(NEW.provider_credential_access_renewed_at) >= julianday(NEW.provider_credential_access_issued_at)
		       AND julianday(NEW.provider_credential_access_expires_at) > julianday(NEW.provider_credential_access_renewed_at)
		       AND julianday(NEW.provider_credential_refresh_deadline_at) >= julianday(NEW.provider_credential_activity_at)
		       AND (
		         NEW.provider_credential_hard_expires_at IS NULL
		         OR julianday(NEW.provider_credential_access_expires_at) <= julianday(NEW.provider_credential_hard_expires_at)
		       )
		     )
		   );
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential access must match its frozen Execution Provider Credential Grant')
		   WHERE NEW.provider_credential_grant_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1
		       FROM execution_provider_credential_grants AS grant
		       WHERE grant.tenant_id = NEW.tenant_id
		         AND grant.id = NEW.provider_credential_grant_id
		         AND grant.execution_id = NEW.execution_id
		         AND grant.generation = NEW.generation
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_leases_provider_credential_access_update`,
		`CREATE TRIGGER trg_worker_leases_provider_credential_access_update
		 BEFORE UPDATE OF tenant_id, execution_id, generation,
		   provider_credential_grant_id, provider_credential_access_serial,
		   provider_credential_activity_sequence, provider_credential_activity_at,
		   provider_credential_access_issued_at, provider_credential_access_renewed_at,
		   provider_credential_access_expires_at, provider_credential_refresh_deadline_at,
		   provider_credential_hard_expires_at ON worker_leases
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential Grant is immutable')
		   WHERE OLD.provider_credential_grant_id IS NOT NULL
		     AND NEW.provider_credential_grant_id IS NOT OLD.provider_credential_grant_id;
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential access issued-at is immutable')
		   WHERE OLD.provider_credential_grant_id IS NOT NULL
		     AND NEW.provider_credential_access_issued_at IS NOT OLD.provider_credential_access_issued_at;
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential access fields are invalid')
		   WHERE NOT (
		     (
		       NEW.provider_credential_grant_id IS NULL
		       AND NEW.provider_credential_access_serial IS NULL
		       AND NEW.provider_credential_activity_sequence IS NULL
		       AND NEW.provider_credential_activity_at IS NULL
		       AND NEW.provider_credential_access_issued_at IS NULL
		       AND NEW.provider_credential_access_renewed_at IS NULL
		       AND NEW.provider_credential_access_expires_at IS NULL
		       AND NEW.provider_credential_refresh_deadline_at IS NULL
		       AND NEW.provider_credential_hard_expires_at IS NULL
		     ) OR (
		       NEW.provider_credential_grant_id IS NOT NULL
		       AND NEW.provider_credential_access_serial IS NOT NULL
		       AND NEW.provider_credential_activity_sequence IS NOT NULL
		       AND NEW.provider_credential_activity_at IS NOT NULL
		       AND NEW.provider_credential_access_issued_at IS NOT NULL
		       AND NEW.provider_credential_access_renewed_at IS NOT NULL
		       AND NEW.provider_credential_access_expires_at IS NOT NULL
		       AND NEW.provider_credential_refresh_deadline_at IS NOT NULL
		       AND NEW.provider_credential_access_serial > 0
		       AND NEW.provider_credential_activity_sequence >= 0
		       AND julianday(NEW.provider_credential_activity_at) IS NOT NULL
		       AND julianday(NEW.provider_credential_access_issued_at) IS NOT NULL
		       AND julianday(NEW.provider_credential_access_renewed_at) IS NOT NULL
		       AND julianday(NEW.provider_credential_access_expires_at) IS NOT NULL
		       AND julianday(NEW.provider_credential_refresh_deadline_at) IS NOT NULL
		       AND (NEW.provider_credential_hard_expires_at IS NULL
		         OR julianday(NEW.provider_credential_hard_expires_at) IS NOT NULL)
		       AND julianday(NEW.provider_credential_access_renewed_at) >= julianday(NEW.provider_credential_access_issued_at)
		       AND julianday(NEW.provider_credential_access_expires_at) > julianday(NEW.provider_credential_access_renewed_at)
		       AND julianday(NEW.provider_credential_refresh_deadline_at) >= julianday(NEW.provider_credential_activity_at)
		       AND (
		         NEW.provider_credential_hard_expires_at IS NULL
		         OR julianday(NEW.provider_credential_access_expires_at) <= julianday(NEW.provider_credential_hard_expires_at)
		       )
		     )
		   );
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential access must match its frozen Execution Provider Credential Grant')
		   WHERE NEW.provider_credential_grant_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1
		       FROM execution_provider_credential_grants AS grant
		       WHERE grant.tenant_id = NEW.tenant_id
		         AND grant.id = NEW.provider_credential_grant_id
		         AND grant.execution_id = NEW.execution_id
		         AND grant.generation = NEW.generation
		     );
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential access serial must advance exactly once per state update')
		   WHERE OLD.provider_credential_grant_id IS NOT NULL
		     AND (
		       (
		         (
		           NEW.provider_credential_activity_sequence IS NOT OLD.provider_credential_activity_sequence
		           OR NEW.provider_credential_activity_at IS NOT OLD.provider_credential_activity_at
		           OR NEW.provider_credential_access_renewed_at IS NOT OLD.provider_credential_access_renewed_at
		           OR NEW.provider_credential_access_expires_at IS NOT OLD.provider_credential_access_expires_at
		           OR NEW.provider_credential_refresh_deadline_at IS NOT OLD.provider_credential_refresh_deadline_at
		           OR NEW.provider_credential_hard_expires_at IS NOT OLD.provider_credential_hard_expires_at
		         )
		         AND NEW.provider_credential_access_serial <> OLD.provider_credential_access_serial + 1
		       ) OR (
		         NEW.provider_credential_access_serial IS NOT OLD.provider_credential_access_serial
		         AND NEW.provider_credential_activity_sequence IS OLD.provider_credential_activity_sequence
		         AND NEW.provider_credential_activity_at IS OLD.provider_credential_activity_at
		         AND NEW.provider_credential_access_renewed_at IS OLD.provider_credential_access_renewed_at
		         AND NEW.provider_credential_access_expires_at IS OLD.provider_credential_access_expires_at
		         AND NEW.provider_credential_refresh_deadline_at IS OLD.provider_credential_refresh_deadline_at
		         AND NEW.provider_credential_hard_expires_at IS OLD.provider_credential_hard_expires_at
		       )
		     );
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential activity sequence cannot go backwards')
		   WHERE OLD.provider_credential_grant_id IS NOT NULL
		     AND NEW.provider_credential_activity_sequence < OLD.provider_credential_activity_sequence;
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential activity_at must stay frozen without a newer activity sequence')
		   WHERE OLD.provider_credential_grant_id IS NOT NULL
		     AND NEW.provider_credential_activity_sequence = OLD.provider_credential_activity_sequence
		     AND NEW.provider_credential_activity_at IS NOT OLD.provider_credential_activity_at;
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential activity_at cannot go backwards')
		   WHERE OLD.provider_credential_grant_id IS NOT NULL
		     AND NEW.provider_credential_activity_sequence > OLD.provider_credential_activity_sequence
		     AND julianday(NEW.provider_credential_activity_at) < julianday(OLD.provider_credential_activity_at);
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential hard expiry cannot be extended')
		   WHERE OLD.provider_credential_grant_id IS NOT NULL
		     AND OLD.provider_credential_hard_expires_at IS NOT NULL
		     AND (
		       NEW.provider_credential_hard_expires_at IS NULL
		       OR julianday(NEW.provider_credential_hard_expires_at) > julianday(OLD.provider_credential_hard_expires_at)
		     );
		   SELECT RAISE(ABORT, 'Worker lease Provider Credential access renewal windows cannot move backwards')
		   WHERE OLD.provider_credential_grant_id IS NOT NULL
		     AND (
		       julianday(NEW.provider_credential_access_renewed_at) < julianday(OLD.provider_credential_access_renewed_at)
		       OR julianday(NEW.provider_credential_access_expires_at) < julianday(OLD.provider_credential_access_expires_at)
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_resource_suspend_shape_insert`,
		`CREATE TRIGGER trg_agent_executions_resource_suspend_shape_insert
		 BEFORE INSERT ON agent_executions
		 WHEN (NEW.status = 'suspended' AND (
		     NEW.worker_id IS NOT NULL
		     OR NEW.next_recovery_reason IS NOT 'suspend-resume'
		     OR EXISTS (SELECT 1 FROM worker_leases AS lease WHERE lease.tenant_id = NEW.tenant_id AND lease.execution_id = NEW.id)
		   ))
		   OR (NEW.next_recovery_reason IS NOT NULL AND NOT (
		     (NEW.next_recovery_reason = 'suspend-resume' AND NEW.status IN ('suspended', 'recovering'))
		     OR (NEW.next_recovery_reason = 'disaster-recovery' AND NEW.status IN ('queued', 'recovering'))
		   ))
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid suspended Execution shape');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_resource_suspend_shape_update`,
		`CREATE TRIGGER trg_agent_executions_resource_suspend_shape_update
		 BEFORE UPDATE OF status, worker_id, next_recovery_reason ON agent_executions
		 WHEN (NEW.status = 'suspended' AND (
		     NEW.worker_id IS NOT NULL
		     OR NEW.next_recovery_reason IS NOT 'suspend-resume'
		     OR EXISTS (SELECT 1 FROM worker_leases AS lease WHERE lease.tenant_id = NEW.tenant_id AND lease.execution_id = NEW.id)
		   ))
		   OR (NEW.next_recovery_reason IS NOT NULL AND NOT (
		     (NEW.next_recovery_reason = 'suspend-resume' AND NEW.status IN ('suspended', 'recovering'))
		     OR (NEW.next_recovery_reason = 'disaster-recovery' AND NEW.status IN ('queued', 'recovering'))
		   ))
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid suspended Execution shape');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_suspend_attempts_shape_insert`,
		`CREATE TRIGGER trg_execution_suspend_attempts_shape_insert
		 BEFORE INSERT ON execution_suspend_attempts
		 WHEN NEW.reason IS NULL
		   OR NEW.status IS NULL
		   OR NEW.completion_mode IS NULL
		   OR NEW.generation IS NULL
		   OR NEW.requested_at IS NULL
		   OR NEW.checkpoint_deadline_at IS NULL
		   OR NEW.reason NOT IN ('waiting-keepalive', 'active-idle-timeout')
		   OR NEW.status NOT IN ('checkpointing', 'completed', 'aborted', 'superseded')
		   OR NEW.completion_mode NOT IN ('worker-attested-v1', 'kubernetes-pod-terminal-v1')
		   OR NEW.generation <= 0
		   OR NEW.checkpoint_deadline_at <= NEW.requested_at
		   OR (NEW.provider_quiesced_at IS NOT NULL AND julianday(NEW.provider_quiesced_at) < julianday(NEW.requested_at))
		   OR (NEW.provider_quiesced_at IS NOT NULL AND julianday(NEW.provider_quiesced_at) > julianday(NEW.checkpoint_deadline_at))
		   OR (NEW.failure_code IS NOT NULL AND length(NEW.failure_code) NOT BETWEEN 1 AND 160)
		   OR (NEW.failure_message IS NOT NULL AND length(NEW.failure_message) > 2000)
		   OR NEW.resume_bundle_id IS NOT NULL
		   OR NEW.resume_generation IS NOT NULL
		   OR (NEW.resume_bound_at IS NOT NULL AND julianday(NEW.resume_bound_at) IS NULL)
		   OR NEW.resume_bound_at IS NOT NULL
		   OR (NEW.resume_outcome_unknown_at IS NOT NULL AND julianday(NEW.resume_outcome_unknown_at) IS NULL)
		   OR NEW.resume_outcome_unknown_at IS NOT NULL
		   OR (NEW.reason = 'waiting-keepalive' AND (
		     NEW.control_command_id IS NOT NULL
		     OR NEW.boundary_meaningful_activity_sequence IS NOT NULL
		     OR NEW.active_command_id IS NOT NULL
		     OR NEW.checkpoint_history_sequence IS NOT NULL
		     OR NEW.current_turn_sequence IS NOT NULL
		     OR NEW.provider_runtime_binding_id IS NOT NULL
		     OR NEW.provider_checkpoint_protocol IS NOT NULL
		     OR NEW.provider_cursor_source_execution_id IS NOT NULL
		     OR NEW.provider_cursor_source_generation IS NOT NULL
		     OR NEW.provider_cursor_history_sequence IS NOT NULL
		     OR NEW.provider_cursor_binding_version IS NOT NULL
		     OR NEW.provider_cursor_binding_digest IS NOT NULL
		     OR NEW.provider_cursor_sha256 IS NOT NULL
		     OR NEW.checkpoint_receipt_sha256 IS NOT NULL
		     OR NEW.resume_bundle_id IS NOT NULL
		     OR NEW.resume_generation IS NOT NULL
		     OR NEW.resume_bound_at IS NOT NULL
		     OR NEW.resume_outcome_unknown_at IS NOT NULL
		   ))
		   OR (NEW.reason = 'active-idle-timeout' AND (
		     NEW.control_command_id IS NULL
		     OR NEW.boundary_meaningful_activity_sequence IS NULL
		     OR NEW.boundary_meaningful_activity_sequence <= 0
		     OR (
		       NEW.provider_quiesced_at IS NULL AND (
		         NEW.active_command_id IS NOT NULL
		         OR NEW.checkpoint_history_sequence IS NOT NULL
		         OR NEW.current_turn_sequence IS NOT NULL
		         OR NEW.provider_runtime_binding_id IS NOT NULL
		         OR NEW.provider_checkpoint_protocol IS NOT NULL
		         OR NEW.provider_cursor_source_execution_id IS NOT NULL
		         OR NEW.provider_cursor_source_generation IS NOT NULL
		         OR NEW.provider_cursor_history_sequence IS NOT NULL
		         OR NEW.provider_cursor_binding_version IS NOT NULL
		         OR NEW.provider_cursor_binding_digest IS NOT NULL
		         OR NEW.provider_cursor_sha256 IS NOT NULL
		         OR NEW.checkpoint_receipt_sha256 IS NOT NULL
		       )
		     )
		     OR (
		       NEW.provider_quiesced_at IS NOT NULL AND (
		         NEW.active_command_id IS NULL
		         OR length(NEW.active_command_id) NOT BETWEEN 1 AND 240
		         OR NEW.checkpoint_history_sequence IS NULL
		         OR NEW.checkpoint_history_sequence <= 0
		         OR NEW.current_turn_sequence IS NULL
		         OR NEW.current_turn_sequence <= 0
		         OR NEW.provider_runtime_binding_id IS NULL
		         OR NEW.provider_checkpoint_protocol IS NULL
		         OR NEW.provider_checkpoint_protocol <> 'provider-host-suspend-terminal-v1'
		         OR NEW.provider_cursor_source_execution_id IS NULL
		         OR NEW.provider_cursor_source_execution_id IS NOT NEW.execution_id
		         OR NEW.provider_cursor_source_generation IS NULL
		         OR NEW.provider_cursor_source_generation <= 0
		         OR NEW.provider_cursor_source_generation IS NOT NEW.generation
		         OR NEW.provider_cursor_history_sequence IS NULL
		         OR NEW.provider_cursor_history_sequence <= 0
		         OR NEW.provider_cursor_history_sequence IS NOT NEW.checkpoint_history_sequence
		         OR NEW.provider_cursor_binding_version IS NULL
		         OR NEW.provider_cursor_binding_version <= 0
		         OR NEW.provider_cursor_binding_digest IS NULL
		         OR length(NEW.provider_cursor_binding_digest) <> 32
		         OR NEW.provider_cursor_sha256 IS NULL
		         OR length(NEW.provider_cursor_sha256) <> 32
		         OR NEW.checkpoint_receipt_sha256 IS NULL
		         OR length(NEW.checkpoint_receipt_sha256) <> 32
		       )
		     )
		   ))
		   OR ((NEW.resume_bundle_id IS NULL) <> (NEW.resume_generation IS NULL))
		   OR ((NEW.resume_bundle_id IS NULL) <> (NEW.resume_bound_at IS NULL))
		   OR (NEW.resume_bundle_id IS NOT NULL AND (
		     NEW.reason <> 'active-idle-timeout'
		     OR NEW.status <> 'completed'
		     OR NEW.resume_generation <= NEW.generation
		   ))
		   OR (NEW.resume_outcome_unknown_at IS NOT NULL AND (
		     NEW.resume_bundle_id IS NULL
		     OR NEW.resume_bound_at IS NULL
		     OR julianday(NEW.resume_outcome_unknown_at) < julianday(NEW.resume_bound_at)
		   ))
		   OR (
		     (NEW.checkpoint_status IS NULL AND NEW.checkpoint_ready_at IS NOT NULL)
		     OR (NEW.checkpoint_status IS NOT NULL AND NEW.checkpoint_ready_at IS NULL)
		   )
		   OR (NEW.checkpoint_status IS NOT NULL AND NEW.checkpoint_status NOT IN ('ready', 'unchanged'))
		   OR (NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) IS NULL)
		   OR (NEW.checkpoint_ready_at IS NOT NULL AND NEW.provider_quiesced_at IS NULL)
		   OR (NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) < julianday(NEW.provider_quiesced_at))
		   OR (NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) > julianday(NEW.checkpoint_deadline_at))
		   OR (NEW.completed_at IS NOT NULL AND NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) > julianday(NEW.completed_at))
		   OR (NEW.aborted_at IS NOT NULL AND NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) > julianday(NEW.aborted_at))
		   OR (
		     (NEW.pod_terminal_phase IS NULL AND NEW.pod_terminal_observed_at IS NOT NULL)
		     OR (NEW.pod_terminal_phase IS NOT NULL AND NEW.pod_terminal_observed_at IS NULL)
		   )
		   OR (NEW.pod_terminal_phase IS NOT NULL AND NEW.pod_terminal_phase <> 'Succeeded')
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND julianday(NEW.pod_terminal_observed_at) IS NULL)
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND NEW.completion_mode <> 'kubernetes-pod-terminal-v1')
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND NEW.status <> 'completed')
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND NEW.provider_quiesced_at IS NULL)
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND NEW.checkpoint_ready_at IS NULL)
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND julianday(NEW.pod_terminal_observed_at) < julianday(NEW.provider_quiesced_at))
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND julianday(NEW.pod_terminal_observed_at) < julianday(NEW.checkpoint_ready_at))
		   OR (NEW.completed_at IS NOT NULL AND NEW.pod_terminal_observed_at IS NOT NULL AND julianday(NEW.pod_terminal_observed_at) > julianday(NEW.completed_at))
		   OR (
		     NEW.completion_mode = 'kubernetes-pod-terminal-v1' AND (
		       NEW.execution_target_id IS NULL
		       OR NEW.execution_target_id = '00000000-0000-0000-0000-000000000000'
		       OR NEW.worker_incarnation IS NULL
		       OR NEW.worker_incarnation <= 0
		       OR length(trim(ifnull(NEW.worker_instance_uid, ''))) <> 36
		       OR length(trim(ifnull(NEW.worker_cluster_id, ''))) NOT BETWEEN 1 AND 160
		       OR length(trim(ifnull(NEW.worker_namespace, ''))) NOT BETWEEN 1 AND 160
		       OR length(trim(ifnull(NEW.worker_pod_name, ''))) NOT BETWEEN 1 AND 253
		     )
		   )
		   OR (NEW.status = 'checkpointing' AND (
		     NEW.completed_at IS NOT NULL
		     OR NEW.aborted_at IS NOT NULL
		     OR NEW.failure_code IS NOT NULL
		     OR NEW.failure_message IS NOT NULL
		     OR NEW.pod_terminal_observed_at IS NOT NULL
		     OR NEW.pod_terminal_phase IS NOT NULL
		   ))
		   OR (NEW.status = 'completed' AND (
		     NEW.provider_quiesced_at IS NULL
		     OR NEW.completed_at IS NULL
		     OR julianday(NEW.provider_quiesced_at) > julianday(NEW.completed_at)
		     OR NEW.aborted_at IS NOT NULL
		     OR NEW.failure_code IS NOT NULL
		     OR NEW.failure_message IS NOT NULL
		     OR (
		       NEW.completion_mode = 'kubernetes-pod-terminal-v1'
		       AND (
		         NEW.checkpoint_status IS NULL
		         OR NEW.checkpoint_ready_at IS NULL
		         OR NEW.pod_terminal_observed_at IS NULL
		         OR NEW.pod_terminal_phase IS NULL
		       )
		     )
		   ))
		   OR (NEW.status IN ('aborted', 'superseded') AND (
		     NEW.completed_at IS NOT NULL
		     OR NEW.aborted_at IS NULL
		     OR NEW.failure_code IS NULL
		     OR (NEW.provider_quiesced_at IS NOT NULL AND julianday(NEW.provider_quiesced_at) > julianday(NEW.aborted_at))
		     OR NEW.pod_terminal_observed_at IS NOT NULL
		     OR NEW.pod_terminal_phase IS NOT NULL
		   ))
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution suspend attempt shape');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_suspend_attempts_lineage_insert`,
		`CREATE TRIGGER trg_execution_suspend_attempts_lineage_insert
		 BEFORE INSERT ON execution_suspend_attempts
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution suspend attempt must reference an existing Tenant Session Turn')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_turns AS turn
		     WHERE turn.tenant_id = NEW.tenant_id
		       AND turn.session_id = NEW.session_id
		       AND turn.id = NEW.turn_id
		   );

		   SELECT RAISE(ABORT, 'Execution suspend attempt must reference an existing Worker')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM worker_instances AS worker
		     WHERE worker.id = NEW.worker_id
		   );

		   SELECT RAISE(ABORT, 'Execution suspend attempt must match the current Tenant Execution generation and Worker owner')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_executions AS execution
		     WHERE execution.tenant_id = NEW.tenant_id
		       AND execution.id = NEW.execution_id
		       AND execution.session_id = NEW.session_id
		       AND execution.turn_id = NEW.turn_id
		       AND execution.generation = NEW.generation
			       AND execution.worker_id = NEW.worker_id
			   );

		   SELECT RAISE(ABORT, 'Active suspend attempt must freeze the Session meaningful activity sequence')
		   WHERE NEW.reason = 'active-idle-timeout'
		     AND NOT EXISTS (
		       SELECT 1 FROM agent_sessions AS session
		       WHERE session.tenant_id = NEW.tenant_id
		         AND session.id = NEW.session_id
		         AND session.meaningful_activity_sequence = NEW.boundary_meaningful_activity_sequence
		     );

		   SELECT RAISE(ABORT, 'Execution suspend attempt must reference an existing Provider Runtime binding')
		   WHERE NEW.provider_runtime_binding_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1 FROM provider_runtime_bindings AS binding
		       WHERE binding.tenant_id = NEW.tenant_id
		         AND binding.id = NEW.provider_runtime_binding_id
		     );

		   SELECT RAISE(ABORT, 'Active suspend attempt must reference the exact bound SuspendTurn Control command')
		   WHERE NEW.reason = 'active-idle-timeout'
		     AND NOT EXISTS (
		       SELECT 1
		       FROM execution_control_commands AS command
		       WHERE command.tenant_id = NEW.tenant_id
		         AND command.id = NEW.control_command_id
		         AND command.execution_id = NEW.execution_id
		         AND command.session_id = NEW.session_id
		         AND command.turn_id = NEW.turn_id
		         AND command.command_type = 'SuspendTurn'
		         AND command.delivery_worker_id = NEW.worker_id
		         AND command.delivery_generation = NEW.generation
		     );

		   SELECT RAISE(ABORT, 'Recovery-bound suspend attempt must reference a matching suspend-resume Recovery Bundle')
		   WHERE NEW.resume_bundle_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1
		       FROM execution_recovery_bundles AS bundle
		       WHERE bundle.tenant_id = NEW.tenant_id
		         AND bundle.id = NEW.resume_bundle_id
		         AND bundle.execution_id = NEW.execution_id
		         AND bundle.generation = NEW.resume_generation
		         AND bundle.recovery_reason = 'suspend-resume'
		     );

		   SELECT RAISE(ABORT, 'Kubernetes suspend attempt must capture the exact Pod-bound Worker snapshot')
		   WHERE NEW.completion_mode = 'kubernetes-pod-terminal-v1'
		     AND NOT EXISTS (
		       SELECT 1
		       FROM agent_executions AS execution
		       JOIN worker_instances AS worker
		         ON worker.id = NEW.worker_id
		       WHERE execution.tenant_id = NEW.tenant_id
		         AND execution.id = NEW.execution_id
		         AND execution.session_id = NEW.session_id
		         AND execution.turn_id = NEW.turn_id
		         AND execution.generation = NEW.generation
		         AND execution.worker_id = NEW.worker_id
		         AND execution.execution_target_id = NEW.execution_target_id
		         AND execution.target_kind = 'kubernetes'
		         AND worker.execution_target_id = NEW.execution_target_id
		         AND worker.target_kind = 'kubernetes'
		         AND worker.registration_trust_mode = 'kubernetes-pod-bound-v1'
		         AND worker.incarnation = NEW.worker_incarnation
		         AND worker.instance_uid = NEW.worker_instance_uid
		         AND worker.cluster_id = NEW.worker_cluster_id
		         AND worker.namespace = NEW.worker_namespace
		         AND worker.pod_name = NEW.worker_pod_name
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_suspend_attempts_shape_update`,
		`CREATE TRIGGER trg_execution_suspend_attempts_shape_update
		 BEFORE UPDATE ON execution_suspend_attempts
		 WHEN NEW.reason IS NULL
		   OR NEW.status IS NULL
		   OR NEW.completion_mode IS NULL
		   OR NEW.generation IS NULL
		   OR NEW.requested_at IS NULL
		   OR NEW.checkpoint_deadline_at IS NULL
		   OR NEW.reason NOT IN ('waiting-keepalive', 'active-idle-timeout')
		   OR NEW.status NOT IN ('checkpointing', 'completed', 'aborted', 'superseded')
		   OR NEW.completion_mode NOT IN ('worker-attested-v1', 'kubernetes-pod-terminal-v1')
		   OR NEW.generation <= 0
		   OR NEW.checkpoint_deadline_at <= NEW.requested_at
		   OR (NEW.provider_quiesced_at IS NOT NULL AND julianday(NEW.provider_quiesced_at) < julianday(NEW.requested_at))
		   OR (NEW.provider_quiesced_at IS NOT NULL AND julianday(NEW.provider_quiesced_at) > julianday(NEW.checkpoint_deadline_at))
		   OR (NEW.failure_code IS NOT NULL AND length(NEW.failure_code) NOT BETWEEN 1 AND 160)
		   OR (NEW.failure_message IS NOT NULL AND length(NEW.failure_message) > 2000)
		   OR (NEW.resume_bound_at IS NOT NULL AND julianday(NEW.resume_bound_at) IS NULL)
		   OR (NEW.resume_outcome_unknown_at IS NOT NULL AND julianday(NEW.resume_outcome_unknown_at) IS NULL)
		   OR (NEW.reason = 'waiting-keepalive' AND (
		     NEW.control_command_id IS NOT NULL
		     OR NEW.boundary_meaningful_activity_sequence IS NOT NULL
		     OR NEW.active_command_id IS NOT NULL
		     OR NEW.checkpoint_history_sequence IS NOT NULL
		     OR NEW.current_turn_sequence IS NOT NULL
		     OR NEW.provider_runtime_binding_id IS NOT NULL
		     OR NEW.provider_checkpoint_protocol IS NOT NULL
		     OR NEW.provider_cursor_source_execution_id IS NOT NULL
		     OR NEW.provider_cursor_source_generation IS NOT NULL
		     OR NEW.provider_cursor_history_sequence IS NOT NULL
		     OR NEW.provider_cursor_binding_version IS NOT NULL
		     OR NEW.provider_cursor_binding_digest IS NOT NULL
		     OR NEW.provider_cursor_sha256 IS NOT NULL
		     OR NEW.checkpoint_receipt_sha256 IS NOT NULL
		     OR NEW.resume_bundle_id IS NOT NULL
		     OR NEW.resume_generation IS NOT NULL
		     OR NEW.resume_bound_at IS NOT NULL
		     OR NEW.resume_outcome_unknown_at IS NOT NULL
		   ))
		   OR (NEW.reason = 'active-idle-timeout' AND (
		     NEW.control_command_id IS NULL
		     OR NEW.boundary_meaningful_activity_sequence IS NULL
		     OR NEW.boundary_meaningful_activity_sequence <= 0
		     OR (
		       NEW.provider_quiesced_at IS NULL AND (
		         NEW.active_command_id IS NOT NULL
		         OR NEW.checkpoint_history_sequence IS NOT NULL
		         OR NEW.current_turn_sequence IS NOT NULL
		         OR NEW.provider_runtime_binding_id IS NOT NULL
		         OR NEW.provider_checkpoint_protocol IS NOT NULL
		         OR NEW.provider_cursor_source_execution_id IS NOT NULL
		         OR NEW.provider_cursor_source_generation IS NOT NULL
		         OR NEW.provider_cursor_history_sequence IS NOT NULL
		         OR NEW.provider_cursor_binding_version IS NOT NULL
		         OR NEW.provider_cursor_binding_digest IS NOT NULL
		         OR NEW.provider_cursor_sha256 IS NOT NULL
		         OR NEW.checkpoint_receipt_sha256 IS NOT NULL
		       )
		     )
		     OR (
		       NEW.provider_quiesced_at IS NOT NULL AND (
		         NEW.active_command_id IS NULL
		         OR length(NEW.active_command_id) NOT BETWEEN 1 AND 240
		         OR NEW.checkpoint_history_sequence IS NULL
		         OR NEW.checkpoint_history_sequence <= 0
		         OR NEW.current_turn_sequence IS NULL
		         OR NEW.current_turn_sequence <= 0
		         OR NEW.provider_runtime_binding_id IS NULL
		         OR NEW.provider_checkpoint_protocol IS NULL
		         OR NEW.provider_checkpoint_protocol <> 'provider-host-suspend-terminal-v1'
		         OR NEW.provider_cursor_source_execution_id IS NULL
		         OR NEW.provider_cursor_source_execution_id IS NOT NEW.execution_id
		         OR NEW.provider_cursor_source_generation IS NULL
		         OR NEW.provider_cursor_source_generation <= 0
		         OR NEW.provider_cursor_source_generation IS NOT NEW.generation
		         OR NEW.provider_cursor_history_sequence IS NULL
		         OR NEW.provider_cursor_history_sequence <= 0
		         OR NEW.provider_cursor_history_sequence IS NOT NEW.checkpoint_history_sequence
		         OR NEW.provider_cursor_binding_version IS NULL
		         OR NEW.provider_cursor_binding_version <= 0
		         OR NEW.provider_cursor_binding_digest IS NULL
		         OR length(NEW.provider_cursor_binding_digest) <> 32
		         OR NEW.provider_cursor_sha256 IS NULL
		         OR length(NEW.provider_cursor_sha256) <> 32
		         OR NEW.checkpoint_receipt_sha256 IS NULL
		         OR length(NEW.checkpoint_receipt_sha256) <> 32
		       )
		     )
		   ))
		   OR ((NEW.resume_bundle_id IS NULL) <> (NEW.resume_generation IS NULL))
		   OR ((NEW.resume_bundle_id IS NULL) <> (NEW.resume_bound_at IS NULL))
		   OR (NEW.resume_bundle_id IS NOT NULL AND (
		     NEW.reason <> 'active-idle-timeout'
		     OR NEW.status <> 'completed'
		     OR NEW.resume_generation <= NEW.generation
		   ))
		   OR (NEW.resume_outcome_unknown_at IS NOT NULL AND (
		     NEW.resume_bundle_id IS NULL
		     OR NEW.resume_bound_at IS NULL
		     OR julianday(NEW.resume_outcome_unknown_at) < julianday(NEW.resume_bound_at)
		   ))
		   OR (
		     (NEW.checkpoint_status IS NULL AND NEW.checkpoint_ready_at IS NOT NULL)
		     OR (NEW.checkpoint_status IS NOT NULL AND NEW.checkpoint_ready_at IS NULL)
		   )
		   OR (NEW.checkpoint_status IS NOT NULL AND NEW.checkpoint_status NOT IN ('ready', 'unchanged'))
		   OR (NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) IS NULL)
		   OR (NEW.checkpoint_ready_at IS NOT NULL AND NEW.provider_quiesced_at IS NULL)
		   OR (NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) < julianday(NEW.provider_quiesced_at))
		   OR (NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) > julianday(NEW.checkpoint_deadline_at))
		   OR (NEW.completed_at IS NOT NULL AND NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) > julianday(NEW.completed_at))
		   OR (NEW.aborted_at IS NOT NULL AND NEW.checkpoint_ready_at IS NOT NULL AND julianday(NEW.checkpoint_ready_at) > julianday(NEW.aborted_at))
		   OR (
		     (NEW.pod_terminal_phase IS NULL AND NEW.pod_terminal_observed_at IS NOT NULL)
		     OR (NEW.pod_terminal_phase IS NOT NULL AND NEW.pod_terminal_observed_at IS NULL)
		   )
		   OR (NEW.pod_terminal_phase IS NOT NULL AND NEW.pod_terminal_phase <> 'Succeeded')
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND julianday(NEW.pod_terminal_observed_at) IS NULL)
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND NEW.completion_mode <> 'kubernetes-pod-terminal-v1')
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND NEW.status <> 'completed')
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND NEW.provider_quiesced_at IS NULL)
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND NEW.checkpoint_ready_at IS NULL)
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND julianday(NEW.pod_terminal_observed_at) < julianday(NEW.provider_quiesced_at))
		   OR (NEW.pod_terminal_observed_at IS NOT NULL AND julianday(NEW.pod_terminal_observed_at) < julianday(NEW.checkpoint_ready_at))
		   OR (NEW.completed_at IS NOT NULL AND NEW.pod_terminal_observed_at IS NOT NULL AND julianday(NEW.pod_terminal_observed_at) > julianday(NEW.completed_at))
		   OR (
		     NEW.completion_mode = 'kubernetes-pod-terminal-v1' AND (
		       NEW.execution_target_id IS NULL
		       OR NEW.execution_target_id = '00000000-0000-0000-0000-000000000000'
		       OR NEW.worker_incarnation IS NULL
		       OR NEW.worker_incarnation <= 0
		       OR length(trim(ifnull(NEW.worker_instance_uid, ''))) <> 36
		       OR length(trim(ifnull(NEW.worker_cluster_id, ''))) NOT BETWEEN 1 AND 160
		       OR length(trim(ifnull(NEW.worker_namespace, ''))) NOT BETWEEN 1 AND 160
		       OR length(trim(ifnull(NEW.worker_pod_name, ''))) NOT BETWEEN 1 AND 253
		     )
		   )
		   OR (NEW.status = 'checkpointing' AND (
		     NEW.completed_at IS NOT NULL
		     OR NEW.aborted_at IS NOT NULL
		     OR NEW.failure_code IS NOT NULL
		     OR NEW.failure_message IS NOT NULL
		     OR NEW.pod_terminal_observed_at IS NOT NULL
		     OR NEW.pod_terminal_phase IS NOT NULL
		   ))
		   OR (NEW.status = 'completed' AND (
		     NEW.provider_quiesced_at IS NULL
		     OR NEW.completed_at IS NULL
		     OR julianday(NEW.provider_quiesced_at) > julianday(NEW.completed_at)
		     OR NEW.aborted_at IS NOT NULL
		     OR NEW.failure_code IS NOT NULL
		     OR NEW.failure_message IS NOT NULL
		     OR (
		       NEW.completion_mode = 'kubernetes-pod-terminal-v1'
		       AND (
		         NEW.checkpoint_status IS NULL
		         OR NEW.checkpoint_ready_at IS NULL
		         OR NEW.pod_terminal_observed_at IS NULL
		         OR NEW.pod_terminal_phase IS NULL
		       )
		     )
		   ))
		   OR (NEW.status IN ('aborted', 'superseded') AND (
		     NEW.completed_at IS NOT NULL
		     OR NEW.aborted_at IS NULL
		     OR NEW.failure_code IS NULL
		     OR (NEW.provider_quiesced_at IS NOT NULL AND julianday(NEW.provider_quiesced_at) > julianday(NEW.aborted_at))
		     OR NEW.pod_terminal_observed_at IS NOT NULL
		     OR NEW.pod_terminal_phase IS NOT NULL
		   ))
		   OR NEW.id IS NOT OLD.id
		   OR NEW.tenant_id IS NOT OLD.tenant_id
		   OR NEW.session_id IS NOT OLD.session_id
		   OR NEW.turn_id IS NOT OLD.turn_id
		   OR NEW.execution_id IS NOT OLD.execution_id
		   OR NEW.worker_id IS NOT OLD.worker_id
		   OR NEW.execution_target_id IS NOT OLD.execution_target_id
		   OR NEW.generation IS NOT OLD.generation
		   OR NEW.reason IS NOT OLD.reason
		   OR NEW.completion_mode IS NOT OLD.completion_mode
		   OR NEW.control_command_id IS NOT OLD.control_command_id
		   OR NEW.boundary_meaningful_activity_sequence IS NOT OLD.boundary_meaningful_activity_sequence
		   OR NEW.worker_incarnation IS NOT OLD.worker_incarnation
		   OR NEW.worker_instance_uid IS NOT OLD.worker_instance_uid
		   OR NEW.worker_cluster_id IS NOT OLD.worker_cluster_id
		   OR NEW.worker_namespace IS NOT OLD.worker_namespace
		   OR NEW.worker_pod_name IS NOT OLD.worker_pod_name
		   OR NEW.requested_at IS NOT OLD.requested_at
		   OR NEW.checkpoint_deadline_at IS NOT OLD.checkpoint_deadline_at
		   OR (OLD.provider_quiesced_at IS NOT NULL AND NEW.provider_quiesced_at IS NOT OLD.provider_quiesced_at)
		   OR ((OLD.checkpoint_status IS NOT NULL OR OLD.checkpoint_ready_at IS NOT NULL) AND (
		         NEW.checkpoint_status IS NOT OLD.checkpoint_status
		         OR NEW.checkpoint_ready_at IS NOT OLD.checkpoint_ready_at
		       ))
		   OR ((OLD.pod_terminal_phase IS NOT NULL OR OLD.pod_terminal_observed_at IS NOT NULL) AND (
		         NEW.pod_terminal_phase IS NOT OLD.pod_terminal_phase
		         OR NEW.pod_terminal_observed_at IS NOT OLD.pod_terminal_observed_at
		       ))
		   OR ((OLD.active_command_id IS NOT NULL
		         OR OLD.checkpoint_history_sequence IS NOT NULL
		         OR OLD.current_turn_sequence IS NOT NULL
		         OR OLD.provider_runtime_binding_id IS NOT NULL
		         OR OLD.provider_checkpoint_protocol IS NOT NULL
		         OR OLD.provider_cursor_source_execution_id IS NOT NULL
		         OR OLD.provider_cursor_source_generation IS NOT NULL
		         OR OLD.provider_cursor_history_sequence IS NOT NULL
		         OR OLD.provider_cursor_binding_version IS NOT NULL
		         OR OLD.provider_cursor_binding_digest IS NOT NULL
		         OR OLD.provider_cursor_sha256 IS NOT NULL
		         OR OLD.checkpoint_receipt_sha256 IS NOT NULL) AND (
		         NEW.active_command_id IS NOT OLD.active_command_id
		         OR NEW.checkpoint_history_sequence IS NOT OLD.checkpoint_history_sequence
		         OR NEW.current_turn_sequence IS NOT OLD.current_turn_sequence
		         OR NEW.provider_runtime_binding_id IS NOT OLD.provider_runtime_binding_id
		         OR NEW.provider_checkpoint_protocol IS NOT OLD.provider_checkpoint_protocol
		         OR NEW.provider_cursor_source_execution_id IS NOT OLD.provider_cursor_source_execution_id
		         OR NEW.provider_cursor_source_generation IS NOT OLD.provider_cursor_source_generation
		         OR NEW.provider_cursor_history_sequence IS NOT OLD.provider_cursor_history_sequence
		         OR NEW.provider_cursor_binding_version IS NOT OLD.provider_cursor_binding_version
		         OR NEW.provider_cursor_binding_digest IS NOT OLD.provider_cursor_binding_digest
		         OR NEW.provider_cursor_sha256 IS NOT OLD.provider_cursor_sha256
		         OR NEW.checkpoint_receipt_sha256 IS NOT OLD.checkpoint_receipt_sha256
		       ))
		   OR ((OLD.resume_bundle_id IS NOT NULL OR OLD.resume_generation IS NOT NULL OR OLD.resume_bound_at IS NOT NULL) AND (
		         NEW.resume_bundle_id IS NOT OLD.resume_bundle_id
		         OR NEW.resume_generation IS NOT OLD.resume_generation
		         OR NEW.resume_bound_at IS NOT OLD.resume_bound_at
		       ))
		   OR (OLD.resume_outcome_unknown_at IS NOT NULL AND NEW.resume_outcome_unknown_at IS NOT OLD.resume_outcome_unknown_at)
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution suspend attempt shape');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_suspend_attempts_finished_update`,
		`CREATE TRIGGER trg_execution_suspend_attempts_finished_update
		 BEFORE UPDATE ON execution_suspend_attempts
		 WHEN OLD.status IN ('completed', 'aborted', 'superseded')
		   AND (
		     NEW.status IS NOT OLD.status
		     OR NEW.provider_quiesced_at IS NOT OLD.provider_quiesced_at
		     OR NEW.active_command_id IS NOT OLD.active_command_id
		     OR NEW.checkpoint_history_sequence IS NOT OLD.checkpoint_history_sequence
		     OR NEW.current_turn_sequence IS NOT OLD.current_turn_sequence
		     OR NEW.provider_runtime_binding_id IS NOT OLD.provider_runtime_binding_id
		     OR NEW.provider_checkpoint_protocol IS NOT OLD.provider_checkpoint_protocol
		     OR NEW.provider_cursor_source_execution_id IS NOT OLD.provider_cursor_source_execution_id
		     OR NEW.provider_cursor_source_generation IS NOT OLD.provider_cursor_source_generation
		     OR NEW.provider_cursor_history_sequence IS NOT OLD.provider_cursor_history_sequence
		     OR NEW.provider_cursor_binding_version IS NOT OLD.provider_cursor_binding_version
		     OR NEW.provider_cursor_binding_digest IS NOT OLD.provider_cursor_binding_digest
		     OR NEW.provider_cursor_sha256 IS NOT OLD.provider_cursor_sha256
		     OR NEW.checkpoint_receipt_sha256 IS NOT OLD.checkpoint_receipt_sha256
		     OR NEW.checkpoint_status IS NOT OLD.checkpoint_status
		     OR NEW.checkpoint_ready_at IS NOT OLD.checkpoint_ready_at
		     OR NEW.pod_terminal_observed_at IS NOT OLD.pod_terminal_observed_at
		     OR NEW.pod_terminal_phase IS NOT OLD.pod_terminal_phase
		     OR NEW.resume_bundle_id IS NOT OLD.resume_bundle_id
		     OR NEW.resume_generation IS NOT OLD.resume_generation
		     OR NEW.resume_bound_at IS NOT OLD.resume_bound_at
		     OR NEW.resume_outcome_unknown_at IS NOT OLD.resume_outcome_unknown_at
		     OR NEW.completed_at IS NOT OLD.completed_at
		     OR NEW.aborted_at IS NOT OLD.aborted_at
		     OR NEW.failure_code IS NOT OLD.failure_code
		     OR NEW.failure_message IS NOT OLD.failure_message
		   )
		   AND NOT (
		     OLD.reason = 'active-idle-timeout'
		     AND OLD.status = 'completed'
		     AND OLD.resume_bundle_id IS NULL
		     AND OLD.resume_generation IS NULL
		     AND OLD.resume_bound_at IS NULL
		     AND OLD.resume_outcome_unknown_at IS NULL
		     AND NEW.status IS OLD.status
		     AND NEW.provider_quiesced_at IS OLD.provider_quiesced_at
		     AND NEW.active_command_id IS OLD.active_command_id
		     AND NEW.checkpoint_history_sequence IS OLD.checkpoint_history_sequence
		     AND NEW.current_turn_sequence IS OLD.current_turn_sequence
		     AND NEW.provider_runtime_binding_id IS OLD.provider_runtime_binding_id
		     AND NEW.provider_checkpoint_protocol IS OLD.provider_checkpoint_protocol
		     AND NEW.provider_cursor_source_execution_id IS OLD.provider_cursor_source_execution_id
		     AND NEW.provider_cursor_source_generation IS OLD.provider_cursor_source_generation
		     AND NEW.provider_cursor_history_sequence IS OLD.provider_cursor_history_sequence
		     AND NEW.provider_cursor_binding_version IS OLD.provider_cursor_binding_version
		     AND NEW.provider_cursor_binding_digest IS OLD.provider_cursor_binding_digest
		     AND NEW.provider_cursor_sha256 IS OLD.provider_cursor_sha256
		     AND NEW.checkpoint_receipt_sha256 IS OLD.checkpoint_receipt_sha256
		     AND NEW.checkpoint_status IS OLD.checkpoint_status
		     AND NEW.checkpoint_ready_at IS OLD.checkpoint_ready_at
		     AND NEW.pod_terminal_observed_at IS OLD.pod_terminal_observed_at
		     AND NEW.pod_terminal_phase IS OLD.pod_terminal_phase
		     AND NEW.resume_bundle_id IS NOT NULL
		     AND NEW.resume_generation IS NOT NULL
		     AND NEW.resume_bound_at IS NOT NULL
		     AND NEW.resume_outcome_unknown_at IS NULL
		     AND NEW.completed_at IS OLD.completed_at
		     AND NEW.aborted_at IS OLD.aborted_at
		     AND NEW.failure_code IS OLD.failure_code
		     AND NEW.failure_message IS OLD.failure_message
		   )
		   AND NOT (
		     OLD.reason = 'active-idle-timeout'
		     AND OLD.status = 'completed'
		     AND OLD.resume_bundle_id IS NOT NULL
		     AND OLD.resume_generation IS NOT NULL
		     AND OLD.resume_bound_at IS NOT NULL
		     AND OLD.resume_outcome_unknown_at IS NULL
		     AND NEW.status IS OLD.status
		     AND NEW.provider_quiesced_at IS OLD.provider_quiesced_at
		     AND NEW.active_command_id IS OLD.active_command_id
		     AND NEW.checkpoint_history_sequence IS OLD.checkpoint_history_sequence
		     AND NEW.current_turn_sequence IS OLD.current_turn_sequence
		     AND NEW.provider_runtime_binding_id IS OLD.provider_runtime_binding_id
		     AND NEW.provider_checkpoint_protocol IS OLD.provider_checkpoint_protocol
		     AND NEW.provider_cursor_source_execution_id IS OLD.provider_cursor_source_execution_id
		     AND NEW.provider_cursor_source_generation IS OLD.provider_cursor_source_generation
		     AND NEW.provider_cursor_history_sequence IS OLD.provider_cursor_history_sequence
		     AND NEW.provider_cursor_binding_version IS OLD.provider_cursor_binding_version
		     AND NEW.provider_cursor_binding_digest IS OLD.provider_cursor_binding_digest
		     AND NEW.provider_cursor_sha256 IS OLD.provider_cursor_sha256
		     AND NEW.checkpoint_receipt_sha256 IS OLD.checkpoint_receipt_sha256
		     AND NEW.checkpoint_status IS OLD.checkpoint_status
		     AND NEW.checkpoint_ready_at IS OLD.checkpoint_ready_at
		     AND NEW.pod_terminal_observed_at IS OLD.pod_terminal_observed_at
		     AND NEW.pod_terminal_phase IS OLD.pod_terminal_phase
		     AND NEW.resume_bundle_id IS OLD.resume_bundle_id
		     AND NEW.resume_generation IS OLD.resume_generation
		     AND NEW.resume_bound_at IS OLD.resume_bound_at
		     AND NEW.resume_outcome_unknown_at IS NOT NULL
		     AND NEW.completed_at IS OLD.completed_at
		     AND NEW.aborted_at IS OLD.aborted_at
		     AND NEW.failure_code IS OLD.failure_code
		     AND NEW.failure_message IS OLD.failure_message
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Finished Execution suspend attempts are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_suspend_attempts_lineage_update`,
		`CREATE TRIGGER trg_execution_suspend_attempts_lineage_update
		 BEFORE UPDATE OF tenant_id, session_id, turn_id, execution_id, worker_id, execution_target_id,
		   generation, reason, completion_mode, control_command_id, boundary_meaningful_activity_sequence, provider_runtime_binding_id,
		   worker_incarnation, worker_instance_uid, worker_cluster_id,
		   worker_namespace, worker_pod_name
		 ON execution_suspend_attempts
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution suspend attempt must reference an existing Tenant Session Turn')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_turns AS turn
		     WHERE turn.tenant_id = NEW.tenant_id
		       AND turn.session_id = NEW.session_id
		       AND turn.id = NEW.turn_id
		   );

		   SELECT RAISE(ABORT, 'Execution suspend attempt must reference an existing Worker')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM worker_instances AS worker
		     WHERE worker.id = NEW.worker_id
		   );

		   SELECT RAISE(ABORT, 'Execution suspend attempt must match the current Tenant Execution generation and Worker owner')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_executions AS execution
		     WHERE execution.tenant_id = NEW.tenant_id
		       AND execution.id = NEW.execution_id
		       AND execution.session_id = NEW.session_id
		       AND execution.turn_id = NEW.turn_id
		       AND execution.generation = NEW.generation
		       AND execution.worker_id = NEW.worker_id
		   );

		   SELECT RAISE(ABORT, 'Execution suspend attempt must reference an existing Provider Runtime binding')
		   WHERE NEW.provider_runtime_binding_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1 FROM provider_runtime_bindings AS binding
		       WHERE binding.tenant_id = NEW.tenant_id
		         AND binding.id = NEW.provider_runtime_binding_id
		     );

		   SELECT RAISE(ABORT, 'Active suspend attempt must reference the exact bound SuspendTurn Control command')
		   WHERE NEW.reason = 'active-idle-timeout'
		     AND NOT EXISTS (
		       SELECT 1
		       FROM execution_control_commands AS command
		       WHERE command.tenant_id = NEW.tenant_id
		         AND command.id = NEW.control_command_id
		         AND command.execution_id = NEW.execution_id
		         AND command.session_id = NEW.session_id
		         AND command.turn_id = NEW.turn_id
		         AND command.command_type = 'SuspendTurn'
		         AND command.delivery_worker_id = NEW.worker_id
		         AND command.delivery_generation = NEW.generation
		     );

		   SELECT RAISE(ABORT, 'Kubernetes suspend attempt must capture the exact Pod-bound Worker snapshot')
		   WHERE NEW.completion_mode = 'kubernetes-pod-terminal-v1'
		     AND NOT EXISTS (
		       SELECT 1
		       FROM agent_executions AS execution
		       JOIN worker_instances AS worker
		         ON worker.id = NEW.worker_id
		       WHERE execution.tenant_id = NEW.tenant_id
		         AND execution.id = NEW.execution_id
		         AND execution.session_id = NEW.session_id
		         AND execution.turn_id = NEW.turn_id
		         AND execution.generation = NEW.generation
		         AND execution.worker_id = NEW.worker_id
		         AND execution.execution_target_id = NEW.execution_target_id
		         AND execution.target_kind = 'kubernetes'
		         AND worker.execution_target_id = NEW.execution_target_id
		         AND worker.target_kind = 'kubernetes'
		         AND worker.registration_trust_mode = 'kubernetes-pod-bound-v1'
		         AND worker.incarnation = NEW.worker_incarnation
		         AND worker.instance_uid = NEW.worker_instance_uid
		         AND worker.cluster_id = NEW.worker_cluster_id
		         AND worker.namespace = NEW.worker_namespace
		         AND worker.pod_name = NEW.worker_pod_name
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_suspend_attempts_resume_reference_update`,
		`CREATE TRIGGER trg_execution_suspend_attempts_resume_reference_update
		 BEFORE UPDATE OF resume_bundle_id, resume_generation ON execution_suspend_attempts
		 WHEN NEW.resume_bundle_id IS NOT NULL
		   AND NOT EXISTS (
		     SELECT 1
		     FROM execution_recovery_bundles AS bundle
		     WHERE bundle.tenant_id = NEW.tenant_id
		       AND bundle.id = NEW.resume_bundle_id
		       AND bundle.execution_id = NEW.execution_id
		       AND bundle.generation = NEW.resume_generation
		       AND bundle.recovery_reason = 'suspend-resume'
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Recovery-bound suspend attempt must reference a matching suspend-resume Recovery Bundle');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_interactions_resume_recorded_insert`,
		`DROP TRIGGER IF EXISTS trg_execution_interactions_resume_recorded_update`,
		`DROP TRIGGER IF EXISTS trg_execution_interactions_delivery_shape_insert`,
		`CREATE TRIGGER trg_execution_interactions_delivery_shape_insert
		 BEFORE INSERT ON execution_interactions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Interaction delivery shape')
		   WHERE NEW.delivery_status IS NULL
		      OR NEW.delivery_status NOT IN ('not-ready', 'pending', 'delivered', 'acknowledged', 'failed', 'superseded', 'resume-recorded', 'resume-bound', 'outcome-unknown')
		      OR (NEW.delivery_generation IS NOT NULL AND NEW.delivery_generation <= 0)
		      OR (NEW.resume_generation IS NOT NULL AND NEW.resume_generation <= 0)
		      OR (NEW.delivery_error IS NOT NULL AND length(NEW.delivery_error) > 2000)
		      OR NOT (
		        (NEW.status = 'pending' AND NEW.resolution_kind IS NULL AND NEW.resolution_command_id IS NULL
		          AND NEW.delivery_status = 'not-ready' AND NEW.delivery_worker_id IS NULL AND NEW.delivery_generation IS NULL
		          AND NEW.delivery_available_at IS NULL AND NEW.delivered_at IS NULL AND NEW.acknowledged_at IS NULL
		          AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL) OR
		        (NEW.status = 'resolved' AND NEW.resolution_kind IS NOT NULL AND NEW.resolution_command_id IS NOT NULL
		          AND NEW.delivery_status IN ('pending', 'delivered', 'acknowledged', 'failed', 'superseded')
		          AND NEW.delivery_worker_id IS NOT NULL AND NEW.delivery_generation IS NOT NULL
		          AND NEW.delivery_available_at IS NOT NULL
		          AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL) OR
		        (NEW.status = 'resolved' AND NEW.resolution_kind IS NOT NULL AND NEW.resolution_command_id IS NOT NULL
		          AND NEW.delivery_status = 'resume-recorded'
		          AND NEW.delivery_worker_id IS NULL AND NEW.delivery_generation IS NULL
		          AND NEW.delivery_available_at IS NOT NULL AND NEW.delivered_at IS NULL AND NEW.acknowledged_at IS NULL
		          AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL) OR
		        (NEW.status = 'resolved' AND NEW.resolution_kind IS NOT NULL AND NEW.resolution_command_id IS NOT NULL
		          AND NEW.delivery_status = 'resume-bound'
		          AND NEW.delivery_worker_id IS NULL AND NEW.delivery_generation IS NULL
		          AND NEW.delivery_available_at IS NOT NULL AND NEW.delivered_at IS NULL AND NEW.acknowledged_at IS NULL
		          AND NEW.resume_bundle_id IS NOT NULL AND NEW.resume_generation IS NOT NULL AND NEW.resume_bound_at IS NOT NULL) OR
		        (NEW.status = 'resolved' AND NEW.resolution_kind IS NOT NULL AND NEW.resolution_command_id IS NOT NULL
		          AND NEW.delivery_status = 'outcome-unknown' AND NEW.delivery_error IS NOT NULL
		          AND NEW.acknowledged_at IS NULL AND (
		            (NEW.delivery_worker_id IS NOT NULL AND NEW.delivery_generation IS NOT NULL AND NEW.delivered_at IS NOT NULL
		              AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL) OR
		            (NEW.delivery_worker_id IS NULL AND NEW.delivery_generation IS NULL AND NEW.delivered_at IS NULL
		              AND NEW.resume_bundle_id IS NOT NULL AND NEW.resume_generation IS NOT NULL AND NEW.resume_bound_at IS NOT NULL)
		          )) OR
		        (NEW.status = 'expired' AND NEW.resolution_kind IS NULL AND NEW.delivery_status IN ('not-ready', 'superseded')
		          AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL)
		      )
		      OR (NEW.delivery_status IN ('delivered', 'acknowledged') AND NEW.delivered_at IS NULL)
		      OR (NEW.delivery_status = 'acknowledged' AND NEW.acknowledged_at IS NULL);

		   SELECT RAISE(ABORT, 'Recovery-bound Interaction resolution must reference its exact Execution generation Bundle')
		   WHERE NEW.delivery_status IN ('resume-bound', 'outcome-unknown')
		     AND NEW.resume_bundle_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1
		       FROM execution_recovery_bundles AS bundle
		       WHERE bundle.tenant_id = NEW.tenant_id
		         AND bundle.id = NEW.resume_bundle_id
		         AND bundle.execution_id = NEW.execution_id
		         AND bundle.generation = NEW.resume_generation
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_interactions_delivery_shape_update`,
		`CREATE TRIGGER trg_execution_interactions_delivery_shape_update
		 BEFORE UPDATE OF status, resolution_kind, resolution_command_id, delivery_status, delivery_worker_id,
		   delivery_generation, delivery_available_at, delivered_at, acknowledged_at, delivery_error,
		   resume_bundle_id, resume_generation, resume_bound_at ON execution_interactions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Interaction delivery shape')
		   WHERE NEW.delivery_status IS NULL
		      OR NEW.delivery_status NOT IN ('not-ready', 'pending', 'delivered', 'acknowledged', 'failed', 'superseded', 'resume-recorded', 'resume-bound', 'outcome-unknown')
		      OR (NEW.delivery_generation IS NOT NULL AND NEW.delivery_generation <= 0)
		      OR (NEW.resume_generation IS NOT NULL AND NEW.resume_generation <= 0)
		      OR (NEW.delivery_error IS NOT NULL AND length(NEW.delivery_error) > 2000)
		      OR NOT (
		        (NEW.status = 'pending' AND NEW.resolution_kind IS NULL AND NEW.resolution_command_id IS NULL
		          AND NEW.delivery_status = 'not-ready' AND NEW.delivery_worker_id IS NULL AND NEW.delivery_generation IS NULL
		          AND NEW.delivery_available_at IS NULL AND NEW.delivered_at IS NULL AND NEW.acknowledged_at IS NULL
		          AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL) OR
		        (NEW.status = 'resolved' AND NEW.resolution_kind IS NOT NULL AND NEW.resolution_command_id IS NOT NULL
		          AND NEW.delivery_status IN ('pending', 'delivered', 'acknowledged', 'failed', 'superseded')
		          AND NEW.delivery_worker_id IS NOT NULL AND NEW.delivery_generation IS NOT NULL
		          AND NEW.delivery_available_at IS NOT NULL
		          AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL) OR
		        (NEW.status = 'resolved' AND NEW.resolution_kind IS NOT NULL AND NEW.resolution_command_id IS NOT NULL
		          AND NEW.delivery_status = 'resume-recorded'
		          AND NEW.delivery_worker_id IS NULL AND NEW.delivery_generation IS NULL
		          AND NEW.delivery_available_at IS NOT NULL AND NEW.delivered_at IS NULL AND NEW.acknowledged_at IS NULL
		          AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL) OR
		        (NEW.status = 'resolved' AND NEW.resolution_kind IS NOT NULL AND NEW.resolution_command_id IS NOT NULL
		          AND NEW.delivery_status = 'resume-bound'
		          AND NEW.delivery_worker_id IS NULL AND NEW.delivery_generation IS NULL
		          AND NEW.delivery_available_at IS NOT NULL AND NEW.delivered_at IS NULL AND NEW.acknowledged_at IS NULL
		          AND NEW.resume_bundle_id IS NOT NULL AND NEW.resume_generation IS NOT NULL AND NEW.resume_bound_at IS NOT NULL) OR
		        (NEW.status = 'resolved' AND NEW.resolution_kind IS NOT NULL AND NEW.resolution_command_id IS NOT NULL
		          AND NEW.delivery_status = 'outcome-unknown' AND NEW.delivery_error IS NOT NULL
		          AND NEW.acknowledged_at IS NULL AND (
		            (NEW.delivery_worker_id IS NOT NULL AND NEW.delivery_generation IS NOT NULL AND NEW.delivered_at IS NOT NULL
		              AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL) OR
		            (NEW.delivery_worker_id IS NULL AND NEW.delivery_generation IS NULL AND NEW.delivered_at IS NULL
		              AND NEW.resume_bundle_id IS NOT NULL AND NEW.resume_generation IS NOT NULL AND NEW.resume_bound_at IS NOT NULL)
		          )) OR
		        (NEW.status = 'expired' AND NEW.resolution_kind IS NULL AND NEW.delivery_status IN ('not-ready', 'superseded')
		          AND NEW.resume_bundle_id IS NULL AND NEW.resume_generation IS NULL AND NEW.resume_bound_at IS NULL)
		      )
		      OR (NEW.delivery_status IN ('delivered', 'acknowledged') AND NEW.delivered_at IS NULL)
		      OR (NEW.delivery_status = 'acknowledged' AND NEW.acknowledged_at IS NULL);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_interactions_resume_binding_update`,
		`CREATE TRIGGER trg_execution_interactions_resume_binding_update
		 BEFORE UPDATE OF delivery_status, resume_bundle_id, resume_generation, resume_bound_at ON execution_interactions
		 BEGIN
		   SELECT RAISE(ABORT, 'Recovery-bound Interaction resolution cannot be rebound or replayed')
		   WHERE OLD.delivery_status = 'resume-bound'
		     AND (
		       NEW.delivery_status NOT IN ('resume-bound', 'outcome-unknown')
		       OR NEW.resume_bundle_id IS NOT OLD.resume_bundle_id
		       OR NEW.resume_generation IS NOT OLD.resume_generation
		       OR NEW.resume_bound_at IS NOT OLD.resume_bound_at
		     );

		   SELECT RAISE(ABORT, 'Outcome-unknown Interaction resolution is terminal')
		   WHERE OLD.delivery_status = 'outcome-unknown'
		     AND (
		       NEW.delivery_status IS NOT OLD.delivery_status
		       OR NEW.resume_bundle_id IS NOT OLD.resume_bundle_id
		       OR NEW.resume_generation IS NOT OLD.resume_generation
		       OR NEW.resume_bound_at IS NOT OLD.resume_bound_at
		     );

		   SELECT RAISE(ABORT, 'Recovery-bound Interaction resolution must reference its exact Execution generation Bundle')
		   WHERE NEW.delivery_status IN ('resume-bound', 'outcome-unknown')
		     AND NEW.resume_bundle_id IS NOT NULL
		     AND NOT EXISTS (
		       SELECT 1
		       FROM execution_recovery_bundles AS bundle
		       WHERE bundle.tenant_id = NEW.tenant_id
		         AND bundle.id = NEW.resume_bundle_id
		         AND bundle.execution_id = NEW.execution_id
		         AND bundle.generation = NEW.resume_generation
		     );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite metadata safety migration: %w", err)
		}
	}
	if err := migrateCredentialScopeSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateWorkerRevocationSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateKubernetesPodDeletionFenceSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateCredentialBindingsSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateProjectGitBindingAuthoritySQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateCloudCostAccountingSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateWorkerClaimFactsSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateWorkerClaimReleaseFactsSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateSharedCostAllocationSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateBillingSharedAllocationScheduleSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateSharedActualAllocationSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateExecutionGenerationMetricRollupSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateRoutingSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migratePlatformRoutingAuthoritySQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateCapacityReservationSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateExecutionSchedulingPolicySQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateExecutionSchedulingDecisionsSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateWorkerStorageScrubSQLiteSafety(ctx, db); err != nil {
		return err
	}
	return migrateWorkerReleaseSQLiteSafety(ctx, db)
}

func migrateExecutionSchedulingPolicySQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_scheduling_policy_head_scope
		 ON execution_scheduling_policy_heads (tenant_id, scope_kind, scope_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_scheduling_policy_revision_number
		 ON execution_scheduling_policy_revisions (tenant_id, policy_head_id, revision_number)`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_heads_insert`,
		`CREATE TRIGGER trg_execution_scheduling_policy_heads_insert
		 BEFORE INSERT ON execution_scheduling_policy_heads
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy Head scope is invalid')
		   WHERE NEW.scope_kind NOT IN ('tenant', 'organization')
		      OR NEW.version <> 0 OR NEW.current_revision_id IS NOT NULL
		      OR (NEW.scope_kind = 'tenant' AND (NEW.scope_id IS NOT NEW.tenant_id OR NEW.organization_id IS NOT NULL))
		      OR (NEW.scope_kind = 'organization' AND (NEW.scope_id IS NOT NEW.organization_id OR NEW.organization_id IS NULL));
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy Organization is outside the Tenant')
		   WHERE NEW.scope_kind = 'organization' AND NOT EXISTS (
		     SELECT 1 FROM organizations o WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.organization_id
		   );
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy updater is unavailable')
		   WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = NEW.updated_by);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_revisions_insert`,
		`CREATE TRIGGER trg_execution_scheduling_policy_revisions_insert
		 BEFORE INSERT ON execution_scheduling_policy_revisions
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy Revision shape is invalid')
		   WHERE NEW.revision_number <= 0 OR length(NEW.sha256) <> 64 OR NEW.sha256 GLOB '*[^0-9a-f]*';
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy Revision must advance its Head by one')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM execution_scheduling_policy_heads h
		     WHERE h.tenant_id = NEW.tenant_id AND h.id = NEW.policy_head_id
		       AND NEW.revision_number = h.version + 1
		   );
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy creator is unavailable')
		   WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = NEW.created_by);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_rules_insert`,
		`CREATE TRIGGER trg_execution_scheduling_policy_rules_insert
		 BEFORE INSERT ON execution_scheduling_policy_rules
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy rule is invalid')
		   WHERE NEW.dimension NOT IN ('target', 'region', 'cluster', 'provider', 'capacity_class')
		      OR NEW.mode NOT IN ('any', 'allow');
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy rule is outside the next unpublished Revision')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM execution_scheduling_policy_revisions r
		     JOIN execution_scheduling_policy_heads h ON h.tenant_id = r.tenant_id AND h.id = r.policy_head_id
		     WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.revision_id
		       AND r.revision_number = h.version + 1
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_rule_values_insert`,
		`CREATE TRIGGER trg_execution_scheduling_policy_rule_values_insert
		 BEFORE INSERT ON execution_scheduling_policy_rule_values
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy rule value is invalid')
		   WHERE length(NEW.value) < 1 OR length(NEW.value) > 200 OR trim(NEW.value) IS NOT NEW.value;
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy any rule cannot contain values')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM execution_scheduling_policy_rules r
		     JOIN execution_scheduling_policy_revisions revision
		       ON revision.tenant_id = r.tenant_id AND revision.id = r.revision_id
		     JOIN execution_scheduling_policy_heads h
		       ON h.tenant_id = revision.tenant_id AND h.id = revision.policy_head_id
		     WHERE r.tenant_id = NEW.tenant_id AND r.revision_id = NEW.revision_id
		       AND r.dimension = NEW.dimension AND r.mode = 'allow'
		       AND revision.revision_number = h.version + 1
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_heads_update`,
		`CREATE TRIGGER trg_execution_scheduling_policy_heads_update
		 BEFORE UPDATE ON execution_scheduling_policy_heads
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy Head identity is immutable')
		   WHERE NEW.id IS NOT OLD.id OR NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.scope_kind IS NOT OLD.scope_kind OR NEW.scope_id IS NOT OLD.scope_id
		      OR NEW.organization_id IS NOT OLD.organization_id OR NEW.created_at IS NOT OLD.created_at;
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy Head must publish exactly one complete Revision')
		   WHERE NEW.current_revision_id IS OLD.current_revision_id OR NEW.version <> OLD.version + 1
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_scheduling_policy_revisions r
		        WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.current_revision_id
		          AND r.policy_head_id = NEW.id AND r.revision_number = NEW.version
		      )
		      OR (SELECT count(*) FROM execution_scheduling_policy_rules r
		          WHERE r.tenant_id = NEW.tenant_id AND r.revision_id = NEW.current_revision_id) <> 5;
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy updater is unavailable')
		   WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = NEW.updated_by);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_revisions_update`,
		`CREATE TRIGGER trg_execution_scheduling_policy_revisions_update BEFORE UPDATE ON execution_scheduling_policy_revisions
		 BEGIN SELECT RAISE(ABORT, 'Execution Scheduling Policy Revisions are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_revisions_delete`,
		`CREATE TRIGGER trg_execution_scheduling_policy_revisions_delete BEFORE DELETE ON execution_scheduling_policy_revisions
		 BEGIN SELECT RAISE(ABORT, 'Execution Scheduling Policy Revisions are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_rules_update`,
		`CREATE TRIGGER trg_execution_scheduling_policy_rules_update BEFORE UPDATE ON execution_scheduling_policy_rules
		 BEGIN SELECT RAISE(ABORT, 'Execution Scheduling Policy rules are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_rules_delete`,
		`CREATE TRIGGER trg_execution_scheduling_policy_rules_delete BEFORE DELETE ON execution_scheduling_policy_rules
		 BEGIN SELECT RAISE(ABORT, 'Execution Scheduling Policy rules are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_rule_values_update`,
		`CREATE TRIGGER trg_execution_scheduling_policy_rule_values_update BEFORE UPDATE ON execution_scheduling_policy_rule_values
		 BEGIN SELECT RAISE(ABORT, 'Execution Scheduling Policy rule values are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_policy_rule_values_delete`,
		`CREATE TRIGGER trg_execution_scheduling_policy_rule_values_delete BEFORE DELETE ON execution_scheduling_policy_rule_values
		 BEGIN SELECT RAISE(ABORT, 'Execution Scheduling Policy rule values are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_organizations_execution_scheduling_policy_protected`,
		`CREATE TRIGGER trg_organizations_execution_scheduling_policy_protected
		 BEFORE DELETE ON organizations
		 WHEN EXISTS (
		   SELECT 1 FROM execution_scheduling_policy_heads h
		   WHERE h.tenant_id = OLD.tenant_id AND h.scope_kind = 'organization' AND h.scope_id = OLD.id
		 )
		 BEGIN SELECT RAISE(ABORT, 'Organization is protected by its Execution Scheduling Policy'); END`,
		`DROP TRIGGER IF EXISTS trg_tenants_execution_scheduling_policy_protected`,
		`CREATE TRIGGER trg_tenants_execution_scheduling_policy_protected
		 BEFORE DELETE ON tenants
		 WHEN EXISTS (
		   SELECT 1 FROM execution_scheduling_policy_heads h WHERE h.tenant_id = OLD.id
		 )
		 BEGIN SELECT RAISE(ABORT, 'Tenant is protected by its Execution Scheduling Policy'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Execution Scheduling Policy safety migration: %w", err)
		}
	}
	if db.Migrator().HasColumn(&persistence.AgentExecution{}, "tenant_scheduling_policy_version") &&
		db.Migrator().HasColumn(&persistence.AgentExecution{}, "tenant_scheduling_policy_digest") &&
		db.Migrator().HasColumn(&persistence.AgentExecution{}, "organization_scheduling_policy_version") &&
		db.Migrator().HasColumn(&persistence.AgentExecution{}, "organization_scheduling_policy_digest") &&
		db.Migrator().HasColumn(&persistence.AgentExecution{}, "placement_region") &&
		db.Migrator().HasColumn(&persistence.AgentExecution{}, "placement_cluster_id") {
		if err := db.WithContext(ctx).Exec(`DROP TRIGGER IF EXISTS trg_agent_executions_scheduling_policy_snapshot_insert`).Error; err != nil {
			return fmt.Errorf("drop sqlite Execution Scheduling Policy snapshot insert trigger: %w", err)
		}
		if err := db.WithContext(ctx).Exec(`CREATE TRIGGER trg_agent_executions_scheduling_policy_snapshot_insert
		 BEFORE INSERT ON agent_executions
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy snapshot is invalid')
		   WHERE NEW.tenant_scheduling_policy_version < 0
		      OR length(NEW.tenant_scheduling_policy_digest) <> 64
		      OR NEW.tenant_scheduling_policy_digest GLOB '*[^0-9a-f]*'
		      OR NEW.organization_scheduling_policy_version < 0
		      OR length(NEW.organization_scheduling_policy_digest) <> 64
		      OR NEW.organization_scheduling_policy_digest GLOB '*[^0-9a-f]*'
		      OR length(NEW.placement_region) > 120 OR trim(NEW.placement_region) IS NOT NEW.placement_region
		      OR length(NEW.placement_cluster_id) > 200 OR trim(NEW.placement_cluster_id) IS NOT NEW.placement_cluster_id;
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy snapshot Session is outside the Tenant')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_sessions s WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.session_id
		   );
		   SELECT RAISE(ABORT, 'Execution Tenant Scheduling Policy Head is corrupt')
		   WHERE EXISTS (
		     SELECT 1 FROM execution_scheduling_policy_heads h
		     WHERE h.tenant_id = NEW.tenant_id AND h.scope_kind = 'tenant' AND h.scope_id = NEW.tenant_id
		   ) AND NOT EXISTS (
		     SELECT 1 FROM execution_scheduling_policy_heads h
		     JOIN execution_scheduling_policy_revisions r
		       ON r.tenant_id = h.tenant_id AND r.id = h.current_revision_id
		      AND r.policy_head_id = h.id AND r.revision_number = h.version
		     WHERE h.tenant_id = NEW.tenant_id AND h.scope_kind = 'tenant' AND h.scope_id = NEW.tenant_id
		   );
		   SELECT RAISE(ABORT, 'Execution Tenant Scheduling Policy snapshot is stale')
		   WHERE NEW.tenant_scheduling_policy_version IS NOT COALESCE((
		       SELECT h.version FROM execution_scheduling_policy_heads h
		       WHERE h.tenant_id = NEW.tenant_id AND h.scope_kind = 'tenant' AND h.scope_id = NEW.tenant_id
		     ), 0)
		      OR NEW.tenant_scheduling_policy_digest IS NOT COALESCE((
		       SELECT r.sha256 FROM execution_scheduling_policy_heads h
		       JOIN execution_scheduling_policy_revisions r
		         ON r.tenant_id = h.tenant_id AND r.id = h.current_revision_id
		        AND r.policy_head_id = h.id AND r.revision_number = h.version
		       WHERE h.tenant_id = NEW.tenant_id AND h.scope_kind = 'tenant' AND h.scope_id = NEW.tenant_id
		     ), '48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2');
		   SELECT RAISE(ABORT, 'Execution Organization Scheduling Policy Head is corrupt')
		   WHERE EXISTS (
		     SELECT 1 FROM execution_scheduling_policy_heads h
		     JOIN agent_sessions s ON s.tenant_id = h.tenant_id AND s.organization_id = h.scope_id
		     WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.session_id AND h.scope_kind = 'organization'
		   ) AND NOT EXISTS (
		     SELECT 1 FROM execution_scheduling_policy_heads h
		     JOIN agent_sessions s ON s.tenant_id = h.tenant_id AND s.organization_id = h.scope_id
		     JOIN execution_scheduling_policy_revisions r
		       ON r.tenant_id = h.tenant_id AND r.id = h.current_revision_id
		      AND r.policy_head_id = h.id AND r.revision_number = h.version
		     WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.session_id AND h.scope_kind = 'organization'
		   );
		   SELECT RAISE(ABORT, 'Execution Organization Scheduling Policy snapshot is stale')
		   WHERE NEW.organization_scheduling_policy_version IS NOT COALESCE((
		       SELECT h.version FROM execution_scheduling_policy_heads h
		       JOIN agent_sessions s ON s.tenant_id = h.tenant_id AND s.organization_id = h.scope_id
		       WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.session_id AND h.scope_kind = 'organization'
		     ), 0)
		      OR NEW.organization_scheduling_policy_digest IS NOT COALESCE((
		       SELECT r.sha256 FROM execution_scheduling_policy_heads h
		       JOIN agent_sessions s ON s.tenant_id = h.tenant_id AND s.organization_id = h.scope_id
		       JOIN execution_scheduling_policy_revisions r
		         ON r.tenant_id = h.tenant_id AND r.id = h.current_revision_id
		        AND r.policy_head_id = h.id AND r.revision_number = h.version
		       WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.session_id AND h.scope_kind = 'organization'
		     ), '48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2');
		   SELECT RAISE(ABORT, 'Execution placement location Worker Pool is unavailable for the Target')
		   WHERE NEW.worker_pool_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1 FROM worker_pools p WHERE p.id = NEW.worker_pool_id AND p.execution_target_id = NEW.execution_target_id
		   );
		   SELECT RAISE(ABORT, 'Execution routed placement location does not match its Member and Worker Pool authority')
		   WHERE NEW.target_group_id IS NOT NULL AND (
		     NEW.selected_region IS NULL OR NEW.selected_cluster_id IS NULL
		     OR EXISTS (SELECT 1 FROM worker_pools p WHERE p.id = NEW.worker_pool_id AND p.region <> '' AND p.region IS NOT NEW.selected_region)
		     OR EXISTS (SELECT 1 FROM worker_pools p WHERE p.id = NEW.worker_pool_id AND p.cluster_id <> '' AND p.cluster_id IS NOT NEW.selected_cluster_id)
		     OR NEW.placement_region IS NOT COALESCE((SELECT NULLIF(p.region, '') FROM worker_pools p WHERE p.id = NEW.worker_pool_id), NEW.selected_region)
		     OR NEW.placement_cluster_id IS NOT COALESCE((SELECT NULLIF(p.cluster_id, '') FROM worker_pools p WHERE p.id = NEW.worker_pool_id), NEW.selected_cluster_id)
		   );
		   SELECT RAISE(ABORT, 'Execution fixed-Target placement location does not match its Worker Pool authority')
		   WHERE NEW.target_group_id IS NULL AND (
		     NEW.placement_region IS NOT COALESCE((SELECT p.region FROM worker_pools p WHERE p.id = NEW.worker_pool_id), '')
		     OR NEW.placement_cluster_id IS NOT COALESCE((SELECT p.cluster_id FROM worker_pools p WHERE p.id = NEW.worker_pool_id), '')
		   );
		 END`).Error; err != nil {
			return fmt.Errorf("create sqlite Execution Scheduling Policy snapshot insert trigger: %w", err)
		}
		if err := db.WithContext(ctx).Exec(`DROP TRIGGER IF EXISTS trg_agent_executions_scheduling_policy_snapshot_immutable`).Error; err != nil {
			return fmt.Errorf("drop sqlite Execution Scheduling Policy snapshot trigger: %w", err)
		}
		if err := db.WithContext(ctx).Exec(`CREATE TRIGGER trg_agent_executions_scheduling_policy_snapshot_immutable
		 BEFORE UPDATE OF tenant_scheduling_policy_version, tenant_scheduling_policy_digest,
		   organization_scheduling_policy_version, organization_scheduling_policy_digest,
		   placement_region, placement_cluster_id ON agent_executions
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Policy snapshot is immutable')
		   WHERE NEW.tenant_scheduling_policy_version IS NOT OLD.tenant_scheduling_policy_version
		      OR NEW.tenant_scheduling_policy_digest IS NOT OLD.tenant_scheduling_policy_digest
		      OR NEW.organization_scheduling_policy_version IS NOT OLD.organization_scheduling_policy_version
		      OR NEW.organization_scheduling_policy_digest IS NOT OLD.organization_scheduling_policy_digest
		      OR NEW.placement_region IS NOT OLD.placement_region
		      OR NEW.placement_cluster_id IS NOT OLD.placement_cluster_id;
		 END`).Error; err != nil {
			return fmt.Errorf("create sqlite Execution Scheduling Policy snapshot trigger: %w", err)
		}
	}
	if db.Migrator().HasColumn(&persistence.AgentExecution{}, "queue_class") &&
		db.Migrator().HasColumn(&persistence.AgentExecution{}, "queue_priority") &&
		db.Migrator().HasColumn(&persistence.AgentExecution{}, "quota_units") &&
		db.Migrator().HasColumn(&persistence.AgentExecution{}, "automation_id") {
		queueStatements := []string{
			`UPDATE agent_executions SET queue_class = 'interactive'
			 WHERE queue_class IS NULL OR trim(queue_class) = ''`,
			`UPDATE agent_executions SET quota_units = 1
			 WHERE quota_units IS NULL OR quota_units <= 0`,
			`DROP TRIGGER IF EXISTS trg_agent_executions_queue_scope_insert`,
			`CREATE TRIGGER trg_agent_executions_queue_scope_insert
			 BEFORE INSERT ON agent_executions
			 BEGIN
			   SELECT RAISE(ABORT, 'Execution queue snapshot is invalid')
			   WHERE NEW.queue_class NOT IN ('interactive', 'automation', 'batch')
			      OR NEW.queue_priority < -100 OR NEW.queue_priority > 100
			      OR NEW.quota_units < 1 OR NEW.quota_units > 1000000
			      OR ((NEW.queue_class = 'automation') IS NOT (NEW.automation_id IS NOT NULL));
			   SELECT RAISE(ABORT, 'Execution Automation is outside its Session Project')
			   WHERE NEW.automation_id IS NOT NULL AND NOT EXISTS (
			     SELECT 1 FROM automations a
			     JOIN agent_sessions s ON s.tenant_id = a.tenant_id AND s.project_id = a.project_id
			     WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.automation_id
			       AND a.archived_at IS NULL AND s.id = NEW.session_id AND s.archived_at IS NULL
			   );
			 END`,
			`DROP TRIGGER IF EXISTS trg_agent_executions_queue_scope_update`,
			`CREATE TRIGGER trg_agent_executions_queue_scope_update
			 BEFORE UPDATE OF automation_id, queue_class, queue_priority, quota_units ON agent_executions
			 BEGIN
			   SELECT RAISE(ABORT, 'Execution queue and quota snapshot is immutable')
			   WHERE NEW.automation_id IS NOT OLD.automation_id
			      OR NEW.queue_class IS NOT OLD.queue_class
			      OR NEW.queue_priority IS NOT OLD.queue_priority
			      OR NEW.quota_units IS NOT OLD.quota_units;
			 END`,
		}
		for _, statement := range queueStatements {
			if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
				return fmt.Errorf("apply sqlite Execution queue safety migration: %w", err)
			}
		}
	}
	return nil
}

func (s *store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
