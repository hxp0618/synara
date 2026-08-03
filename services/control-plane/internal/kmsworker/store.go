package kmsworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type StoreConfig struct {
	Driver      string
	DatabaseURL string
	SQLitePath  string
}

func OpenStore(ctx context.Context, config StoreConfig) (*gorm.DB, error) {
	var dialector gorm.Dialector
	switch strings.ToLower(strings.TrimSpace(config.Driver)) {
	case "sqlite":
		path := strings.TrimSpace(config.SQLitePath)
		if path == "" {
			return nil, errors.New("KMS SQLite path is required")
		}
		if path != ":memory:" && !strings.HasPrefix(path, "file:") {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, fmt.Errorf("create KMS SQLite directory: %w", err)
			}
			file, err := os.OpenFile(path, os.O_CREATE, 0o600)
			if err != nil {
				return nil, fmt.Errorf("create KMS SQLite database: %w", err)
			}
			if err := file.Close(); err != nil {
				return nil, fmt.Errorf("close KMS SQLite database bootstrap file: %w", err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return nil, fmt.Errorf("secure KMS SQLite database: %w", err)
			}
		}
		dialector = sqlite.Open(path)
	case "postgres":
		if strings.TrimSpace(config.DatabaseURL) == "" {
			return nil, errors.New("KMS PostgreSQL database URL is required")
		}
		dialector = postgres.New(postgres.Config{DSN: config.DatabaseURL, PreferSimpleProtocol: true})
	default:
		return nil, fmt.Errorf("unsupported KMS database driver %q", config.Driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		TranslateError:         true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open KMS database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("resolve KMS database pool: %w", err)
	}
	if db.Dialector.Name() == "sqlite" {
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		for _, statement := range []string{
			"PRAGMA foreign_keys = ON",
			"PRAGMA busy_timeout = 5000",
			"PRAGMA journal_mode = WAL",
			"PRAGMA synchronous = FULL",
		} {
			if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
				_ = sqlDB.Close()
				return nil, fmt.Errorf("configure KMS SQLite store: %w", err)
			}
		}
	} else {
		sqlDB.SetMaxOpenConns(20)
		sqlDB.SetMaxIdleConns(5)
		sqlDB.SetConnMaxLifetime(time.Hour)
		sqlDB.SetConnMaxIdleTime(15 * time.Minute)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping KMS database: %w", err)
	}
	return db, nil
}

func MigrateStore(ctx context.Context, db *gorm.DB) error {
	migrate := func(scoped *gorm.DB) error {
		if err := scoped.WithContext(ctx).AutoMigrate(
			&LogicalKey{},
			&KeyVersion{},
			&AuditEvent{},
			&IdempotencyRecord{},
			&DeletionRequest{},
			&DestructionReceipt{},
			&ResealRun{},
			&ResealReceipt{},
		); err != nil {
			return fmt.Errorf("migrate KMS schema: %w", err)
		}
		return installImmutableGuards(ctx, scoped)
	}
	if db.Dialector.Name() != "postgres" {
		return migrate(db)
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// One deployment may start several replicas concurrently. Serialize the
		// dedicated KMS schema/trigger installation without sharing the Control
		// Plane migration lock or relying on one process winning a DDL race.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(0x53594e4b4d533031)).Error; err != nil {
			return fmt.Errorf("lock KMS schema migration: %w", err)
		}
		return migrate(tx)
	})
}

func installImmutableGuards(ctx context.Context, db *gorm.DB) error {
	var statements []string
	switch db.Dialector.Name() {
	case "sqlite":
		statements = []string{
			`CREATE TRIGGER IF NOT EXISTS kms_audit_events_no_update BEFORE UPDATE ON kms_audit_events BEGIN SELECT RAISE(ABORT, 'kms audit events are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_audit_events_no_delete BEFORE DELETE ON kms_audit_events BEGIN SELECT RAISE(ABORT, 'kms audit events are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_idempotency_no_update BEFORE UPDATE ON kms_idempotency_records BEGIN SELECT RAISE(ABORT, 'kms idempotency records are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_idempotency_no_delete BEFORE DELETE ON kms_idempotency_records BEGIN SELECT RAISE(ABORT, 'kms idempotency records are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_reseal_receipts_no_update BEFORE UPDATE ON kms_reseal_receipts BEGIN SELECT RAISE(ABORT, 'kms reseal receipts are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_reseal_receipts_no_delete BEFORE DELETE ON kms_reseal_receipts BEGIN SELECT RAISE(ABORT, 'kms reseal receipts are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_reseal_runs_no_update BEFORE UPDATE ON kms_reseal_runs BEGIN SELECT RAISE(ABORT, 'kms reseal runs are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_reseal_runs_no_delete BEFORE DELETE ON kms_reseal_runs BEGIN SELECT RAISE(ABORT, 'kms reseal runs are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_destruction_receipts_no_update BEFORE UPDATE ON kms_destruction_receipts BEGIN SELECT RAISE(ABORT, 'kms destruction receipts are immutable'); END`,
			`CREATE TRIGGER IF NOT EXISTS kms_destruction_receipts_no_delete BEFORE DELETE ON kms_destruction_receipts BEGIN SELECT RAISE(ABORT, 'kms destruction receipts are immutable'); END`,
		}
	case "postgres":
		statements = []string{
			`CREATE OR REPLACE FUNCTION synara_kms_reject_immutable_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'KMS evidence rows are immutable'; END $$`,
			`DROP TRIGGER IF EXISTS kms_audit_events_immutable ON kms_audit_events`,
			`CREATE TRIGGER kms_audit_events_immutable BEFORE UPDATE OR DELETE ON kms_audit_events FOR EACH ROW EXECUTE FUNCTION synara_kms_reject_immutable_mutation()`,
			`DROP TRIGGER IF EXISTS kms_idempotency_immutable ON kms_idempotency_records`,
			`CREATE TRIGGER kms_idempotency_immutable BEFORE UPDATE OR DELETE ON kms_idempotency_records FOR EACH ROW EXECUTE FUNCTION synara_kms_reject_immutable_mutation()`,
			`DROP TRIGGER IF EXISTS kms_reseal_receipts_immutable ON kms_reseal_receipts`,
			`CREATE TRIGGER kms_reseal_receipts_immutable BEFORE UPDATE OR DELETE ON kms_reseal_receipts FOR EACH ROW EXECUTE FUNCTION synara_kms_reject_immutable_mutation()`,
			`DROP TRIGGER IF EXISTS kms_reseal_runs_immutable ON kms_reseal_runs`,
			`CREATE TRIGGER kms_reseal_runs_immutable BEFORE UPDATE OR DELETE ON kms_reseal_runs FOR EACH ROW EXECUTE FUNCTION synara_kms_reject_immutable_mutation()`,
			`DROP TRIGGER IF EXISTS kms_destruction_receipts_immutable ON kms_destruction_receipts`,
			`CREATE TRIGGER kms_destruction_receipts_immutable BEFORE UPDATE OR DELETE ON kms_destruction_receipts FOR EACH ROW EXECUTE FUNCTION synara_kms_reject_immutable_mutation()`,
		}
	default:
		return fmt.Errorf("unsupported KMS database dialect %q", db.Dialector.Name())
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("install KMS immutable guard: %w", err)
		}
	}
	return nil
}
