package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var migrationName = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int64
	name     string
	checksum string
	script   string
}

type migrationRecord struct {
	Version   int64     `gorm:"column:version;primaryKey"`
	Name      string    `gorm:"column:name"`
	Checksum  string    `gorm:"column:checksum"`
	AppliedAt time.Time `gorm:"column:applied_at"`
}

func (migrationRecord) TableName() string { return "control_plane_schema_migrations" }

func Migrate(ctx context.Context, db *gorm.DB, files fs.FS, lockTimeoutValues ...time.Duration) error {
	lockTimeout := 30 * time.Second
	if len(lockTimeoutValues) > 0 {
		lockTimeout = lockTimeoutValues[0]
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("resolve migration pool: %w", err)
	}
	rawConnection, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer rawConnection.Close()

	connection := db.Session(&gorm.Session{NewDB: true, Context: ctx})
	connection.ConnPool = rawConnection
	connection.Statement.ConnPool = rawConnection
	lockContext, cancelLock := context.WithTimeout(ctx, lockTimeout)
	defer cancelLock()
	if err := connection.WithContext(lockContext).Exec(`SELECT pg_advisory_lock(hashtext('synara_control_plane_migrations'))`).Error; err != nil {
		if errors.Is(lockContext.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("acquire migration lock within %s: %w", lockTimeout, lockContext.Err())
		}
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_ = connection.Exec(`SELECT pg_advisory_unlock(hashtext('synara_control_plane_migrations'))`).Error
	}()

	if err := connection.Exec(`
		CREATE TABLE IF NOT EXISTS control_plane_schema_migrations (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`).Error; err != nil {
		return fmt.Errorf("ensure migration table: %w", err)
	}

	migrations, err := readMigrations(files)
	if err != nil {
		return err
	}
	for _, item := range migrations {
		var record migrationRecord
		err := connection.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "version = ?", item.version).Error
		if err == nil {
			if record.Checksum != item.checksum {
				return fmt.Errorf("migration %d checksum changed after application", item.version)
			}
			continue
		}
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("read migration %d state: %w", item.version, err)
		}

		if err := connection.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(item.script).Error; err != nil {
				return err
			}
			return tx.Create(&migrationRecord{
				Version: item.version, Name: item.name, Checksum: item.checksum, AppliedAt: time.Now().UTC(),
			}).Error
		}); err != nil {
			return fmt.Errorf("apply migration %d_%s: %w", item.version, item.name, err)
		}
	}
	return nil
}

func readMigrations(files fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("list embedded migrations: %w", err)
	}
	result := make([]migration, 0, len(entries))
	seen := make(map[int64]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		matches := migrationName.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse migration version %q: %w", matches[1], err)
		}
		if previous, exists := seen[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %d in %s and %s", version, previous, entry.Name())
		}
		seen[version] = entry.Name()
		content, err := fs.ReadFile(files, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(content)
		result = append(result, migration{
			version: version, name: matches[2], checksum: hex.EncodeToString(digest[:]), script: string(content),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	return result, nil
}
