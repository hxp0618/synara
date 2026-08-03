package tenancy

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

func TestTenantAndOrganizationOperationsRejectInactiveTenantContext(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "tenant-active-context-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB())
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	other, err := service.CreateTenant(ctx, principal, CreateTenantInput{
		Slug: "inactive-" + uuid.NewString()[:8], Name: "Inactive context Tenant", PlanCode: "free", Status: "active",
	}, "tenant-inactive-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var root persistence.Organization
	if err := store.DB().Where("tenant_id = ? AND kind = ?", other.ID, "root").Take(&root).Error; err != nil {
		t.Fatal(err)
	}

	_, err = service.GetTenant(ctx, principal, other.ID)
	assertActiveTenantProblem(t, err)
	name := "Cross-context mutation"
	_, err = service.UpdateTenant(ctx, principal, other.ID, UpdateTenantInput{Name: &name}, "tenant-inactive-update", "127.0.0.1")
	assertActiveTenantProblem(t, err)
	_, err = service.ListTenantMembers(ctx, principal, other.ID)
	assertActiveTenantProblem(t, err)
	_, err = service.ListAuditLogs(ctx, principal, other.ID, AuditLogQuery{})
	assertActiveTenantProblem(t, err)
	_, err = service.ListOrganizations(ctx, principal, other.ID)
	assertActiveTenantProblem(t, err)
	_, err = service.GetOrganization(ctx, principal, other.ID, root.ID)
	assertActiveTenantProblem(t, err)
	_, err = service.CreateOrganization(
		ctx, principal, other.ID, CreateOrganizationInput{},
		"tenant-inactive-organization-create", "127.0.0.1",
	)
	assertActiveTenantProblem(t, err)
	_, err = service.UpdateOrganization(
		ctx, principal, other.ID, root.ID, UpdateOrganizationInput{},
		"tenant-inactive-organization-update", "127.0.0.1",
	)
	assertActiveTenantProblem(t, err)
	err = service.ArchiveOrganization(
		ctx, principal, other.ID, root.ID,
		"tenant-inactive-organization-archive", "127.0.0.1",
	)
	assertActiveTenantProblem(t, err)
	_, err = service.ListOrganizationMembers(ctx, principal, other.ID, root.ID)
	assertActiveTenantProblem(t, err)
	_, err = service.PutOrganizationMember(
		ctx, principal, other.ID, root.ID, PutOrganizationMemberInput{},
		"tenant-inactive-organization-member-put", "127.0.0.1",
	)
	assertActiveTenantProblem(t, err)
	_, err = service.UpdateOrganizationMember(
		ctx, principal, other.ID, root.ID, uuid.New(), UpdateOrganizationMemberInput{},
		"tenant-inactive-organization-member-update", "127.0.0.1",
	)
	assertActiveTenantProblem(t, err)
	err = service.RemoveOrganizationMember(
		ctx, principal, other.ID, root.ID, uuid.New(),
		"tenant-inactive-organization-member-remove", "127.0.0.1",
	)
	assertActiveTenantProblem(t, err)
	err = service.RequestTenantDeletion(
		ctx, principal, other.ID, DeleteTenantInput{
			ExpectedVersion: other.LifecycleVersion,
			Reason:          "Inactive Tenant deletion must be rejected before mutation.",
		},
		"tenant-inactive-delete", "127.0.0.1",
	)
	assertActiveTenantProblem(t, err)
	_, err = service.TransitionTenant(ctx, principal, other.ID, TransitionTenantInput{
		ToStatus: "suspended", ExpectedVersion: other.LifecycleVersion,
		Reason: "Cross-context lifecycle mutation must be rejected.",
	}, "tenant-inactive-transition", "127.0.0.1")
	assertActiveTenantProblem(t, err)

	var persisted persistence.Tenant
	if err := store.DB().Where("id = ?", other.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Name != other.Name || persisted.Status != "active" || persisted.LifecycleVersion != other.LifecycleVersion {
		t.Fatalf("inactive Tenant context mutated target Tenant: %#v", persisted)
	}

	principal.ActiveTenantID = &other.ID
	loaded, err := service.GetTenant(ctx, principal, other.ID)
	if err != nil || loaded.ID != other.ID {
		t.Fatalf("active Tenant context GetTenant = %#v, %v", loaded, err)
	}
	organizations, err := service.ListOrganizations(ctx, principal, other.ID)
	if err != nil || len(organizations) != 1 || organizations[0].ID != root.ID {
		t.Fatalf("active Tenant context Organizations = %#v, %v", organizations, err)
	}
	unrelatedTenantID := uuid.New()
	if err := store.DB().Create(&persistence.Tenant{
		ID: unrelatedTenantID, Slug: "unrelated-" + uuid.NewString()[:8], Name: "Unrelated Tenant",
		Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	tenants, err := service.ListTenants(ctx, principal)
	if err != nil || len(tenants) != 2 {
		t.Fatalf("membership-filtered Tenant list = %#v, %v", tenants, err)
	}
	for _, tenant := range tenants {
		if tenant.ID == unrelatedTenantID {
			t.Fatalf("Tenant list exposed a Tenant without Membership: %#v", tenant)
		}
	}
}

func assertActiveTenantProblem(t *testing.T, err error) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "tenant_not_found" {
		t.Fatalf("inactive Tenant context error = %v, want tenant_not_found", err)
	}
}
