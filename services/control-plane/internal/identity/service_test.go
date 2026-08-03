package identity

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestAuthenticateEnforcesIdleAndAbsoluteExpiry(t *testing.T) {
	service, domain, store := newIdentityFixture(t)
	now := time.Date(2026, time.July, 12, 8, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	issued, err := service.DevLogin(context.Background(), DevLoginInput{
		Email: "owner@example.com", DisplayName: "Owner",
	}, "127.0.0.1", "test", "login")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), issued.Token); err != nil {
		t.Fatalf("new session was not authenticated: %v", err)
	}

	now = now.Add(10 * time.Minute)
	if _, err := service.Authenticate(context.Background(), issued.Token); err != nil {
		t.Fatalf("active session was not refreshed: %v", err)
	}
	var refreshed persistence.LoginSession
	if err := store.DB().Where("id = ?", issued.State.User.SessionID).Take(&refreshed).Error; err != nil {
		t.Fatal(err)
	}
	if !refreshed.LastSeenAt.Equal(now) {
		t.Fatalf("last seen = %s, want %s", refreshed.LastSeenAt, now)
	}

	now = now.Add(61 * time.Minute)
	if _, err := service.Authenticate(context.Background(), issued.Token); err == nil {
		t.Fatal("idle session was authenticated")
	}

	now = time.Date(2026, time.July, 13, 8, 0, 0, 0, time.UTC)
	second, err := service.DevLogin(context.Background(), DevLoginInput{
		Email: "owner@example.com", DisplayName: "Owner",
	}, "127.0.0.1", "test", "second-login")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.LoginSession{}).Where("id = ?", second.State.User.SessionID).
		Update("expires_at", now.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), second.Token); err == nil {
		t.Fatal("absolutely expired session was authenticated")
	}
	if second.State.User.ActiveTenantID == nil || *second.State.User.ActiveTenantID != domain.TenantID {
		t.Fatal("login did not retain the personal tenant")
	}
}

func TestLoginRotatesTokenAndRevocationIsVisibleAcrossInstances(t *testing.T) {
	firstReplica, domain, store := newIdentityFixture(t)
	secondReplica := NewService(store.DB(), 24*time.Hour, time.Hour, PersonalDomain{
		UserID: domain.UserID, TenantID: domain.TenantID,
	})
	issued, err := firstReplica.DevLogin(context.Background(), DevLoginInput{
		Email: "owner@example.com", DisplayName: "Owner",
	}, "127.0.0.1", "test", "first-login")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := firstReplica.DevLogin(context.Background(), DevLoginInput{
		Email: "owner@example.com", DisplayName: "Owner",
	}, "127.0.0.1", "test", "rotated-login")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Token == rotated.Token || issued.State.User.SessionID == rotated.State.User.SessionID {
		t.Fatal("successful login reused the previous token or session id")
	}
	principal, err := secondReplica.Authenticate(context.Background(), rotated.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstReplica.Revoke(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	if _, err := secondReplica.Authenticate(context.Background(), rotated.Token); err == nil {
		t.Fatal("another service instance accepted a revoked session")
	}
}

func TestTenantAdministratorRevokesActiveTenantSessionsWithAudit(t *testing.T) {
	service, domain, store := newIdentityFixture(t)
	first, err := service.DevLogin(context.Background(), DevLoginInput{
		Email: "owner@example.com", DisplayName: "Owner",
	}, "127.0.0.1", "test", "admin-revoke-first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.DevLogin(context.Background(), DevLoginInput{
		Email: "owner@example.com", DisplayName: "Owner",
	}, "127.0.0.1", "test", "admin-revoke-second")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.Authenticate(context.Background(), first.Token)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := service.RevokeTenantUserSessions(
		context.Background(), principal, domain.TenantID, domain.UserID, "admin-revoke", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 2 {
		t.Fatalf("revoked sessions = %d, want 2", revoked)
	}
	for _, token := range []string{first.Token, second.Token} {
		if _, err := service.Authenticate(context.Background(), token); err == nil {
			t.Fatal("administrator-revoked session was authenticated")
		}
	}
	var auditCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND resource_id = ?", domain.TenantID, "identity.sessions_revoked", domain.UserID).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("session revocation audit count = %d", auditCount)
	}
}

func TestSetActiveTenantEnforcesSSOSessionOrRecoveryOwner(t *testing.T) {
	service, domain, store := newIdentityFixture(t)
	issued, err := service.DevLogin(context.Background(), DevLoginInput{
		Email: "owner@example.com", DisplayName: "Owner",
	}, "127.0.0.1", "test", "sso-enforcement-login")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, recoveryUserID, connectionID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	clientID := "client"
	for _, model := range []any{
		&persistence.User{ID: recoveryUserID, Email: "recovery@example.com", DisplayName: "Recovery", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: "sso-enforced-" + uuid.NewString()[:8], Name: "SSO enforced", Status: "active", LifecycleVersion: 1, PlanCode: "enterprise", Region: "default", Settings: map[string]any{}, CreatedBy: domain.UserID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: domain.UserID, Role: "owner", Status: "active", JoinedAt: &now},
		&persistence.TenantMembership{TenantID: tenantID, UserID: recoveryUserID, Role: "owner", Status: "active", JoinedAt: &now},
		&persistence.IdentityConnection{ID: connectionID, TenantID: tenantID, Kind: "oidc", Name: "SSO", Status: "active", Issuer: "https://idp.example.com", ClientID: &clientID, Configuration: map[string]any{}, CreatedBy: domain.UserID, UpdatedBy: domain.UserID},
		&persistence.TenantIdentityPolicy{TenantID: tenantID, SSOEnforcement: "required", Version: 1, RecoveryUserID: &recoveryUserID, EnforcementSetAt: &now, EnforcementSetBy: &domain.UserID, UpdatedBy: domain.UserID},
	} {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	principal := issued.State.User
	_, err = service.SetActiveTenant(context.Background(), principal, tenantID)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "sso_required" {
		t.Fatalf("local Session entered an SSO-enforced Tenant: %v", err)
	}
	if err := store.DB().Model(&persistence.LoginSession{}).Where("id = ?", principal.SessionID).
		Updates(map[string]any{"auth_method": "sso", "identity_connection_id": connectionID}).Error; err != nil {
		t.Fatal(err)
	}
	updated, err := service.SetActiveTenant(context.Background(), principal, tenantID)
	if err != nil {
		t.Fatalf("SSO Session could not enter enforced Tenant: %v", err)
	}
	if updated.ActiveTenantID == nil || *updated.ActiveTenantID != tenantID {
		t.Fatalf("active Tenant = %v", updated.ActiveTenantID)
	}
}

func TestSessionStateIncludesTenantLifecycleAuthority(t *testing.T) {
	service, domain, store := newIdentityFixture(t)
	issued, err := service.DevLogin(context.Background(), DevLoginInput{
		Email: "owner@example.com", DisplayName: "Owner",
	}, "127.0.0.1", "test", "lifecycle-authority-login")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	suspendedAt := now.Add(-2 * time.Hour)
	closedAt := now.Add(-time.Hour)
	tenant := persistence.Tenant{
		ID: uuid.New(), Slug: "session-lifecycle-" + uuid.NewString()[:8], Name: "Session lifecycle",
		Status: "closed", LifecycleVersion: 7, SuspendedAt: &suspendedAt, ClosedAt: &closedAt,
		PlanCode: "enterprise", Region: "default", Settings: map[string]any{}, CreatedBy: domain.UserID,
	}
	if err := store.DB().Create(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: tenant.ID, UserID: domain.UserID, Role: "owner", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	state, err := service.GetSessionState(context.Background(), issued.State.User)
	if err != nil {
		t.Fatal(err)
	}
	var access *TenantAccess
	for index := range state.Tenants {
		if state.Tenants[index].ID == tenant.ID {
			access = &state.Tenants[index]
			break
		}
	}
	if access == nil {
		t.Fatalf("Tenant lifecycle authority missing from Session state: %#v", state.Tenants)
	}
	if access.LifecycleVersion != tenant.LifecycleVersion || access.SuspendedAt == nil || access.ClosedAt == nil {
		t.Fatalf("Tenant lifecycle authority = %#v", access)
	}
	if !access.SuspendedAt.Equal(suspendedAt) || !access.ClosedAt.Equal(closedAt) {
		t.Fatalf("Tenant lifecycle timestamps = suspended %v closed %v", access.SuspendedAt, access.ClosedAt)
	}
}

func newIdentityFixture(t *testing.T) (*Service, bootstrap.Result, database.MetadataStore) {
	t.Helper()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(context.Background(), profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(context.Background(), store.DB(), platform.ProfilePersonal, "identity-service-test")
	if err != nil {
		t.Fatal(err)
	}
	return NewService(store.DB(), 24*time.Hour, time.Hour, PersonalDomain{
		UserID: domain.UserID, TenantID: domain.TenantID,
	}), domain, store
}
