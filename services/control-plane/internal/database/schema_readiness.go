package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

type SchemaStatus struct {
	Kind            platform.MetadataStore
	ExpectedVersion int64
	AppliedVersion  int64
}

type SchemaChecker struct {
	db       *gorm.DB
	kind     platform.MetadataStore
	expected *migration
}

func NewSchemaChecker(db *gorm.DB, kind platform.MetadataStore, files fs.FS) (*SchemaChecker, error) {
	checker := &SchemaChecker{db: db, kind: kind}
	if kind != platform.MetadataPostgres {
		return checker, nil
	}
	items, err := readMigrations(files)
	if err != nil {
		return nil, fmt.Errorf("load schema readiness expectation: %w", err)
	}
	if len(items) > 0 {
		expected := items[len(items)-1]
		checker.expected = &expected
	}
	return checker, nil
}

func (c *SchemaChecker) Check(ctx context.Context) (SchemaStatus, error) {
	if c == nil {
		return SchemaStatus{}, errors.New("schema checker is not configured")
	}
	status := SchemaStatus{Kind: c.kind}
	if c.db == nil {
		return status, errors.New("schema checker is not configured")
	}
	if c.kind != platform.MetadataPostgres {
		for _, model := range persistence.AllModels() {
			if !c.db.WithContext(ctx).Migrator().HasTable(model) {
				return status, fmt.Errorf("metadata schema is missing table for %T", model)
			}
		}
		return status, nil
	}
	if c.expected == nil {
		return status, nil
	}
	status.ExpectedVersion = c.expected.version
	if err := c.db.WithContext(ctx).Table((migrationRecord{}).TableName()).
		Select("COALESCE(MAX(version), 0)").Scan(&status.AppliedVersion).Error; err != nil {
		return status, fmt.Errorf("read applied schema version: %w", err)
	}
	var record migrationRecord
	err := c.db.WithContext(ctx).Where("version = ?", c.expected.version).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return status, fmt.Errorf("required schema migration %d is not applied", c.expected.version)
	}
	if err != nil {
		return status, fmt.Errorf("read required schema migration %d: %w", c.expected.version, err)
	}
	if record.Name != c.expected.name {
		return status, fmt.Errorf("schema migration %d name does not match this build", c.expected.version)
	}
	if record.Checksum != c.expected.checksum {
		return status, fmt.Errorf("schema migration %d checksum does not match this build", c.expected.version)
	}
	return status, nil
}

func (c *SchemaChecker) CheckWrite(ctx context.Context) error {
	if c == nil || c.db == nil {
		return errors.New("database write checker is not configured")
	}
	statement := "UPDATE users SET updated_at = updated_at WHERE 1 = 0"
	if c.kind == platform.MetadataPostgres {
		statement = "UPDATE control_plane_schema_migrations SET applied_at = applied_at WHERE FALSE"
	}
	if err := c.db.WithContext(ctx).Exec(statement).Error; err != nil {
		return fmt.Errorf("metadata store is not writable: %w", err)
	}
	return nil
}
