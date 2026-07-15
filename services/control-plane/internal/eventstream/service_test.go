package eventstream

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

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestConnectionLimitsAndExpiredLeaseCleanup(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "event-stream-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	projectService := projects.NewService(store.DB())
	project, err := projectService.Create(ctx, principal, domain.TenantID, domain.OrganizationID, projects.CreateProjectInput{
		Name: "SSE limits", DefaultBranch: "main", Visibility: "organization",
	}, "event-stream-project", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	sessionService := sessions.NewService(store.DB(), projectService, executiontargets.NewService(store.DB(), profile, nil))
	session, err := sessionService.Create(ctx, principal, project.ID, sessions.CreateSessionInput{
		Title: "SSE limits", Visibility: "project", Provider: "codex",
	}, "event-stream-session", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	otherUsers := []uuid.UUID{uuid.New(), uuid.New()}
	for _, userID := range otherUsers {
		if err := store.DB().Create(&persistence.User{
			ID: userID, Email: userID.String() + "@example.com", DisplayName: "SSE User",
			Status: "active", CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service, err := New(store.DB(), Config{
		InstanceID: "replica-a", LeaseTTL: 30 * time.Second,
		MaxConnectionsPerUser: 1, MaxConnectionsPerTenant: 2,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Acquire(ctx, domain.TenantID, domain.UserID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Acquire(ctx, domain.TenantID, domain.UserID, session.ID)
	assertProblemCode(t, err, "sse_user_connection_limit")
	second, err := service.Acquire(ctx, domain.TenantID, otherUsers[0], session.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Acquire(ctx, domain.TenantID, otherUsers[1], session.ID)
	assertProblemCode(t, err, "sse_tenant_connection_limit")

	now = now.Add(31 * time.Second)
	recovered, err := service.Acquire(ctx, domain.TenantID, domain.UserID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID == first.ID || recovered.ID == second.ID {
		t.Fatal("expired connection lease was reused")
	}
	now = now.Add(time.Second)
	renewed, err := service.Renew(ctx, recovered.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.ExpiresAt.After(recovered.ExpiresAt) {
		t.Fatal("renewal did not extend the connection lease")
	}
	if err := service.Release(ctx, recovered.ID); err != nil {
		t.Fatal(err)
	}
	var leases int64
	if err := store.DB().Model(&persistence.SSEConnectionLease{}).Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("released or expired leases remain: %d", leases)
	}
}

func TestPostgresConcurrentAcquireEnforcesGlobalUserLimit(t *testing.T) {
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
		t.Fatal(err)
	}
	for _, db := range []*gorm.DB{firstDB, secondDB} {
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := database.Migrate(ctx, firstDB, migrations.Files); err != nil {
		t.Fatal(err)
	}
	// PostgreSQL integration packages share the configured database when
	// `go test ./...` runs. Reuse its persisted installation instead of racing
	// other packages with a test-specific installation ID.
	domain, err := bootstrap.Ensure(ctx, firstDB, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	projectService := projects.NewService(firstDB)
	project, err := projectService.Create(ctx, principal, domain.TenantID, domain.OrganizationID, projects.CreateProjectInput{
		Name: "Postgres SSE limits", DefaultBranch: "main", Visibility: "organization",
	}, "event-stream-postgres-project", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	sessionService := sessions.NewService(firstDB, projectService, executiontargets.NewService(firstDB, profile, nil))
	session, err := sessionService.Create(ctx, principal, project.ID, sessions.CreateSessionInput{
		Title: "Postgres SSE limits", Visibility: "project", Provider: "codex",
	}, "event-stream-postgres-session", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = firstDB.Where("tenant_id = ?", domain.TenantID).Delete(&persistence.SSEConnectionLease{}).Error
	})
	now := time.Now().UTC()
	services := make([]*Service, 0, 2)
	for index, db := range []*gorm.DB{firstDB, secondDB} {
		service, err := New(db, Config{
			InstanceID: "postgres-replica-" + []string{"a", "b"}[index], LeaseTTL: time.Minute,
			MaxConnectionsPerUser: 1, MaxConnectionsPerTenant: 10,
			Now: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		services = append(services, service)
	}
	type outcome struct {
		lease Lease
		err   error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, service := range services {
		wait.Add(1)
		go func(service *Service) {
			defer wait.Done()
			<-start
			lease, err := service.Acquire(ctx, domain.TenantID, domain.UserID, session.ID)
			outcomes <- outcome{lease: lease, err: err}
		}(service)
	}
	close(start)
	wait.Wait()
	close(outcomes)
	succeeded, limited := 0, 0
	for result := range outcomes {
		if result.err == nil {
			succeeded++
			continue
		}
		var apiError *problem.Error
		if errors.As(result.err, &apiError) && apiError.Code == "sse_user_connection_limit" {
			limited++
			continue
		}
		t.Fatalf("unexpected concurrent acquisition error: %v", result.err)
	}
	if succeeded != 1 || limited != 1 {
		t.Fatalf("concurrent acquisitions = success %d limited %d", succeeded, limited)
	}
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}
