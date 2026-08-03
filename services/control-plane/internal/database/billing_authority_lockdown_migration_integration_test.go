package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestBillingAuthorityLockdownRevokesLegacyPostgresGrant(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	databaseURL = postgresisolation.URL(t, databaseURL)
	ctx := context.Background()
	db, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(ctx, db, migrationsThrough(t, "000159_project_internal_cost_allocations.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "billing-authority-lockdown-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	operator := persistence.User{
		ID: uuid.New(), Email: "legacy-billing-authority@example.test", DisplayName: "Legacy Billing Authority",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&operator).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: operator.ID, Role: "admin", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	legacy := persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: operator.ID,
		AuthorityKey: "billing_exercise.finance", Status: "active", Version: 1,
		ExpiresAt: now.Add(90 * 24 * time.Hour), GrantedBy: domain.UserID,
		Reason:            "Legacy billing exercise authority predates the internal self-hosted boundary.",
		EvidenceReference: "https://evidence.example.test/authority/legacy-billing-pg",
		EvidenceSHA256:    &digest, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&legacy, "id = ?", legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.Status != "revoked" || legacy.Version != 2 || legacy.RevokedAt == nil ||
		legacy.RevokedBy == nil || *legacy.RevokedBy != domain.UserID || legacy.RevocationReason == nil ||
		!strings.Contains(*legacy.RevocationReason, "Migration 000160") {
		t.Fatalf("legacy billing authority after Migration 000160 = %#v", legacy)
	}
	newGrant := legacy
	newGrant.ID = uuid.New()
	newGrant.Status = "active"
	newGrant.Version = 1
	newGrant.RevokedAt = nil
	newGrant.RevokedBy = nil
	newGrant.RevocationReason = nil
	newGrant.CreatedAt = now
	newGrant.UpdatedAt = now
	if err := db.Create(&newGrant).Error; err == nil {
		t.Fatal("PostgreSQL accepted a new billing exercise governance authority")
	}
}
