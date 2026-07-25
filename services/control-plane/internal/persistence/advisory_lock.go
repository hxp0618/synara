package persistence

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// TryAdvisoryLock coordinates singleton reconcilers across PostgreSQL control
// plane replicas. SQLite profiles are single-replica by contract and acquire
// the lock without an external database primitive.
func TryAdvisoryLock(ctx context.Context, db *gorm.DB, key string) (func(), bool, error) {
	if db.Dialector.Name() != "postgres" {
		return func() {}, true, nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, false, err
	}
	connection, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := connection.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key).Scan(&acquired); err != nil {
		connection.Close()
		return nil, false, fmt.Errorf("acquire PostgreSQL advisory lock: %w", err)
	}
	if !acquired {
		connection.Close()
		return func() {}, false, nil
	}
	release := func() {
		var released bool
		_ = connection.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key).Scan(&released)
		_ = connection.Close()
	}
	return release, true, nil
}

// TryTransactionAdvisoryLock coordinates a durable transaction with a
// session-level controller-cycle lock that uses the same key. PostgreSQL keeps
// the lock until the surrounding transaction commits or rolls back, avoiding
// an extra connection and closing the release-before-commit gap.
func TryTransactionAdvisoryLock(ctx context.Context, tx *gorm.DB, key string) (bool, error) {
	if tx.Dialector.Name() != "postgres" {
		return true, nil
	}
	var acquired bool
	if err := tx.WithContext(ctx).
		Raw("SELECT pg_try_advisory_xact_lock(hashtextextended(?, 0))", key).
		Scan(&acquired).Error; err != nil {
		return false, fmt.Errorf("acquire PostgreSQL transaction advisory lock: %w", err)
	}
	return acquired, nil
}
