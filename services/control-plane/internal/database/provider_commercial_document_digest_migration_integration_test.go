package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProviderCommercialDocumentDigestMigrationRevokesLegacyPostgresAuthorization(t *testing.T) {
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
	if err := Migrate(ctx, db, migrationsThrough(t, "000135_stage6_final_ga_review_release_gate.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "provider-digest-migration-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	legacy := persistence.ProviderCommercialAuthorization{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, AuthorizationKey: "legacy-url-only-pg",
		Provider: "codex", ProviderProduct: "OpenAI API", AccountType: "Enterprise API organization",
		ContractingEntity: "OpenAI contracting entity", CredentialMode: "customer_byok",
		AllowedCredentialScopes: []string{"tenant"}, AllowedRegions: []string{"default"},
		DataUsePolicy: "no_training", RetentionPolicy: "Approved provider retention applies.",
		TermsEffectiveAt: now.Add(-24 * time.Hour), TermsReference: "https://evidence.example.test/legacy-pg/terms",
		AgreementReference:          "https://evidence.example.test/legacy-pg/agreement",
		DPAReference:                "https://evidence.example.test/legacy-pg/dpa",
		ProhibitedUseSummary:        "Consumer credentials and unapproved training are prohibited.",
		TerminationRunbookReference: "https://evidence.example.test/legacy-pg/termination",
		ReviewExpiresAt:             now.Add(90 * 24 * time.Hour), State: "draft", Version: 1,
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	legacyColumns := []string{"TermsSHA256", "AgreementSHA256", "DPASHA256", "TerminationRunbookSHA256"}
	if err := db.Omit(legacyColumns...).Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrationsThrough(t, "000136_provider_commercial_document_digests.sql")); err != nil {
		t.Fatal(err)
	}
	var upgraded persistence.ProviderCommercialAuthorization
	if err := db.First(&upgraded, "id = ?", legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if upgraded.State != "revoked" || upgraded.Version != 2 || upgraded.RevokedAt == nil ||
		upgraded.TermsSHA256 != nil || upgraded.AgreementSHA256 != nil || upgraded.DPASHA256 != nil || upgraded.TerminationRunbookSHA256 != nil {
		t.Fatalf("legacy PostgreSQL authorization after Migration 000136 = %#v", upgraded)
	}

	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	approvalLegacy := legacy
	approvalLegacy.ID = uuid.New()
	approvalLegacy.AuthorizationKey = "legacy-approval-url-only-pg"
	approvalLegacy.TermsSHA256 = &digest
	approvalLegacy.AgreementSHA256 = &digest
	approvalLegacy.DPASHA256 = &digest
	approvalLegacy.TerminationRunbookSHA256 = &digest
	if err := db.Create(&approvalLegacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP TRIGGER trg_provider_commercial_authorization_approvals_insert ON provider_commercial_authorization_approvals").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP TRIGGER trg_provider_commercial_approval_authority ON provider_commercial_authorization_approvals").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Omit("EvidenceSHA256").Create(&persistence.ProviderCommercialAuthorizationApproval{
		ID: uuid.New(), AuthorizationID: approvalLegacy.ID, OperatorTenantID: domain.TenantID,
		ApprovalRole: "legal", Decision: "approved", ApproverUserID: domain.UserID,
		Reason:            "Legacy approval referenced mutable evidence only.",
		EvidenceReference: "https://evidence.example.test/legacy-pg/approval", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&approvalLegacy, "id = ?", approvalLegacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if approvalLegacy.State != "revoked" || approvalLegacy.Version != 2 || approvalLegacy.RevokedAt == nil {
		t.Fatalf("legacy PostgreSQL approval authorization after Migration 000137 = %#v", approvalLegacy)
	}

	invalid := legacy
	invalid.ID = uuid.New()
	invalid.AuthorizationKey = "new-unbound-pg"
	invalid.Version = 1
	invalid.State = "draft"
	invalid.RevokedAt = nil
	if err := db.Omit(legacyColumns...).Create(&invalid).Error; err == nil {
		t.Fatal("PostgreSQL accepted a new Provider commercial authorization without document digests")
	}
}
