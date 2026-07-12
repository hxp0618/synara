package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteMetadataStoreAutoMigratesAllModels(t *testing.T) {
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(context.Background(), config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	for _, model := range persistence.AllModels() {
		if !store.DB().Migrator().HasTable(model) {
			t.Fatalf("sqlite migration omitted model %T", model)
		}
	}
	if !store.DB().Migrator().HasTable(&persistence.ExecutionControlCommand{}) {
		t.Fatal("sqlite migration omitted execution_control_commands")
	}
}
