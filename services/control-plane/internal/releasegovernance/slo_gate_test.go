package releasegovernance

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
)

func seedApprovedSLOGate(
	t *testing.T,
	db *gorm.DB,
	tenantID, creatorID uuid.UUID,
	approverIDs []uuid.UUID,
	candidate Candidate,
	now time.Time,
) persistence.Stage6SLOWindow {
	t.Helper()
	if len(approverIDs) < len(requiredApprovalRoles) {
		t.Fatal("approved SLO gate fixture requires four separated approvers")
	}
	now = now.UTC().Truncate(time.Second)
	completedAt := now.Add(-time.Hour)
	startedAt := completedAt.Add(-30 * 24 * time.Hour)
	windowID := uuid.New()
	receipt := map[string]any{
		"schemaVersion":                        "synara.slo-window-evidence-receipt.v1",
		"assessment":                           "evidence-validated-not-slo-passed",
		"windowId":                             windowID.String(),
		"releaseCommit":                        candidate.SourceCommit,
		"environmentClass":                     "production-like",
		"environmentId":                        candidate.EnvironmentID,
		"publicOrigin":                         "https://slo-gate.example.test",
		"windowStartedAt":                      startedAt.Format(time.RFC3339),
		"windowCompletedAt":                    completedAt.Format(time.RFC3339),
		"validatedAt":                          now.Format(time.RFC3339),
		"queryRevision":                        repeat("c", 40),
		"allObjectivesAssessable":              true,
		"declaredMeasurementsWithinObjectives": true,
		"eligibleForHumanGateReview":           true,
		"objectives": map[string]any{
			"availability":        map[string]any{},
			"apiLatency":          map[string]any{},
			"executionStartDelay": map[string]any{},
			"eventDelay":          map[string]any{},
		},
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	window := persistence.Stage6SLOWindow{
		ID: uuid.New(), OperatorTenantID: tenantID, CandidateRecordID: candidate.ID, WindowID: windowID,
		Receipt: encoded, ReceiptSHA256: digest[:], ReceiptSizeBytes: int64(len(encoded)),
		Schema: "synara.slo-window-evidence-receipt.v1", Assessment: "evidence-validated-not-slo-passed",
		ReleaseCommit: candidate.SourceCommit, EnvironmentClass: "production-like", EnvironmentID: candidate.EnvironmentID,
		PublicOrigin: "https://slo-gate.example.test", WindowStartedAt: startedAt, WindowCompletedAt: completedAt,
		ValidatedAt: now, QueryRevision: repeat("c", 40), AllObjectivesAssessable: true, AllObjectivesMet: true,
		EligibleForHumanGateReview: true, WorstBudgetRemainingRatio: 1, BudgetPolicyState: "normal-delivery",
		State: "recorded", Version: 1, CreatedBy: creatorID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&window).Error; err != nil {
		t.Fatal(err)
	}
	for index, role := range requiredApprovalRoles {
		approval := persistence.Stage6SLOApproval{
			ID: uuid.New(), SLOWindowRecordID: window.ID, OperatorTenantID: tenantID,
			Role: role, Decision: "approved", ApproverUserID: approverIDs[index],
			Reason:            "The separated authority approved the exact candidate-bound SLO evidence window.",
			EvidenceReference: "https://evidence.example.test/release-slo-gate/" + role,
			EvidenceSHA256:    stage6authority.DigestPointer("release-slo-gate-" + role), CreatedAt: now,
		}
		if err := db.Create(&approval).Error; err != nil {
			t.Fatal(err)
		}
	}
	approvedAt := now
	if err := db.Model(&persistence.Stage6SLOWindow{}).Where("id = ?", window.ID).Updates(map[string]any{
		"state": "approved", "version": 2, "approved_at": approvedAt, "updated_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	window.State = "approved"
	window.Version = 2
	window.ApprovedAt = &approvedAt
	return window
}
