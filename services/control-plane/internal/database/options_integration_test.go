package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresConnectionPoolUsesConfiguredLimits(t *testing.T) {
	db := openPostgresIntegrationDB(t, Options{
		MaxOpenConnections: 7, MaxIdleConnections: 3,
		ConnectionMaxLifetime: 2 * time.Hour, ConnectionMaxIdleTime: 10 * time.Minute,
		MigrationLockTimeout: 5 * time.Second,
	})
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB.Stats().MaxOpenConnections != 7 {
		t.Fatalf("max open connections = %d, want 7", sqlDB.Stats().MaxOpenConnections)
	}
}

func TestPostgresMigrationLockWaitIsBounded(t *testing.T) {
	db := openPostgresIntegrationDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	connection, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := connection.ExecContext(context.Background(), "SELECT pg_advisory_lock(hashtext('synara_control_plane_migrations'))"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = connection.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtext('synara_control_plane_migrations'))")
	})

	started := time.Now()
	err = Migrate(context.Background(), db, migrations.Files, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "acquire migration lock within") {
		t.Fatalf("expected bounded migration lock error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("migration lock timeout took %s", elapsed)
	}
}
