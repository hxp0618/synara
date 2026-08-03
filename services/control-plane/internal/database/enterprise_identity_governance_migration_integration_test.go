package database

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresEnterpriseIdentityGovernanceConstraints(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	first, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "postgres-identity-governance-a-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: first.UserID, ActiveTenantID: &first.TenantID}
	second, err := tenancy.NewService(db).CreateTenant(ctx, principal, tenancy.CreateTenantInput{
		Slug: "pg-identity-b-" + uuid.NewString()[:8], Name: "Identity governance B", Status: "active",
	}, "postgres-identity-governance-b", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	tokenHash := sha256.Sum256([]byte("domain-verification-token"))
	firstDomain := persistence.TenantDomain{
		ID: uuid.New(), TenantID: first.TenantID, Domain: "example.com", Status: "pending",
		VerificationTokenHash: tokenHash[:], VerificationExpiresAt: now.Add(time.Hour),
		CreatedBy: first.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&firstDomain).Error; err != nil {
		t.Fatal(err)
	}
	duplicate := firstDomain
	duplicate.ID = uuid.New()
	duplicate.TenantID = second.ID
	duplicate.CreatedBy = first.UserID
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("PostgreSQL accepted two active claims for the same verified Domain")
	}
	invalid := firstDomain
	invalid.ID = uuid.New()
	invalid.Domain = "invalid_domain"
	if err := db.Create(&invalid).Error; err == nil {
		t.Fatal("PostgreSQL accepted an invalid Tenant Domain")
	}
	if err := db.Create(&persistence.TenantIdentityPolicy{
		TenantID: first.TenantID, SSOEnforcement: "required", Version: 1, UpdatedBy: first.UserID,
	}).Error; err == nil {
		t.Fatal("PostgreSQL accepted required SSO without recovery authority")
	}
	clientID := "client"
	connection := persistence.IdentityConnection{
		ID: uuid.New(), TenantID: first.TenantID, Kind: "oidc", Name: "Governance SSO", Status: "active",
		Issuer: "https://idp.example.com", ClientID: &clientID, Configuration: map[string]any{},
		CreatedBy: first.UserID, UpdatedBy: first.UserID,
	}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.LoginSession{
		ID: uuid.New(), UserID: first.UserID, ActiveTenantID: &first.TenantID,
		AuthMethod: "sso", RefreshTokenHash: []byte(uuid.NewString()), ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL accepted an SSO Login Session without an Identity Connection")
	}
}
