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
