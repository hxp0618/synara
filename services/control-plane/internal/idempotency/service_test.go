package idempotency

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

type testResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func TestExecuteReplaysSameRequestAndRejectsConflict(t *testing.T) {
	db := newIdempotencyDB(t)
	tenantID, actorID := seedIdempotencyScope(t, db)
	scope := Scope{
		TenantID: tenantID, ActorID: actorID, Key: "same-key", Operation: "test.create",
		Request: map[string]any{"name": "same"}, SuccessStatus: 201,
	}
	calls := 0
	createdID := uuid.New()
	first, err := Execute(context.Background(), db, scope, func(_ *gorm.DB) (testResponse, error) {
		calls++
		return testResponse{ID: createdID, Name: "same"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Execute(context.Background(), db, scope, func(_ *gorm.DB) (testResponse, error) {
		calls++
		return testResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.Replayed || !second.Replayed || second.Value.ID != createdID || second.StatusCode != 201 {
		t.Fatalf("unexpected replay result: calls=%d first=%#v second=%#v", calls, first, second)
	}

	scope.Request = map[string]any{"name": "different"}
	if _, err := Execute(context.Background(), db, scope, func(_ *gorm.DB) (testResponse, error) {
		return testResponse{}, nil
	}); err == nil {
		t.Fatal("same key with a different request was accepted")
	}
}

func TestExecuteRollsBackClaimWhenBusinessOperationFails(t *testing.T) {
	db := newIdempotencyDB(t)
	tenantID, actorID := seedIdempotencyScope(t, db)
	scope := Scope{
		TenantID: tenantID, ActorID: actorID, Key: "retry-after-failure", Operation: "test.create",
		Request: map[string]any{"name": "retry"}, SuccessStatus: 201,
	}
	wantErr := errors.New("business failure")
	if _, err := Execute(context.Background(), db, scope, func(_ *gorm.DB) (testResponse, error) {
		return testResponse{}, wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("unexpected operation error: %v", err)
	}

	createdID := uuid.New()
	result, err := Execute(context.Background(), db, scope, func(_ *gorm.DB) (testResponse, error) {
		return testResponse{ID: createdID, Name: "retry"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed || result.Value.ID != createdID {
		t.Fatalf("failed operation left a stale idempotency claim: %#v", result)
	}
}

func TestPostgresConcurrentExecuteCommitsBusinessWriteOnce(t *testing.T) {
	firstDB, secondDB := openPostgresIdempotencyDBs(t)
	tenantID, actorID, organizationID := seedPostgresIdempotencyScope(t, firstDB)
	scope := Scope{
		TenantID: tenantID, ActorID: actorID, Key: "postgres-concurrent-key", Operation: "project.create",
		Request: map[string]any{"organizationId": organizationID, "name": "Concurrent"}, SuccessStatus: 201,
	}
	type outcome struct {
		result Result[testResponse]
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, db := range []*gorm.DB{firstDB, secondDB} {
		wait.Add(1)
		go func(db *gorm.DB) {
			defer wait.Done()
			<-start
			result, err := Execute(context.Background(), db, scope, func(tx *gorm.DB) (testResponse, error) {
				projectID := uuid.New()
				err := tx.Create(&persistence.Project{
					ID: projectID, TenantID: tenantID, OrganizationID: organizationID,
					Name: "Concurrent Idempotency", DefaultBranch: "main", Visibility: "organization", CreatedBy: actorID,
				}).Error
				return testResponse{ID: projectID, Name: "Concurrent Idempotency"}, err
			})
			outcomes <- outcome{result: result, err: err}
		}(db)
	}
	close(start)
	wait.Wait()
	close(outcomes)

	results := make([]Result[testResponse], 0, 2)
	for item := range outcomes {
		if item.err != nil {
			t.Fatal(item.err)
		}
		results = append(results, item.result)
	}
	if len(results) != 2 || results[0].Value.ID != results[1].Value.ID {
		t.Fatalf("concurrent responses differed: %#v", results)
	}
	if results[0].Replayed == results[1].Replayed {
		t.Fatalf("expected exactly one replayed response: %#v", results)
	}
	var projects int64
	if err := firstDB.Model(&persistence.Project{}).
		Where("tenant_id = ? AND name = ?", tenantID, "Concurrent Idempotency").Count(&projects).Error; err != nil {
		t.Fatal(err)
	}
	if projects != 1 {
		t.Fatalf("concurrent idempotency persisted %d Projects", projects)
	}
}

func newIdempotencyDB(t *testing.T) *gorm.DB {
	t.Helper()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(context.Background(), profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	return store.DB()
}

func seedIdempotencyScope(t *testing.T, db *gorm.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	actorID, tenantID := uuid.New(), uuid.New()
	if err := db.Create(&persistence.User{
		ID: actorID, Email: actorID.String() + "@example.com", DisplayName: "Idempotency Actor", Status: "active",
	}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: tenantID, Slug: "tenant-" + tenantID.String()[:8], Name: "Idempotency Tenant",
			Status: "active", PlanCode: "test", Region: "test", Settings: map[string]any{}, CreatedBy: actorID,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: actorID, Role: "owner", Status: "active", JoinedAt: &now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	return tenantID, actorID
}

func openPostgresIdempotencyDBs(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	first, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.Open(ctx, databaseURL)
	if err != nil {
		closeIdempotencyDB(first)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeIdempotencyDB(second)
		closeIdempotencyDB(first)
	})
	if err := database.Migrate(ctx, first, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return first, second
}

func seedPostgresIdempotencyScope(t *testing.T, db *gorm.DB) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	tenantID, actorID := seedIdempotencyScope(t, db)
	organizationID := uuid.New()
	if err := db.Create(&persistence.Organization{
		ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root",
		Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: actorID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return tenantID, actorID, organizationID
}

func closeIdempotencyDB(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
}
