package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6candidate"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSLOApprovalEvidenceDigestMigrationReopensLegacyPostgresWindow(t *testing.T) {
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
	if err := Migrate(ctx, db, migrationsThrough(t, "000141_stage6_compliance_decision_evidence_digests.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "slo-digest-migration-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	encodedCandidate, candidateDigest, err := stage6candidate.LegacyV3Receipt("v0.6.3-stage6.slo-digest", "stage6/slo-digest")
	if err != nil {
		t.Fatal(err)
	}
	lockfileSHA256, _ := hex.DecodeString(strings.Repeat("b", 64))
	evidenceBundleSHA256, _ := hex.DecodeString(strings.TrimPrefix(candidateDigest, "sha256:"))
	finalAssetSetSHA256, _ := hex.DecodeString(strings.Repeat("d", 64))
	candidate := persistence.Stage6ReleaseCandidate{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, CandidateID: "v0.6.3-stage6.slo-digest",
		SourceCommit: strings.Repeat("a", 40), LockfileSHA256: lockfileSHA256,
		EvidenceBundleSHA256: evidenceBundleSHA256, EvidenceBundleReceipt: encodedCandidate,
		EvidenceBundleSchema:      "synara.stage6-candidate-evidence-bundle-validation.v3",
		EvidenceBundleAssessment:  "evidence-consistent-not-ga-approved",
		EvidenceBundleValidatedAt: "2026-07-31T12:00:00Z", EvidenceBundleReceiptSizeBytes: int64(len(encodedCandidate)),
		DesktopArtifactSetSHA256: "sha256:" + strings.Repeat("e", 64), EvidenceReceiptBound: true,
		FinalAssetSetSHA256: finalAssetSetSHA256, EnvironmentID: "stage6/slo-digest",
		ImpactDomains: []string{"code_change"}, State: "draft", Version: 1, CreatedBy: domain.UserID,
		ResidualRisks: []map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	roles := []string{"engineering", "operations", "security", "product"}
	approvers := make([]uuid.UUID, 0, len(roles))
	for index, role := range roles {
		user := persistence.User{
			ID: uuid.New(), Email: "slo-digest-" + role + "@example.test", DisplayName: "SLO Digest " + role,
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		membershipRole := "admin"
		if role == "security" {
			membershipRole = "security_admin"
		}
		if err := db.Create(&persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: user.ID, Role: membershipRole, Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		grant := persistence.Stage6GovernanceAuthorityGrant{
			ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID, AuthorityKey: "release." + role,
			Status: "active", Version: 1, ExpiresAt: now.AddDate(0, 6, 0), GrantedBy: domain.UserID,
			Reason:            "Owner delegated the exact SLO decision function for migration verification.",
			EvidenceReference: "https://evidence.example.test/authority/slo-digest/" + role,
			EvidenceSHA256:    stage6authority.DigestPointer("slo-digest-authority-" + role), CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&grant).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
		_ = index
	}
	windowID := uuid.New()
	completedAt := now.Add(-time.Hour)
	startedAt := completedAt.Add(-30 * 24 * time.Hour)
	receipt := map[string]any{
		"schemaVersion": "synara.slo-window-evidence-receipt.v1", "assessment": "evidence-validated-not-slo-passed",
		"windowId": windowID, "releaseCommit": candidate.SourceCommit, "environmentClass": "production-like",
		"environmentId": candidate.EnvironmentID, "publicOrigin": "https://slo-digest.example.test",
		"windowStartedAt": startedAt, "windowCompletedAt": completedAt, "validatedAt": now,
		"queryRevision": strings.Repeat("c", 40), "allObjectivesAssessable": true,
		"declaredMeasurementsWithinObjectives": true, "eligibleForHumanGateReview": true,
		"objectives": map[string]any{"availability": map[string]any{}, "apiLatency": map[string]any{}, "executionStartDelay": map[string]any{}, "eventDelay": map[string]any{}},
	}
	receiptBytes, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptDigest := sha256.Sum256(receiptBytes)
	window := persistence.Stage6SLOWindow{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, CandidateRecordID: candidate.ID, WindowID: windowID,
		Receipt: receiptBytes, ReceiptSHA256: receiptDigest[:], ReceiptSizeBytes: int64(len(receiptBytes)),
		Schema: "synara.slo-window-evidence-receipt.v1", Assessment: "evidence-validated-not-slo-passed",
		ReleaseCommit: candidate.SourceCommit, EnvironmentClass: "production-like", EnvironmentID: candidate.EnvironmentID,
		PublicOrigin: "https://slo-digest.example.test", WindowStartedAt: startedAt, WindowCompletedAt: completedAt,
		ValidatedAt: now, QueryRevision: strings.Repeat("c", 40), AllObjectivesAssessable: true, AllObjectivesMet: true,
		EligibleForHumanGateReview: true, WorstBudgetRemainingRatio: 1, BudgetPolicyState: "normal-delivery",
		State: "recorded", Version: 1, CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&window).Error; err != nil {
		t.Fatal(err)
	}
	legacyIDs := make([]uuid.UUID, 0, len(roles))
	for index, role := range roles {
		approval := persistence.Stage6SLOApproval{
			ID: uuid.New(), SLOWindowRecordID: window.ID, OperatorTenantID: domain.TenantID,
			Role: role, Decision: "approved", ApproverUserID: approvers[index],
			Reason:            "Legacy SLO decision referenced mutable external evidence without a byte digest.",
			EvidenceReference: "https://evidence.example.test/slo-digest/legacy/" + role, CreatedAt: now,
		}
		if err := db.Omit("EvidenceSHA256", "SupersededAt", "SupersededReason").Create(&approval).Error; err != nil {
			t.Fatal(err)
		}
		legacyIDs = append(legacyIDs, approval.ID)
	}
	if err := db.Model(&persistence.Stage6SLOWindow{}).Where("id = ?", window.ID).Updates(map[string]any{
		"state": "approved", "version": 2, "approved_at": now, "updated_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// Migration 000153 intentionally permits v3 evidence only for terminal
	// historical candidates. This test owns the SLO approval migration, so close
	// its legacy candidate before applying the current migration tail.
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "ready_for_review", "version": 2, "updated_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "rejected", "version": 3, "rejected_at": now, "updated_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&window, "id = ?", window.ID).Error; err != nil {
		t.Fatal(err)
	}
	if window.State != "recorded" || window.Version != 3 || window.ApprovedAt != nil {
		t.Fatalf("legacy SLO window after Migration 000142 = %#v", window)
	}
	for _, approvalID := range legacyIDs {
		var legacy persistence.Stage6SLOApproval
		if err := db.First(&legacy, "id = ?", approvalID).Error; err != nil {
			t.Fatal(err)
		}
		if legacy.SupersededAt == nil || legacy.SupersededReason == nil || !strings.Contains(*legacy.SupersededReason, "Migration 000142") {
			t.Fatalf("legacy SLO approval was not superseded: %#v", legacy)
		}
	}
	for index, role := range roles {
		replacement := persistence.Stage6SLOApproval{
			ID: uuid.New(), SLOWindowRecordID: window.ID, OperatorTenantID: domain.TenantID,
			Role: role, Decision: "approved", ApproverUserID: approvers[index],
			Reason:            "Replacement decision reviewed the exact byte-bound SLO approval evidence.",
			EvidenceReference: "https://evidence.example.test/slo-digest/replacement/" + role,
			EvidenceSHA256:    stage6authority.DigestPointer("slo-digest-replacement-" + role), CreatedAt: now.Add(time.Second),
		}
		if err := db.Create(&replacement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&persistence.Stage6SLOWindow{}).Where("id = ?", window.ID).Updates(map[string]any{
		"state": "approved", "version": 4, "approved_at": now.Add(2 * time.Second), "updated_at": now.Add(2 * time.Second),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Stage6SLOApproval{}).Where("slo_window_record_id = ? AND superseded_at IS NULL", window.ID).
		Update("evidence_sha256", "sha256:"+strings.Repeat("f", 64)).Error; err == nil {
		t.Fatal("PostgreSQL allowed replacement SLO approval digest mutation")
	}
}
