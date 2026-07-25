package leadership

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestAssertFenceInTransactionRejectsExpiredOrTransferredEpoch(t *testing.T) {
	db := openSQLiteTestDB(t)
	first := mustNewService(t, db, "holder-a", 2*time.Second)
	second := mustNewService(t, db, "holder-b", 2*time.Second)
	lease, held, err := first.Acquire(context.Background(), "lease-transaction-fence")
	if err != nil || !held {
		t.Fatalf("first acquire = %#v held=%v err=%v", lease, held, err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		asserted, err := AssertFenceInTransaction(
			context.Background(), tx, lease.Name, lease.HolderID, lease.FencingToken,
		)
		if err != nil {
			return err
		}
		if asserted.FencingToken != lease.FencingToken {
			t.Fatalf("asserted lease = %#v", asserted)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	expireLease(t, db, lease.Name, lease.AcquiredAt.Add(-time.Second))
	taken, held, err := second.Acquire(context.Background(), lease.Name)
	if err != nil || !held || taken.FencingToken != lease.FencingToken+1 {
		t.Fatalf("takeover = %#v held=%v err=%v", taken, held, err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		_, err := AssertFenceInTransaction(
			context.Background(), tx, lease.Name, lease.HolderID, lease.FencingToken,
		)
		return err
	})
	if !errors.Is(err, ErrFenceInactive) {
		t.Fatalf("stale transactional fence err = %v", err)
	}
}

func TestAcquireRenewsSameHolderWithoutTokenBump(t *testing.T) {
	db := openSQLiteTestDB(t)
	service := mustNewService(t, db, "holder-a", 1500*time.Millisecond)

	first, held, err := service.Acquire(context.Background(), "lease-renew")
	if err != nil {
		t.Fatal(err)
	}
	if !held || first.FencingToken != 1 {
		t.Fatalf("first acquire = %#v held=%v", first, held)
	}
	assertLeaseTTL(t, first, 1500*time.Millisecond)

	time.Sleep(25 * time.Millisecond)

	second, held, err := service.Acquire(context.Background(), "lease-renew")
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("same holder lost leadership on renew")
	}
	if second.FencingToken != first.FencingToken {
		t.Fatalf("same-holder renew bumped fencing token: first=%d second=%d", first.FencingToken, second.FencingToken)
	}
	if !second.AcquiredAt.Equal(first.AcquiredAt) {
		t.Fatalf("same-holder renew changed acquired_at: first=%s second=%s", first.AcquiredAt, second.AcquiredAt)
	}
	if !second.RenewedAt.After(first.RenewedAt) {
		t.Fatalf("same-holder renew did not advance renewed_at: first=%s second=%s", first.RenewedAt, second.RenewedAt)
	}
	assertLeaseTTL(t, second, 1500*time.Millisecond)
}

func TestExpiredTakeoverBumpsFenceAndFencesStaleEpoch(t *testing.T) {
	db := openSQLiteTestDB(t)
	first := mustNewService(t, db, "holder-a", 2*time.Second)
	second := mustNewService(t, db, "holder-b", 2*time.Second)

	lease, held, err := first.Acquire(context.Background(), "lease-takeover")
	if err != nil {
		t.Fatal(err)
	}
	if !held || lease.FencingToken != 1 {
		t.Fatalf("first acquire = %#v held=%v", lease, held)
	}
	expireLease(t, db, "lease-takeover", lease.AcquiredAt.Add(-time.Second))

	if _, active, err := first.AssertActive(context.Background(), "lease-takeover", lease.FencingToken); err != nil {
		t.Fatal(err)
	} else if active {
		t.Fatal("expired lease remained active")
	}

	taken, held, err := second.Acquire(context.Background(), "lease-takeover")
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatalf("takeover did not acquire leadership: %#v", taken)
	}
	if taken.FencingToken != lease.FencingToken+1 {
		t.Fatalf("takeover token = %d, want %d", taken.FencingToken, lease.FencingToken+1)
	}

	if renewed, active, err := first.Renew(context.Background(), "lease-takeover", lease.FencingToken); err != nil {
		t.Fatal(err)
	} else if active {
		t.Fatalf("stale holder renewed fenced lease: %#v", renewed)
	}
	if asserted, active, err := first.AssertActive(context.Background(), "lease-takeover", lease.FencingToken); err != nil {
		t.Fatal(err)
	} else if active || asserted.FencingToken != taken.FencingToken || asserted.HolderID != taken.HolderID {
		t.Fatalf("stale holder sees unexpected active lease: %#v active=%v", asserted, active)
	}
}

func TestReleaseImmediatelyExpiresLeaseAndPreservesMonotonicFence(t *testing.T) {
	db := openSQLiteTestDB(t)
	first := mustNewService(t, db, "holder-a", 2*time.Second)
	second := mustNewService(t, db, "holder-b", 2*time.Second)

	lease, held, err := first.Acquire(context.Background(), "lease-release")
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatalf("first acquire did not hold leadership: %#v", lease)
	}

	released, err := first.Release(context.Background(), "lease-release", lease.FencingToken)
	if err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("active holder failed to release leadership")
	}

	if _, active, err := first.AssertActive(context.Background(), "lease-release", lease.FencingToken); err != nil {
		t.Fatal(err)
	} else if active {
		t.Fatal("released lease remained active")
	}
	if releasedAgain, err := first.Release(context.Background(), "lease-release", lease.FencingToken); err != nil {
		t.Fatal(err)
	} else if releasedAgain {
		t.Fatal("expired epoch released twice")
	}

	next, held, err := second.Acquire(context.Background(), "lease-release")
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatalf("new holder failed to acquire released lease: %#v", next)
	}
	if next.FencingToken != lease.FencingToken+1 {
		t.Fatalf("released takeover token = %d, want %d", next.FencingToken, lease.FencingToken+1)
	}
}

func openSQLiteTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "leadership.sqlite")), &gorm.Config{
		TranslateError: true, SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if err := db.Exec("PRAGMA busy_timeout = 5000").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.ReconcilerLease{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func mustNewService(t *testing.T, db *gorm.DB, holderID string, ttl time.Duration) *Service {
	t.Helper()
	service, err := New(db, Config{HolderID: holderID, LeaseTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func expireLease(t *testing.T, db *gorm.DB, leaseName string, expiresAt time.Time) {
	t.Helper()
	if err := db.Model(&persistence.ReconcilerLease{}).
		Where("lease_name = ?", leaseName).
		Updates(map[string]any{
			"acquired_at": expiresAt.Add(-2 * time.Second),
			"renewed_at":  expiresAt.Add(-time.Second),
			"expires_at":  expiresAt,
		}).Error; err != nil {
		t.Fatal(err)
	}
}

func assertLeaseTTL(t *testing.T, lease Lease, ttl time.Duration) {
	t.Helper()
	got := lease.ExpiresAt.Sub(lease.RenewedAt)
	tolerance := 150 * time.Millisecond
	if got < ttl-tolerance || got > ttl+tolerance {
		t.Fatalf("lease TTL = %s, want about %s", got, ttl)
	}
}
