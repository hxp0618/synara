package releasegovernance

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/capacitygovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6capacity"
)

var capacityGateRoles = []string{"engineering", "operations"}

func seedApprovedCapacityGate(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	tenantID, creatorID uuid.UUID,
	approverIDs []uuid.UUID,
	candidate Candidate,
	now time.Time,
) persistence.Stage6CapacityRun {
	t.Helper()
	if len(approverIDs) < len(capacityGateRoles) {
		t.Fatal("approved Capacity gate fixture requires two separated approvers")
	}
	now = now.UTC().Truncate(time.Second)
	for index, role := range capacityGateRoles {
		var count int64
		if err := db.Model(&persistence.Stage6GovernanceAuthorityGrant{}).
			Where("operator_tenant_id = ? AND user_id = ? AND authority_key = ? AND status = ?", tenantID, approverIDs[index], "capacity."+role, "active").
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			grant := persistence.Stage6GovernanceAuthorityGrant{
				ID: uuid.New(), OperatorTenantID: tenantID, UserID: approverIDs[index], AuthorityKey: "capacity." + role,
				Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: creatorID,
				Reason:            "Owner assigned the exact Capacity release-gate review function.",
				EvidenceReference: "https://evidence.example.test/authority/release-capacity/" + role,
				EvidenceSHA256:    stage6authority.DigestPointer("release-capacity-" + role),
				CreatedAt:         now, UpdatedAt: now,
			}
			if err := db.Create(&grant).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	receipt, digest, err := stage6capacity.Receipt(candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	service := capacitygovernance.NewService(db, tenantID)
	run, err := service.Import(ctx, creatorID, capacitygovernance.ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest,
	}, "release-capacity-gate-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for index, role := range capacityGateRoles {
		run, err = service.RecordApproval(ctx, approverIDs[index], run.ID, capacitygovernance.ApprovalInput{
			Role: role, Decision: "approved", Reason: "The separated authority approved the exact candidate-bound Capacity release gate.",
			EvidenceReference: "https://evidence.example.test/release-capacity-gate/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "release-capacity-gate-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if run.State != "approved" {
		t.Fatalf("Capacity gate state = %q", run.State)
	}
	var stored persistence.Stage6CapacityRun
	if err := db.Where("id = ?", run.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	return stored
}
