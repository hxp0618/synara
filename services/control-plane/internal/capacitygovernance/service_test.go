package capacitygovernance

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
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6capacity"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestCapacityGovernanceBindsExactReceiptAndRequiresSeparatedAuthorities(t *testing.T) {
	fixture := newCapacityFixture(t)
	run, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "capacity-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "recorded" || !run.EligibleForHumanGateReview || len(run.Phases) != 5 || run.CryptographicSignaturesVerified || !run.ExternalAuthorityVerificationRequired {
		t.Fatalf("imported Capacity run = %#v", run)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, run.ID, ApprovalInput{Role: "engineering", Decision: "approved", Reason: "The importer must not approve their own Capacity run evidence.", EvidenceReference: "https://evidence.example.test/capacity/self-review", EvidenceSHA256: "sha256:" + strings.Repeat("1", 64)}, "self-review", "127.0.0.1")
	assertCapacityProblemCode(t, err, "capacity_run_not_reviewable")
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], run.ID, ApprovalInput{Role: "engineering", Decision: "approved", Reason: "The assigned authority reviewed evidence with an invalid zero digest.", EvidenceReference: "https://evidence.example.test/capacity/zero-digest", EvidenceSHA256: "sha256:" + strings.Repeat("0", 64)}, "zero-digest", "127.0.0.1")
	assertCapacityProblemCode(t, err, "capacity_approval_invalid")
	for index, role := range approvalRoles {
		run, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], run.ID, ApprovalInput{Role: role, Decision: "approved", Reason: "The assigned authority reviewed the exact candidate-bound Capacity receipt.", EvidenceReference: "https://evidence.example.test/capacity/internal/" + role, EvidenceSHA256: "sha256:" + strings.Repeat(string(rune('2'+index)), 64)}, "capacity-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if run.State != "approved" || run.ApprovedAt == nil || len(run.Approvals) != 2 {
		t.Fatalf("approved Capacity run = %#v", run)
	}
	for _, approval := range run.Approvals {
		if approval.EvidenceSHA256 == nil || approval.SupersededAt != nil || approval.SupersededReason != nil {
			t.Fatalf("active byte-bound Capacity approval = %#v", approval)
		}
	}
	if err := fixture.db.Model(&persistence.Stage6CapacityRun{}).Where("id = ?", run.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("exact Capacity receipt was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6CapacityPhase{}).Where("capacity_run_id = ?", run.ID).Update("load_multiplier", 0.1).Error; err == nil {
		t.Fatal("Capacity phase projection was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6CapacityApproval{}).Where("capacity_run_id = ?", run.ID).Update("evidence_sha256", "sha256:"+strings.Repeat("f", 64)).Error; err == nil {
		t.Fatal("Capacity approval evidence digest was mutable")
	}
	if err := fixture.db.Delete(&persistence.Stage6CapacityRun{}, "id = ?", run.ID).Error; err == nil {
		t.Fatal("Capacity history was deletable")
	}
}

func TestCapacityGovernanceRejectsForgedProjectionsAndDatabaseBypass(t *testing.T) {
	fixture := newCapacityFixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"candidate drift": func(receipt map[string]any) { receipt["environmentId"] = "stage6/other" },
		"forged headroom": func(receipt map[string]any) {
			receipt["load"].(map[string]any)["peakSSEConnections"] = 200.0
			receipt["forecastHeadroomCovered"] = true
		},
		"forged objective": func(receipt map[string]any) {
			receipt["measurements"].(map[string]any)["oomKillCount"] = 1
			receipt["declaredMeasurementsWithinObjectives"] = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			input, candidate := fixture.boundMutatedInput(t, mutate)
			_, err := bindReceipt(input, candidate, fixture.now)
			assertCapacityProblemCode(t, err, "capacity_receipt_invalid")
		})
	}
	run, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "valid", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.Stage6CapacityRun{}).Where("id = ?", run.ID).Updates(map[string]any{"state": "approved", "version": 2, "approved_at": fixture.now}).Error; err == nil {
		t.Fatal("SQLite allowed Capacity approval without separated decisions")
	}
}

type capacityFixture struct {
	ctx         context.Context
	db          *gorm.DB
	service     *Service
	creatorID   uuid.UUID
	candidate   persistence.Stage6ReleaseCandidate
	approverIDs []uuid.UUID
	now         time.Time
}

func newCapacityFixture(t *testing.T) capacityFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "capacity-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := stage6candidate.Receipt("v0.6.3-stage6.capacity", "stage6/capacity")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	view, err := releasegovernance.NewService(store.DB(), domain.TenantID).Create(ctx, domain.UserID, releasegovernance.CreateInput{CandidateID: "v0.6.3-stage6.capacity", SourceCommit: strings.Repeat("a", 40), LockfileSHA256: "sha256:" + strings.Repeat("b", 64), EvidenceBundleSHA256: "sha256:" + hex.EncodeToString(digest[:]), EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded), FinalAssetSetSHA256: "sha256:" + strings.Repeat("d", 64), EnvironmentID: "stage6/capacity", ImpactDomains: []string{"code_change"}}, "candidate", "127.0.0.1")
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
		user := persistence.User{ID: uuid.New(), Email: "capacity-" + role + "@example.test", DisplayName: "Capacity " + role, Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.DB().Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.DB().Create(&persistence.Stage6GovernanceAuthorityGrant{ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID, AuthorityKey: "capacity." + role, Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: domain.UserID, Reason: "Owner assigned the exact Capacity governance function.", EvidenceReference: "https://evidence.example.test/authority/capacity/" + role, EvidenceSHA256: stage6authority.DigestPointer("capacity-" + role), CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	return capacityFixture{ctx: ctx, db: store.DB(), service: service, creatorID: domain.UserID, candidate: candidate, approverIDs: approvers, now: now}
}

func (f capacityFixture) importInput(t *testing.T, mutate func(map[string]any)) ImportInput {
	t.Helper()
	encoded, _, err := stage6capacity.Receipt(f.candidate.EnvironmentID)
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

func (f capacityFixture) boundMutatedInput(t *testing.T, mutate func(map[string]any)) (ImportInput, persistence.Stage6ReleaseCandidate) {
	t.Helper()
	input := f.importInput(t, mutate)
	digest := input.ReceiptSHA256
	candidate := f.candidate
	var receipt map[string]any
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt["receipts"].(map[string]any)["capacity"].(map[string]any)["sha256"] = digest
	var err error
	candidate.EvidenceBundleReceipt, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return input, candidate
}

func assertCapacityProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}
