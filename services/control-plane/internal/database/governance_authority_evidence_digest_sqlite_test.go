package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteGovernanceAuthorityDigestUpgradeRevokesLegacyGrant(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "authority-digest-upgrade-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	operator := persistence.User{
		ID: uuid.New(), Email: "authority-digest@example.test", DisplayName: "Authority Digest",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&operator).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: operator.ID, Role: "admin", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Exec("DROP TRIGGER IF EXISTS trg_stage6_governance_authority_insert").Error; err != nil {
		t.Fatal(err)
	}
	legacy := persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: operator.ID,
		AuthorityKey: "release.engineering", Status: "active", Version: 1,
		ExpiresAt: now.Add(90 * 24 * time.Hour), GrantedBy: domain.UserID,
		Reason:            "Legacy authority referenced mutable delegation evidence only.",
		EvidenceReference: "https://evidence.example.test/authority/legacy", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	billingDigest := "sha256:" + strings.Repeat("c", 64)
	legacyBilling := persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: operator.ID,
		AuthorityKey: "billing_exercise.finance", Status: "active", Version: 1,
		ExpiresAt: now.Add(90 * 24 * time.Hour), GrantedBy: domain.UserID,
		Reason:            "Legacy billing exercise authority predates the internal self-hosted boundary.",
		EvidenceReference: "https://evidence.example.test/authority/legacy-billing",
		EvidenceSHA256:    &billingDigest, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&legacyBilling).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateGovernanceAuthoritySQLiteSafety(ctx, store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().First(&legacy, "id = ?", legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.Status != "revoked" || legacy.Version != 2 || legacy.RevokedAt == nil ||
		legacy.RevokedBy == nil || *legacy.RevokedBy != domain.UserID || legacy.RevocationReason == nil ||
		!strings.Contains(*legacy.RevocationReason, "Migration 000140") || legacy.EvidenceSHA256 != nil {
		t.Fatalf("legacy governance authority after digest upgrade = %#v", legacy)
	}
	if err := store.DB().First(&legacyBilling, "id = ?", legacyBilling.ID).Error; err != nil {
		t.Fatal(err)
	}
	if legacyBilling.Status != "revoked" || legacyBilling.Version != 2 || legacyBilling.RevocationReason == nil ||
		!strings.Contains(*legacyBilling.RevocationReason, "Migration 000160") {
		t.Fatalf("legacy billing authority after internal self-hosted upgrade = %#v", legacyBilling)
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
	if err := store.DB().Create(&invalid).Error; err == nil {
		t.Fatal("SQLite accepted a new governance authority without an evidence digest")
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	valid := invalid
	valid.ID = uuid.New()
	valid.EvidenceSHA256 = &digest
	if err := store.DB().Create(&valid).Error; err != nil {
		t.Fatal(err)
	}
	invalidBilling := valid
	invalidBilling.ID = uuid.New()
	invalidBilling.AuthorityKey = "billing_exercise.finance"
	if err := store.DB().Create(&invalidBilling).Error; err == nil {
		t.Fatal("SQLite accepted a new billing exercise governance authority")
	}
	mutated := "sha256:" + strings.Repeat("b", 64)
	if err := store.DB().Model(&persistence.Stage6GovernanceAuthorityGrant{}).
		Where("id = ?", valid.ID).Update("evidence_sha256", mutated).Error; err == nil {
		t.Fatal("SQLite allowed governance authority evidence digest mutation")
	}
}
