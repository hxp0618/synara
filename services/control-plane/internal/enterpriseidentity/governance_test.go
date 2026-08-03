package enterpriseidentity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestEnterpriseIdentityOperationsRejectInactiveTenantBeforeDNSSecretsOrStorage(t *testing.T) {
	ctx := context.Background()
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	service := NewService(nil, nil, nil)

	_, err := service.Create(
		ctx, principal, requestedTenantID, CreateConnectionInput{},
		"identity-inactive-create", "127.0.0.1",
	)
	assertGovernanceProblem(t, err, "tenant_not_found")
	_, err = service.ListDomains(ctx, principal, requestedTenantID)
	assertGovernanceProblem(t, err, "tenant_not_found")
	_, err = service.CreateDomain(
		ctx, principal, requestedTenantID, CreateDomainInput{},
		"identity-inactive-domain-create", "127.0.0.1",
	)
	assertGovernanceProblem(t, err, "tenant_not_found")
	_, err = service.VerifyDomain(
		ctx, principal, requestedTenantID, uuid.New(),
		"identity-inactive-domain-verify", "127.0.0.1",
	)
	assertGovernanceProblem(t, err, "tenant_not_found")
	err = service.RevokeDomain(
		ctx, principal, requestedTenantID, uuid.New(),
		"identity-inactive-domain-revoke", "127.0.0.1",
	)
	assertGovernanceProblem(t, err, "tenant_not_found")
	_, err = service.GetIdentityPolicy(ctx, principal, requestedTenantID)
	assertGovernanceProblem(t, err, "tenant_not_found")
	_, err = service.UpdateIdentityPolicy(
		ctx, principal, requestedTenantID, UpdateIdentityPolicyInput{},
		"identity-inactive-policy-update", "127.0.0.1",
	)
	assertGovernanceProblem(t, err, "tenant_not_found")
	_, err = service.ReplaceMappings(
		ctx, principal, requestedTenantID, uuid.New(), nil,
		"identity-inactive-mapping-replace", "127.0.0.1",
	)
	assertGovernanceProblem(t, err, "tenant_not_found")
}

func TestDomainVerificationAndSSOEnforcementFences(t *testing.T) {
	ctx := context.Background()
	db, principal, tenantID := setupIdentityTest(t)
	service := NewService(db, identity.NewService(db, time.Hour, 30*time.Minute), nil)
	challenge, err := service.CreateDomain(ctx, principal, tenantID, CreateDomainInput{Domain: "Example.COM."}, "domain-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if challenge.Domain.Domain != "example.com" || challenge.Domain.Status != "pending" ||
		challenge.VerificationRecordName != "_synara-verification.example.com" ||
		!strings.HasPrefix(challenge.VerificationRecordValue, domainVerificationPrefix) {
		t.Fatalf("unexpected Domain challenge: %#v", challenge)
	}
	service.lookupTXT = func(context.Context, string) ([]string, error) { return []string{"unrelated=value"}, nil }
	_, err = service.VerifyDomain(ctx, principal, tenantID, challenge.ID, "domain-verify-missing", "127.0.0.1")
	assertGovernanceProblem(t, err, "domain_verification_record_missing")
	service.lookupTXT = func(_ context.Context, name string) ([]string, error) {
		if name != challenge.VerificationRecordName {
			t.Fatalf("DNS name = %q", name)
		}
		return []string{challenge.VerificationRecordValue}, nil
	}
	verified, err := service.VerifyDomain(ctx, principal, tenantID, challenge.ID, "domain-verify", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if verified.Status != "verified" || verified.VerifiedAt == nil {
		t.Fatalf("verified Domain = %#v", verified)
	}

	connectionID := uuid.New()
	if err := db.Create(&persistence.IdentityConnection{
		ID: connectionID, TenantID: tenantID, Kind: "oidc", Name: "Company SSO", Status: "active",
		Issuer: "https://idp.example.com", ClientID: stringPointer("client"), Configuration: map[string]any{},
		CreatedBy: principal.UserID, UpdatedBy: principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	otherUserID := uuid.New()
	now := time.Now().UTC()
	if err := db.Create(&persistence.User{
		ID: otherUserID, Email: "member@example.com", DisplayName: "Member", Status: "active", EmailVerifiedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{
		TenantID: tenantID, UserID: otherUserID, Role: "member", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	localOtherID, localRecoveryID, ssoOtherID := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []persistence.LoginSession{
		{ID: localOtherID, UserID: otherUserID, ActiveTenantID: &tenantID, AuthMethod: "local", RefreshTokenHash: []byte(uuid.NewString()), ExpiresAt: now.Add(time.Hour), LastSeenAt: now},
		{ID: localRecoveryID, UserID: principal.UserID, ActiveTenantID: &tenantID, AuthMethod: "local", RefreshTokenHash: []byte(uuid.NewString()), ExpiresAt: now.Add(time.Hour), LastSeenAt: now},
		{ID: ssoOtherID, UserID: otherUserID, ActiveTenantID: &tenantID, AuthMethod: "sso", IdentityConnectionID: &connectionID, RefreshTokenHash: []byte(uuid.NewString()), ExpiresAt: now.Add(time.Hour), LastSeenAt: now},
	} {
		if err := db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
	}
	policy, err := service.UpdateIdentityPolicy(ctx, principal, tenantID, UpdateIdentityPolicyInput{
		SSOEnforcement: "required", ExpectedVersion: 0, RecoveryUserID: &principal.UserID,
	}, "identity-policy-enable", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if policy.SSOEnforcement != "required" || policy.Version != 1 || policy.RecoveryUserID == nil {
		t.Fatalf("required SSO policy = %#v", policy)
	}
	assertLoginSessionRevoked(t, db, localOtherID, true)
	assertLoginSessionRevoked(t, db, localRecoveryID, false)
	assertLoginSessionRevoked(t, db, ssoOtherID, false)
	if err := service.Disable(ctx, principal, tenantID, connectionID, "connection-disable", "127.0.0.1"); err == nil {
		t.Fatal("required SSO allowed disabling its last active connection")
	} else {
		assertGovernanceProblem(t, err, "sso_enforcement_requires_connection")
	}
	if err := service.RevokeDomain(ctx, principal, tenantID, verified.ID, "domain-revoke", "127.0.0.1"); err == nil {
		t.Fatal("required SSO allowed revoking its last verified domain")
	} else {
		assertGovernanceProblem(t, err, "sso_enforcement_requires_verified_domain")
	}
	policy, err = service.UpdateIdentityPolicy(ctx, principal, tenantID, UpdateIdentityPolicyInput{
		SSOEnforcement: "optional", ExpectedVersion: policy.Version,
	}, "identity-policy-disable", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if policy.SSOEnforcement != "optional" || policy.Version != 2 || policy.RecoveryUserID != nil {
		t.Fatalf("optional SSO policy = %#v", policy)
	}
	if err := service.RevokeDomain(ctx, principal, tenantID, verified.ID, "domain-revoke", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := service.Disable(ctx, principal, tenantID, connectionID, "connection-disable", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	assertLoginSessionRevoked(t, db, ssoOtherID, true)
}

func TestSSOEnforcementRequiresVerifiedDomainConnectionAndRecoveryOwner(t *testing.T) {
	ctx := context.Background()
	db, principal, tenantID := setupIdentityTest(t)
	service := NewService(db, identity.NewService(db, time.Hour, 30*time.Minute), nil)
	_, err := service.UpdateIdentityPolicy(ctx, principal, tenantID, UpdateIdentityPolicyInput{
		SSOEnforcement: "required", ExpectedVersion: 0, RecoveryUserID: &principal.UserID,
	}, "identity-policy", "127.0.0.1")
	assertGovernanceProblem(t, err, "sso_enforcement_requires_verified_domain")
	if _, err := service.CreateDomain(ctx, principal, tenantID, CreateDomainInput{Domain: "localhost"}, "invalid-domain", "127.0.0.1"); err == nil {
		t.Fatal("single-label Domain was accepted")
	} else {
		assertGovernanceProblem(t, err, "invalid_tenant_domain")
	}
}

func assertLoginSessionRevoked(t *testing.T, db *gorm.DB, sessionID uuid.UUID, want bool) {
	t.Helper()
	var session persistence.LoginSession
	if err := db.Where("id = ?", sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if (session.RevokedAt != nil) != want {
		t.Fatalf("Session %s revoked=%v, want %v", sessionID, session.RevokedAt != nil, want)
	}
}

func assertGovernanceProblem(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}

func stringPointer(value string) *string { return &value }
