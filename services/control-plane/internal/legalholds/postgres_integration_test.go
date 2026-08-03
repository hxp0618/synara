package legalholds

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresConcurrentLegalHoldReleaseHasOneWinner(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	firstDB, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	firstSQL, err := firstDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstSQL.Close() })
	if err := database.Migrate(ctx, firstDB, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, firstDB, platform.ProfilePersonal, "postgres-legal-hold-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	firstService := NewService(firstDB)
	hold, err := firstService.Create(ctx, principal, domain.TenantID, CreateInput{
		ScopeType: "tenant", ScopeID: domain.TenantID, Name: "PostgreSQL release race",
		MatterReference: "PG-MAT-001", Reason: "Preserve the entire Tenant during PostgreSQL release concurrency verification.",
	}, "pg-legal-hold-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	invalid := persistence.LegalHold{
		ID: uuid.New(), TenantID: domain.TenantID, ScopeType: "session", ScopeID: uuid.New(),
		Name: "Invalid scope", MatterReference: "PG-MAT-INVALID",
		Reason: "PostgreSQL must reject an unknown Session scope for Legal Hold.",
		Status: "active", Version: 1, CreatedBy: domain.UserID,
	}
	if err := firstDB.Create(&invalid).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Legal Hold for a foreign Session")
	}

	secondDB, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	secondSQL, err := secondDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondSQL.Close() })
	services := []*Service{firstService, NewService(secondDB)}
	start := make(chan struct{})
	results := make(chan error, len(services))
	var wait sync.WaitGroup
	for index, service := range services {
		wait.Add(1)
		go func(index int, service *Service) {
			defer wait.Done()
			<-start
			_, releaseErr := service.Release(ctx, principal, domain.TenantID, hold.ID, ReleaseInput{
				ExpectedVersion: 1, Reason: "Counsel approved release after the PostgreSQL concurrency test completed.",
			}, "pg-legal-hold-release-"+string(rune('a'+index)), "127.0.0.1")
			results <- releaseErr
		}(index, service)
	}
	close(start)
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for releaseErr := range results {
		if releaseErr == nil {
			successes++
			continue
		}
		var apiError *problem.Error
		if errors.As(releaseErr, &apiError) && apiError.Code == "legal_hold_version_conflict" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent Legal Hold release error: %v", releaseErr)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent Legal Hold releases: successes=%d conflicts=%d", successes, conflicts)
	}
	var persisted persistence.LegalHold
	if err := firstDB.Where("id = ?", hold.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "released" || persisted.Version != 2 || persisted.ReleasedBy == nil {
		t.Fatalf("unexpected PostgreSQL Legal Hold: %#v", persisted)
	}
	if err := firstDB.Model(&persistence.LegalHold{}).Where("id = ?", hold.ID).
		Update("matter_reference", "PG-MAT-TAMPERED").Error; err == nil {
		t.Fatal("PostgreSQL allowed immutable Legal Hold evidence to change")
	}
}
