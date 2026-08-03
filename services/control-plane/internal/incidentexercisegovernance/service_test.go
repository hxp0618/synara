package incidentexercisegovernance

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
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6incident"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestIncidentExerciseGovernanceBindsExactReceiptAndRequiresSeparatedAuthorities(t *testing.T) {
	fixture := newIncidentExerciseFixture(t)
	exercise, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "incident-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if exercise.State != "recorded" || !exercise.EligibleForHumanGateReview || exercise.CryptographicSignaturesVerified || !exercise.DeploymentAuthorityVerificationRequired {
		t.Fatalf("imported Incident exercise = %#v", exercise)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, exercise.ID, ApprovalInput{
		Role: "operations", Decision: "approved",
		Reason:            "The importer must not approve their own Incident exercise evidence.",
		EvidenceReference: "https://evidence.example.test/incident/self-review",
		EvidenceSHA256:    "sha256:" + strings.Repeat("1", 64),
	}, "self-review", "127.0.0.1")
	assertIncidentExerciseProblemCode(t, err, "incident_exercise_not_reviewable")
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], exercise.ID, ApprovalInput{
		Role: "operations", Decision: "approved",
		Reason: "The assigned authority reviewed evidence with an invalid zero digest.", EvidenceReference: "https://evidence.example.test/incident/zero-digest",
		EvidenceSHA256: "sha256:" + strings.Repeat("0", 64),
	}, "zero-digest", "127.0.0.1")
	assertIncidentExerciseProblemCode(t, err, "incident_exercise_approval_invalid")
	for index, role := range approvalRoles {
		exercise, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], exercise.ID, ApprovalInput{
			Role: role, Decision: "approved",
			Reason:            "The assigned authority reviewed the exact candidate-bound Incident exercise receipt.",
			EvidenceReference: "https://evidence.example.test/incident/internal/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "incident-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if exercise.State != "approved" || exercise.ApprovedAt == nil || len(exercise.Approvals) != 2 {
		t.Fatalf("approved Incident exercise = %#v", exercise)
	}
	for _, approval := range exercise.Approvals {
		if approval.EvidenceSHA256 == nil || approval.SupersededAt != nil || approval.SupersededReason != nil {
			t.Fatalf("active byte-bound Incident exercise approval = %#v", approval)
		}
	}
	if err := fixture.db.Model(&persistence.Stage6IncidentExercise{}).Where("id = ?", exercise.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("exact Incident exercise receipt was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6IncidentExerciseApproval{}).Where("incident_exercise_id = ?", exercise.ID).Update("reason", "mutated decision").Error; err == nil {
		t.Fatal("Incident exercise approval was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6IncidentExerciseApproval{}).Where("incident_exercise_id = ?", exercise.ID).Update("evidence_sha256", "sha256:"+strings.Repeat("f", 64)).Error; err == nil {
		t.Fatal("Incident exercise approval evidence digest was mutable")
	}
	if err := fixture.db.Delete(&persistence.Stage6IncidentExercise{}, "id = ?", exercise.ID).Error; err == nil {
		t.Fatal("Incident exercise history was deletable")
	}
}

func TestIncidentExerciseGovernanceRejectsForgedProjectionsAndDatabaseBypass(t *testing.T) {
	fixture := newIncidentExerciseFixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"candidate drift": func(receipt map[string]any) { receipt["environmentId"] = "stage6/other" },
		"forged paging": func(receipt map[string]any) {
			receipt["paging"].(map[string]any)["acknowledgedAt"] = "2026-07-30T00:18:00Z"
			receipt["pagingExerciseComplete"] = true
		},
		"forged employee notification delivery": func(receipt map[string]any) {
			receipt["employeeNotificationDelivery"].(map[string]any)["deliverySucceeded"] = false
			receipt["employeeNotificationDeliveryComplete"] = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			input, candidate := fixture.boundMutatedInput(t, mutate)
			_, err := bindReceipt(input, candidate, fixture.now)
			assertIncidentExerciseProblemCode(t, err, "incident_exercise_receipt_invalid")
		})
	}
	exercise, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t, nil), "valid", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.Stage6IncidentExercise{}).Where("id = ?", exercise.ID).Updates(map[string]any{
		"state": "approved", "version": 2, "approved_at": fixture.now,
	}).Error; err == nil {
		t.Fatal("SQLite allowed Incident exercise approval without separated decisions")
	}
}

type incidentExerciseFixture struct {
	ctx         context.Context
	db          *gorm.DB
	service     *Service
	creatorID   uuid.UUID
	candidate   persistence.Stage6ReleaseCandidate
	approverIDs []uuid.UUID
	now         time.Time
}

func newIncidentExerciseFixture(t *testing.T) incidentExerciseFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "incident-exercise-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := stage6candidate.Receipt("v0.6.3-stage6.incident-exercise", "stage6/incident-exercise")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	view, err := releasegovernance.NewService(store.DB(), domain.TenantID).Create(ctx, domain.UserID, releasegovernance.CreateInput{
		CandidateID: "v0.6.3-stage6.incident-exercise", SourceCommit: strings.Repeat("a", 40),
		LockfileSHA256:              "sha256:" + strings.Repeat("b", 64),
		EvidenceBundleSHA256:        "sha256:" + hex.EncodeToString(digest[:]),
		EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded),
		FinalAssetSetSHA256:         "sha256:" + strings.Repeat("d", 64),
		EnvironmentID:               "stage6/incident-exercise", ImpactDomains: []string{"code_change"},
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
			ID: uuid.New(), Email: "incident-exercise-" + role + "@example.test",
			DisplayName: "Incident exercise " + role, Status: "active",
			EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.DB().Create(&persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.DB().Create(&persistence.Stage6GovernanceAuthorityGrant{
			ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID,
			AuthorityKey: "incident_exercise." + role, Status: "active", Version: 1,
			ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: domain.UserID,
			Reason:            "Owner assigned the exact Incident exercise governance function.",
			EvidenceReference: "https://evidence.example.test/authority/incident-exercise/" + role,
			EvidenceSHA256:    stage6authority.DigestPointer("incident-exercise-" + role),
			CreatedAt:         now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	return incidentExerciseFixture{
		ctx: ctx, db: store.DB(), service: service, creatorID: domain.UserID,
		candidate: candidate, approverIDs: approvers, now: now,
	}
}

func (f incidentExerciseFixture) importInput(t *testing.T, mutate func(map[string]any)) ImportInput {
	t.Helper()
	encoded, _, err := stage6incident.Receipt(f.candidate.EnvironmentID)
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

func (f incidentExerciseFixture) boundMutatedInput(t *testing.T, mutate func(map[string]any)) (ImportInput, persistence.Stage6ReleaseCandidate) {
	t.Helper()
	input := f.importInput(t, mutate)
	candidate := f.candidate
	var receipt map[string]any
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt["receipts"].(map[string]any)["incident"].(map[string]any)["sha256"] = input.ReceiptSHA256
	var err error
	candidate.EvidenceBundleReceipt, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return input, candidate
}

func assertIncidentExerciseProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}
