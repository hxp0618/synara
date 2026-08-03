package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteProviderCommercialDigestUpgradeRevokesLegacyAuthorization(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "provider-digest-upgrade-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{
		"trg_provider_commercial_authorizations_insert",
		"trg_provider_commercial_authorizations_update",
	} {
		if err := store.DB().Exec("DROP TRIGGER IF EXISTS " + trigger).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	legacy := persistence.ProviderCommercialAuthorization{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, AuthorizationKey: "legacy-url-only",
		Provider: "codex", ProviderProduct: "OpenAI API", AccountType: "Enterprise API organization",
		ContractingEntity: "OpenAI contracting entity", CredentialMode: "customer_byok",
		AllowedCredentialScopes: []string{"tenant"}, AllowedRegions: []string{"default"},
		DataUsePolicy: "no_training", RetentionPolicy: "Approved provider retention applies.",
		TermsEffectiveAt: now.Add(-24 * time.Hour), TermsReference: "https://evidence.example.test/legacy/terms",
		AgreementReference:          "https://evidence.example.test/legacy/agreement",
		DPAReference:                "https://evidence.example.test/legacy/dpa",
		ProhibitedUseSummary:        "Consumer credentials and unapproved training are prohibited.",
		TerminationRunbookReference: "https://evidence.example.test/legacy/termination",
		ReviewExpiresAt:             now.Add(90 * 24 * time.Hour), State: "active", Version: 1,
		CreatedBy: domain.UserID, ActivatedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateProviderCommercialAuthorizationSQLiteSafety(ctx, store.DB()); err != nil {
		t.Fatal(err)
	}
	var upgraded persistence.ProviderCommercialAuthorization
	if err := store.DB().First(&upgraded, "id = ?", legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if upgraded.State != "revoked" || upgraded.Version != 2 || upgraded.RevokedAt == nil ||
		upgraded.TermsSHA256 != nil || upgraded.AgreementSHA256 != nil || upgraded.DPASHA256 != nil || upgraded.TerminationRunbookSHA256 != nil {
		t.Fatalf("legacy authorization after digest upgrade = %#v", upgraded)
	}

	for _, trigger := range []string{
		"trg_provider_commercial_authorizations_insert",
		"trg_provider_commercial_authorizations_update",
		"trg_provider_commercial_authorization_approvals_insert",
		"trg_provider_commercial_approval_authority",
	} {
		if err := store.DB().Exec("DROP TRIGGER IF EXISTS " + trigger).Error; err != nil {
			t.Fatal(err)
		}
	}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	approvalLegacy := legacy
	approvalLegacy.ID = uuid.New()
	approvalLegacy.AuthorizationKey = "legacy-approval-url-only"
	approvalLegacy.TermsSHA256 = &digest
	approvalLegacy.AgreementSHA256 = &digest
	approvalLegacy.DPASHA256 = &digest
	approvalLegacy.TerminationRunbookSHA256 = &digest
	if err := store.DB().Create(&approvalLegacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.ProviderCommercialAuthorizationApproval{
		ID: uuid.New(), AuthorizationID: approvalLegacy.ID, OperatorTenantID: domain.TenantID,
		ApprovalRole: "legal", Decision: "approved", ApproverUserID: domain.UserID,
		Reason:            "Legacy approval referenced mutable evidence only.",
		EvidenceReference: "https://evidence.example.test/legacy/approval", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateProviderCommercialAuthorizationSQLiteSafety(ctx, store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().First(&approvalLegacy, "id = ?", approvalLegacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if approvalLegacy.State != "revoked" || approvalLegacy.Version != 2 || approvalLegacy.RevokedAt == nil {
		t.Fatalf("legacy approval authorization after digest upgrade = %#v", approvalLegacy)
	}

	invalid := legacy
	invalid.ID = uuid.New()
	invalid.AuthorizationKey = "new-unbound"
	invalid.State = "draft"
	invalid.Version = 1
	invalid.ActivatedAt = nil
	invalid.RevokedAt = nil
	if err := store.DB().Create(&invalid).Error; err == nil {
		t.Fatal("SQLite accepted a new Provider commercial authorization without document digests")
	}
}
