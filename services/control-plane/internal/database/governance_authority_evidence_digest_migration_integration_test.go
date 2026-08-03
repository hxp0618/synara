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

func TestGovernanceAuthorityEvidenceDigestMigrationRevokesLegacyPostgresGrant(t *testing.T) {
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
	if err := Migrate(ctx, db, migrationsThrough(t, "000139_stage6_release_approval_evidence_digest.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "authority-digest-migration-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	operator := persistence.User{
		ID: uuid.New(), Email: "authority-digest-pg@example.test", DisplayName: "Authority Digest PG",
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
	legacy := persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: operator.ID,
		AuthorityKey: "release.engineering", Status: "active", Version: 1,
		ExpiresAt: now.Add(90 * 24 * time.Hour), GrantedBy: domain.UserID,
		Reason:            "Legacy PostgreSQL authority referenced mutable delegation evidence only.",
		EvidenceReference: "https://evidence.example.test/authority/legacy-pg", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Omit("EvidenceSHA256").Create(&legacy).Error; err != nil {
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
		!strings.Contains(*legacy.RevocationReason, "Migration 000140") || legacy.EvidenceSHA256 != nil {
		t.Fatalf("legacy PostgreSQL governance authority after Migration 000140 = %#v", legacy)
	}
	invalid := legacy
	invalid.ID = uuid.New()
	invalid.Status = "active"
	invalid.Version = 1
	invalid.RevokedAt = nil
	invalid.RevokedBy = nil
	invalid.RevocationReason = nil
	invalid.CreatedAt = now
	invalid.UpdatedAt = now
	if err := db.Create(&invalid).Error; err == nil {
		t.Fatal("PostgreSQL accepted a new governance authority without an evidence digest")
	}
}
