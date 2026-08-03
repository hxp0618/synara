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

func TestSelfServiceTenantCreationCannotClaimEnterpriseProfileOrEvaluation(t *testing.T) {
	ctx, store, principal := newTenantProvisioningFixture(t)
	service := NewService(store.DB())

	_, err := service.CreateSelfServiceTenant(ctx, principal, CreateTenantInput{
		Slug: "forbidden-enterprise", Name: "Forbidden Enterprise", PlanCode: "enterprise", Status: "active",
	}, "self-service-enterprise", "127.0.0.1")
	assertTenantLifecycleProblem(t, err, "self_service_entitlement_profile_forbidden")

	trialEnd := time.Now().UTC().Add(14 * 24 * time.Hour)
	_, err = service.CreateSelfServiceTenant(ctx, principal, CreateTenantInput{
		Slug: "forbidden-evaluation", Name: "Forbidden Evaluation", PlanCode: "standard", Status: "evaluation",
		TrialExpiresAt: &trialEnd,
	}, "self-service-trial", "127.0.0.1")
	assertTenantLifecycleProblem(t, err, "self_service_status_forbidden")

	created, err := service.CreateSelfServiceTenant(ctx, principal, CreateTenantInput{
		Slug: "self-service-free", Name: "Self-service Free", Region: "default",
	}, "self-service-free", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if created.PlanCode != "standard" || created.Status != "active" || created.Role != "owner" {
		t.Fatalf("self-service Tenant = %#v", created)
	}
	var subscription persistence.TenantSubscription
	if err := store.DB().Where("tenant_id = ?", created.ID).Take(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	if subscription.PlanCode != "free" || subscription.AssignmentSource != "self_service" {
		t.Fatalf("self-service Subscription = %#v", subscription)
	}
}

func TestPlatformProvisioningAssignsExistingCustomerOwnerWithoutOperatorMembership(t *testing.T) {
	ctx, store, operator := newTenantProvisioningFixture(t)
	service := NewService(store.DB())
	now := time.Now().UTC().Truncate(time.Second)
	owner := persistence.User{
		ID: uuid.New(), Email: "customer-owner@example.com", DisplayName: "Customer Owner",
		Status: "active", EmailVerifiedAt: &now,
	}
	if err := store.DB().Create(&owner).Error; err != nil {
		t.Fatal(err)
	}

	result, err := service.ProvisionTenant(ctx, operator, ProvisionTenantInput{
		OwnerEmail: " CUSTOMER-OWNER@example.com ", Slug: "platform-enterprise",
		Name: "Platform Enterprise", Region: "eu-west", PlanCode: "enterprise", Status: "active",
	}, "platform-provision", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.PlanCode != "enterprise" || result.OwnerUserID != owner.ID ||
		result.OwnerEmail != "customer-owner@example.com" || result.LifecycleVersion != 1 {
		t.Fatalf("provisioned Tenant = %#v", result)
	}
	var customerMembership persistence.TenantMembership
	if err := store.DB().Where("tenant_id = ? AND user_id = ?", result.ID, owner.ID).
		Take(&customerMembership).Error; err != nil {
		t.Fatal(err)
	}
	if customerMembership.Role != "owner" || customerMembership.Status != "active" {
		t.Fatalf("customer membership = %#v", customerMembership)
	}
	var operatorMembershipCount int64
	if err := store.DB().Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", result.ID, operator.UserID).
		Count(&operatorMembershipCount).Error; err != nil {
		t.Fatal(err)
	}
	if operatorMembershipCount != 0 {
		t.Fatalf("Platform operator received %d customer memberships", operatorMembershipCount)
	}
	var subscription persistence.TenantSubscription
	if err := store.DB().Where("tenant_id = ?", result.ID).Take(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	if subscription.AssignmentSource != "platform_admin" {
		t.Fatalf("platform Subscription = %#v", subscription)
	}
	var auditEntry persistence.AuditLog
	if err := store.DB().Where("tenant_id = ? AND action = ?", result.ID, "tenant.created").
		Take(&auditEntry).Error; err != nil {
		t.Fatal(err)
	}
	if auditEntry.ActorID == nil || *auditEntry.ActorID != operator.UserID {
		t.Fatalf("platform provisioning Audit actor = %#v", auditEntry.ActorID)
	}

	_, err = service.ProvisionTenant(ctx, operator, ProvisionTenantInput{
		OwnerEmail: "missing@example.com", Slug: "missing-owner", Name: "Missing Owner",
		PlanCode: "enterprise", Status: "active",
	}, "platform-missing-owner", "127.0.0.1")
	assertTenantLifecycleProblem(t, err, "tenant_owner_not_found")
}

func newTenantProvisioningFixture(
	t *testing.T,
) (context.Context, database.MetadataStore, identity.Principal) {
	t.Helper()
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(
		ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(
		ctx, store.DB(), platform.ProfilePersonal, "tenant-provisioning-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
}
