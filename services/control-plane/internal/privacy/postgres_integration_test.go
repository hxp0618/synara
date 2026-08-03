package privacy

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresConcurrentPrivacyTransitionHasOneWinner(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, firstDB, platform.ProfilePersonal, "postgres-privacy-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	subject := persistence.User{
		ID: uuid.New(), Email: "pg-privacy-subject@example.com", DisplayName: "PG Privacy Subject",
		Status: "active", EmailVerifiedAt: &now,
	}
	if err := firstDB.Create(&subject).Error; err != nil {
		t.Fatal(err)
	}
	joinedAt := now
	if err := firstDB.Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: subject.ID, Role: "member", Status: "active", JoinedAt: &joinedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	subjectPrincipal := identity.Principal{UserID: subject.ID, ActiveTenantID: &domain.TenantID}
	adminPrincipal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	firstService := NewService(firstDB, nil)
	request, err := firstService.Create(ctx, subjectPrincipal, domain.TenantID, CreateInput{
		RequestType: "access_export", Reason: "Verify PostgreSQL Privacy Request transition concurrency.",
	}, "pg-privacy-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	invalid := persistence.PrivacyRequest{
		ID: uuid.New(), TenantID: domain.TenantID, SubjectUserID: subject.ID,
		RequestType: "erasure", Status: "requested", Version: 1, RequestedBy: domain.UserID,
		IntakeReason: "Create a direct transition guard fixture for PostgreSQL.", DueAt: now.Add(30 * 24 * time.Hour),
		LastTransitionBy: domain.UserID, LastTransitionReason: "Create a direct transition guard fixture for PostgreSQL.",
		ResultSummary: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := firstDB.Create(&invalid).Error; err != nil {
		t.Fatal(err)
	}
	if err := firstDB.Model(&persistence.PrivacyRequest{}).Where("id = ?", invalid.ID).
		Updates(map[string]any{
			"status": "approved", "version": 2, "last_transition_by": domain.UserID,
			"last_transition_reason": "Direct requested to approved transition must fail.",
		}).Error; err == nil {
		t.Fatal("PostgreSQL accepted an invalid requested to approved Privacy transition")
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
	services := []*Service{firstService, NewService(secondDB, nil)}
	start := make(chan struct{})
	results := make(chan error, len(services))
	var wait sync.WaitGroup
	for index, service := range services {
		wait.Add(1)
		go func(index int, service *Service) {
			defer wait.Done()
			<-start
			_, transitionErr := service.Transition(ctx, adminPrincipal, domain.TenantID, request.ID, TransitionInput{
				ExpectedVersion: 1, ToStatus: "verified",
				Reason: "Identity was verified by PostgreSQL concurrency reviewer " + string(rune('A'+index)) + ".",
			}, "pg-privacy-verify-"+string(rune('a'+index)), "127.0.0.1")
			results <- transitionErr
		}(index, service)
	}
	close(start)
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for transitionErr := range results {
		if transitionErr == nil {
			successes++
			continue
		}
		var apiError *problem.Error
		if errors.As(transitionErr, &apiError) && apiError.Code == "privacy_request_version_conflict" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent Privacy transition error: %v", transitionErr)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent Privacy transitions: successes=%d conflicts=%d", successes, conflicts)
	}
	var persisted persistence.PrivacyRequest
	if err := firstDB.Where("id = ?", request.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "verified" || persisted.Version != 2 {
		t.Fatalf("unexpected PostgreSQL Privacy Request: %#v", persisted)
	}
	var eventCount int64
	if err := firstDB.Model(&persistence.PrivacyRequestEvent{}).
		Where("privacy_request_id = ?", request.ID).Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 {
		t.Fatalf("Privacy Request event count = %d, want 2", eventCount)
	}
	var firstEvent persistence.PrivacyRequestEvent
	if err := firstDB.Where("privacy_request_id = ? AND version = ?", request.ID, 1).Take(&firstEvent).Error; err != nil {
		t.Fatal(err)
	}
	if err := firstDB.Model(&persistence.PrivacyRequestEvent{}).Where("id = ?", firstEvent.ID).
		Update("reason", "PostgreSQL immutable event mutation must fail.").Error; err == nil {
		t.Fatal("PostgreSQL allowed Privacy Request event mutation")
	}
	tenantExport, err := firstService.ExecuteTenantExport(
		ctx, adminPrincipal, domain.TenantID, "pg-tenant-export", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if tenantExport.Receipt.DigestSHA256 == "" || tenantExport.Receipt.ByteCount <= 0 ||
		len(tenantExport.Bundle.Users) != 2 || len(tenantExport.Bundle.PrivacyRequests) != 2 {
		t.Fatalf("unexpected PostgreSQL Tenant export: %#v", tenantExport)
	}
	if err := firstDB.Model(&persistence.TenantDataExport{}).Where("id = ?", tenantExport.Receipt.ID).
		Update("byte_count", tenantExport.Receipt.ByteCount+1).Error; err == nil {
		t.Fatal("PostgreSQL allowed Tenant data export receipt mutation")
	}
	if err := firstDB.Delete(&persistence.TenantDataExport{}, "id = ?", tenantExport.Receipt.ID).Error; err == nil {
		t.Fatal("PostgreSQL allowed Tenant data export receipt deletion")
	}
}
