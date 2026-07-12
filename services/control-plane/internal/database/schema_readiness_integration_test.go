package database

import (
	"context"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresSchemaCheckerRejectsMissingRequiredMigration(t *testing.T) {
	db := openPostgresIntegrationDB(t)
	var err error
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	checker, err := NewSchemaChecker(db, platform.MetadataPostgres, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	status, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("migrated schema was not ready: %v", err)
	}
	if status.ExpectedVersion == 0 || status.AppliedVersion < status.ExpectedVersion {
		t.Fatalf("unexpected schema status: %#v", status)
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	if err := tx.Exec("DELETE FROM control_plane_schema_migrations WHERE version = ?", status.ExpectedVersion).Error; err != nil {
		t.Fatal(err)
	}
	transactionChecker, err := NewSchemaChecker(tx, platform.MetadataPostgres, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := transactionChecker.Check(context.Background())
	if err == nil {
		t.Fatal("schema checker accepted a missing required migration")
	}
	if missing.ExpectedVersion != status.ExpectedVersion || missing.AppliedVersion >= missing.ExpectedVersion {
		t.Fatalf("unexpected missing schema status: %#v", missing)
	}
}

func TestPostgresWriteReadinessRejectsReadOnlyTransaction(t *testing.T) {
	db := openPostgresIntegrationDB(t)
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	if err := tx.Exec("SET TRANSACTION READ ONLY").Error; err != nil {
		t.Fatal(err)
	}
	checker, err := NewSchemaChecker(tx, platform.MetadataPostgres, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckWrite(context.Background()); err == nil {
		t.Fatal("write readiness accepted a read-only PostgreSQL transaction")
	}
}
