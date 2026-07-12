package database

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
	db   *gorm.DB
	kind platform.MetadataStore
}

func OpenMetadataStore(ctx context.Context, config platform.Config, databaseURL, sqlitePath string) (MetadataStore, error) {
	switch config.MetadataStore {
	case platform.MetadataPostgres:
		db, err := Open(ctx, databaseURL)
		if err != nil {
			return nil, err
		}
		return &store{db: db, kind: platform.MetadataPostgres}, nil
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
		return &store{db: db, kind: platform.MetadataSQLite}, nil
	default:
		return nil, fmt.Errorf("unsupported metadata store %q", config.MetadataStore)
	}
}

func (s *store) DB() *gorm.DB { return s.db }

func (s *store) Kind() platform.MetadataStore { return s.kind }

func (s *store) Migrate(ctx context.Context, files fs.FS) error {
	if s.kind == platform.MetadataPostgres {
		return Migrate(ctx, s.db, files)
	}
	if err := s.db.WithContext(ctx).AutoMigrate(persistence.AllModels()...); err != nil {
		return fmt.Errorf("auto-migrate sqlite metadata schema: %w", err)
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
