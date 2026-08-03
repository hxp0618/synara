package releasegovernance

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/operationsexercisegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6operations"
)

var operationsExerciseGateRoles = []string{"operations", "security"}

func seedApprovedOperationsExerciseGate(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	tenantID, creatorID uuid.UUID,
	approverIDs []uuid.UUID,
	candidate Candidate,
	now time.Time,
) persistence.Stage6OperationsExercise {
	t.Helper()
	if len(approverIDs) < len(operationsExerciseGateRoles) {
		t.Fatal("approved Operations exercise gate fixture requires two separated approvers")
	}
	now = now.UTC().Truncate(time.Second)
	for index, role := range operationsExerciseGateRoles {
		var count int64
		if err := db.Model(&persistence.Stage6GovernanceAuthorityGrant{}).
			Where("operator_tenant_id = ? AND user_id = ? AND authority_key = ? AND status = ?", tenantID, approverIDs[index], "operations_exercise."+role, "active").
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			grant := persistence.Stage6GovernanceAuthorityGrant{
				ID: uuid.New(), OperatorTenantID: tenantID, UserID: approverIDs[index],
				AuthorityKey: "operations_exercise." + role, Status: "active", Version: 1,
				ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: creatorID,
				Reason:            "Owner assigned the exact Operations exercise release-gate review function.",
				EvidenceReference: "https://evidence.example.test/authority/release-operations-exercise/" + role,
				EvidenceSHA256:    stage6authority.DigestPointer("release-operations-exercise-" + role),
				CreatedAt:         now, UpdatedAt: now,
			}
			if err := db.Create(&grant).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	receipt, digest, err := stage6operations.Receipt(candidate.CandidateID, candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	service := operationsexercisegovernance.NewService(db, tenantID)
	exercise, err := service.Import(ctx, creatorID, operationsexercisegovernance.ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest,
	}, "release-operations-exercise-gate-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for index, role := range operationsExerciseGateRoles {
		exercise, err = service.RecordApproval(ctx, approverIDs[index], exercise.ID, operationsexercisegovernance.ApprovalInput{
			Role: role, Decision: "approved",
			Reason:            "The separated authority approved the exact candidate-bound Operations exercise release gate.",
			EvidenceReference: "https://evidence.example.test/release-operations-exercise-gate/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "release-operations-exercise-gate-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if exercise.State != "approved" {
		t.Fatalf("Operations exercise gate state = %q", exercise.State)
	}
	var stored persistence.Stage6OperationsExercise
	if err := db.Where("id = ?", exercise.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	return stored
}
