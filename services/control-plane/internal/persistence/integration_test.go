package persistence_test

import (
	"context"
	"os"
	"testing"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func openIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return db
}
