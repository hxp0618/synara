package supportaccess

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestTenantSupportPolicyRejectsInactiveTenantBeforeStorageAccess(t *testing.T) {
	service := NewService(nil, uuid.New())
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}

	for name, operation := range map[string]func() error{
		"get policy": func() error {
			_, err := service.GetPolicy(context.Background(), principal, requestedTenantID)
			return err
		},
		"update policy": func() error {
			_, err := service.UpdatePolicy(
				context.Background(), principal, requestedTenantID, UpdatePolicyInput{},
				"inactive-support-policy", "127.0.0.1",
			)
			return err
		},
		"list history": func() error {
			_, err := service.ListTenant(context.Background(), principal, requestedTenantID)
			return err
		},
		"get operations overview": func() error {
			_, err := service.GetTenantOverview(context.Background(), principal, requestedTenantID)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			var apiError *problem.Error
			err := operation()
			if !errors.As(err, &apiError) || apiError.Code != "tenant_not_found" {
				t.Fatalf("inactive Tenant Support operation error = %v", err)
			}
		})
	}
}

func TestPlatformSupportOperationsRejectCustomerContextBeforeStorageAccess(t *testing.T) {
	service := NewService(nil, uuid.New())
	customerTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &customerTenantID}

	for name, operation := range map[string]func() error{
		"list Grants": func() error {
			_, err := service.ListPlatform(context.Background(), principal)
			return err
		},
		"list Tenant operations": func() error {
			_, err := service.ListPlatformTenants(context.Background(), principal)
			return err
		},
		"require Platform Admin": func() error {
			_, err := service.RequirePlatformAdmin(context.Background(), principal)
			return err
		},
		"require Platform Operator": func() error {
			_, err := service.RequirePlatformOperator(context.Background(), principal)
			return err
		},
		"deny Grant": func() error {
			_, err := service.Deny(
				context.Background(), principal, uuid.New(), DecisionInput{},
				"support-customer-deny", "127.0.0.1",
			)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			var apiError *problem.Error
			err := operation()
			if !errors.As(err, &apiError) || apiError.Code != "platform_operator_forbidden" {
				t.Fatalf("customer-context Platform Support operation error = %v", err)
			}
		})
	}
}

func TestControlledSupportAccessRequiresFourEyesAndRemainsReadOnly(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "support-access-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	supportUser := persistence.User{
		ID: uuid.New(), Email: "support@example.com", DisplayName: "Support Engineer",
		Status: "active", EmailVerifiedAt: &now,
	}
	customerUser := persistence.User{
		ID: uuid.New(), Email: "customer@example.com", DisplayName: "Customer Owner",
		Status: "active", EmailVerifiedAt: &now,
	}
	for _, user := range []persistence.User{supportUser, customerUser} {
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: supportUser.ID, Role: "security_admin",
		Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	customerPrincipal := identity.Principal{UserID: customerUser.ID}
	customerTenant, err := tenancy.NewService(store.DB()).CreateTenant(ctx, customerPrincipal, tenancy.CreateTenantInput{
		Slug: "support-customer-" + uuid.NewString()[:8], Name: "Support Customer", PlanCode: "free", Status: "active",
	}, "support-customer-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	customerPrincipal.ActiveTenantID = &customerTenant.ID
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	policy, err := service.UpdatePolicy(ctx, customerPrincipal, customerTenant.ID, UpdatePolicyInput{
		SupportAccessEnabled: true, ExpectedVersion: 0,
		Reason: "Enable audited support access for this incident.",
	}, "support-policy-enable", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !policy.SupportAccessEnabled || policy.Version != 1 {
		t.Fatalf("unexpected support policy: %#v", policy)
	}
	supportPrincipal := identity.Principal{
		UserID: supportUser.ID, SessionID: uuid.New(), ActiveTenantID: &domain.TenantID,
	}
	grant, err := service.Request(ctx, supportPrincipal, RequestInput{
		TenantID: customerTenant.ID, Reason: "Investigate the customer queue delay without changing data.",
		RequestedDurationSeconds: 30 * 60,
	}, "support-request", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if grant.Status != "pending" || grant.Version != 1 {
		t.Fatalf("unexpected pending support grant: %#v", grant)
	}
	if _, err := service.Approve(ctx, supportPrincipal, grant.ID, DecisionInput{
		ExpectedVersion: 1, Reason: "Self approval must be rejected by the service.",
	}, "support-self-approve", "127.0.0.1"); err == nil {
		t.Fatal("support requester approved their own access")
	}
	adminPrincipal := identity.Principal{
		UserID: domain.UserID, SessionID: uuid.New(), ActiveTenantID: &domain.TenantID,
	}
	grant, err = service.Approve(ctx, adminPrincipal, grant.ID, DecisionInput{
		ExpectedVersion: 1, Reason: "Approved for read-only queue and execution diagnostics.",
	}, "support-approve", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if grant.Status != "active" || grant.Version != 2 || grant.ExpiresAt == nil ||
		!grant.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("unexpected active support grant: %#v", grant)
	}
	otherCustomerTenant, err := tenancy.NewService(store.DB()).CreateTenant(
		ctx, customerPrincipal,
		tenancy.CreateTenantInput{
			Slug: "support-other-" + uuid.NewString()[:8], Name: "Support Other",
			PlanCode: "free", Status: "active",
		},
		"support-other-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	otherCustomerPrincipal := customerPrincipal
	otherCustomerPrincipal.ActiveTenantID = &otherCustomerTenant.ID
	if _, err := service.UpdatePolicy(ctx, otherCustomerPrincipal, otherCustomerTenant.ID, UpdatePolicyInput{
		SupportAccessEnabled: true, ExpectedVersion: 0,
		Reason: "Enable audited support access for the second customer.",
	}, "support-other-policy", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	otherGrant, err := service.Request(ctx, supportPrincipal, RequestInput{
		TenantID:                 otherCustomerTenant.ID,
		Reason:                   "Investigate the second customer without changing any customer data.",
		RequestedDurationSeconds: 30 * 60,
	}, "support-other-request", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	otherGrant, err = service.Approve(ctx, adminPrincipal, otherGrant.ID, DecisionInput{
		ExpectedVersion: 1, Reason: "Approve the isolated second-customer read-only investigation.",
	}, "support-other-approve", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Revoke(ctx, customerPrincipal, otherGrant.ID, DecisionInput{
		ExpectedVersion: otherGrant.Version,
		Reason:          "Cross-Tenant revocation must not reach the other grant.",
	}, "support-cross-tenant-revoke", "127.0.0.1")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "tenant_not_found" {
		t.Fatalf("cross-Tenant Support Grant revoke error = %v", err)
	}
	var retainedOtherGrant persistence.SupportAccessGrant
	if err := store.DB().Where("id = ?", otherGrant.ID).Take(&retainedOtherGrant).Error; err != nil {
		t.Fatal(err)
	}
	if retainedOtherGrant.Status != "active" || retainedOtherGrant.Version != otherGrant.Version {
		t.Fatalf("cross-Tenant revoke mutated Support Grant: %#v", retainedOtherGrant)
	}
	authorizer := authorization.NewAuthorizer(store.DB())
	if role, err := authorizer.RequireTenant(ctx, supportUser.ID, customerTenant.ID, authorization.TenantRead); err != nil || role != authorization.SupportReadOnlyRole {
		t.Fatalf("support read authorization = %q, %v", role, err)
	}
	if _, err := authorizer.RequireTenant(ctx, supportUser.ID, customerTenant.ID, authorization.TenantUpdate); err == nil {
		t.Fatal("support read-only role authorized a Tenant update")
	}
	supportToken := "support-session-token"
	supportTokenHash := sha256.Sum256([]byte(supportToken))
	if err := store.DB().Create(&persistence.LoginSession{
		ID: supportPrincipal.SessionID, UserID: supportUser.ID, ActiveTenantID: &domain.TenantID,
		AuthMethod: "local", RefreshTokenHash: supportTokenHash[:],
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	identityService := identity.NewService(store.DB(), time.Hour, time.Hour)
	switched, err := identityService.SetActiveTenant(ctx, supportPrincipal, customerTenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if switched.SupportAccessGrantID == nil || *switched.SupportAccessGrantID != grant.ID {
		t.Fatalf("support Tenant switch did not bind the active grant: %#v", switched)
	}
	state, err := identityService.GetSessionState(ctx, switched)
	if err != nil {
		t.Fatal(err)
	}
	foundSupportTenant := false
	for _, access := range state.Tenants {
		if access.ID == customerTenant.ID && access.Role == authorization.SupportReadOnlyRole {
			foundSupportTenant = true
		}
	}
	if !foundSupportTenant {
		t.Fatalf("active Support Access Tenant missing from Session state: %#v", state.Tenants)
	}
	supportPrincipal.ActiveTenantID = &customerTenant.ID
	supportPrincipal.SupportAccessGrantID = &grant.ID
	if err := service.RecordAccess(ctx, supportPrincipal, "GET", "GET /v1/tenants/{tenantID}/workers", "support-read", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, supportUser.ID).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.RequireTenant(ctx, supportUser.ID, customerTenant.ID, authorization.TenantRead); err == nil {
		t.Fatal("offboarded Platform Support user retained customer Tenant read access")
	}
	if err := service.RecordAccess(ctx, supportPrincipal, "GET", "GET /v1/tenants/{tenantID}/workers", "support-offboarded-read", "127.0.0.1"); err == nil {
		t.Fatal("offboarded Platform Support user retained an active Support Access grant")
	}
	authenticatedAfterOffboarding, err := identityService.Authenticate(ctx, supportToken)
	if err != nil {
		t.Fatal(err)
	}
	if authenticatedAfterOffboarding.ActiveTenantID != nil || authenticatedAfterOffboarding.SupportAccessGrantID != nil {
		t.Fatalf("offboarded Platform Support session retained customer context: %#v", authenticatedAfterOffboarding)
	}
	if err := store.DB().Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, supportUser.ID).
		Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	items, err := service.ListTenant(ctx, customerPrincipal, customerTenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].RequesterEmail != supportUser.Email {
		t.Fatalf("customer-visible support access = %#v", items)
	}
	policy, err = service.UpdatePolicy(ctx, customerPrincipal, customerTenant.ID, UpdatePolicyInput{
		SupportAccessEnabled: false, ExpectedVersion: 1,
		Reason: "Disable support access after the incident investigation completed.",
	}, "support-policy-disable", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if policy.SupportAccessEnabled || policy.Version != 2 {
		t.Fatalf("unexpected disabled support policy: %#v", policy)
	}
	if _, err := authorizer.RequireTenant(ctx, supportUser.ID, customerTenant.ID, authorization.TenantRead); err == nil {
		t.Fatal("disabled Tenant policy left Support Access authorized")
	}
	authenticated, err := identityService.Authenticate(ctx, supportToken)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.ActiveTenantID != nil || authenticated.SupportAccessGrantID != nil {
		t.Fatalf("revoked Support Access remained active in the Login Session: %#v", authenticated)
	}
	var persisted persistence.SupportAccessGrant
	if err := store.DB().Where("id = ?", grant.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "revoked" || persisted.Version != 3 || persisted.RevokedBy == nil || *persisted.RevokedBy != customerUser.ID {
		t.Fatalf("support grant was not immediately revoked: %#v", persisted)
	}
	var auditActions []string
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action LIKE ?", customerTenant.ID, "support.%").
		Order("occurred_at, event_id").Pluck("action", &auditActions).Error; err != nil {
		t.Fatal(err)
	}
	if len(auditActions) != 6 {
		t.Fatalf("support audit actions = %#v, want policy/request/approve/read/revoke/disable", auditActions)
	}
	var policyRevocationAudit persistence.AuditLog
	if err := store.DB().Where(
		"tenant_id = ? AND action = ? AND resource_id = ?",
		customerTenant.ID, "support.access_revoked", grant.ID,
	).Take(&policyRevocationAudit).Error; err != nil {
		t.Fatal(err)
	}
	if policyRevocationAudit.ActorType != "user" || policyRevocationAudit.ActorID == nil ||
		*policyRevocationAudit.ActorID != customerUser.ID || policyRevocationAudit.RequestID != "support-policy-disable" {
		t.Fatalf("unexpected policy revocation audit: %#v", policyRevocationAudit)
	}
}

func TestExpiredSupportAccessWritesTenantVisibleSystemAudit(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "support-expiry-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	databaseNow := time.Now().UTC().Truncate(time.Second)
	decisionTime := databaseNow.Add(-2 * time.Hour)
	supportUser := persistence.User{
		ID: uuid.New(), Email: "expiry-support@example.com", DisplayName: "Expiry Support",
		Status: "active", EmailVerifiedAt: &databaseNow,
	}
	customerUser := persistence.User{
		ID: uuid.New(), Email: "expiry-customer@example.com", DisplayName: "Expiry Customer",
		Status: "active", EmailVerifiedAt: &databaseNow,
	}
	for _, user := range []persistence.User{supportUser, customerUser} {
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: supportUser.ID, Role: "security_admin",
		Status: "active", JoinedAt: &databaseNow,
	}).Error; err != nil {
		t.Fatal(err)
	}

	customerPrincipal := identity.Principal{UserID: customerUser.ID}
	customerTenant, err := tenancy.NewService(store.DB()).CreateTenant(ctx, customerPrincipal, tenancy.CreateTenantInput{
		Slug: "support-expiry-" + uuid.NewString()[:8], Name: "Support Expiry Customer",
		PlanCode: "free", Status: "active",
	}, "support-expiry-customer-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	customerPrincipal.ActiveTenantID = &customerTenant.ID
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return decisionTime }
	if _, err := service.UpdatePolicy(ctx, customerPrincipal, customerTenant.ID, UpdatePolicyInput{
		SupportAccessEnabled: true, ExpectedVersion: 0,
		Reason: "Enable expiry audit verification for this Tenant.",
	}, "support-expiry-policy", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	supportPrincipal := identity.Principal{UserID: supportUser.ID, ActiveTenantID: &domain.TenantID}
	grant, err := service.Request(ctx, supportPrincipal, RequestInput{
		TenantID: customerTenant.ID, Reason: "Verify automatic expiry keeps a Tenant-visible audit trail.",
		RequestedDurationSeconds: 30 * 60,
	}, "support-expiry-request", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	adminPrincipal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	grant, err = service.Approve(ctx, adminPrincipal, grant.ID, DecisionInput{
		ExpectedVersion: grant.Version, Reason: "Approve the bounded expiry audit verification window.",
	}, "support-expiry-approve", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if grant.ExpiresAt == nil || !grant.ExpiresAt.Before(databaseNow) {
		t.Fatalf("test Grant is not ready for expiry reconciliation: %#v", grant)
	}

	service.now = func() time.Time { return databaseNow }
	items, err := service.ListTenant(ctx, customerPrincipal, customerTenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "expired" || items[0].Version != 3 {
		t.Fatalf("expired Support Access history = %#v", items)
	}
	if _, err := service.ListTenant(ctx, customerPrincipal, customerTenant.ID); err != nil {
		t.Fatal(err)
	}
	var expiryAudit persistence.AuditLog
	if err := store.DB().Where(
		"tenant_id = ? AND action = ? AND resource_id = ?",
		customerTenant.ID, "support.access_expired", grant.ID,
	).Take(&expiryAudit).Error; err != nil {
		t.Fatal(err)
	}
	if expiryAudit.ActorType != "system" || expiryAudit.ActorID != nil ||
		expiryAudit.RequestID != "support-expiry:"+grant.ID.String() {
		t.Fatalf("unexpected Support Access expiry audit: %#v", expiryAudit)
	}
	var expiryAuditCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).Where(
		"tenant_id = ? AND action = ? AND resource_id = ?",
		customerTenant.ID, "support.access_expired", grant.ID,
	).Count(&expiryAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if expiryAuditCount != 1 {
		t.Fatalf("Support Access expiry audits = %d, want one idempotent event", expiryAuditCount)
	}
}
