package leadership

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresAcquireSerializesExpiredTakeover(t *testing.T) {
	db := openPostgresIntegrationDB(t)
	first := mustNewService(t, db, "postgres-holder-a", 3*time.Second)
	second := mustNewService(t, db, "postgres-holder-b", 3*time.Second)
	leaseName := "postgres-leadership-" + uuid.NewString()

	initial, held, err := first.Acquire(context.Background(), leaseName)
	if err != nil {
		t.Fatal(err)
	}
	if !held || initial.FencingToken != 1 {
		t.Fatalf("initial acquire = %#v held=%v", initial, held)
	}
	expireLease(t, db, leaseName, initial.AcquiredAt.Add(-time.Second))

	type result struct {
		lease Lease
		held  bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	runAcquire := func(service *Service) {
		<-start
		lease, held, err := service.Acquire(context.Background(), leaseName)
		results <- result{lease: lease, held: held, err: err}
	}

	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		runAcquire(first)
	}()
	go func() {
		defer wait.Done()
		runAcquire(second)
	}()
	close(start)
	wait.Wait()
	close(results)

	outcomes := make([]result, 0, 2)
	for outcome := range results {
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		outcomes = append(outcomes, outcome)
	}
	if len(outcomes) != 2 {
		t.Fatalf("acquire outcomes = %d, want 2", len(outcomes))
	}

	var winners []result
	var losers []result
	for _, outcome := range outcomes {
		if outcome.held {
			winners = append(winners, outcome)
		} else {
			losers = append(losers, outcome)
		}
	}
	if len(winners) != 1 || len(losers) != 1 {
		t.Fatalf("unexpected acquire split: winners=%d losers=%d outcomes=%#v", len(winners), len(losers), outcomes)
	}
	if winners[0].lease.FencingToken != 2 {
		t.Fatalf("winner fencing token = %d, want 2", winners[0].lease.FencingToken)
	}
	if losers[0].lease.FencingToken != winners[0].lease.FencingToken ||
		losers[0].lease.HolderID != winners[0].lease.HolderID {
		t.Fatalf("loser observed stale lease: winner=%#v loser=%#v", winners[0].lease, losers[0].lease)
	}

	var stored persistence.ReconcilerLease
	if err := db.Where("lease_name = ?", leaseName).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.FencingToken != 2 || stored.HolderID != winners[0].lease.HolderID {
		t.Fatalf("stored lease = %#v, winner=%#v", stored, winners[0].lease)
	}
}

func TestPostgresAcquireDefersTakeoverWhileControllerCycleLockIsHeld(t *testing.T) {
	db := openPostgresIntegrationDB(t)
	first := mustNewService(t, db, "postgres-cycle-holder-a", 3*time.Second)
	second := mustNewService(t, db, "postgres-cycle-holder-b", 3*time.Second)
	leaseName := "postgres-cycle-guard-" + uuid.NewString()

	initial, held, err := first.Acquire(context.Background(), leaseName)
	if err != nil || !held {
		t.Fatalf("initial acquire held=%v err=%v", held, err)
	}
	cycleRelease, acquired, err := persistence.TryAdvisoryLock(context.Background(), db, leaseName)
	if err != nil || !acquired {
		t.Fatalf("controller cycle lock acquired=%v err=%v", acquired, err)
	}
	expireLease(t, db, leaseName, initial.AcquiredAt.Add(-time.Second))

	blockedLease, held, err := second.Acquire(context.Background(), leaseName)
	if err != nil {
		t.Fatal(err)
	}
	if held || blockedLease.FencingToken != initial.FencingToken || blockedLease.HolderID != initial.HolderID {
		t.Fatalf("takeover while cycle lock held = %#v held=%v", blockedLease, held)
	}
	cycleRelease()

	takenOver, held, err := second.Acquire(context.Background(), leaseName)
	if err != nil || !held {
		t.Fatalf("takeover after cycle release held=%v err=%v", held, err)
	}
	if takenOver.FencingToken != initial.FencingToken+1 || takenOver.HolderID != "postgres-cycle-holder-b" {
		t.Fatalf("takeover lease = %#v", takenOver)
	}
}

func openPostgresIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return db
}
