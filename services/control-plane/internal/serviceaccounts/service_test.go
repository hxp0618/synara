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
