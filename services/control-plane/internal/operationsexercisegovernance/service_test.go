package operationsexercisegovernance

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
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6operations"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestOperationsExerciseGovernanceBindsExactReceiptAndRequiresSeparatedAuthorities(t *testing.T) {
	fixture := newOperationsExerciseFixture(t)
	exercise, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "operations-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if exercise.State != "recorded" || exercise.OperationCount != 48 || exercise.MatrixProfile != requiredMatrixProfile || exercise.AccountCount != 11 ||
		!exercise.EligibleForHumanGateReview || exercise.CryptographicSignaturesVerified || !exercise.ExternalAuthorityVerificationRequired {
		t.Fatalf("imported Operations exercise = %#v", exercise)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, exercise.ID, ApprovalInput{
		Role: "operations", Decision: "approved",
		Reason:            "The importer must not approve their own Operations exercise evidence.",
		EvidenceReference: "https://evidence.example.test/operations/self-review",
		EvidenceSHA256:    "sha256:" + strings.Repeat("1", 64),
	}, "self-review", "127.0.0.1")
	assertOperationsExerciseProblemCode(t, err, "operations_exercise_not_reviewable")
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], exercise.ID, ApprovalInput{
		Role: "operations", Decision: "approved",
		Reason: "The assigned authority must bind the exact non-zero decision evidence bytes.", EvidenceReference: "https://evidence.example.test/operations/zero-digest",
		EvidenceSHA256: "sha256:" + strings.Repeat("0", 64),
	}, "zero-digest", "127.0.0.1")
	assertOperationsExerciseProblemCode(t, err, "operations_exercise_approval_invalid")
	for index, role := range approvalRoles {
		exercise, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], exercise.ID, ApprovalInput{
			Role: role, Decision: "approved",
			Reason:            "The assigned authority reviewed the exact candidate-bound Operations exercise receipt.",
			EvidenceReference: "https://evidence.example.test/operations/internal/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "operations-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if exercise.State != "approved" || exercise.ApprovedAt == nil || len(exercise.Approvals) != 2 {
		t.Fatalf("approved Operations exercise = %#v", exercise)
	}
	for _, approval := range exercise.Approvals {
		if approval.EvidenceSHA256 == nil || approval.SupersededAt != nil || approval.SupersededReason != nil {
			t.Fatalf("active byte-bound Operations exercise approval = %#v", approval)
		}
	}
	if err := fixture.db.Model(&persistence.Stage6OperationsExercise{}).Where("id = ?", exercise.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("exact Operations exercise receipt was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6OperationsExerciseApproval{}).Where("operations_exercise_id = ?", exercise.ID).Update("reason", "mutated decision").Error; err == nil {
		t.Fatal("Operations exercise approval was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6OperationsExerciseApproval{}).Where("operations_exercise_id = ?", exercise.ID).Update("evidence_sha256", "sha256:"+strings.Repeat("f", 64)).Error; err == nil {
		t.Fatal("Operations exercise approval evidence digest was mutable")
	}
	if err := fixture.db.Delete(&persistence.Stage6OperationsExercise{}, "id = ?", exercise.ID).Error; err == nil {
		t.Fatal("Operations exercise history was deletable")
	}
}

func TestOperationsExerciseGovernanceRejectsForgedProjectionsAndDatabaseBypass(t *testing.T) {
	fixture := newOperationsExerciseFixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"candidate drift": func(receipt map[string]any) {
			receipt["candidate"].(map[string]any)["environmentId"] = "stage6/other"
		},
		"forged operation status": func(receipt map[string]any) {
			receipt["operations"].([]any)[0].(map[string]any)["status"] = "failed"
			receipt["operationCounts"].(map[string]any)["passed"] = float64(48)
		},
		"forged fallback": func(receipt map[string]any) {
			receipt["operations"].([]any)[0].(map[string]any)["usedDatabaseClient"] = true
			receipt["fallbackCounts"].(map[string]any)["databaseClient"] = float64(0)
		},
		"forged operation policy": func(receipt map[string]any) {
			receipt["operations"].([]any)[0].(map[string]any)["actor"] = "platform-admin"
			receipt["operations"].([]any)[0].(map[string]any)["actorSubjectReference"] = "actor/platform-admin"
		},
		"forged support actors": func(receipt map[string]any) {
			receipt["supportAccess"].(map[string]any)["requesterSubjectReference"] = "actor/tenant-admin"
		},
		"forged support revocation audit action": func(receipt map[string]any) {
			receipt["supportAccess"].(map[string]any)["revocationAuditAction"] = "support.policy_updated"
		},
		"missing support expiry audit": func(receipt map[string]any) {
			receipt["supportAccess"].(map[string]any)["expiryAuditVisible"] = false
			receipt["supportLifecycleComplete"] = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			input, candidate := fixture.boundMutatedInput(t, mutate)
			_, err := bindReceipt(input, candidate, fixture.now)
			assertOperationsExerciseProblemCode(t, err, "operations_exercise_receipt_invalid")
		})
	}
	exercise, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "valid", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.Stage6OperationsExercise{}).Where("id = ?", exercise.ID).Updates(map[string]any{
		"state": "approved", "version": 2, "approved_at": fixture.now,
	}).Error; err == nil {
		t.Fatal("SQLite allowed Operations exercise approval without separated decisions")
	}
}

type operationsExerciseFixture struct {
	ctx         context.Context
	db          *gorm.DB
	service     *Service
	creatorID   uuid.UUID
	candidate   persistence.Stage6ReleaseCandidate
	approverIDs []uuid.UUID
	now         time.Time
}

func newOperationsExerciseFixture(t *testing.T) operationsExerciseFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "operations-exercise-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidateID, environmentID := "v0.6.3-stage6.operations-exercise", "stage6/operations-exercise"
	encoded, _, err := stage6candidate.Receipt(candidateID, environmentID)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	view, err := releasegovernance.NewService(store.DB(), domain.TenantID).Create(ctx, domain.UserID, releasegovernance.CreateInput{
		CandidateID: candidateID, SourceCommit: strings.Repeat("a", 40),
		LockfileSHA256:              "sha256:" + strings.Repeat("b", 64),
		EvidenceBundleSHA256:        "sha256:" + hex.EncodeToString(digest[:]),
		EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded),
		FinalAssetSetSHA256:         "sha256:" + strings.Repeat("d", 64),
		EnvironmentID:               environmentID, ImpactDomains: []string{"code_change"},
	}, "candidate", "127.0.0.1")
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
		user := persistence.User{
			ID: uuid.New(), Email: "operations-exercise-" + role + "@example.test", DisplayName: "Operations exercise " + role,
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.DB().Create(&persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active", JoinedAt: &now,
			CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.DB().Create(&persistence.Stage6GovernanceAuthorityGrant{
			ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID,
			AuthorityKey: "operations_exercise." + role, Status: "active", Version: 1,
			ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: domain.UserID,
			Reason:            "Owner assigned the exact Operations exercise governance function.",
			EvidenceReference: "https://evidence.example.test/authority/operations-exercise/" + role,
			EvidenceSHA256:    stage6authority.DigestPointer("operations-exercise-" + role),
			CreatedAt:         now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	return operationsExerciseFixture{
		ctx: ctx, db: store.DB(), service: service, creatorID: domain.UserID,
		candidate: candidate, approverIDs: approvers, now: now,
	}
}

func (f operationsExerciseFixture) importInput(t *testing.T, mutate func(map[string]any)) ImportInput {
	t.Helper()
	encoded, _, err := stage6operations.Receipt(f.candidate.CandidateID, f.candidate.EnvironmentID)
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
	return ImportInput{
		CandidateRecordID: f.candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(encoded),
		ReceiptSHA256: "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func (f operationsExerciseFixture) boundMutatedInput(t *testing.T, mutate func(map[string]any)) (ImportInput, persistence.Stage6ReleaseCandidate) {
	t.Helper()
	input := f.importInput(t, mutate)
	candidate := f.candidate
	var receipt map[string]any
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt["receipts"].(map[string]any)["operations"].(map[string]any)["sha256"] = input.ReceiptSHA256
	var err error
	candidate.EvidenceBundleReceipt, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return input, candidate
}

func assertOperationsExerciseProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}
