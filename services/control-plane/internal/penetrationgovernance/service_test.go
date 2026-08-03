package penetrationgovernance

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
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6penetration"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPenetrationGovernanceBindsExactReceiptAndRequiresThreeAuthorities(t *testing.T) {
	fixture := newPenetrationFixture(t)
	engagement, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "penetration-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if engagement.State != "recorded" || !engagement.EligibleForHumanGateReview || len(engagement.Assets) != 4 || engagement.CryptographicSignaturesVerified || !engagement.ExternalAuthorityVerificationRequired {
		t.Fatalf("imported Penetration engagement = %#v", engagement)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, engagement.ID, ApprovalInput{Role: "security", Decision: "approved", Reason: "The importer must not approve their own Penetration engagement.", EvidenceReference: "https://evidence.example.test/penetration/self-review", EvidenceSHA256: "sha256:" + strings.Repeat("1", 64)}, "self-review", "127.0.0.1")
	assertProblemCode(t, err, "penetration_engagement_not_reviewable")
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], engagement.ID, ApprovalInput{Role: "engineering", Decision: "approved", Reason: "The assigned authority reviewed evidence with an invalid zero digest.", EvidenceReference: "https://evidence.example.test/penetration/zero-digest", EvidenceSHA256: "sha256:" + strings.Repeat("0", 64)}, "zero-digest", "127.0.0.1")
	assertProblemCode(t, err, "penetration_approval_invalid")
	for index, role := range approvalRoles {
		engagement, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], engagement.ID, ApprovalInput{Role: role, Decision: "approved", Reason: "The assigned authority reviewed the exact candidate-bound Penetration receipt.", EvidenceReference: "https://evidence.example.test/penetration/internal/" + role, EvidenceSHA256: "sha256:" + strings.Repeat(string(rune('2'+index)), 64)}, "penetration-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if engagement.State != "approved" || engagement.ApprovedAt == nil || len(engagement.Approvals) != 3 {
		t.Fatalf("approved Penetration engagement = %#v", engagement)
	}
	for _, approval := range engagement.Approvals {
		if approval.EvidenceSHA256 == nil || approval.SupersededAt != nil || approval.SupersededReason != nil {
			t.Fatalf("active byte-bound Penetration approval = %#v", approval)
		}
	}
	if err := fixture.db.Model(&persistence.Stage6PenetrationEngagement{}).Where("id = ?", engagement.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("exact Penetration receipt was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6PenetrationAsset{}).Where("penetration_engagement_id = ?", engagement.ID).Update("tested", false).Error; err == nil {
		t.Fatal("Penetration asset projection was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6PenetrationApproval{}).Where("penetration_engagement_id = ?", engagement.ID).Update("evidence_sha256", "sha256:"+strings.Repeat("f", 64)).Error; err == nil {
		t.Fatal("Penetration approval evidence digest was mutable")
	}
	if err := fixture.db.Delete(&persistence.Stage6PenetrationEngagement{}, "id = ?", engagement.ID).Error; err == nil {
		t.Fatal("Penetration history was deletable")
	}
}

func TestPenetrationGovernanceRejectsReceiptAndDatabaseGateDrift(t *testing.T) {
	fixture := newPenetrationFixture(t)
	_, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, func(receipt map[string]any) {
		receipt["environmentId"] = "stage6/other"
	}), "candidate-drift", "127.0.0.1")
	assertProblemCode(t, err, "penetration_receipt_invalid")
	_, err = fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, func(receipt map[string]any) {
		receipt["declaredNoUnacceptedHighOrCriticalFindings"] = false
	}), "finding-drift", "127.0.0.1")
	assertProblemCode(t, err, "penetration_receipt_invalid")

	engagement, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "valid", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.Stage6PenetrationEngagement{}).Where("id = ?", engagement.ID).Updates(map[string]any{"state": "approved", "version": 2, "approved_at": fixture.now}).Error; err == nil {
		t.Fatal("SQLite allowed Penetration approval without three separated decisions")
	}
}

func TestPenetrationGovernanceRevalidatesMethodologiesAndFindingProjection(t *testing.T) {
	fixture := newPenetrationFixture(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unrecognized methodology", mutate: func(receipt map[string]any) {
			receipt["methodologies"] = append(receipt["methodologies"].([]any), "vendor-secret-method")
		}},
		{name: "forged eligible open High finding", mutate: func(receipt map[string]any) {
			receipt["findings"] = []any{map[string]any{
				"findingId": "finding-high-open", "severity": "high", "status": "open",
				"affectedAssetTypes": []any{"control-plane-api"},
				"discoveredAt":       "2026-07-30T01:00:00Z", "lastReviewedAt": "2026-07-30T02:00:00Z",
				"retestPassed": false, "riskAcceptance": nil,
			}}
			receipt["findingCounts"] = map[string]any{
				"bySeverity": map[string]any{"critical": 0, "high": 1, "medium": 0, "low": 0, "informational": 0},
				"byStatus":   map[string]any{"open": 1, "remediation-in-progress": 0, "remediated-verified": 0, "risk-accepted": 0},
			}
			receipt["declaredNoUnacceptedHighOrCriticalFindings"] = true
			receipt["eligibleForHumanGateReview"] = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, _, err := stage6penetration.Receipt(fixture.candidate.EnvironmentID)
			if err != nil {
				t.Fatal(err)
			}
			var receipt map[string]any
			if err := json.Unmarshal(encoded, &receipt); err != nil {
				t.Fatal(err)
			}
			test.mutate(receipt)
			encoded, err = json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(encoded)
			digestText := "sha256:" + hex.EncodeToString(digest[:])
			candidate := fixture.candidate
			var candidateReceipt map[string]any
			if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &candidateReceipt); err != nil {
				t.Fatal(err)
			}
			candidateReceipt["receipts"].(map[string]any)["penetration"].(map[string]any)["sha256"] = digestText
			candidate.EvidenceBundleReceipt, err = json.Marshal(candidateReceipt)
			if err != nil {
				t.Fatal(err)
			}
			_, err = bindReceipt(ImportInput{CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(encoded), ReceiptSHA256: digestText}, candidate, fixture.now)
			assertProblemCode(t, err, "penetration_receipt_invalid")
		})
	}
}

type penetrationFixture struct {
	ctx              context.Context
	db               *gorm.DB
	service          *Service
	creatorID        uuid.UUID
	operatorTenantID uuid.UUID
	candidate        persistence.Stage6ReleaseCandidate
	approverIDs      []uuid.UUID
	now              time.Time
}

func newPenetrationFixture(t *testing.T) penetrationFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "penetration-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := stage6candidate.Receipt("v0.6.3-stage6.penetration", "stage6/penetration")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	view, err := releasegovernance.NewService(store.DB(), domain.TenantID).Create(ctx, domain.UserID, releasegovernance.CreateInput{CandidateID: "v0.6.3-stage6.penetration", SourceCommit: strings.Repeat("a", 40), LockfileSHA256: "sha256:" + strings.Repeat("b", 64), EvidenceBundleSHA256: "sha256:" + hex.EncodeToString(digest[:]), EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded), FinalAssetSetSHA256: "sha256:" + strings.Repeat("d", 64), EnvironmentID: "stage6/penetration", ImpactDomains: []string{"runtime_isolation"}}, "candidate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var candidate persistence.Stage6ReleaseCandidate
	if err := store.DB().Where("id = ?", view.ID).Take(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for _, role := range approvalRoles {
		user := persistence.User{ID: uuid.New(), Email: "penetration-" + role + "@example.test", DisplayName: "Penetration " + role, Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
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
		if err := store.DB().Create(&persistence.Stage6GovernanceAuthorityGrant{ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID, AuthorityKey: governanceauthorityKey(role), Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: domain.UserID, Reason: "Owner assigned the exact Penetration governance function.", EvidenceReference: "https://evidence.example.test/authority/penetration/" + role, EvidenceSHA256: stage6authority.DigestPointer("penetration-" + role), CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	return penetrationFixture{ctx: ctx, db: store.DB(), service: service, creatorID: domain.UserID, operatorTenantID: domain.TenantID, candidate: candidate, approverIDs: approvers, now: now}
}

func (f penetrationFixture) importInput(t *testing.T, mutate func(map[string]any)) ImportInput {
	t.Helper()
	encoded, _, err := stage6penetration.Receipt(f.candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		var receipt map[string]any
		if err := json.Unmarshal(encoded, &receipt); err != nil {
			t.Fatal(err)
		}
		mutate(receipt)
		encoded, err = json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
	}
	digest := sha256.Sum256(encoded)
	return ImportInput{CandidateRecordID: f.candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(encoded), ReceiptSHA256: "sha256:" + hex.EncodeToString(digest[:])}
}

func governanceauthorityKey(role string) string { return "penetration." + role }

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}
