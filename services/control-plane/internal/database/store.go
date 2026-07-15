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
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_executions_session_active
		 ON agent_executions (tenant_id, session_id)
		 WHERE status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_interactions_request
		 ON execution_interactions (tenant_id, execution_id, request_id)`,
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
	if err := migrateCredentialBindingsSQLiteSafety(ctx, db); err != nil {
		return err
	}
	if err := migrateProjectGitBindingAuthoritySQLiteSafety(ctx, db); err != nil {
		return err
	}
	return migrateWorkerReleaseSQLiteSafety(ctx, db)
}

func (s *store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
