package governanceauthority

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
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

func TestGovernanceAuthorityRequiresOwnerGrantAndFencesRoleClaims(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "governance-authority-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	operator := persistence.User{ID: uuid.New(), Email: "functional-operator@example.test", DisplayName: "Functional Operator", Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := store.DB().Create(&operator).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: operator.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	_, err = service.Create(ctx, domain.UserID, CreateInput{
		UserID: operator.ID, AuthorityKey: "billing_exercise.finance", ExpiresAt: now.Add(24 * time.Hour),
		Reason:            "Legacy commercial billing authority must remain outside this internal product.",
		EvidenceReference: "https://evidence.example.test/legacy-billing-authority",
		EvidenceSHA256:    testGovernanceEvidenceDigest("legacy-billing-authority"),
	}, "legacy-billing-authority", "127.0.0.1")
	assertGovernanceAuthorityProblem(t, err, "governance_authority_invalid")
	_, err = service.Create(ctx, domain.UserID, CreateInput{
		UserID: operator.ID, AuthorityKey: ReleaseEngineering, ExpiresAt: now.Add(24 * time.Hour),
		Reason:            "Reject governance authority without immutable delegation evidence bytes.",
		EvidenceReference: "https://evidence.example.test/unbound-authority",
		EvidenceSHA256:    "sha256:" + strings.Repeat("0", 64),
	}, "unbound-authority", "127.0.0.1")
	assertGovernanceAuthorityProblem(t, err, "governance_authority_invalid")

	_, err = service.Create(ctx, operator.ID, CreateInput{
		UserID: domain.UserID, AuthorityKey: ReleaseEngineering, ExpiresAt: now.Add(24 * time.Hour),
		Reason: "An Admin must not assign governance authority.", EvidenceReference: "https://evidence.example.test/admin-grant",
		EvidenceSHA256: testGovernanceEvidenceDigest("admin-grant"),
	}, "admin-grant", "127.0.0.1")
	assertGovernanceAuthorityProblem(t, err, "governance_authority_manage_forbidden")

	grant, err := service.Create(ctx, domain.UserID, CreateInput{
		UserID: operator.ID, AuthorityKey: ReleaseEngineering, ExpiresAt: now.Add(30 * 24 * time.Hour),
		Reason:            "Owner verified Engineering release authority and separation of duties.",
		EvidenceReference: "https://evidence.example.test/functional-authority/engineering",
		EvidenceSHA256:    testGovernanceEvidenceDigest("engineering"),
	}, "owner-grant", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if grant.EvidenceSHA256 == nil || *grant.EvidenceSHA256 != testGovernanceEvidenceDigest("engineering") {
		t.Fatalf("governance authority evidence digest = %#v", grant.EvidenceSHA256)
	}
	if err := service.Require(ctx, operator.ID, ReleaseEngineering); err != nil {
		t.Fatal(err)
	}
	assertGovernanceAuthorityProblem(t, service.Require(ctx, operator.ID, ReleaseSecurity), "governance_authority_required")
	expiring, err := service.Create(ctx, domain.UserID, CreateInput{
		UserID: operator.ID, AuthorityKey: ReleaseSecurity, ExpiresAt: now.Add(time.Hour),
		Reason:            "Owner assigned a short bounded Security release responsibility.",
		EvidenceReference: "https://evidence.example.test/functional-authority/security",
		EvidenceSHA256:    testGovernanceEvidenceDigest("security"),
	}, "owner-security-grant", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, operator.ID).Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	assertGovernanceAuthorityProblem(t, service.Require(ctx, operator.ID, ReleaseSecurity), "governance_authority_required")
	if err := store.DB().Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, operator.ID).Updates(map[string]any{"status": "active", "joined_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.Require(ctx, operator.ID, ReleaseSecurity); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return expiring.ExpiresAt.Add(time.Second) }
	assertGovernanceAuthorityProblem(t, service.Require(ctx, operator.ID, ReleaseSecurity), "governance_authority_required")
	service.now = func() time.Time { return now }

	if err := store.DB().Model(&persistence.Stage6GovernanceAuthorityGrant{}).Where("id = ?", grant.ID).
		Update("authority_key", ReleaseSecurity).Error; err == nil {
		t.Fatal("governance authority identity was mutable")
	}
	if err := store.DB().Delete(&persistence.Stage6GovernanceAuthorityGrant{}, "id = ?", grant.ID).Error; err == nil {
		t.Fatal("governance authority history was deletable")
	}
	if err := store.DB().Model(&persistence.Stage6GovernanceAuthorityGrant{}).Where("id = ?", grant.ID).
		Update("evidence_sha256", testGovernanceEvidenceDigest("mutated")).Error; err == nil {
		t.Fatal("governance authority evidence digest was mutable")
	}

	revoked, err := service.Revoke(ctx, domain.UserID, grant.ID, RevokeInput{
		ExpectedVersion: grant.Version, Reason: "Engineering approval responsibility moved to another operator.",
	}, "owner-revoke", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != "revoked" || revoked.Version != 2 || revoked.RevokedAt == nil {
		t.Fatalf("revoked grant = %#v", revoked)
	}
	assertGovernanceAuthorityProblem(t, service.Require(ctx, operator.ID, ReleaseEngineering), "governance_authority_required")

	var auditCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).Where("tenant_id = ? AND action LIKE ?", domain.TenantID, "governance.authority_%").Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 3 {
		t.Fatalf("governance authority Audit count = %d, want 3", auditCount)
	}
}

func testGovernanceEvidenceDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertGovernanceAuthorityProblem(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}
