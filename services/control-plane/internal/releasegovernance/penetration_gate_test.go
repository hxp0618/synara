package releasegovernance

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/penetrationgovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6penetration"
)

var penetrationGateRoles = []string{"engineering", "product", "security"}

func seedApprovedPenetrationGate(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	tenantID, creatorID uuid.UUID,
	approverIDs []uuid.UUID,
	candidate Candidate,
	now time.Time,
) persistence.Stage6PenetrationEngagement {
	t.Helper()
	if len(approverIDs) < len(penetrationGateRoles) {
		t.Fatal("approved Penetration gate fixture requires three separated approvers")
	}
	now = now.UTC().Truncate(time.Second)
	for index, role := range penetrationGateRoles {
		var count int64
		if err := db.Model(&persistence.Stage6GovernanceAuthorityGrant{}).
			Where("operator_tenant_id = ? AND user_id = ? AND authority_key = ? AND status = ?", tenantID, approverIDs[index], "penetration."+role, "active").
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			grant := persistence.Stage6GovernanceAuthorityGrant{
				ID: uuid.New(), OperatorTenantID: tenantID, UserID: approverIDs[index], AuthorityKey: "penetration." + role,
				Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: creatorID,
				Reason:            "Owner assigned the exact Penetration release-gate review function.",
				EvidenceReference: "https://evidence.example.test/authority/release-penetration/" + role,
				EvidenceSHA256:    stage6authority.DigestPointer("release-penetration-" + role),
				CreatedAt:         now, UpdatedAt: now,
			}
			if err := db.Create(&grant).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	receipt, digest, err := stage6penetration.Receipt(candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	service := penetrationgovernance.NewService(db, tenantID)
	engagement, err := service.Import(ctx, creatorID, penetrationgovernance.ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest,
	}, "release-penetration-gate-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for index, role := range penetrationGateRoles {
		engagement, err = service.RecordApproval(ctx, approverIDs[index], engagement.ID, penetrationgovernance.ApprovalInput{
			Role: role, Decision: "approved", Reason: "The separated authority approved the exact candidate-bound Penetration release gate.",
			EvidenceReference: "https://evidence.example.test/release-penetration-gate/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "release-penetration-gate-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if engagement.State != "approved" {
		t.Fatalf("Penetration gate state = %q", engagement.State)
	}
	var stored persistence.Stage6PenetrationEngagement
	if err := db.Where("id = ?", engagement.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	return stored
}
