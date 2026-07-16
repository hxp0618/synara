package database

import (
	"context"
	"os"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestDiffArtifactKindMigrationExtendsOnlyTheArtifactKindBoundary(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000040_worker_release_transition_policy_fencing.sql")); err != nil {
		t.Fatal(err)
	}
	if definition := artifactKindConstraintDefinition(t, db); strings.Contains(definition, "'diff'") {
		t.Fatalf("pre-000041 Artifact kind constraint already accepted diff: %s", definition)
	}
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	definition := artifactKindConstraintDefinition(t, db)
	for _, kind := range []string{"attachment", "generated_file", "terminal_log", "diff", "workspace_snapshot", "checkpoint"} {
		if !strings.Contains(definition, "'"+kind+"'") {
			t.Fatalf("post-000041 Artifact kind constraint omitted %q: %s", kind, definition)
		}
	}
}

func artifactKindConstraintDefinition(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var definition string
	if err := db.Raw(`
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'artifacts'::regclass
		  AND conname = 'artifacts_kind_check'
	`).Scan(&definition).Error; err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(definition) == "" {
		t.Fatal("Artifact kind constraint is missing")
	}
	return definition
}
