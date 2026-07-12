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

	billingQuota, err := fixture.service.Get(ctx, fixture.billingAdmin, fixture.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if billingQuota.MaxConcurrentExecutions == nil || *billingQuota.MaxConcurrentExecutions != maxExecutions {
		t.Fatalf("billing admin did not read the tenant quota: %#v", billingQuota)
	}

	maxExecutions = 5
	updated, err = fixture.service.Put(ctx, fixture.billingAdmin, fixture.tenantID, PutInput{
		MaxConcurrentExecutions: &maxExecutions,
	}, "quota-billing-update", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.MaxConcurrentExecutions == nil || *updated.MaxConcurrentExecutions != maxExecutions || updated.MaxArtifactBytes != nil {
		t.Fatalf("billing admin quota update did not replace the limits: %#v", updated)
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

type quotaFixture struct {
	db           *gorm.DB
	service      *Service
	tenantID     uuid.UUID
	owner        identity.Principal
	billingAdmin identity.Principal
	member       identity.Principal
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
	billingAdminID := uuid.New()
	memberID := uuid.New()
	models := []any{
		&persistence.User{ID: billingAdminID, Email: uuid.NewString() + "@example.com", DisplayName: "Billing Admin", Status: "active", EmailVerifiedAt: &now},
		&persistence.User{ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Member", Status: "active", EmailVerifiedAt: &now},
		&persistence.TenantMembership{TenantID: domain.TenantID, UserID: billingAdminID, Role: "billing_admin", Status: "active", JoinedAt: &now},
		&persistence.TenantMembership{TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active", JoinedAt: &now},
	}
	for _, model := range models {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed quota fixture %T: %v", model, err)
		}
	}
	return quotaFixture{
		db: store.DB(), service: NewService(store.DB()), tenantID: domain.TenantID,
		owner:        identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		billingAdmin: identity.Principal{UserID: billingAdminID, ActiveTenantID: &domain.TenantID},
		member:       identity.Principal{UserID: memberID, ActiveTenantID: &domain.TenantID},
	}
}

func assertQuotaProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
