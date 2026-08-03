package quotas

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestTenantQuotaOperationsRejectInactiveTenantBeforeStorageAccess(t *testing.T) {
	ctx := context.Background()
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	service := NewService(nil)

	_, err := service.Get(ctx, principal, requestedTenantID)
	assertQuotaProblemCode(t, err, "tenant_not_found")
	_, err = service.Put(
		ctx, principal, requestedTenantID, PutInput{},
		"quota-inactive-update", "127.0.0.1",
	)
	assertQuotaProblemCode(t, err, "tenant_not_found")
}

func TestTenantQuotaAccessForOwnerAndBillingAdmin(t *testing.T) {
	fixture := newQuotaFixture(t)
	ctx := context.Background()
	maxExecutions := 3
	maxArtifactBytes := int64(1 << 30)

	updated, err := fixture.service.Put(ctx, fixture.owner, fixture.tenantID, PutInput{
		MaxConcurrentExecutions: &maxExecutions,
		MaxArtifactBytes:        &maxArtifactBytes,
	}, "quota-owner-update", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.MaxConcurrentExecutions == nil || *updated.MaxConcurrentExecutions != maxExecutions ||
		updated.MaxArtifactBytes == nil || *updated.MaxArtifactBytes != maxArtifactBytes {
		t.Fatalf("unexpected owner quota update: %#v", updated)
	}

	costQuota, err := fixture.service.Get(ctx, fixture.billingAdmin, fixture.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if costQuota.MaxConcurrentExecutions == nil || *costQuota.MaxConcurrentExecutions != maxExecutions {
		t.Fatalf("cost admin did not read the tenant quota: %#v", costQuota)
	}

	maxExecutions = 5
	updated, err = fixture.service.Put(ctx, fixture.billingAdmin, fixture.tenantID, PutInput{
		MaxConcurrentExecutions: &maxExecutions,
	}, "quota-billing-update", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.MaxConcurrentExecutions == nil || *updated.MaxConcurrentExecutions != maxExecutions || updated.MaxArtifactBytes != nil {
		t.Fatalf("cost admin quota update did not replace the limits: %#v", updated)
	}

	_, err = fixture.service.Get(ctx, fixture.member, fixture.tenantID)
	assertQuotaProblemCode(t, err, "tenant_forbidden")
	_, err = fixture.service.Put(ctx, fixture.member, fixture.tenantID, PutInput{}, "quota-member-update", "127.0.0.1")
	assertQuotaProblemCode(t, err, "tenant_forbidden")
}

func TestTenantQuotaRejectsInvalidLimits(t *testing.T) {
	fixture := newQuotaFixture(t)
	ctx := context.Background()

	invalidExecutions := 0
	_, err := fixture.service.Put(ctx, fixture.owner, fixture.tenantID, PutInput{
		MaxConcurrentExecutions: &invalidExecutions,
	}, "quota-invalid-executions", "127.0.0.1")
	assertQuotaProblemCode(t, err, "invalid_execution_quota")

	invalidArtifactBytes := int64(-1)
	_, err = fixture.service.Put(ctx, fixture.owner, fixture.tenantID, PutInput{
		MaxArtifactBytes: &invalidArtifactBytes,
	}, "quota-invalid-artifacts", "127.0.0.1")
	assertQuotaProblemCode(t, err, "invalid_artifact_quota")

	var quotaRows int64
	if err := fixture.db.Model(&persistence.TenantQuota{}).Where("tenant_id = ?", fixture.tenantID).Count(&quotaRows).Error; err != nil {
		t.Fatal(err)
	}
	if quotaRows != 0 {
		t.Fatalf("invalid quota input persisted %d rows", quotaRows)
	}
}

func TestScopedExecutionQuotaUsesExactScopeAndCAS(t *testing.T) {
	fixture := newQuotaFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	projectID := uuid.New()
	sessionID := uuid.New()
	automationID := uuid.New()
	models := []any{
		&persistence.Project{
			ID: projectID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
			Name: "Quota Project", DefaultBranch: "main", Visibility: "private",
			CreatedBy: fixture.owner.UserID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
			ProjectID: projectID, CreatedBy: fixture.owner.UserID, Title: "Quota Session",
			Status: "active", Visibility: "private", Provider: "codex",
			ExecutionTargetID: fixture.executionTargetID, RequestedExecutionTargetID: fixture.executionTargetID,
			CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Automation{
			ID: automationID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
			ProjectID: projectID, CreatedBy: fixture.owner.UserID, Name: "Quota Automation",
			Prompt: "run", Schedule: "0 * * * *", Timezone: "UTC", Status: "active",
			CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatalf("seed scoped quota %T: %v", model, err)
		}
	}

	maxConcurrent := 2
	maxQueued := 4
	maxUnits := int64(8)
	created, err := fixture.service.PutScoped(
		ctx, fixture.owner, fixture.tenantID, ScopeAutomation, automationID,
		PutScopedInput{
			MaxConcurrentExecutions: &maxConcurrent, MaxQueuedExecutions: &maxQueued,
			MaxConcurrentExecutionUnits: &maxUnits,
		},
		"quota-scope-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || created.ScopeID != automationID || created.ScopeKind != ScopeAutomation {
		t.Fatalf("created scoped quota = %#v", created)
	}

	stale := int64(0)
	_, err = fixture.service.PutScoped(
		ctx, fixture.owner, fixture.tenantID, ScopeAutomation, automationID,
		PutScopedInput{ExpectedVersion: &stale, MaxConcurrentExecutions: &maxConcurrent},
		"quota-scope-stale", "127.0.0.1",
	)
	assertQuotaProblemCode(t, err, "execution_quota_policy_version_conflict")

	expected := created.Version
	updated, err := fixture.service.PutScoped(
		ctx, fixture.owner, fixture.tenantID, ScopeAutomation, automationID,
		PutScopedInput{ExpectedVersion: &expected, MaxConcurrentExecutions: &maxConcurrent},
		"quota-scope-update", "127.0.0.1",
	)
	if err != nil || updated.Version != 2 || updated.MaxQueuedExecutions != nil || updated.MaxConcurrentExecutionUnits != nil {
		t.Fatalf("updated scoped quota = %#v, %v", updated, err)
	}

	loaded, err := fixture.service.GetScoped(ctx, fixture.owner, fixture.tenantID, ScopeAutomation, automationID)
	if err != nil || loaded.Version != updated.Version {
		t.Fatalf("loaded scoped quota = %#v, %v", loaded, err)
	}

	expected = updated.Version
	cleared, err := fixture.service.PutScoped(
		ctx, fixture.owner, fixture.tenantID, ScopeAutomation, automationID,
		PutScopedInput{ExpectedVersion: &expected}, "quota-scope-clear", "127.0.0.1",
	)
	if err != nil || cleared.Version != 0 {
		t.Fatalf("cleared scoped quota = %#v, %v", cleared, err)
	}
}

type quotaFixture struct {
	db                *gorm.DB
	service           *Service
	tenantID          uuid.UUID
	organizationID    uuid.UUID
	executionTargetID uuid.UUID
	owner             identity.Principal
	billingAdmin      identity.Principal
	member            identity.Principal
}

func newQuotaFixture(t *testing.T) quotaFixture {
	t.Helper()
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "quota-test-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	costAdminID := uuid.New()
	memberID := uuid.New()
	models := []any{
		&persistence.User{ID: costAdminID, Email: uuid.NewString() + "@example.com", DisplayName: "Cost Admin", Status: "active", EmailVerifiedAt: &now},
		&persistence.User{ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Member", Status: "active", EmailVerifiedAt: &now},
		&persistence.TenantMembership{TenantID: domain.TenantID, UserID: costAdminID, Role: "cost_admin", Status: "active", JoinedAt: &now},
		&persistence.TenantMembership{TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active", JoinedAt: &now},
	}
	for _, model := range models {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed quota fixture %T: %v", model, err)
		}
	}
	return quotaFixture{
		db: store.DB(), service: NewService(store.DB()), tenantID: domain.TenantID,
		organizationID:    domain.OrganizationID,
		executionTargetID: domain.ExecutionTargetID,
		owner:             identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		billingAdmin:      identity.Principal{UserID: costAdminID, ActiveTenantID: &domain.TenantID},
		member:            identity.Principal{UserID: memberID, ActiveTenantID: &domain.TenantID},
	}
}

func assertQuotaProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
