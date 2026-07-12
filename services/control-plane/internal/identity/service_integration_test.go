package identity

import (
	"context"
	"fmt"
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

func TestPostgresLoginSessionIdleExpiryAcrossReplicas(t *testing.T) {
	first, second, db := newPostgresIdentityReplicas(t, time.Hour)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now.Add(61 * time.Minute) }

	issued := issuePostgresLogin(t, first)
	if _, err := second.Authenticate(context.Background(), issued.Token); err == nil {
		t.Fatal("another replica authenticated an idle-expired login session")
	}

	var stored persistence.LoginSession
	if err := db.Where("id = ?", issued.State.User.SessionID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RevokedAt != nil {
		t.Fatal("idle expiry unexpectedly persisted a revocation")
	}
}

func TestPostgresAdministratorRevocationIsImmediatelyVisibleAcrossReplicas(t *testing.T) {
	first, second, _ := newPostgresIdentityReplicas(t, time.Hour)
	issued := issuePostgresLogin(t, first)
	principal, err := second.Authenticate(context.Background(), issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.ActiveTenantID == nil {
		t.Fatal("login session has no active tenant")
	}

	revoked, err := first.RevokeTenantUserSessions(
		context.Background(), principal, *principal.ActiveTenantID, principal.UserID,
		"postgres-admin-revoke", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("revoked sessions = %d, want 1", revoked)
	}
	if _, err := second.Authenticate(context.Background(), issued.Token); err == nil {
		t.Fatal("another replica authenticated an administrator-revoked login session")
	}
}

func TestPostgresConcurrentAuthenticateAndRevokeDoesNotResurrectSession(t *testing.T) {
	first, second, db := newPostgresIdentityReplicas(t, time.Hour)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }
	issued := issuePostgresLogin(t, first)
	principal := issued.State.User

	if err := db.Model(&persistence.LoginSession{}).
		Where("id = ?", principal.SessionID).
		Update("last_seen_at", now.Add(-10*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}

	const authenticators = 24
	start := make(chan struct{})
	errorsByReplica := make(chan error, authenticators)
	var wait sync.WaitGroup
	for range authenticators {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := second.Authenticate(context.Background(), issued.Token)
			errorsByReplica <- err
		}()
	}
	close(start)
	if err := first.Revoke(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	close(errorsByReplica)

	for err := range errorsByReplica {
		// An authentication already in flight may linearize before the revoke.
		// The post-revoke assertions below prove that no refresh resurrected it.
		_ = err
	}
	for range 10 {
		if _, err := second.Authenticate(context.Background(), issued.Token); err == nil {
			t.Fatal("revoked session became valid after concurrent authentication refresh")
		}
	}

	var stored persistence.LoginSession
	if err := db.Where("id = ?", principal.SessionID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RevokedAt == nil {
		t.Fatal("concurrent authentication cleared the persisted revocation")
	}
}

func newPostgresIdentityReplicas(t *testing.T, idleTTL time.Duration) (*Service, *Service, *gorm.DB) {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	firstDB, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	secondDB, err := database.Open(ctx, databaseURL)
	if err != nil {
		closeGormDB(firstDB)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeGormDB(secondDB)
		closeGormDB(firstDB)
	})
	if err := database.Migrate(ctx, firstDB, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return NewService(firstDB, 24*time.Hour, idleTTL), NewService(secondDB, 24*time.Hour, idleTTL), firstDB
}

func issuePostgresLogin(t *testing.T, service *Service) IssuedSession {
	t.Helper()
	email := fmt.Sprintf("identity-integration-%s@example.com", uuid.NewString())
	issued, err := service.DevLogin(context.Background(), DevLoginInput{
		Email: email, DisplayName: "PostgreSQL Identity Test",
	}, "127.0.0.1", "integration-test", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func closeGormDB(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
}
