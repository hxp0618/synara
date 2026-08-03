package serviceaccounts

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestServiceAccountCreateAndListRejectInactiveTenantBeforeStorageAccess(t *testing.T) {
	ctx := context.Background()
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	service := NewService(nil)

	_, err := service.Create(
		ctx, principal, requestedTenantID, CreateInput{},
		"service-account-inactive-create", "127.0.0.1",
	)
	if problemCode(err) != "tenant_not_found" {
		t.Fatalf("inactive Tenant Service Account create error = %v", err)
	}
	_, err = service.List(ctx, principal, requestedTenantID)
	if problemCode(err) != "tenant_not_found" {
		t.Fatalf("inactive Tenant Service Account list error = %v", err)
	}
}

func TestServiceAccountTokenIsOneTimeHashedAndRotatable(t *testing.T) {
	db, principal, tenantID := setupServiceAccountTest(t)
	service := NewService(db)
	issued, err := service.Create(context.Background(), principal, tenantID, CreateInput{
		Name: "SCIM Provisioner", Scopes: []string{"scim.write", "scim.read"},
	}, "service-account-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Token == "" || issued.Account.Status != "active" {
		t.Fatalf("unexpected issued account: %#v", issued)
	}
	var stored persistence.ServiceAccountToken
	if err := db.Where("service_account_id = ?", issued.Account.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.TokenHash, []byte(issued.Token)) {
		t.Fatal("service account plaintext token was persisted")
	}
	authenticated, err := service.Authenticate(context.Background(), issued.Token)
	if err != nil || !authenticated.Allows("scim.write") || authenticated.TenantID != tenantID {
		t.Fatalf("service account authentication failed: %#v %v", authenticated, err)
	}
	rotated, err := service.RotateToken(context.Background(), principal, tenantID, issued.Account.ID, nil, "service-account-rotate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), issued.Token); problemCode(err) != "invalid_service_account_token" {
		t.Fatalf("old token remained valid after rotation: %v", err)
	}
	if _, err := service.Authenticate(context.Background(), rotated.Token); err != nil {
		t.Fatalf("rotated token is invalid: %v", err)
	}
}

func TestServiceAccountAuthenticationRechecksTokenAndTenantLifecycle(t *testing.T) {
	db, principal, tenantID := setupServiceAccountTest(t)
	service := NewService(db)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	issued, err := service.Create(context.Background(), principal, tenantID, CreateInput{
		Name: "Lifecycle SCIM Provisioner", Scopes: []string{"scim.read"},
	}, "service-account-lifecycle-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	future := now.Add(time.Hour)
	past := now.Add(-time.Second)
	tests := []struct {
		name           string
		status         string
		trialExpiresAt *time.Time
		deletedAt      *time.Time
		wantValid      bool
	}{
		{name: "active", status: "active", wantValid: true},
		{name: "current trial", status: "trialing", trialExpiresAt: &future, wantValid: true},
		{name: "expired trial", status: "trialing", trialExpiresAt: &past},
		{name: "trial without expiry", status: "trialing"},
		{name: "suspended", status: "suspended"},
		{name: "closed", status: "closed"},
		{name: "deleting", status: "deleting", deletedAt: &now},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := db.Model(&persistence.Tenant{}).Where("id = ?", tenantID).Updates(map[string]any{
				"status": test.status, "trial_expires_at": test.trialExpiresAt, "deleted_at": test.deletedAt,
			}).Error; err != nil {
				t.Fatal(err)
			}
			_, err := service.Authenticate(context.Background(), issued.Token)
			if test.wantValid {
				if err != nil {
					t.Fatalf("operational Tenant rejected Service Account token: %v", err)
				}
				return
			}
			if problemCode(err) != "invalid_service_account_token" {
				t.Fatalf("non-operational Tenant authentication error = %v", err)
			}
		})
	}

	if err := db.Model(&persistence.Tenant{}).Where("id = ?", tenantID).Updates(map[string]any{
		"status": "active", "trial_expires_at": nil, "deleted_at": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(time.Hour)
	expiring, err := service.Create(context.Background(), principal, tenantID, CreateInput{
		Name: "Expiring SCIM Provisioner", Scopes: []string{"scim.read"}, ExpiresAt: &expiresAt,
	}, "service-account-expiring-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	now = expiresAt
	if _, err := service.Authenticate(context.Background(), expiring.Token); problemCode(err) != "invalid_service_account_token" {
		t.Fatalf("expired Service Account token authentication error = %v", err)
	}

	now = expiresAt.Add(-time.Minute)
	revoked, err := service.Create(context.Background(), principal, tenantID, CreateInput{
		Name: "Revoked SCIM Provisioner", Scopes: []string{"scim.read"},
	}, "service-account-revoked-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(
		context.Background(), principal, tenantID, revoked.Account.ID,
		"service-account-revoked", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), revoked.Token); problemCode(err) != "invalid_service_account_token" {
		t.Fatalf("revoked Service Account token authentication error = %v", err)
	}
}

func TestServiceAccountOperationsRejectCrossTenantAccountSubstitution(t *testing.T) {
	db, principal, tenantID := setupServiceAccountTest(t)
	service := NewService(db)
	issued, err := service.Create(context.Background(), principal, tenantID, CreateInput{
		Name: "Tenant A automation", Scopes: []string{"scim.read"},
	}, "service-account-tenant-a", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	otherTenantID := uuid.New()
	now := time.Now().UTC()
	if err := db.Create(&persistence.Tenant{
		ID: otherTenantID, Slug: "service-account-other", Name: "Service Account Other",
		Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{},
		CreatedBy: principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{
		TenantID: otherTenantID, UserID: principal.UserID, Role: "owner", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	otherPrincipal := principal
	otherPrincipal.ActiveTenantID = &otherTenantID

	_, err = service.RotateToken(
		context.Background(), otherPrincipal, otherTenantID, issued.Account.ID, nil,
		"service-account-cross-tenant-rotate", "127.0.0.1",
	)
	if problemCode(err) != "service_account_not_found" {
		t.Fatalf("cross-Tenant Service Account rotate error = %v", err)
	}
	err = service.Revoke(
		context.Background(), otherPrincipal, otherTenantID, issued.Account.ID,
		"service-account-cross-tenant-revoke", "127.0.0.1",
	)
	if problemCode(err) != "service_account_not_found" {
		t.Fatalf("cross-Tenant Service Account revoke error = %v", err)
	}
	if _, err := service.Authenticate(context.Background(), issued.Token); err != nil {
		t.Fatalf("cross-Tenant substitution revoked the original token: %v", err)
	}
	var account persistence.ServiceAccount
	if err := db.Where("tenant_id = ? AND id = ?", tenantID, issued.Account.ID).Take(&account).Error; err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" || account.RevokedAt != nil {
		t.Fatalf("cross-Tenant operations mutated original Service Account: %#v", account)
	}
}

func setupServiceAccountTest(t *testing.T) (*gorm.DB, identity.Principal, uuid.UUID) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}
	userID, tenantID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	if err := db.Create(&persistence.User{ID: userID, Email: "owner@example.com", DisplayName: "Owner", Status: "active", EmailVerifiedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.Tenant{ID: tenantID, Slug: "service-account-test", Name: "Service Account Test", Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{}, CreatedBy: userID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	return db, identity.Principal{UserID: userID, SessionID: uuid.New(), ActiveTenantID: &tenantID, Email: "owner@example.com", DisplayName: "Owner"}, tenantID
}

func problemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}
