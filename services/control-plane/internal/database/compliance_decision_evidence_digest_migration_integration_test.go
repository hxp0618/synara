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

func TestComplianceDecisionEvidenceDigestMigrationSupersedesLegacyPostgresRecords(t *testing.T) {
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
	if err := Migrate(ctx, db, migrationsThrough(t, "000140_stage6_governance_authority_evidence_digest.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "compliance-digest-migration-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	operators := make([]uuid.UUID, 0, 2)
	for index := range 2 {
		user := persistence.User{
			ID: uuid.New(), Email: "compliance-digest-pg-" + string(rune('a'+index)) + "@example.test",
			DisplayName: "Compliance Digest PG", Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		operators = append(operators, user.ID)
	}
	for index, binding := range []struct {
		userID       uuid.UUID
		authorityKey string
	}{
		{operators[0], "compliance.evidence.security"},
		{operators[0], "compliance.security"},
		{operators[1], "compliance.security"},
	} {
		digest := "sha256:" + strings.Repeat(string(rune('d'+index)), 64)
		grant := persistence.Stage6GovernanceAuthorityGrant{
			ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: binding.userID, AuthorityKey: binding.authorityKey,
			Status: "active", Version: 1, ExpiresAt: now.AddDate(0, 6, 0), GrantedBy: domain.UserID,
			Reason:            "Owner delegated the exact compliance function for migration verification.",
			EvidenceReference: "https://evidence.example.test/authority/" + string(rune('a'+index)), EvidenceSHA256: &digest,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&grant).Error; err != nil {
			t.Fatal(err)
		}
	}
	program := persistence.Stage6ComplianceProgram{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, ProgramKey: "legacy-compliance-digest",
		Framework: "soc2_type2", ScopeVersion: "2026.1",
		ScopeSummary:           "Legacy compliance program used to prove deterministic evidence digest upgrade behavior.",
		ExecutiveSponsorUserID: domain.UserID, AuditorOrganization: "Independent Auditor LLP",
		AuditorEngagementReference: "https://evidence.example.test/auditor", ObservationStart: now.Add(time.Hour), ObservationEnd: now.AddDate(0, 6, 0),
		EvidenceRepositoryReference: "https://evidence.example.test/repository", EvidenceAccessPolicyReference: "https://evidence.example.test/access-policy",
		EvidenceRetentionDays: 365, VendorRegisterReference: "https://evidence.example.test/vendors", RiskRegisterReference: "https://evidence.example.test/risks",
		State: "draft", Version: 1, CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&program).Error; err != nil {
		t.Fatal(err)
	}
	control := persistence.Stage6ComplianceControl{
		ID: uuid.New(), ProgramID: program.ID, OperatorTenantID: domain.TenantID, ControlID: "LEGACY.1",
		Family: "change_release", Title: "Legacy release manifest", Description: "Control used to retain the exact legacy release-manifest evidence sample.",
		OwnerUserID: operators[0], Cadence: "per_release", EvidenceRequirement: "Retain the immutable release manifest and independent decision evidence.",
		CreatedBy: domain.UserID, CreatedAt: now,
	}
	if err := db.Create(&control).Error; err != nil {
		t.Fatal(err)
	}
	evidence := persistence.Stage6ComplianceEvidence{
		ID: uuid.New(), ProgramID: program.ID, ControlRecordID: control.ID, OperatorTenantID: domain.TenantID,
		EvidenceID: "legacy-manifest", EvidenceType: "release_manifest", PeriodStart: now.Add(-time.Hour), PeriodEnd: now,
		SourceReference: "https://evidence.example.test/manifest", SHA256: []byte(strings.Repeat("a", 32)), MediaType: "application/json",
		Classification: "confidential", CollectedAt: now, RetentionUntil: now.AddDate(2, 0, 0), SubmittedBy: domain.UserID, CreatedAt: now,
	}
	if err := db.Create(&evidence).Error; err != nil {
		t.Fatal(err)
	}
	legacyReview := persistence.Stage6ComplianceEvidenceReview{
		ID: uuid.New(), EvidenceRecordID: evidence.ID, ProgramID: program.ID, OperatorTenantID: domain.TenantID,
		Decision: "accepted", ReviewRole: "security", ReviewerUserID: operators[0], Reason: "Legacy review referenced mutable evidence without an exact digest.",
		EvidenceReference: "https://evidence.example.test/review/legacy", CreatedAt: now,
	}
	if err := db.Omit("EvidenceSHA256", "SupersededAt", "SupersededReason").Create(&legacyReview).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Stage6ComplianceProgram{}).Where("id = ?", program.ID).Updates(map[string]any{
		"state": "ready_for_review", "version": 2, "updated_at": now.Add(time.Second),
	}).Error; err != nil {
		t.Fatal(err)
	}
	legacyDecision := persistence.Stage6ComplianceProgramDecision{
		ID: uuid.New(), ProgramID: program.ID, OperatorTenantID: domain.TenantID, DecisionRole: "security", Decision: "approved",
		DeciderUserID: operators[0], Reason: "Legacy program decision referenced mutable evidence without an exact digest.",
		EvidenceReference: "https://evidence.example.test/decision/legacy", CreatedAt: now,
	}
	if err := db.Omit("EvidenceSHA256", "SupersededAt", "SupersededReason").Create(&legacyDecision).Error; err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&legacyReview, "id = ?", legacyReview.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&legacyDecision, "id = ?", legacyDecision.ID).Error; err != nil {
		t.Fatal(err)
	}
	if legacyReview.SupersededAt == nil || legacyReview.SupersededReason == nil || !strings.Contains(*legacyReview.SupersededReason, "Migration 000141") ||
		legacyDecision.SupersededAt == nil || legacyDecision.SupersededReason == nil || !strings.Contains(*legacyDecision.SupersededReason, "Migration 000141") {
		t.Fatalf("legacy compliance records were not superseded: review=%#v decision=%#v", legacyReview, legacyDecision)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	replacementReview := legacyReview
	replacementReview.ID = uuid.New()
	replacementReview.EvidenceSHA256 = &digest
	replacementReview.SupersededAt = nil
	replacementReview.SupersededReason = nil
	replacementReview.CreatedAt = now.Add(2 * time.Second)
	if err := db.Create(&replacementReview).Error; err != nil {
		t.Fatal(err)
	}
	replacementDecision := legacyDecision
	replacementDecision.ID = uuid.New()
	replacementDecision.DeciderUserID = operators[1]
	replacementDecision.EvidenceSHA256 = &digest
	replacementDecision.SupersededAt = nil
	replacementDecision.SupersededReason = nil
	replacementDecision.CreatedAt = now.Add(2 * time.Second)
	if err := db.Create(&replacementDecision).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Stage6ComplianceProgramDecision{}).Where("id = ?", replacementDecision.ID).
		Update("evidence_sha256", "sha256:"+strings.Repeat("c", 64)).Error; err == nil {
		t.Fatal("PostgreSQL allowed compliance decision evidence digest mutation")
	}
}
