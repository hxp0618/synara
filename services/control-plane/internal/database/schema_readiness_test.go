package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteSchemaCheckerRejectsMissingTable(t *testing.T) {
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(context.Background(), profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	checker, err := NewSchemaChecker(store.DB(), store.Kind(), migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checker.Check(context.Background()); err != nil {
		t.Fatalf("migrated schema was not ready: %v", err)
	}
	if err := checker.CheckWrite(context.Background()); err != nil {
		t.Fatalf("writable schema was not ready: %v", err)
	}
	if err := store.DB().Exec("PRAGMA query_only = ON").Error; err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckWrite(context.Background()); err == nil {
		t.Fatal("write readiness accepted query-only SQLite")
	}
	if err := store.DB().Exec("PRAGMA query_only = OFF").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Migrator().DropTable(&persistence.OutboxMessage{}); err != nil {
		t.Fatal(err)
	}
	if _, err := checker.Check(context.Background()); err == nil {
		t.Fatal("schema checker accepted a metadata store with a missing table")
	}
}
