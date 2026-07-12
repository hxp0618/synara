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
