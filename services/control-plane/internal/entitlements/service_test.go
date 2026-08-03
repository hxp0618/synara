package entitlements

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestEntitlementReadRejectsInactiveTenantBeforeStorageAccess(t *testing.T) {
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	_, err := NewService(nil).Get(context.Background(), identity.Principal{
		UserID: uuid.New(), ActiveTenantID: &activeTenantID,
	}, requestedTenantID)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "tenant_not_found" {
		t.Fatalf("inactive Tenant Entitlement read error = %v", err)
	}
}

func TestResolvePlanEntitlementsFeaturesAndSubscription(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "entitlements-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB())
	featureEnabled := true
	if err := store.DB().Create(&persistence.PlanEntitlement{
		PlanCode: "personal", Key: "usage.cost_visibility", ValueKind: "boolean", BooleanValue: &featureEnabled,
	}).Error; err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	if _, err := service.SetFeatureOverrideByPlatform(ctx, domain.UserID, domain.TenantID, SetFeatureOverrideInput{
		FlagKey: "identity.sso", Enabled: false, ExpectedVersion: 0,
		Reason: "controlled rollout", ExpiresAt: &future,
	}, "feature-override", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Get(ctx, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
	}, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Plan.Code != "personal" || snapshot.Subscription.Status != "active" ||
		snapshot.Subscription.AssignmentSource != "migration" || snapshot.Entitlements["usage.cost_visibility"].Boolean == nil ||
		!*snapshot.Entitlements["usage.cost_visibility"].Boolean || !snapshot.Subscription.EntitlementsActive ||
		snapshot.Subscription.EntitlementGrace || snapshot.Subscription.EntitlementPolicy != "effective-subscription-v1" ||
		snapshot.Features["identity.sso"] {
		t.Fatalf("unexpected Entitlement snapshot: %#v", snapshot)
	}
	if err := store.DB().Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", domain.TenantID).
		Update("status", "past_due").Error; err == nil {
		t.Fatal("internal self-hosted Subscription accepted payment-derived past_due state")
	}
	if err := store.DB().Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", domain.TenantID).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	suspended, err := service.Resolve(ctx, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Subscription.EntitlementsActive || suspended.Subscription.EntitlementGrace ||
		len(suspended.Entitlements) != 0 || suspended.Subscription.EntitlementDecision != "internally_suspended" {
		t.Fatalf("suspended Subscription retained Entitlements: %#v", suspended)
	}
	if err := store.DB().Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", domain.TenantID).
		Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return future.Add(time.Second) }
	expired, err := service.Resolve(ctx, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if !expired.Features["identity.sso"] {
		t.Fatalf("expired Feature Flag override remained active: %#v", expired.Features)
	}

	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	created, err := tenancy.NewService(store.DB()).CreateTenant(ctx, owner, tenancy.CreateTenantInput{
		Slug: "entitled-" + uuid.NewString()[:8], Name: "Entitled Tenant", PlanCode: "free", Status: "active",
	}, "entitled-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	createdSnapshot, err := service.Resolve(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if createdSnapshot.Plan.Code != "free" || createdSnapshot.Subscription.AssignmentSource != "self_service" ||
		createdSnapshot.Subscription.Version != 1 {
		t.Fatalf("new Tenant Subscription = %#v", createdSnapshot)
	}
	periodStart := time.Now().UTC()
	assigned, err := service.AssignPlanByPlatform(ctx, domain.UserID, created.ID, AssignPlanInput{
		PlanCode: "enterprise", Status: "active", ExpectedVersion: 1,
		CurrentPeriodStart: periodStart, CurrentPeriodEnd: periodStart.AddDate(0, 1, 0),
		AssignmentSource: "platform_admin", Reason: "approved enterprise contract",
	}, "assign-enterprise", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if assigned.Plan.Code != "enterprise" || assigned.Subscription.Version != 2 {
		t.Fatalf("assigned Tenant Plan = %#v", assigned)
	}
	_, err = service.AssignPlanByPlatform(ctx, domain.UserID, created.ID, AssignPlanInput{
		PlanCode: "free", Status: "active", ExpectedVersion: 1,
		CurrentPeriodStart: periodStart, CurrentPeriodEnd: periodStart.AddDate(0, 1, 0),
		AssignmentSource: "platform_admin", Reason: "stale assignment",
	}, "assign-stale", "127.0.0.1")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "tenant_entitlement_profile_version_conflict" {
		t.Fatalf("stale Plan assignment error = %v", err)
	}
	if err := store.DB().Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", created.ID).
		Updates(map[string]any{
			"assignment_source":        "billing_provider",
			"external_customer_id":     "customer-managed",
			"external_subscription_id": "subscription-managed",
			"billing_provider":         "stripe",
			"external_price_id":        "price-managed",
			"provider_observed_at":     periodStart,
		}).Error; err == nil {
		t.Fatal("internal self-hosted Subscription accepted payment-provider state")
	}
}
