package releasegovernance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/internalcostgovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6internalcost"
)

var internalCostGateRoles = []string{"operations", "owner"}

func seedApprovedInternalCostGate(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	tenantID, creatorID uuid.UUID,
	approverIDs []uuid.UUID,
	candidate Candidate,
	now time.Time,
) persistence.Stage6InternalCostReview {
	t.Helper()
	if len(approverIDs) < len(internalCostGateRoles) {
		t.Fatal("approved internal cost gate fixture requires two separated approvers")
	}
	now = now.UTC().Truncate(time.Second)
	for index, role := range internalCostGateRoles {
		var count int64
		if err := db.Model(&persistence.Stage6GovernanceAuthorityGrant{}).
			Where("operator_tenant_id = ? AND user_id = ? AND authority_key = ? AND status = ?", tenantID, approverIDs[index], "internal_cost."+role, "active").
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			grant := persistence.Stage6GovernanceAuthorityGrant{
				ID: uuid.New(), OperatorTenantID: tenantID, UserID: approverIDs[index],
				AuthorityKey: "internal_cost." + role, Status: "active", Version: 1,
				ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: creatorID,
				Reason:            "Owner assigned the exact internal usage and cost release-gate review function.",
				EvidenceReference: "https://evidence.example.test/authority/release-internal-cost/" + role,
				EvidenceSHA256:    stage6authority.DigestPointer("release-internal-cost-" + role),
				CreatedAt:         now, UpdatedAt: now,
			}
			if err := db.Create(&grant).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	var candidateModel persistence.Stage6ReleaseCandidate
	if err := db.Where("id = ?", candidate.ID).Take(&candidateModel).Error; err != nil {
		t.Fatal(err)
	}
	var candidateReceipt struct {
		Candidate struct {
			MigrationTail struct{ Name, SHA256 string } `json:"migrationTail"`
		} `json:"candidate"`
		Receipts map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"receipts"`
	}
	if err := json.Unmarshal(candidateModel.EvidenceBundleReceipt, &candidateReceipt); err != nil {
		t.Fatal(err)
	}
	receipt, digest, err := stage6internalcost.Receipt(
		candidate.CandidateID, candidate.EnvironmentID,
		candidateReceipt.Candidate.MigrationTail.Name, candidateReceipt.Candidate.MigrationTail.SHA256,
	)
	if err != nil {
		t.Fatal(err)
	}
	if digest != candidateReceipt.Receipts["internalCost"].SHA256 {
		t.Fatalf("internal cost receipt digest = %s, candidate expects %s", digest, candidateReceipt.Receipts["internalCost"].SHA256)
	}
	service := internalcostgovernance.NewService(db, tenantID)
	review, err := service.Import(ctx, creatorID, internalcostgovernance.ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest,
	}, "release-internal-cost-gate-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for index, role := range internalCostGateRoles {
		review, err = service.RecordApproval(ctx, approverIDs[index], review.ID, internalcostgovernance.ApprovalInput{
			Role: role, Decision: "approved",
			Reason:            "The separated authority approved the exact candidate-bound internal usage and cost review.",
			EvidenceReference: "https://evidence.example.test/release-internal-cost/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "release-internal-cost-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if review.State != "approved" {
		t.Fatalf("internal cost gate state = %q", review.State)
	}
	var stored persistence.Stage6InternalCostReview
	if err := db.Where("id = ?", review.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	return stored
}
