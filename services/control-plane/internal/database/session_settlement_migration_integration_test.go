package database

import (
	"context"
	"os"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSessionSettlementMigrationAddsNullableAuthorityColumnAndPartialIndex(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	databaseURL = postgresisolation.URL(t, databaseURL)
	ctx := context.Background()
	db, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := Migrate(ctx, db, migrationsThrough(t, "000163_stage6_candidate_compatibility_matrix_evidence.sql")); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'agent_sessions' AND column_name = 'settled_at'`).Scan(&before).Error; err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("settled_at existed before Migration 000164: %d", before)
	}

	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var columns, indexes int64
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'agent_sessions' AND column_name = 'settled_at' AND is_nullable = 'YES'`).Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw(`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'agent_sessions' AND indexname = 'idx_agent_sessions_settled' AND indexdef LIKE '%settled_at IS NOT NULL%' AND indexdef LIKE '%archived_at IS NULL%'`).Scan(&indexes).Error; err != nil {
		t.Fatal(err)
	}
	if columns != 1 || indexes != 1 {
		t.Fatalf("Migration 000164 columns/indexes = %d/%d", columns, indexes)
	}
}
