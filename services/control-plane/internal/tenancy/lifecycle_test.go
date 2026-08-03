package tenancy

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
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestTenantLifecycleTransitionsDeletionRecoveryAndAudit(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "tenant-lifecycle-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB())
	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	trialExpiry := time.Now().UTC().Add(14 * 24 * time.Hour)
	tenant, err := service.CreateTenant(ctx, owner, CreateTenantInput{
		Slug: "trial-" + uuid.NewString()[:8], Name: "Lifecycle trial", Region: "default", PlanCode: "free",
		Status: "evaluation", TrialExpiresAt: &trialExpiry,
	}, "tenant-register", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if tenant.Status != "evaluation" || tenant.PlanCode != "standard" || tenant.LifecycleVersion != 1 || tenant.TrialExpiresAt == nil {
		t.Fatalf("registered Tenant lifecycle = %#v", tenant)
	}
	owner.ActiveTenantID = &tenant.ID

	tenant = transitionTenantForTest(t, service, ctx, owner, tenant, "active", "trial converted")
	if tenant.LifecycleVersion != 2 || tenant.TrialExpiresAt != nil {
		t.Fatalf("activated Tenant lifecycle = %#v", tenant)
	}
	_, err = service.TransitionTenant(ctx, owner, tenant.ID, TransitionTenantInput{
		ToStatus: "suspended", ExpectedVersion: 1, Reason: "stale operator view",
	}, "tenant-stale", "127.0.0.1")
	assertTenantLifecycleProblem(t, err, "tenant_lifecycle_version_conflict")

	tenant = transitionTenantForTest(t, service, ctx, owner, tenant, "suspended", "billing review")
	if tenant.SuspendedAt == nil {
		t.Fatalf("suspended Tenant omitted timestamp: %#v", tenant)
	}
	tenant = transitionTenantForTest(t, service, ctx, owner, tenant, "active", "billing cleared")
	tenant = transitionTenantForTest(t, service, ctx, owner, tenant, "closed", "customer requested closure")
	if tenant.ClosedAt == nil {
		t.Fatalf("closed Tenant omitted timestamp: %#v", tenant)
	}
	tenant = transitionTenantForTest(t, service, ctx, owner, tenant, "active", "customer resumed service")

	status := "suspended"
	_, err = service.UpdateTenant(ctx, owner, tenant.ID, UpdateTenantInput{Status: &status}, "legacy-status", "127.0.0.1")
	assertTenantLifecycleProblem(t, err, "tenant_lifecycle_transition_required")

	versionBeforeDelete := tenant.LifecycleVersion
	if err := service.RequestTenantDeletion(ctx, owner, tenant.ID, DeleteTenantInput{
		ExpectedVersion: versionBeforeDelete - 1, Reason: "stale deletion request",
	}, "tenant-delete-stale", "127.0.0.1"); err == nil {
		t.Fatal("stale Tenant deletion request succeeded")
	} else {
		assertTenantLifecycleProblem(t, err, "tenant_lifecycle_version_conflict")
	}
	if err := service.RequestTenantDeletion(ctx, owner, tenant.ID, DeleteTenantInput{
		ExpectedVersion: versionBeforeDelete, Reason: "customer requested deletion",
	}, "tenant-delete", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var deleting persistence.Tenant
	if err := store.DB().Where("id = ?", tenant.ID).Take(&deleting).Error; err != nil {
		t.Fatal(err)
	}
	if deleting.Status != "deleting" || deleting.DeletedAt == nil || deleting.LifecycleVersion != versionBeforeDelete+1 {
		t.Fatalf("deleting Tenant lifecycle = %#v", deleting)
	}
	deletionRequests, err := service.ListDeletingTenants(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletionRequests) != 1 || deletionRequests[0].ID != tenant.ID ||
		deletionRequests[0].DeletionRequestedAt == nil || deletionRequests[0].Role != "owner" {
		t.Fatalf("deletion recovery inventory = %#v", deletionRequests)
	}
	owner.ActiveTenantID = &domain.TenantID
	restored, err := service.RestoreTenant(ctx, owner, tenant.ID, RestoreTenantInput{
		ExpectedVersion: deleting.LifecycleVersion, Reason: "deletion request withdrawn",
	}, "tenant-restore", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != "closed" || restored.ClosedAt == nil || restored.LifecycleVersion != deleting.LifecycleVersion+1 {
		t.Fatalf("restored Tenant lifecycle = %#v", restored)
	}
	deletionRequests, err = service.ListDeletingTenants(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletionRequests) != 0 {
		t.Fatalf("restored Tenant remained in deletion recovery inventory: %#v", deletionRequests)
	}

	var lifecycleAuditCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action IN ?", tenant.ID, []string{"tenant.lifecycle_transitioned", "tenant.deleted", "tenant.restored"}).
		Count(&lifecycleAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if lifecycleAuditCount != 7 {
		t.Fatalf("Tenant lifecycle audit rows = %d, want 7", lifecycleAuditCount)
	}
}

func TestTenantLifecycleRejectsInvalidRegistrationAndNonOwnerTransition(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "tenant-lifecycle-auth-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB())
	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	_, err = service.CreateTenant(ctx, owner, CreateTenantInput{
		Slug: "expired-" + uuid.NewString()[:8], Name: "Expired evaluation", Status: "evaluation",
		TrialExpiresAt: timePointer(time.Now().UTC().Add(-time.Minute)),
	}, "tenant-expired", "127.0.0.1")
	assertTenantLifecycleProblem(t, err, "invalid_tenant_evaluation_expiry")

	tenant, err := service.CreateTenant(ctx, owner, CreateTenantInput{
		Slug: "auth-" + uuid.NewString()[:8], Name: "Lifecycle authorization", Status: "active",
	}, "tenant-auth", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	memberID := uuid.New()
	now := time.Now().UTC()
	if err := store.DB().Create(&persistence.User{
		ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Lifecycle member",
		Status: "active", EmailVerifiedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: tenant.ID, UserID: memberID, Role: "admin", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	owner.ActiveTenantID = &tenant.ID
	_, err = service.TransitionTenant(ctx, identity.Principal{UserID: memberID, ActiveTenantID: &tenant.ID}, tenant.ID, TransitionTenantInput{
		ToStatus: "suspended", ExpectedVersion: tenant.LifecycleVersion, Reason: "unauthorized suspension",
	}, "tenant-forbidden", "127.0.0.1")
	assertTenantLifecycleProblem(t, err, "tenant_status_forbidden")
	if err := service.RequestTenantDeletion(ctx, owner, tenant.ID, DeleteTenantInput{
		ExpectedVersion: tenant.LifecycleVersion, Reason: "owner-only recovery inventory",
	}, "tenant-delete-owner-only", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	adminRequests, err := service.ListDeletingTenants(ctx, identity.Principal{UserID: memberID})
	if err != nil {
		t.Fatal(err)
	}
	if len(adminRequests) != 0 {
		t.Fatalf("non-owner saw Tenant deletion requests: %#v", adminRequests)
	}
}

func transitionTenantForTest(
	t *testing.T,
	service *Service,
	ctx context.Context,
	principal identity.Principal,
	tenant Tenant,
	toStatus string,
	reason string,
) Tenant {
	t.Helper()
	updated, err := service.TransitionTenant(ctx, principal, tenant.ID, TransitionTenantInput{
		ToStatus: toStatus, ExpectedVersion: tenant.LifecycleVersion, Reason: reason,
	}, "tenant-transition-"+toStatus, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != toStatus || updated.LifecycleVersion != tenant.LifecycleVersion+1 {
		t.Fatalf("Tenant transition %s -> %s returned %#v", tenant.Status, toStatus, updated)
	}
	return updated
}

func assertTenantLifecycleProblem(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
