package slogovernance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/releasegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6candidate"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSLOGovernanceImportsExactWindowAndRequiresFourSeparatedAuthorities(t *testing.T) {
	fixture := newSLOFixture(t)
	input := fixture.importInput(t, true)
	window, err := fixture.service.Import(fixture.ctx, fixture.creatorID, input, "slo-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !window.EligibleForHumanGateReview || window.State != "recorded" || len(window.Objectives) != 4 || window.BudgetPolicyState != "normal-delivery" {
		t.Fatalf("imported SLO window = %#v", window)
	}
	if _, err := fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], window.ID, ApprovalInput{
		Role: "engineering", Decision: "approved", Reason: "A zero digest cannot bind the exact SLO approval evidence bytes.",
		EvidenceReference: "https://evidence.example.test/slo/zero-digest",
		EvidenceSHA256:    "sha256:" + strings.Repeat("0", 64),
	}, "zero-digest", "127.0.0.1"); problemCode(err) != "slo_approval_invalid" {
		t.Fatalf("zero digest error = %v", err)
	}
	if _, err := fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, window.ID, ApprovalInput{
		Role: "engineering", Decision: "approved", Reason: "The importer must not approve their own SLO evidence window.",
		EvidenceReference: "https://evidence.example.test/slo/self-review",
		EvidenceSHA256:    "sha256:" + strings.Repeat("1", 64),
	}, "self-review", "127.0.0.1"); problemCode(err) != "slo_window_not_reviewable" {
		t.Fatalf("self-review error = %v", err)
	}
	for index, role := range approvalRoles {
		window, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], window.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "The assigned authority reviewed the exact production-like SLO evidence receipt.",
			EvidenceReference: "https://evidence.example.test/slo/approval/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "slo-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if window.State != "approved" || window.ApprovedAt == nil || len(window.Approvals) != 4 {
		t.Fatalf("approved SLO window = %#v", window)
	}
	for _, approval := range window.Approvals {
		if approval.EvidenceSHA256 == nil || approval.SupersededAt != nil {
			t.Fatalf("SLO approval is not byte-bound and active: %#v", approval)
		}
	}
	if err := fixture.db.Model(&persistence.Stage6SLOObjective{}).Where("slo_window_record_id = ?", window.ID).Update("good_ratio", 0).Error; err == nil {
		t.Fatal("SLO objective history was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6SLOWindow{}).Where("id = ?", window.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("exact SLO receipt was mutable")
	}
	if err := fixture.db.Delete(&persistence.Stage6SLOWindow{}, "id = ?", window.ID).Error; err == nil {
		t.Fatal("SLO window history was deletable")
	}
	if err := fixture.db.Model(&persistence.Stage6SLOApproval{}).Where("id = ?", window.Approvals[0].ID).
		Update("evidence_sha256", "sha256:"+strings.Repeat("9", 64)).Error; err == nil {
		t.Fatal("SLO approval digest was mutable")
	}
}

func TestSLOGovernanceRetainsFailedWindowButBlocksApproval(t *testing.T) {
	fixture := newSLOFixture(t)
	input := fixture.importInput(t, false)
	window, err := fixture.service.Import(fixture.ctx, fixture.creatorID, input, "slo-failed", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if window.EligibleForHumanGateReview || window.AllObjectivesMet {
		t.Fatalf("failed window became eligible: %#v", window)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], window.ID, ApprovalInput{
		Role: "engineering", Decision: "approved", Reason: "Attempt to approve an objectively failed SLO window must fail closed.",
		EvidenceReference: "https://evidence.example.test/slo/failed-approval",
		EvidenceSHA256:    "sha256:" + strings.Repeat("6", 64),
	}, "failed-approval", "127.0.0.1")
	if problemCode(err) != "slo_window_ineligible" {
		t.Fatalf("failed approval error = %v", err)
	}
	window, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], window.ID, ApprovalInput{
		Role: "engineering", Decision: "rejected", Reason: "The SLO window missed its objective and must remain retained as failed evidence.",
		EvidenceReference: "https://evidence.example.test/slo/rejection",
		EvidenceSHA256:    "sha256:" + strings.Repeat("7", 64),
	}, "slo-rejection", "127.0.0.1")
	if err != nil || window.State != "rejected" || window.RejectedAt == nil {
		t.Fatalf("rejected failed window = %#v, err = %v", window, err)
	}
}

func TestSLOGovernanceRejectsNonCanonicalReceiptProjections(t *testing.T) {
	fixture := newSLOFixture(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "query revision", mutate: func(receipt map[string]any) { receipt["queryRevision"] = strings.Repeat("A", 40) }},
		{name: "duplicate probe region", mutate: func(receipt map[string]any) {
			receipt["externalProbe"].(map[string]any)["regions"] = []string{"region-a", "region-a", "region-c"}
		}},
		{name: "understated probe samples", mutate: func(receipt map[string]any) {
			probe := receipt["externalProbe"].(map[string]any)
			probe["expectedSamples"] = 259199
			probe["observedSamples"] = 259199
			receipt["objectives"].(map[string]any)["availability"].(map[string]any)["sampleCount"] = 259199
		}},
		{name: "unexpected objective field", mutate: func(receipt map[string]any) {
			receipt["objectives"].(map[string]any)["availability"].(map[string]any)["unreviewedProjection"] = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receipt := sloReceiptMap(true)
			test.mutate(receipt)
			_, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importReceipt(t, receipt), "invalid-slo", "127.0.0.1")
			if problemCode(err) != "slo_receipt_invalid" {
				t.Fatalf("invalid receipt error = %v", err)
			}
		})
	}
}

type sloFixture struct {
	ctx              context.Context
	db               *gorm.DB
	service          *Service
	creatorID        uuid.UUID
	operatorTenantID uuid.UUID
	candidateID      uuid.UUID
	approverIDs      []uuid.UUID
	now              time.Time
}

func newSLOFixture(t *testing.T) sloFixture {
	t.Helper()
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "slo-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := releasegovernance.NewService(store.DB(), domain.TenantID).Create(ctx, domain.UserID, candidateInput(), "candidate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for index, role := range approvalRoles {
		user := persistence.User{ID: uuid.New(), Email: "slo-" + role + "@example.test", DisplayName: "SLO " + role, Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		membershipRole := "admin"
		if role == "security" {
			membershipRole = "security_admin"
		}
		if err := store.DB().Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: user.ID, Role: membershipRole, Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		grant := persistence.Stage6GovernanceAuthorityGrant{ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID, AuthorityKey: "release." + role, Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: domain.UserID, Reason: "Owner assigned the exact SLO review authority for this release function.", EvidenceReference: "https://evidence.example.test/authority/slo/" + role, EvidenceSHA256: stage6authority.DigestPointer("slo-" + role), CreatedAt: now, UpdatedAt: now}
		if err := store.DB().Create(&grant).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
		_ = index
	}
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	return sloFixture{ctx: ctx, db: store.DB(), service: service, creatorID: domain.UserID, operatorTenantID: domain.TenantID, candidateID: candidate.ID, approverIDs: approvers, now: now}
}

func (f sloFixture) importInput(t *testing.T, passing bool) ImportInput {
	t.Helper()
	return f.importReceipt(t, sloReceiptMap(passing))
}

func (f sloFixture) importReceipt(t *testing.T, receipt map[string]any) ImportInput {
	t.Helper()
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return ImportInput{CandidateRecordID: f.candidateID, ReceiptBase64: base64.StdEncoding.EncodeToString(encoded), ReceiptSHA256: "sha256:" + hex.EncodeToString(digest[:])}
}

func sloReceiptMap(passing bool) map[string]any {
	availability := objectiveMap("availability", 0.999, 259200, passing)
	objectives := map[string]any{
		"availability":        availability,
		"apiLatency":          objectiveMap("apiLatency", 0.99, 10000, true),
		"executionStartDelay": objectiveMap("executionStartDelay", 0.99, 100, true),
		"eventDelay":          objectiveMap("eventDelay", 0.999, 1000, true),
	}
	return map[string]any{
		"schemaVersion": receiptSchema, "windowId": uuid.NewString(), "releaseCommit": strings.Repeat("a", 40),
		"environmentClass": "production-like", "environmentId": "stage6/slo", "publicOrigin": "https://slo.example.test",
		"windowStartedAt": "2026-07-01T00:00:00Z", "windowCompletedAt": "2026-07-31T00:00:00Z", "windowSeconds": 2592000,
		"queryRevision": strings.Repeat("c", 40), "manifest": map[string]any{},
		"externalProbe":                 map[string]any{"regions": []string{"region-a", "region-b", "region-c"}, "regionCount": 3, "cadenceSeconds": 30, "expectedSamples": 259200, "observedSamples": 259200, "coverageRatio": 1.0},
		"externalProbeCoverageComplete": true, "objectives": objectives,
		"allObjectivesAssessable": true, "declaredMeasurementsWithinObjectives": passing,
		"alertSummary": map[string]any{}, "companionSignals": map[string]any{"reviewed": true},
		"burnReview": map[string]any{}, "burnReviewComplete": true, "evidence": map[string]any{},
		"releaseEligibleEnvironment": true, "eligibleForHumanGateReview": passing,
		"validatedAt": "2026-07-31T01:00:00Z", "assessment": receiptAssessment,
	}
}

func objectiveMap(key string, target float64, samples int64, passing bool) map[string]any {
	good := 1.0
	if !passing {
		good = target - .001
	}
	remaining := budgetRemaining(good, target)
	return map[string]any{"targetRatio": target, "goodRatio": good, "sampleCount": samples, "minimumSamples": objectiveMinimumSamples[key], "volumeSufficient": true, "errorBudgetRemainingRatio": remaining, "errorBudgetPolicyState": budgetPolicy(remaining), "noDataIntervalCount": 0, "excludedSampleCount": 0, "querySucceeded": true, "assessable": true, "objectiveMet": passing}
}

func candidateInput() releasegovernance.CreateInput {
	encoded, digest, err := stage6candidate.Receipt("v0.6.3-stage6.slo", "stage6/slo")
	if err != nil {
		panic(err)
	}
	return releasegovernance.CreateInput{CandidateID: "v0.6.3-stage6.slo", SourceCommit: strings.Repeat("a", 40), LockfileSHA256: "sha256:" + strings.Repeat("b", 64), EvidenceBundleSHA256: digest, EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded), FinalAssetSetSHA256: "sha256:" + strings.Repeat("d", 64), EnvironmentID: "stage6/slo", ImpactDomains: []string{"code_change"}}
}

func problemCode(err error) string {
	var item *problem.Error
	if errors.As(err, &item) {
		return item.Code
	}
	return ""
}
