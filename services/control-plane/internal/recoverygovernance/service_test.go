package recoverygovernance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
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

func TestRecoveryGovernanceImportsExactReceiptAndRequiresFiveAuthorities(t *testing.T) {
	fixture := newRecoveryFixture(t)
	drill, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "recovery-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if drill.State != "recorded" || !drill.EligibleForHumanGateReview || len(drill.Components) != 4 || drill.CryptographicSignaturesVerified || !drill.ExternalAuthorityVerificationRequired {
		t.Fatalf("imported Recovery drill = %#v", drill)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], drill.ID, ApprovalInput{Role: "database", Decision: "approved", Reason: "A zero digest cannot bind the exact Recovery approval evidence bytes.", EvidenceReference: "https://evidence.example.test/recovery/zero-digest", EvidenceSHA256: "sha256:" + strings.Repeat("0", 64)}, "zero-digest", "127.0.0.1")
	assertProblemCode(t, err, "recovery_approval_invalid")
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, drill.ID, ApprovalInput{Role: "database", Decision: "approved", Reason: "The importer must not approve the exact Recovery drill they imported.", EvidenceReference: "https://evidence.example.test/recovery/self-review", EvidenceSHA256: "sha256:" + strings.Repeat("1", 64)}, "self-review", "127.0.0.1")
	assertProblemCode(t, err, "recovery_drill_not_reviewable")
	for index, role := range approvalRoles {
		drill, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], drill.ID, ApprovalInput{Role: role, Decision: "approved", Reason: "The assigned authority reviewed the exact candidate-bound Recovery receipt.", EvidenceReference: "https://evidence.example.test/recovery/internal/" + role, EvidenceSHA256: "sha256:" + strings.Repeat(string(rune('2'+index)), 64)}, "recovery-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if drill.State != "approved" || drill.ApprovedAt == nil || len(drill.Approvals) != 5 {
		t.Fatalf("approved Recovery drill = %#v", drill)
	}
	for _, approval := range drill.Approvals {
		if approval.EvidenceSHA256 == nil || approval.SupersededAt != nil {
			t.Fatalf("Recovery approval is not byte-bound and active: %#v", approval)
		}
	}
	if err := fixture.db.Model(&persistence.Stage6RecoveryDrill{}).Where("id = ?", drill.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("exact Recovery receipt was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6RecoveryComponent{}).Where("recovery_drill_record_id = ?", drill.ID).Update("measured_rpo_seconds", 999999).Error; err == nil {
		t.Fatal("Recovery component projection was mutable")
	}
	if err := fixture.db.Delete(&persistence.Stage6RecoveryDrill{}, "id = ?", drill.ID).Error; err == nil {
		t.Fatal("Recovery history was deletable")
	}
	if err := fixture.db.Model(&persistence.Stage6RecoveryApproval{}).Where("id = ?", drill.Approvals[0].ID).
		Update("evidence_sha256", "sha256:"+strings.Repeat("9", 64)).Error; err == nil {
		t.Fatal("Recovery approval digest was mutable")
	}
}

func TestRecoveryGovernanceRejectsReceiptDriftAndIneligibleApproval(t *testing.T) {
	fixture := newRecoveryFixture(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "candidate drift", mutate: func(receipt map[string]any) { receipt["candidate"].(map[string]any)["environmentId"] = "stage6/drift" }},
		{name: "component measurement drift", mutate: func(receipt map[string]any) {
			receipt["components"].(map[string]any)["postgresql"].(map[string]any)["measuredRpoSeconds"] = 1
		}},
		{name: "subject drift", mutate: func(receipt map[string]any) { receipt["recoverySubjectSha256"] = "sha256:" + strings.Repeat("f", 64) }},
		{name: "weakened boundary", mutate: func(receipt map[string]any) {
			receipt["verificationBoundary"].(map[string]any)["realBackupRestoreAndApproverAuthorityVerificationRequired"] = false
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, test.mutate), "invalid-recovery", "127.0.0.1")
			assertProblemCode(t, err, "recovery_receipt_invalid")
		})
	}

	drill, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, func(receipt map[string]any) {
		receipt["eligibleForHumanGateReview"] = false
		receipt["allRequiredApprovalsApproved"] = false
		receipt["approvals"].(map[string]any)["storage"].(map[string]any)["decision"] = "reviewed-not-approved"
	}), "failed-recovery", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], drill.ID, ApprovalInput{Role: "database", Decision: "approved", Reason: "An ineligible Recovery drill must remain blocked from internal approval.", EvidenceReference: "https://evidence.example.test/recovery/ineligible", EvidenceSHA256: "sha256:" + strings.Repeat("8", 64)}, "ineligible", "127.0.0.1")
	assertProblemCode(t, err, "recovery_drill_ineligible")
}

type recoveryFixture struct {
	ctx              context.Context
	db               *gorm.DB
	service          *Service
	creatorID        uuid.UUID
	operatorTenantID uuid.UUID
	candidate        persistence.Stage6ReleaseCandidate
	approverIDs      []uuid.UUID
	now              time.Time
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "recovery-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	releaseService := releasegovernance.NewService(store.DB(), domain.TenantID)
	encoded, _, err := stage6candidate.Receipt("v0.6.3-stage6.recovery", "stage6/recovery")
	if err != nil {
		t.Fatal(err)
	}
	var candidateReceipt map[string]any
	if err := json.Unmarshal(encoded, &candidateReceipt); err != nil {
		t.Fatal(err)
	}
	candidateReceipt["candidate"].(map[string]any)["regions"] = []string{"region-one", "region-two"}
	candidateReceipt["receipts"].(map[string]any)["residency"].(map[string]any)["allowedRegions"] = []string{"region-one", "region-two"}
	encoded, err = json.Marshal(candidateReceipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	candidateView, err := releaseService.Create(ctx, domain.UserID, releasegovernance.CreateInput{CandidateID: "v0.6.3-stage6.recovery", SourceCommit: strings.Repeat("a", 40), LockfileSHA256: "sha256:" + strings.Repeat("b", 64), EvidenceBundleSHA256: "sha256:" + hex.EncodeToString(digest[:]), EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded), FinalAssetSetSHA256: "sha256:" + strings.Repeat("d", 64), EnvironmentID: "stage6/recovery", ImpactDomains: []string{"code_change"}}, "candidate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var candidate persistence.Stage6ReleaseCandidate
	if err := store.DB().Where("id = ?", candidateView.ID).Take(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for index, role := range approvalRoles {
		user := persistence.User{ID: uuid.New(), Email: "recovery-" + role + "@example.test", DisplayName: "Recovery " + role, Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
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
		grant := persistence.Stage6GovernanceAuthorityGrant{ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID, AuthorityKey: "recovery." + role, Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: domain.UserID, Reason: "Owner assigned the exact Recovery governance function.", EvidenceReference: "https://evidence.example.test/authority/recovery/" + role, EvidenceSHA256: stage6authority.DigestPointer("recovery-" + role), CreatedAt: now, UpdatedAt: now}
		if err := store.DB().Create(&grant).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
		_ = index
	}
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	return recoveryFixture{ctx: ctx, db: store.DB(), service: service, creatorID: domain.UserID, operatorTenantID: domain.TenantID, candidate: candidate, approverIDs: approvers, now: now}
}

func (f recoveryFixture) importInput(t *testing.T, mutate func(map[string]any)) ImportInput {
	t.Helper()
	receipt := validRecoveryReceipt(t, f.candidate, f.now)
	if mutate != nil {
		mutate(receipt)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return ImportInput{CandidateRecordID: f.candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(encoded), ReceiptSHA256: "sha256:" + hex.EncodeToString(digest[:])}
}

func validRecoveryReceipt(t *testing.T, candidate persistence.Stage6ReleaseCandidate, validatedAt time.Time) map[string]any {
	t.Helper()
	candidateJSON, err := expectedRecoveryCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var candidateValue map[string]any
	if err := json.Unmarshal(candidateJSON, &candidateValue); err != nil {
		t.Fatal(err)
	}
	restoredJSON, err := expectedRestoredIdentity(candidateJSON)
	if err != nil {
		t.Fatal(err)
	}
	var restoredValue map[string]any
	if err := json.Unmarshal(restoredJSON, &restoredValue); err != nil {
		t.Fatal(err)
	}
	completedAt := validatedAt.Add(-time.Hour)
	startedAt := completedAt.Add(-2 * time.Hour)
	profiles := map[string]string{"postgresql": "postgresql-pitr", "object-storage": "object-versioned-replica", "kms": "kms-vault-raft-snapshot", "queue": "queue-postgres-outbox-replay"}
	components := map[string]any{}
	for index, key := range []string{"kms", "object-storage", "postgresql", "queue"} {
		objectives := componentObjectives[key][profiles[key]]
		restoredThrough := startedAt.Add(time.Duration(5+index) * time.Minute)
		measuredRPO := math.Min(objectives[0], 120)
		sourceCutoff := restoredThrough.Add(time.Duration(measuredRPO) * time.Second)
		recoveryStarted := sourceCutoff.Add(5 * time.Minute)
		measuredRTO := math.Min(objectives[1], 1800)
		serviceReady := recoveryStarted.Add(time.Duration(measuredRTO) * time.Second)
		components[key] = map[string]any{"profile": profiles[key], "sourceAuthority": "source-" + key, "restoreTarget": "restore-" + key, "sourceRegion": "region-one", "restoreRegion": "region-two", "sourceFailureDomain": "region-one/source", "restoreFailureDomain": "region-two/restore", "sourceCutoffAt": formatUTC(sourceCutoff), "restoredThroughAt": formatUTC(restoredThrough), "recoveryStartedAt": formatUTC(recoveryStarted), "serviceReadyAt": formatUTC(serviceReady), "measuredRpoSeconds": sourceCutoff.Sub(restoredThrough).Seconds(), "rpoObjectiveSeconds": objectives[0], "rpoWithinObjective": true, "measuredRtoSeconds": serviceReady.Sub(recoveryStarted).Seconds(), "rtoObjectiveSeconds": objectives[1], "rtoWithinObjective": true, "restoreServedCanary": true, "evidence": map[string]any{"backupEvidence": evidenceRef(key + "-backup"), "restoreEvidence": evidenceRef(key + "-restore"), "clientEvidence": evidenceRef(key + "-client")}}
	}
	drillID := uuid.New()
	candidateBinding := sha256.Sum256(mustJSON(candidateValue))
	subject := map[string]any{"drillId": drillID.String(), "candidate": candidateValue, "startedAt": formatUTC(startedAt), "completedAt": formatUTC(completedAt), "restoredReleaseIdentity": restoredValue, "components": components}
	subjectDigest := sha256.Sum256(mustJSON(subject))
	subjectSHA := "sha256:" + hex.EncodeToString(subjectDigest[:])
	approvals := map[string]any{}
	approvalEvidence := map[string]any{}
	for _, role := range approvalRoles {
		ref := evidenceRef(role + "-approval")
		approvals[role] = map[string]any{"role": role, "approverId": role + "-source-approver", "decision": "approved-for-human-gate-review", "approvedAt": formatUTC(completedAt.Add(15 * time.Minute)), "expiresAt": formatUTC(validatedAt.Add(30 * 24 * time.Hour)), "subjectSha256": subjectSHA, "evidence": ref}
		approvalEvidence[role] = ref
	}
	return map[string]any{"schemaVersion": receiptSchema, "drillId": drillID.String(), "candidate": candidateValue, "candidateBindingSha256": "sha256:" + hex.EncodeToString(candidateBinding[:]), "restoredReleaseIdentity": restoredValue, "startedAt": formatUTC(startedAt), "completedAt": formatUTC(completedAt), "recoverySubjectSha256": subjectSHA, "manifest": evidenceRef("manifest"), "components": components, "approvals": approvals, "approvalEvidence": approvalEvidence, "declaredMeasurementsWithinObjectives": true, "allRestoreCanariesPassed": true, "allRequiredApprovalsApproved": true, "releaseEligibleEnvironment": true, "eligibleForHumanGateReview": true, "verificationBoundary": map[string]any{"approvalContentAndSubjectValidated": true, "cryptographicSignaturesVerified": false, "realBackupRestoreAndApproverAuthorityVerificationRequired": true}, "validatedAt": formatUTC(validatedAt), "assessment": receiptAssessment}
}

func evidenceRef(value string) map[string]any {
	digest := sha256.Sum256([]byte(value))
	return map[string]any{"path": value + ".json", "sha256": "sha256:" + hex.EncodeToString(digest[:])}
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}
