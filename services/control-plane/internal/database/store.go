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
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite metadata safety migration: %w", err)
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
