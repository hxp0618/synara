package developerapi

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
)

func TestUsageServicePostgresAdmissionIsAtomicAcrossReplicas(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, postgresisolation.URL(t, databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&persistence.ServiceAccountAPIUsageWindow{}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 3, 14, 30, 0, 0, time.UTC)
	firstReplica := NewUsageService(db, WithNow(func() time.Time { return now }))
	secondReplica := NewUsageService(db, WithNow(func() time.Time { return now }))
	tenantID := uuid.New()
	accountID := uuid.New()
	const limit = 9
	const attempts = 36
	var allowed atomic.Int64
	errors := make(chan error, attempts)
	var wait sync.WaitGroup
	for index := range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			service := firstReplica
			if index%2 == 1 {
				service = secondReplica
			}
			admission, err := service.Admit(ctx, tenantID, accountID, nil, "GET /v1/sessions/{sessionID}", limit)
			if err != nil {
				errors <- err
				return
			}
			if admission.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	if got := allowed.Load(); got != limit {
		t.Fatalf("allowed requests = %d, want %d", got, limit)
	}
}
