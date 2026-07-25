package reconcilerleadership

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/leadership"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestRunnerCancelsWorkAndStopsSweepAfterLeadershipTransfer(t *testing.T) {
	db := openSQLiteTestDB(t)
	first := mustNewService(t, db, "holder-a", 90*time.Millisecond)
	second := mustNewService(t, db, "holder-b", 90*time.Millisecond)
	runner := mustNewRunner(t, first, RunnerConfig{
		LeaseName:         "sweep-leadership",
		CycleInterval:     time.Hour,
		AcquireRetryDelay: 5 * time.Millisecond,
		RenewInterval:     20 * time.Millisecond,
		AssertInterval:    10 * time.Millisecond,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	readyForTakeover := make(chan struct{})
	stopped := make(chan struct{})
	var onceStart sync.Once
	var onceTakeover sync.Once
	var onceStop sync.Once

	var mu sync.Mutex
	steps := 0
	var stopErr error
	go runner.Run(ctx, func(run RunContext) error {
		onceStart.Do(func() { close(started) })
		for {
			if err := run.AssertActive(run.Context); err != nil {
				mu.Lock()
				stopErr = err
				mu.Unlock()
				onceStop.Do(func() { close(stopped) })
				return err
			}
			mu.Lock()
			steps++
			currentStep := steps
			mu.Unlock()
			if currentStep == 2 {
				onceTakeover.Do(func() { close(readyForTakeover) })
			}
			select {
			case <-run.Context.Done():
				mu.Lock()
				stopErr = run.Context.Err()
				mu.Unlock()
				onceStop.Do(func() { close(stopped) })
				return run.Context.Err()
			case <-time.After(12 * time.Millisecond):
			}
		}
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner never started work")
	}
	select {
	case <-readyForTakeover:
	case <-time.After(time.Second):
		t.Fatal("runner never advanced to the second sweep step")
	}

	expireLease(t, db, "sweep-leadership", time.Now().UTC().Add(-time.Second))
	taken, held, err := second.Acquire(context.Background(), "sweep-leadership")
	if err != nil {
		t.Fatal(err)
	}
	if !held || taken.FencingToken != 2 {
		t.Fatalf("second holder did not take over leadership: %#v held=%v", taken, held)
	}

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("leader-scoped work was not canceled after leadership transfer")
	}

	mu.Lock()
	defer mu.Unlock()
	if stopErr == nil || (!errors.Is(stopErr, ErrLeadershipLost) && !errors.Is(stopErr, context.Canceled)) {
		t.Fatalf("unexpected stop error: %v", stopErr)
	}
	if steps < 2 || steps > 4 {
		t.Fatalf("stale sweep continued too long after leadership loss: steps=%d", steps)
	}
}

func TestRunnerReleasesLeaseOnShutdown(t *testing.T) {
	db := openSQLiteTestDB(t)
	first := mustNewService(t, db, "holder-a", 2*time.Second)
	second := mustNewService(t, db, "holder-b", 2*time.Second)
	runner := mustNewRunner(t, first, RunnerConfig{
		LeaseName:         "release-on-shutdown",
		CycleInterval:     time.Hour,
		AcquireRetryDelay: 5 * time.Millisecond,
		RenewInterval:     150 * time.Millisecond,
		AssertInterval:    100 * time.Millisecond,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(ctx, func(run RunContext) error {
			select {
			case <-started:
			default:
				close(started)
			}
			<-run.Context.Done()
			return run.Context.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner never acquired leadership")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop after shutdown")
	}

	var released persistence.ReconcilerLease
	if err := db.Where("lease_name = ?", "release-on-shutdown").Take(&released).Error; err != nil {
		t.Fatal(err)
	}
	if released.ExpiresAt.After(time.Now().UTC().Add(100 * time.Millisecond)) {
		t.Fatalf("released lease still active after shutdown: expires_at=%s", released.ExpiresAt)
	}

	next, held, err := second.Acquire(context.Background(), "release-on-shutdown")
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatalf("next holder could not acquire released lease: %#v", next)
	}
	if next.FencingToken != released.FencingToken+1 {
		t.Fatalf("takeover fencing token = %d, want %d", next.FencingToken, released.FencingToken+1)
	}
}

func mustNewRunner(t *testing.T, service *leadership.Service, cfg RunnerConfig) *Runner {
	t.Helper()
	runner, err := NewRunner(service, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func openSQLiteTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.ReconcilerLease{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func mustNewService(t *testing.T, db *gorm.DB, holderID string, ttl time.Duration) *leadership.Service {
	t.Helper()
	service, err := leadership.New(db, leadership.Config{HolderID: holderID, LeaseTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func expireLease(t *testing.T, db *gorm.DB, name string, expiresAt time.Time) {
	t.Helper()
	if err := db.Model(&persistence.ReconcilerLease{}).Where("lease_name = ?", name).
		Update("expires_at", expiresAt).Error; err != nil {
		t.Fatal(err)
	}
}
