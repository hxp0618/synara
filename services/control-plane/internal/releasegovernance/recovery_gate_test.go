package releasegovernance

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/recoverygovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6recovery"
)

var recoveryGateRoles = []string{"database", "kms", "operations", "security", "storage"}

func seedApprovedRecoveryGate(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	tenantID, creatorID uuid.UUID,
	approverIDs []uuid.UUID,
	candidate Candidate,
	now time.Time,
) persistence.Stage6RecoveryDrill {
	t.Helper()
	if len(approverIDs) < len(recoveryGateRoles) {
		t.Fatal("approved Recovery gate fixture requires five separated approvers")
	}
	now = now.UTC().Truncate(time.Second)
	for index, role := range recoveryGateRoles {
		var count int64
		if err := db.Model(&persistence.Stage6GovernanceAuthorityGrant{}).
			Where("operator_tenant_id = ? AND user_id = ? AND authority_key = ? AND status = ?", tenantID, approverIDs[index], "recovery."+role, "active").
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			grant := persistence.Stage6GovernanceAuthorityGrant{
				ID: uuid.New(), OperatorTenantID: tenantID, UserID: approverIDs[index], AuthorityKey: "recovery." + role,
				Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: creatorID,
				Reason:            "Owner assigned the exact Recovery release-gate review function.",
				EvidenceReference: "https://evidence.example.test/authority/release-recovery/" + role,
				EvidenceSHA256:    stage6authority.DigestPointer("release-recovery-" + role),
				CreatedAt:         now, UpdatedAt: now,
			}
			if err := db.Create(&grant).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	var storedCandidate persistence.Stage6ReleaseCandidate
	if err := db.Where("id = ?", candidate.ID).Take(&storedCandidate).Error; err != nil {
		t.Fatal(err)
	}
	receipt, digest, err := stage6recovery.Receipt(storedCandidate, now)
	if err != nil {
		t.Fatal(err)
	}
	service := recoverygovernance.NewService(db, tenantID)
	drill, err := service.Import(ctx, creatorID, recoverygovernance.ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest,
	}, "release-recovery-gate-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for index, role := range recoveryGateRoles {
		drill, err = service.RecordApproval(ctx, approverIDs[index], drill.ID, recoverygovernance.ApprovalInput{
			Role: role, Decision: "approved", Reason: "The separated authority approved the exact candidate-bound Recovery release gate.",
			EvidenceReference: "https://evidence.example.test/release-recovery-gate/" + role,
			EvidenceSHA256:    *stage6authority.DigestPointer("release-recovery-gate-" + role),
		}, "release-recovery-gate-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if drill.State != "approved" {
		t.Fatalf("Recovery gate state = %q", drill.State)
	}
	var stored persistence.Stage6RecoveryDrill
	if err := db.Where("id = ?", drill.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	return stored
}
