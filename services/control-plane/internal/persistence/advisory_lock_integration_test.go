package persistence_test

import (
	"context"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestPostgresBackgroundAdvisoryLocksAllowOnlyOneReplica(t *testing.T) {
	db := openIntegrationDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, key := range []string{
		"synara:docker-worker-pool-reconciler",
		"synara:kubernetes-execution-reconciler",
		"synara:tenant-retention-sweeper",
		"synara:metric-rollup",
	} {
		t.Run(key, func(t *testing.T) {
			releaseFirst, acquired, err := persistence.TryAdvisoryLock(context.Background(), db, key)
			if err != nil {
				t.Fatal(err)
			}
			if !acquired {
				t.Fatal("first replica did not acquire advisory lock")
			}
			releaseSecond, acquired, err := persistence.TryAdvisoryLock(context.Background(), db, key)
			if err != nil {
				t.Fatal(err)
			}
			releaseSecond()
			if acquired {
				releaseFirst()
				t.Fatal("second replica acquired an advisory lock already held by the first replica")
			}

			releaseFirst()
			releaseThird, acquired, err := persistence.TryAdvisoryLock(context.Background(), db, key)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseThird()
			if !acquired {
				t.Fatal("another replica could not acquire the advisory lock after release")
			}
		})
	}
}

func TestPostgresTransactionAdvisoryLockReleasesWithRollupTransaction(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	const key = "synara:worker-incarnation-metric-rollup-cycle"

	first := db.WithContext(ctx).Begin()
	if first.Error != nil {
		t.Fatal(first.Error)
	}
	defer first.Rollback()
	second := db.WithContext(ctx).Begin()
	if second.Error != nil {
		t.Fatal(second.Error)
	}
	defer second.Rollback()

	acquired, err := persistence.TryTransactionAdvisoryLock(ctx, first, key)
	if err != nil || !acquired {
		t.Fatalf("first rollup transaction acquired=%v err=%v", acquired, err)
	}
	acquired, err = persistence.TryTransactionAdvisoryLock(ctx, second, key)
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		t.Fatal("second rollup transaction acquired the lock while the first transaction held it")
	}
	if err := first.Commit().Error; err != nil {
		t.Fatal(err)
	}

	acquired, err = persistence.TryTransactionAdvisoryLock(ctx, second, key)
	if err != nil || !acquired {
		t.Fatalf("second rollup transaction acquired after commit=%v err=%v", acquired, err)
	}
	if err := second.Commit().Error; err != nil {
		t.Fatal(err)
	}
}
