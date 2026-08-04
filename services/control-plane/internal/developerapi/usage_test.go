package developerapi

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestUsageServiceAdmissionIsAtomicAcrossConcurrentRequests(t *testing.T) {
	db := newUsageTestDB(t)
	now := time.Date(2026, 8, 3, 10, 15, 20, 0, time.UTC)
	service := NewUsageService(db, WithNow(func() time.Time { return now }))
	tenantID := uuid.New()
	accountID := uuid.New()
	const limit = 7
	const attempts = 24

	var allowed atomic.Int64
	errors := make(chan error, attempts)
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			admission, err := service.Admit(
				context.Background(), tenantID, accountID, nil, "GET /v1/sessions/{sessionID}", limit,
			)
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

	var aggregate persistence.ServiceAccountAPIUsageWindow
	if err := db.Where("tenant_id = ? AND service_account_id = ? AND route_pattern = ?", tenantID, accountID, "*").Take(&aggregate).Error; err != nil {
		t.Fatal(err)
	}
	if aggregate.AdmittedCount != limit || aggregate.RateLimitedCount != attempts-limit || aggregate.RequestCount != limit {
		t.Fatalf("aggregate counters = %#v", aggregate)
	}
}

func TestUsageServiceSeparatesKeysAndMinuteWindowsAndRecordsOutcomes(t *testing.T) {
	db := newUsageTestDB(t)
	now := time.Date(2026, 8, 3, 11, 59, 40, 0, time.UTC)
	service := NewUsageService(db, WithNow(func() time.Time { return now }))
	tenantID := uuid.New()
	firstAccountID := uuid.New()
	secondAccountID := uuid.New()
	route := "POST /v1/projects/{projectID}/sessions"

	first, err := service.Admit(context.Background(), tenantID, firstAccountID, nil, route, 1)
	if err != nil || !first.Allowed || first.Remaining != 0 || first.ResetAfter != 20*time.Second {
		t.Fatalf("first admission = %#v, err=%v", first, err)
	}
	denied, err := service.Admit(context.Background(), tenantID, firstAccountID, nil, route, 1)
	if err != nil || denied.Allowed {
		t.Fatalf("denied admission = %#v, err=%v", denied, err)
	}
	other, err := service.Admit(context.Background(), tenantID, secondAccountID, nil, route, 1)
	if err != nil || !other.Allowed {
		t.Fatalf("other key admission = %#v, err=%v", other, err)
	}
	if err := service.RecordOutcome(
		context.Background(), tenantID, firstAccountID, first.ResetAt.Add(-time.Minute), route,
		422, 125*time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}

	var routeUsage persistence.ServiceAccountAPIUsageWindow
	if err := db.Where(
		"tenant_id = ? AND service_account_id = ? AND window_started_at = ? AND route_pattern = ?",
		tenantID, firstAccountID, first.ResetAt.Add(-time.Minute), route,
	).Take(&routeUsage).Error; err != nil {
		t.Fatal(err)
	}
	if routeUsage.AdmittedCount != 1 || routeUsage.RateLimitedCount != 1 || routeUsage.RequestCount != 1 ||
		routeUsage.ClientErrorCount != 1 || routeUsage.TotalDurationMS != 125 {
		t.Fatalf("route usage = %#v", routeUsage)
	}

	now = now.Add(time.Minute)
	nextWindow, err := service.Admit(context.Background(), tenantID, firstAccountID, nil, route, 1)
	if err != nil || !nextWindow.Allowed {
		t.Fatalf("next-window admission = %#v, err=%v", nextWindow, err)
	}
}

func newUsageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "usage.sqlite") + "?_busy_timeout=10000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.ServiceAccountAPIUsageWindow{}); err != nil {
		t.Fatal(err)
	}
	return db
}
