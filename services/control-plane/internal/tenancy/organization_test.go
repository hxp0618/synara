package tenancy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestListOrganizationsIncludesCurrentUserRole(t *testing.T) {
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(
		ctx,
		platformConfig,
		"",
		filepath.Join(t.TempDir(), "metadata.sqlite"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(
		ctx,
		store.DB(),
		platform.ProfilePersonal,
		"organization-role-test-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatal(err)
	}

	service := NewService(store.DB())
	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	ownerOrganizations, err := service.ListOrganizations(ctx, owner, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerOrganizations) != 1 || ownerOrganizations[0].CurrentUserRole == nil ||
		*ownerOrganizations[0].CurrentUserRole != "owner" {
		t.Fatalf("owner Organization role was not projected: %#v", ownerOrganizations)
	}

	now := time.Now().UTC()
	memberID := uuid.New()
	if err := store.DB().Create(&persistence.User{
		ID: memberID, Email: memberID.String() + "@example.com", DisplayName: "Viewer",
		Status: "active", EmailVerifiedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.OrganizationMembership{
		TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, UserID: memberID,
		Role: "viewer", Status: "active",
	}).Error; err != nil {
		t.Fatal(err)
	}

	viewer := identity.Principal{UserID: memberID, ActiveTenantID: &domain.TenantID}
	viewerOrganizations, err := service.ListOrganizations(ctx, viewer, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(viewerOrganizations) != 1 || viewerOrganizations[0].CurrentUserRole == nil ||
		*viewerOrganizations[0].CurrentUserRole != "viewer" {
		t.Fatalf("viewer Organization role was not projected: %#v", viewerOrganizations)
	}
}
