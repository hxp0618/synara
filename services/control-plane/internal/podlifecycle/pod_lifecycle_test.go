package podlifecycle

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsLogicalIdentityLockUnavailableMatchesOnlyOwnedContention(t *testing.T) {
	exact := &pgconn.PgError{
		Code:    "40001",
		Message: logicalIdentityLockUnavailablePostgresMessage,
	}
	if !IsLogicalIdentityLockUnavailable(exact) {
		t.Fatal("exact PostgreSQL logical-identity contention was not recognized")
	}
	if !IsLogicalIdentityLockUnavailable(fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", exact))) {
		t.Fatal("wrapped PostgreSQL logical-identity contention was not recognized")
	}
	if !IsLogicalIdentityLockUnavailable(fmt.Errorf("wrapped sentinel: %w", ErrLogicalIdentityLockUnavailable)) {
		t.Fatal("package logical-identity contention sentinel was not recognized")
	}
	if IsLogicalIdentityLockUnavailable(&pgconn.PgError{
		Code:    "40001",
		Message: "could not serialize access due to concurrent update",
	}) {
		t.Fatal("unrelated PostgreSQL serialization failure was recognized as logical-identity contention")
	}
	if IsLogicalIdentityLockUnavailable(&pgconn.PgError{
		Code:    "23514",
		Message: logicalIdentityLockUnavailablePostgresMessage,
	}) {
		t.Fatal("logical-identity message with an unrelated SQLSTATE was recognized")
	}
	if IsLogicalIdentityLockUnavailable(errors.New(logicalIdentityLockUnavailablePostgresMessage)) {
		t.Fatal("untyped matching text was recognized as logical-identity contention")
	}
}
