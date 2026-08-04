package projects

import (
	"context"
	"errors"
	"path/filepath"
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

func TestProjectCreateIdempotencyReplaysAndRejectsHashConflict(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "project-idempotency-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	service := NewService(store.DB())
	input := CreateProjectInput{Name: "Idempotent Project", DefaultBranch: "main", Visibility: "organization"}

	first, replayed, err := service.CreateWithIdempotency(
		ctx, principal, domain.TenantID, domain.OrganizationID, input, "project-key", "project-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("first Project creation was marked as replayed")
	}
	second, replayed, err := service.CreateWithIdempotency(
		ctx, principal, domain.TenantID, domain.OrganizationID, input, "project-key", "project-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || second.ID != first.ID {
		t.Fatalf("Project replay mismatch: first=%#v second=%#v replayed=%t", first, second, replayed)
	}

	conflicting := input
	conflicting.Name = "Different Project"
	_, _, err = service.CreateWithIdempotency(
		ctx, principal, domain.TenantID, domain.OrganizationID, conflicting,
		"project-key", "project-conflict", "127.0.0.1",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "idempotency_conflict" {
		t.Fatalf("expected idempotency_conflict, got %v", err)
	}

	var projects int64
	if err := store.DB().Model(&persistence.Project{}).
		Where("tenant_id = ? AND name = ?", domain.TenantID, input.Name).Count(&projects).Error; err != nil {
		t.Fatal(err)
	}
	if projects != 1 {
		t.Fatalf("idempotent request persisted %d Projects", projects)
	}
}

func TestProjectUpdateAndArchiveIdempotency(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "project-mutation-idempotency-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	service := NewService(store.DB())
	created, err := service.Create(
		ctx, principal, domain.TenantID, domain.OrganizationID,
		CreateProjectInput{Name: "Mutable Project", DefaultBranch: "main", Visibility: "organization"},
		"project-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}

	name := "Updated Project"
	update := UpdateProjectInput{Name: &name}
	first, replayed, err := service.UpdateWithIdempotency(
		ctx, principal, domain.TenantID, created.ID, update, "project-update-key", "project-update-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || first.Name != name {
		t.Fatalf("unexpected first update: %#v replayed=%t", first, replayed)
	}
	second, replayed, err := service.UpdateWithIdempotency(
		ctx, principal, domain.TenantID, created.ID, update, "project-update-key", "project-update-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || second.ID != first.ID || second.Name != first.Name {
		t.Fatalf("Project update replay mismatch: first=%#v second=%#v replayed=%t", first, second, replayed)
	}
	conflictingName := "Conflicting Project"
	_, _, err = service.UpdateWithIdempotency(
		ctx, principal, domain.TenantID, created.ID, UpdateProjectInput{Name: &conflictingName},
		"project-update-key", "project-update-conflict", "127.0.0.1",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "idempotency_conflict" {
		t.Fatalf("expected update idempotency_conflict, got %v", err)
	}

	archived, replayed, err := service.ArchiveWithIdempotency(
		ctx, principal, domain.TenantID, created.ID, "project-archive-key", "project-archive-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || archived.ArchivedAt == nil {
		t.Fatalf("unexpected first archive: %#v replayed=%t", archived, replayed)
	}
	replayedArchive, replayed, err := service.ArchiveWithIdempotency(
		ctx, principal, domain.TenantID, created.ID, "project-archive-key", "project-archive-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || replayedArchive.ID != archived.ID || replayedArchive.ArchivedAt == nil {
		t.Fatalf("Project archive replay mismatch: first=%#v second=%#v replayed=%t", archived, replayedArchive, replayed)
	}

	for _, action := range []string{"project.updated", "project.archived"} {
		var count int64
		if err := store.DB().Model(&persistence.AuditLog{}).
			Where("tenant_id = ? AND resource_id = ? AND action = ?", domain.TenantID, created.ID, action).
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("expected one %s audit record, got %d", action, count)
		}
	}
}
