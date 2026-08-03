package releasegovernance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	providercommercial "github.com/synara-ai/synara/services/control-plane/internal/providercommercial"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6capacity"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6incident"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6internalcost"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6operations"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6penetration"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestReleaseGovernanceRequiresSeparatedApprovalsAndRecordsFinalDecision(t *testing.T) {
	fixture := newReleaseGovernanceFixture(t)
	candidate := fixture.createCandidate(t)
	candidate = fixture.transition(t, fixture.creatorID, candidate, "ready_for_review", nil)

	if _, err := fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, candidate.ID, ApprovalInput{
		Role: "engineering", Decision: "approved", Reason: "Creator cannot approve the same candidate.",
		EvidenceReference: "https://evidence.example.test/engineering", EvidenceSHA256: testReleaseApprovalDigest("creator-engineering"),
	}, "creator-self-approval", "127.0.0.1"); err == nil {
		t.Fatal("candidate creator approved their own release")
	}
	_, err := fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[3], candidate.ID, ApprovalInput{
		Role: "security", Decision: "approved", Reason: "A Product authority holder must not self-assert Security authority.",
		EvidenceReference: "https://evidence.example.test/self-asserted-security", EvidenceSHA256: testReleaseApprovalDigest("self-asserted-security"),
	}, "self-asserted-security", "127.0.0.1")
	assertProblemCode(t, err, "governance_authority_required")
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], candidate.ID, ApprovalInput{
		Role: "engineering", Decision: "approved", Reason: "Reject mutable or missing evidence bytes before recording a decision.",
		EvidenceReference: "https://evidence.example.test/engineering-unbound",
		EvidenceSHA256:    "sha256:" + strings.Repeat("0", 64),
	}, "unbound-engineering-evidence", "127.0.0.1")
	assertProblemCode(t, err, "release_approval_invalid")

	for index, role := range requiredApprovalRoles {
		var err error
		candidate, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], candidate.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "Reviewed exact candidate evidence and approved the bounded role.",
			EvidenceReference: "https://evidence.example.test/" + role, EvidenceSHA256: testReleaseApprovalDigest("primary-" + role),
		}, "approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(candidate.Approvals) != 4 || candidate.State != "ready_for_review" {
		t.Fatalf("candidate approvals/state = %d/%q", len(candidate.Approvals), candidate.State)
	}
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Attempt approval before the candidate-bound SLO window is independently approved.",
	}, "missing-slo-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_slo_gate_incomplete")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed release approval without an approved candidate-bound SLO window")
	}
	seedApprovedSLOGate(t, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Attempt approval before the candidate-bound Recovery drill is independently approved.",
	}, "missing-recovery-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_recovery_gate_incomplete")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed release approval without an approved candidate-bound Recovery drill")
	}
	otherCandidate, err := fixture.service.Create(
		fixture.ctx, fixture.creatorID,
		newCandidateCreateInput("v0.6.3-stage6.other-recovery", "stage6/other-recovery", []string{"code_change"}),
		"other-recovery-candidate", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	seedApprovedRecoveryGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, otherCandidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "A Recovery drill approved for another candidate must not satisfy this release gate.",
	}, "cross-candidate-recovery-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_recovery_gate_incomplete")
	seedApprovedRecoveryGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Attempt approval before the candidate-bound Penetration engagement is independently approved.",
	}, "missing-penetration-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_penetration_gate_incomplete")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed release approval without an approved candidate-bound Penetration engagement")
	}
	otherPenetrationCandidate, err := fixture.service.Create(
		fixture.ctx, fixture.creatorID,
		newCandidateCreateInput("v0.6.3-stage6.other-penetration", "stage6/other-penetration", []string{"code_change"}),
		"other-penetration-candidate", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	seedApprovedPenetrationGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, otherPenetrationCandidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "A Penetration engagement approved for another candidate must not satisfy this release gate.",
	}, "cross-candidate-penetration-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_penetration_gate_incomplete")
	seedApprovedPenetrationGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Attempt approval before the candidate-bound Capacity run is independently approved.",
	}, "missing-capacity-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_capacity_gate_incomplete")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed release approval without an approved candidate-bound Capacity run")
	}
	otherCapacityCandidate, err := fixture.service.Create(
		fixture.ctx, fixture.creatorID,
		newCandidateCreateInput("v0.6.3-stage6.other-capacity", "stage6/other-capacity", []string{"code_change"}),
		"other-capacity-candidate", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	seedApprovedCapacityGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, otherCapacityCandidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "A Capacity run approved for another candidate must not satisfy this release gate.",
	}, "cross-candidate-capacity-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_capacity_gate_incomplete")
	seedApprovedCapacityGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Attempt approval before the candidate-bound Incident exercise is independently approved.",
	}, "missing-incident-exercise-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_incident_exercise_gate_incomplete")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed release approval without an approved candidate-bound Incident exercise")
	}
	otherIncidentExerciseCandidate, err := fixture.service.Create(
		fixture.ctx, fixture.creatorID,
		newCandidateCreateInput("v0.6.3-stage6.other-incident-exercise", "stage6/other-incident-exercise", []string{"code_change"}),
		"other-incident-exercise-candidate", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	seedApprovedIncidentExerciseGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, otherIncidentExerciseCandidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "An Incident exercise approved for another candidate must not satisfy this release gate.",
	}, "cross-candidate-incident-exercise-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_incident_exercise_gate_incomplete")
	seedApprovedIncidentExerciseGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Attempt approval before the candidate-bound Operations exercise is independently approved.",
	}, "missing-operations-exercise-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_operations_exercise_gate_incomplete")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed release approval without an approved candidate-bound Operations exercise")
	}
	otherOperationsExerciseCandidate, err := fixture.service.Create(
		fixture.ctx, fixture.creatorID,
		newCandidateCreateInput("v0.6.3-stage6.other-operations-exercise", "stage6/other-operations-exercise", []string{"code_change"}),
		"other-operations-exercise-candidate", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	seedApprovedOperationsExerciseGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, otherOperationsExerciseCandidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "An Operations exercise approved for another candidate must not satisfy this release gate.",
	}, "cross-candidate-operations-exercise-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_operations_exercise_gate_incomplete")
	seedApprovedOperationsExerciseGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Attempt approval before the candidate-bound internal cost review is independently approved.",
	}, "missing-internal-cost-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_internal_cost_gate_incomplete")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed release approval without an approved candidate-bound internal cost review")
	}
	otherInternalCostCandidate, err := fixture.service.Create(
		fixture.ctx, fixture.creatorID,
		newCandidateCreateInput("v0.6.3-stage6.other-internal-cost", "stage6/other-internal-cost", []string{"code_change"}),
		"other-internal-cost-candidate", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	seedApprovedInternalCostGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, otherInternalCostCandidate, time.Now())
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "An internal cost review approved for another candidate must not satisfy this release gate.",
	}, "cross-candidate-internal-cost-gate", "127.0.0.1")
	assertProblemCode(t, err, "release_internal_cost_gate_incomplete")
	seedApprovedInternalCostGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())

	candidate = fixture.transition(t, fixture.creatorID, candidate, "approved", nil)
	candidate = fixture.transition(t, fixture.creatorID, candidate, "deploying", nil)
	candidate = fixture.transition(t, fixture.creatorID, candidate, "observing", nil)
	summary := "All required evidence and the completed observation window were reviewed for this exact candidate."
	disposition := "none"
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "released",
		Reason:          "Attempt release before binding the exact-candidate Final Review receipt.",
		DecisionSummary: &summary, ResidualRiskDisposition: &disposition, ResidualRisks: []ResidualRisk{},
	}, "missing-final-review", "127.0.0.1")
	assertProblemCode(t, err, "release_final_review_unbound")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "released", "version": candidate.Version + 1, "released_at": time.Now().UTC(),
		"decision_summary": summary, "residual_risk_disposition": disposition, "residual_risks": "[]", "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed released state without an exact-candidate Final Review receipt")
	}
	finalReviewInput := newFinalReviewInput(t, candidate, summary, disposition, []ResidualRisk{})
	nullRisksInput := mutateFinalReviewInput(t, finalReviewInput, func(receipt map[string]any) {
		receipt["residualRisks"] = nil
	})
	_, err = fixture.service.RecordFinalReview(
		fixture.ctx, fixture.creatorID, candidate.ID, nullRisksInput, "null-final-review-risks", "127.0.0.1",
	)
	assertProblemCode(t, err, "release_final_review_invalid")
	digestMismatch := mutatedFinalReviewModel(
		t, fixture.db, candidate.ID, fixture.creatorID, finalReviewInput, func(*finalReviewReceipt) {},
	)
	digestMismatch.ReceiptSHA256[0] ^= 0xff
	if err := fixture.db.Create(&digestMismatch).Error; err == nil {
		t.Fatal("SQLite accepted Final Review bytes whose stored SHA-256 projection did not match")
	}
	for _, forged := range []struct {
		name   string
		mutate func(*finalReviewReceipt)
	}{
		{name: "duplicated control", mutate: func(receipt *finalReviewReceipt) {
			receipt.ControlDecisions[1] = receipt.ControlDecisions[0]
		}},
		{name: "wrong control owner", mutate: func(receipt *finalReviewReceipt) {
			receipt.ControlDecisions[0].OwnerRole = "wrong-owner"
		}},
		{name: "reused final approver", mutate: func(receipt *finalReviewReceipt) {
			receipt.FinalApprovals[1].ApproverID = receipt.FinalApprovals[0].ApproverID
		}},
		{name: "null residual-risk array", mutate: func(receipt *finalReviewReceipt) {
			receipt.ResidualRisks = nil
		}},
	} {
		model := mutatedFinalReviewModel(
			t, fixture.db, candidate.ID, fixture.creatorID, finalReviewInput, forged.mutate,
		)
		if err := fixture.db.Create(&model).Error; err == nil {
			t.Fatalf("SQLite accepted a Final Review with %s through direct insert", forged.name)
		}
	}
	candidate, err = fixture.service.RecordFinalReview(
		fixture.ctx, fixture.creatorID, candidate.ID, finalReviewInput, "bind-final-review", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.FinalReview == nil || !candidate.FinalReview.EligibleForExternalGAAuthorityReview ||
		candidate.FinalReview.ControlCount != len(finalReviewControlOwners) ||
		candidate.FinalReview.DecisionSummary != summary ||
		candidate.FinalReview.ResidualRiskDisposition != disposition ||
		candidate.FinalReview.ResidualRisks == nil || len(candidate.FinalReview.ResidualRisks) != 0 {
		t.Fatalf("bound Final Review = %#v", candidate.FinalReview)
	}
	finalReadiness, err := fixture.service.GetReadiness(fixture.ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !finalReadiness.FinalReviewGate.Satisfied || !finalReadiness.ReleaseTransitionEligible {
		t.Fatalf("bound Final Review readiness = %#v", finalReadiness)
	}
	driftedSummary := summary + " This text was not present in the immutable review."
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "released",
		Reason:          "Attempt release with a decision summary that drifted after Final Review.",
		DecisionSummary: &driftedSummary, ResidualRiskDisposition: &disposition, ResidualRisks: []ResidualRisk{},
	}, "drifted-final-review", "127.0.0.1")
	assertProblemCode(t, err, "release_final_review_decision_mismatch")
	candidate = fixture.transition(t, fixture.creatorID, candidate, "released", &TransitionInput{
		DecisionSummary: &summary, ResidualRiskDisposition: &disposition, ResidualRisks: []ResidualRisk{},
	})
	if candidate.State != "released" || candidate.ReleasedAt == nil || candidate.DecisionSummary == nil ||
		candidate.ResidualRiskDisposition == nil || *candidate.ResidualRiskDisposition != "none" {
		t.Fatalf("released candidate = %#v", candidate)
	}

	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND resource_id = ? AND action LIKE ?", fixture.operatorTenantID, candidate.ID, "release.%").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 11 {
		t.Fatalf("release Audit event count = %d, want 11", auditCount)
	}

	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).
		Where("id = ?", candidate.ID).Update("source_commit", "b"+candidate.SourceCommit[1:]).Error; err == nil {
		t.Fatal("release candidate identity was mutable")
	}
	if err := fixture.db.Where("candidate_record_id = ?", candidate.ID).
		Delete(&persistence.Stage6ReleaseApproval{}).Error; err == nil {
		t.Fatal("release approval history was deletable")
	}
	if err := fixture.db.Model(&persistence.Stage6ReleaseApproval{}).
		Where("candidate_record_id = ?", candidate.ID).Update("evidence_sha256", testReleaseApprovalDigest("mutated")).Error; err == nil {
		t.Fatal("release approval evidence digest was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6ReleaseFinalReview{}).
		Where("candidate_record_id = ?", candidate.ID).Update("decision_summary", "mutated").Error; err == nil {
		t.Fatal("Final Review record was mutable")
	}
	if err := fixture.db.Where("candidate_record_id = ?", candidate.ID).
		Delete(&persistence.Stage6ReleaseFinalReview{}).Error; err == nil {
		t.Fatal("Final Review record was deletable")
	}
}

func TestReleaseGovernanceReadinessUsesTheApprovalTransitionPredicates(t *testing.T) {
	fixture := newReleaseGovernanceFixture(t)
	candidate := fixture.createCandidate(t)
	candidate = fixture.transition(t, fixture.creatorID, candidate, "ready_for_review", nil)

	readiness, err := fixture.service.GetReadiness(fixture.ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(readiness.Gates) != 10 || readiness.InternalGatesSatisfied || readiness.ApprovalTransitionEligible ||
		readiness.FinalReviewGate.Satisfied || readiness.ReleaseTransitionEligible ||
		readiness.ExternalGAStatus != externalGAStatus {
		t.Fatalf("initial readiness = %#v", readiness)
	}
	if !readiness.Gates[0].Satisfied || readiness.Gates[0].ID != "candidate_evidence" ||
		!readiness.Gates[1].Satisfied || readiness.Gates[1].ID != "provider_commercial_authorizations" ||
		readiness.Gates[2].Satisfied || readiness.Gates[2].ID != "release_approvals" {
		t.Fatalf("initial gate projection = %#v", readiness.Gates)
	}

	for index, role := range requiredApprovalRoles {
		candidate, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], candidate.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "Reviewed exact candidate evidence for the readiness projection.",
			EvidenceReference: "https://evidence.example.test/readiness/" + role, EvidenceSHA256: testReleaseApprovalDigest("readiness-" + role),
		}, "readiness-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	seedApprovedSLOGate(t, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, now)
	seedApprovedRecoveryGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, now)
	seedApprovedPenetrationGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, now)
	seedApprovedCapacityGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, now)
	seedApprovedIncidentExerciseGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, now)
	seedApprovedOperationsExerciseGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, now)
	seedApprovedInternalCostGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, now)

	readiness, err = fixture.service.GetReadiness(fixture.ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !readiness.InternalGatesSatisfied || !readiness.ApprovalTransitionEligible {
		t.Fatalf("complete readiness = %#v", readiness)
	}
	for _, gate := range readiness.Gates {
		if !gate.Satisfied || !gate.ExternalVerificationRequired || gate.ExternalBoundary == "" {
			t.Fatalf("complete gate = %#v", gate)
		}
	}

	candidate = fixture.transition(t, fixture.creatorID, candidate, "approved", nil)
	readiness, err = fixture.service.GetReadiness(fixture.ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !readiness.InternalGatesSatisfied || readiness.ApprovalTransitionEligible || readiness.CandidateState != "approved" {
		t.Fatalf("post-transition readiness = %#v", readiness)
	}
	if readiness.FinalReviewGate.ID != "final_review" || readiness.FinalReviewGate.Satisfied ||
		readiness.ReleaseTransitionEligible {
		t.Fatalf("post-transition Final Review readiness = %#v", readiness)
	}
}

func TestReleaseGovernanceRejectsIncompleteApprovalVersionReuseAndRiskDrift(t *testing.T) {
	fixture := newReleaseGovernanceFixture(t)
	candidate := fixture.createCandidate(t)
	candidate = fixture.transition(t, fixture.creatorID, candidate, "ready_for_review", nil)

	_, err := fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved", Reason: "Attempt approval before required reviews exist.",
	}, "incomplete-approval", "127.0.0.1")
	assertProblemCode(t, err, "release_approvals_incomplete")

	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version - 1, TargetState: "approved", Reason: "Attempt transition with a stale version value.",
	}, "stale-version", "127.0.0.1")
	assertProblemCode(t, err, "release_candidate_version_conflict")

	summary := "This decision declares no risk but supplies a contradictory accepted-risk inventory."
	disposition := "none"
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "released", Reason: "Attempt invalid direct release and risk mismatch.",
		DecisionSummary: &summary, ResidualRiskDisposition: &disposition,
		ResidualRisks: []ResidualRisk{{
			ID: "risk-one", Summary: "A bounded remaining risk.", Owner: "Security owner",
			DueAt: time.Now().UTC().Add(24 * time.Hour), AcceptanceReason: "Accepted for a bounded follow-up period.",
			EvidenceReference: "https://evidence.example.test/risk-one",
		}},
	}, "risk-mismatch", "127.0.0.1")
	assertProblemCode(t, err, "release_risk_disposition_mismatch")
}

func TestReleaseGovernanceImpactScopeRequiresPrivacyLegalApproval(t *testing.T) {
	fixture := newReleaseGovernanceFixture(t)
	directUnboundInput := newCandidateCreateInput(
		"v0.6.3-stage6.commercial-direct-unbound", "stage6/commercial-direct-unbound",
		[]string{"code_change", "provider_commercial"},
	)
	directUnbound, err := fixture.service.normalizeCreate(fixture.creatorID, directUnboundInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&directUnbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", directUnbound.ID).Updates(map[string]any{
		"state": "ready_for_review", "version": 2, "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed a Provider-commercial candidate without an exact authorization binding to enter review")
	}
	commercialInput := newCandidateCreateInput("v0.6.3-stage6.commercial", "stage6/commercial", []string{"code_change", "provider_commercial"})
	_, err = fixture.service.Create(
		fixture.ctx, fixture.creatorID, commercialInput, "commercial-create-unbound", "127.0.0.1",
	)
	assertProblemCode(t, err, "release_provider_authorizations_invalid")
	commercialInput.ProviderCommercialAuthorizationIDs = []uuid.UUID{fixture.providerAuthorizationID}
	candidate, err := fixture.service.Create(
		fixture.ctx, fixture.creatorID,
		commercialInput,
		"commercial-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.PrivacyLegalRequired || !slices.Equal(candidate.RequiredApprovalRoles, append(slices.Clone(requiredApprovalRoles), "privacy_legal")) ||
		len(candidate.ProviderCommercialAuthorizations) != 1 || !candidate.ProviderCommercialAuthorizations[0].BindingValid {
		t.Fatalf("impact-derived approval policy = %#v", candidate)
	}
	if err := fixture.db.Model(&persistence.Stage6ReleaseProviderAuthorizationBinding{}).
		Where("candidate_record_id = ?", candidate.ID).Update("authorization_version", 99).Error; err == nil {
		t.Fatal("SQLite allowed a release Provider authorization binding to be mutated")
	}
	if err := fixture.db.Where("candidate_record_id = ?", candidate.ID).
		Delete(&persistence.Stage6ReleaseProviderAuthorizationBinding{}).Error; err == nil {
		t.Fatal("SQLite allowed a release Provider authorization binding to be deleted")
	}
	candidate = fixture.transition(t, fixture.creatorID, candidate, "ready_for_review", nil)
	for index, role := range requiredApprovalRoles {
		candidate, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], candidate.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "Reviewed exact candidate evidence for the assigned release function.",
			EvidenceReference: "https://evidence.example.test/impact/" + role, EvidenceSHA256: testReleaseApprovalDigest("impact-" + role),
		}, "impact-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved", Reason: "Attempt without required Privacy and Legal review.",
	}, "missing-privacy-legal", "127.0.0.1")
	assertProblemCode(t, err, "release_approvals_incomplete")
	candidate, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[4], candidate.ID, ApprovalInput{
		Role: "privacy_legal", Decision: "approved", Reason: "Privacy and Legal reviewed the exact commercial impact scope.",
		EvidenceReference: "https://evidence.example.test/impact/privacy-legal", EvidenceSHA256: testReleaseApprovalDigest("impact-privacy-legal"),
	}, "impact-approval-privacy-legal", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	seedApprovedSLOGate(t, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	seedApprovedRecoveryGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	seedApprovedPenetrationGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	seedApprovedCapacityGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	seedApprovedIncidentExerciseGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	seedApprovedOperationsExerciseGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	seedApprovedInternalCostGate(t, fixture.ctx, fixture.db, fixture.operatorTenantID, fixture.creatorID, fixture.approverIDs, candidate, time.Now())
	candidate = fixture.transition(t, fixture.creatorID, candidate, "approved", nil)
	providerService := providercommercial.NewService(fixture.db, fixture.operatorTenantID)
	if _, err := providerService.Transition(fixture.ctx, fixture.creatorID, fixture.providerAuthorizationID, providercommercial.TransitionInput{
		ExpectedVersion: 3, TargetState: "revoked",
		Reason: "Revoke the exact Provider authorization before deployment to test the release fence.",
	}, "commercial-authorization-revoked", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "deploying",
		Reason: "A revoked Provider commercial authorization must block candidate deployment.",
	}, "commercial-provider-revoked", "127.0.0.1")
	assertProblemCode(t, err, "release_provider_authorization_gate_incomplete")

	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).
		Update("impact_domains", `["code_change"]`).Error; err == nil {
		t.Fatal("release impact scope was mutable")
	}
}

func TestReleaseGovernanceBindsExactEligibleEvidenceReceipt(t *testing.T) {
	fixture := newReleaseGovernanceFixture(t)
	input := newCandidateCreateInput("v0.6.3-stage6.evidence", "stage6/evidence", []string{"code_change"})
	candidate, err := fixture.service.Create(
		fixture.ctx, fixture.creatorID, input, "evidence-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.EvidenceReceiptBound || candidate.EvidenceBundleSchema != candidateEvidenceReceiptSchema ||
		candidate.EvidenceBundleAssessment != candidateEvidenceReceiptAssessment ||
		candidate.DesktopArtifactSetSHA256 != "sha256:"+repeat("e", 64) ||
		candidate.EvidenceBundleReceiptSizeBytes <= 0 {
		t.Fatalf("candidate evidence binding = %#v", candidate)
	}
	raw, err := base64.StdEncoding.DecodeString(input.EvidenceBundleReceiptBase64)
	if err != nil {
		t.Fatal(err)
	}
	var stored persistence.Stage6ReleaseCandidate
	if err := fixture.db.Where("id = ?", candidate.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.EvidenceBundleReceipt, raw) {
		t.Fatal("exact evidence receipt bytes were not persisted")
	}
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).
		Update("evidence_bundle_receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("bound evidence receipt was mutable")
	}
	directInput := newCandidateCreateInput("v0.6.3-stage6.sqlite-empty-projection", "stage6/sqlite-empty-projection", []string{"code_change"})
	directModel, err := fixture.service.normalizeCreate(fixture.creatorID, directInput)
	if err != nil {
		t.Fatal(err)
	}
	var directReceipt map[string]any
	if err := json.Unmarshal(directModel.EvidenceBundleReceipt, &directReceipt); err != nil {
		t.Fatal(err)
	}
	directReceipt["receipts"].(map[string]any)["internalCost"] = map[string]any{}
	directModel.EvidenceBundleReceipt, err = json.Marshal(directReceipt)
	if err != nil {
		t.Fatal(err)
	}
	directDigest := sha256.Sum256(directModel.EvidenceBundleReceipt)
	directModel.EvidenceBundleSHA256 = directDigest[:]
	directModel.EvidenceBundleReceiptSizeBytes = int64(len(directModel.EvidenceBundleReceipt))
	if err := fixture.db.Create(&directModel).Error; err == nil {
		t.Fatal("SQLite accepted an empty projected control receipt through a direct insert")
	}
	directWorkerInput := newCandidateCreateInput("v0.6.3-stage6.sqlite-worker-projection", "stage6/sqlite-worker-projection", []string{"code_change"})
	directWorkerModel, err := fixture.service.normalizeCreate(fixture.creatorID, directWorkerInput)
	if err != nil {
		t.Fatal(err)
	}
	var directWorkerReceipt map[string]any
	if err := json.Unmarshal(directWorkerModel.EvidenceBundleReceipt, &directWorkerReceipt); err != nil {
		t.Fatal(err)
	}
	directWorkerReceipt["receipts"].(map[string]any)["workerSupplyChain"].(map[string]any)["registryReportSha256"] = "sha256:" + repeat("F", 64)
	directWorkerModel.EvidenceBundleReceipt, err = json.Marshal(directWorkerReceipt)
	if err != nil {
		t.Fatal(err)
	}
	directWorkerDigest := sha256.Sum256(directWorkerModel.EvidenceBundleReceipt)
	directWorkerModel.EvidenceBundleSHA256 = directWorkerDigest[:]
	directWorkerModel.EvidenceBundleReceiptSizeBytes = int64(len(directWorkerModel.EvidenceBundleReceipt))
	if err := fixture.db.Create(&directWorkerModel).Error; err == nil {
		t.Fatal("SQLite accepted an invalid Worker supply-chain projection through a direct insert")
	}

	digestMismatch := newCandidateCreateInput("v0.6.3-stage6.digest-mismatch", "stage6/digest-mismatch", []string{"code_change"})
	digestMismatch.EvidenceBundleSHA256 = "sha256:" + repeat("f", 64)
	_, err = fixture.service.Create(fixture.ctx, fixture.creatorID, digestMismatch, "digest-mismatch", "127.0.0.1")
	assertProblemCode(t, err, "release_candidate_evidence_invalid")

	ineligible := newCandidateCreateInput("v0.6.3-stage6.ineligible", "stage6/ineligible", []string{"code_change"})
	ineligible = mutateCandidateEvidenceInput(t, ineligible, func(receipt map[string]any) {
		receipt["eligibleForCandidateEvidenceReview"] = false
	})
	_, err = fixture.service.Create(fixture.ctx, fixture.creatorID, ineligible, "ineligible", "127.0.0.1")
	assertProblemCode(t, err, "release_candidate_evidence_invalid")

	wrongEnvironment := newCandidateCreateInput("v0.6.3-stage6.wrong-env", "stage6/wrong-env", []string{"code_change"})
	wrongEnvironment = mutateCandidateEvidenceInput(t, wrongEnvironment, func(receipt map[string]any) {
		receipt["candidate"].(map[string]any)["environmentId"] = "stage6/other"
	})
	_, err = fixture.service.Create(fixture.ctx, fixture.creatorID, wrongEnvironment, "wrong-environment", "127.0.0.1")
	assertProblemCode(t, err, "release_candidate_evidence_invalid")

	invalidProjections := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "empty control receipt", mutate: func(receipt map[string]any) {
			receipt["receipts"].(map[string]any)["internalCost"] = map[string]any{}
		}},
		{name: "control schema drift", mutate: func(receipt map[string]any) {
			receipt["receipts"].(map[string]any)["capacity"].(map[string]any)["schemaVersion"] = "synara.capacity-soak-evidence-receipt.v2"
		}},
		{name: "duplicate evidence path", mutate: func(receipt map[string]any) {
			receipts := receipt["receipts"].(map[string]any)
			receipts["incident"].(map[string]any)["path"] = receipts["operations"].(map[string]any)["path"]
		}},
		{name: "desktop artifact set drift", mutate: func(receipt map[string]any) {
			receipt["receipts"].(map[string]any)["desktop"].(map[string]any)["desktopArtifactSetSha256"] = "sha256:" + repeat("f", 64)
		}},
		{name: "recovery authority boundary weakened", mutate: func(receipt map[string]any) {
			receipt["receipts"].(map[string]any)["recovery"].(map[string]any)["realBackupRestoreAndApproverAuthorityVerificationRequired"] = false
		}},
		{name: "candidate artifact projection missing", mutate: func(receipt map[string]any) {
			delete(receipt["candidate"].(map[string]any)["artifacts"].(map[string]any), "workerImage")
		}},
		{name: "legacy v2 bundle", mutate: func(receipt map[string]any) {
			receipt["schemaVersion"] = "synara.stage6-candidate-evidence-bundle-validation.v2"
		}},
		{name: "worker supply chain receipt missing", mutate: func(receipt map[string]any) {
			delete(receipt["receipts"].(map[string]any), "workerSupplyChain")
		}},
		{name: "worker supply chain registry evidence invalid", mutate: func(receipt map[string]any) {
			receipt["receipts"].(map[string]any)["workerSupplyChain"].(map[string]any)["registryReportSha256"] = "sha256:" + repeat("0", 64)
		}},
		{name: "compatibility matrix migration drift", mutate: func(receipt map[string]any) {
			receipt["compatibilityMatrix"].(map[string]any)["migrationTail"].(map[string]any)["sha256"] = "sha256:" + repeat("0", 64)
		}},
	}
	for _, test := range invalidProjections {
		t.Run(test.name, func(t *testing.T) {
			input := newCandidateCreateInput("v0.6.3-stage6.invalid-projection", "stage6/invalid-projection", []string{"code_change"})
			input = mutateCandidateEvidenceInput(t, input, test.mutate)
			_, err := fixture.service.Create(fixture.ctx, fixture.creatorID, input, "invalid-projection", "127.0.0.1")
			assertProblemCode(t, err, "release_candidate_evidence_invalid")
		})
	}
}

func TestReleaseGovernanceRejectionIsAtomicAndRoleDecisionsAreUnique(t *testing.T) {
	fixture := newReleaseGovernanceFixture(t)
	candidate := fixture.createCandidate(t)
	candidate = fixture.transition(t, fixture.creatorID, candidate, "ready_for_review", nil)

	approved, err := fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], candidate.ID, ApprovalInput{
		Role: "engineering", Decision: "approved", Reason: "Engineering evidence is complete for this candidate.",
		EvidenceReference: "https://evidence.example.test/engineering", EvidenceSHA256: testReleaseApprovalDigest("unique-engineering"),
	}, "engineering-approval", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[1], candidate.ID, ApprovalInput{
		Role: "engineering", Decision: "approved", Reason: "Duplicate role approval must not replace history.",
		EvidenceReference: "https://evidence.example.test/engineering-second", EvidenceSHA256: testReleaseApprovalDigest("duplicate-engineering"),
	}, "duplicate-role", "127.0.0.1")
	assertProblemCode(t, err, "release_approval_conflict")

	rejected, err := fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[1], approved.ID, ApprovalInput{
		Role: "security", Decision: "rejected", Reason: "Security evidence contains an unresolved release blocker.",
		EvidenceReference: "https://evidence.example.test/security-rejection", EvidenceSHA256: testReleaseApprovalDigest("security-rejection"),
	}, "security-rejection", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.State != "rejected" || rejected.RejectedAt == nil || rejected.Version != 3 {
		t.Fatalf("rejected candidate = %#v", rejected)
	}
	var approvalCount int64
	if err := fixture.db.Model(&persistence.Stage6ReleaseApproval{}).Where("candidate_record_id = ?", candidate.ID).Count(&approvalCount).Error; err != nil {
		t.Fatal(err)
	}
	if approvalCount != 2 {
		t.Fatalf("approval history count = %d", approvalCount)
	}
}

func TestReleaseGovernanceLegacyURLOnlyApprovalCannotAuthorizeCandidate(t *testing.T) {
	fixture := newReleaseGovernanceFixture(t)
	candidate := fixture.createCandidate(t)
	candidate = fixture.transition(t, fixture.creatorID, candidate, "ready_for_review", nil)
	if err := fixture.db.Exec("DROP TRIGGER IF EXISTS trg_stage6_release_approvals_insert").Error; err != nil {
		t.Fatal(err)
	}
	legacy := persistence.Stage6ReleaseApproval{
		ID: uuid.New(), CandidateRecordID: candidate.ID, OperatorTenantID: fixture.operatorTenantID,
		Role: "engineering", Decision: "approved", ApproverUserID: fixture.approverIDs[0],
		Reason:            "Legacy decision referenced mutable evidence without a byte digest.",
		EvidenceReference: "https://evidence.example.test/legacy-release/engineering", CreatedAt: time.Now().UTC(),
	}
	if err := fixture.db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	for index, role := range []string{"operations", "security", "product"} {
		var err error
		candidate, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index+1], candidate.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "Approved exact evidence while retaining one legacy URL-only decision.",
			EvidenceReference: "https://evidence.example.test/legacy-release/" + role,
			EvidenceSHA256:    testReleaseApprovalDigest("legacy-" + role),
		}, "legacy-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	readiness, err := fixture.service.GetReadiness(fixture.ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Gates[2].ID != "release_approvals" || readiness.Gates[2].Satisfied {
		t.Fatalf("legacy URL-only Release approval readiness = %#v", readiness.Gates[2])
	}
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "A legacy URL-only Release decision must not authorize the candidate.",
	}, "legacy-url-only-release-approval", "127.0.0.1")
	assertProblemCode(t, err, "release_approvals_incomplete")
	if err := fixture.db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite allowed a legacy URL-only Release decision to authorize the candidate")
	}
}

type releaseGovernanceFixture struct {
	ctx                     context.Context
	db                      *gorm.DB
	service                 *Service
	operatorTenantID        uuid.UUID
	creatorID               uuid.UUID
	approverIDs             []uuid.UUID
	providerAuthorizationID uuid.UUID
}

func newReleaseGovernanceFixture(t *testing.T) releaseGovernanceFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "release-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, 5)
	for index := range 5 {
		user := persistence.User{
			ID: uuid.New(), Email: "release-approver-" + string(rune('a'+index)) + "@example.test",
			DisplayName: "Release Approver " + string(rune('A'+index)), Status: "active",
			EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		membership := persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := store.DB().Create(&membership).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	for index, role := range requiredApprovalRoles {
		seedReleaseAuthority(t, store.DB(), domain.TenantID, domain.UserID, approvers[index], "release."+role, now)
	}
	for index, role := range []string{"legal", "privacy", "security", "product"} {
		seedReleaseAuthority(t, store.DB(), domain.TenantID, domain.UserID, approvers[index], governanceauthority.ProviderCommercialPrefix+role, now)
	}
	seedReleaseAuthority(t, store.DB(), domain.TenantID, domain.UserID, approvers[4], governanceauthority.ReleasePrivacyLegal, now)
	seedReleaseAuthority(t, store.DB(), domain.TenantID, domain.UserID, approvers[1], governanceauthority.ReleaseEngineering, now)
	seedReleaseAuthority(t, store.DB(), domain.TenantID, domain.UserID, approvers[1], governanceauthority.ReleaseSecurity, now)
	providerAuthorizationID := seedActiveProviderCommercialAuthorization(
		t, ctx, store.DB(), domain.TenantID, domain.UserID, approvers[:4], now,
	)
	return releaseGovernanceFixture{
		ctx: ctx, db: store.DB(), service: NewService(store.DB(), domain.TenantID),
		operatorTenantID: domain.TenantID, creatorID: domain.UserID, approverIDs: approvers,
		providerAuthorizationID: providerAuthorizationID,
	}
}

func seedActiveProviderCommercialAuthorization(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	operatorTenantID, creatorID uuid.UUID,
	approverIDs []uuid.UUID,
	now time.Time,
) uuid.UUID {
	t.Helper()
	service := providercommercial.NewService(db, operatorTenantID)
	digest := func(value string) string { return "sha256:" + strings.Repeat(value, 64) }
	authorization, err := service.Create(ctx, creatorID, providercommercial.CreateInput{
		AuthorizationKey: "openai-api-release-2026", Provider: "codex", ProviderProduct: "OpenAI API",
		AccountType: "Enterprise API organization", ContractingEntity: "OpenAI contracting entity",
		CredentialMode: "customer_byok", AllowedCredentialScopes: []string{"tenant"}, AllowedRegions: []string{"region-one"},
		DataUsePolicy: "no_training", RetentionPolicy: "Approved API retention policy applies.",
		TermsEffectiveAt: now.Add(-24 * time.Hour), TermsReference: "https://evidence.example.test/provider-release/terms",
		TermsSHA256: digest("a"), AgreementReference: "https://evidence.example.test/provider-release/agreement",
		AgreementSHA256: digest("b"), DPAReference: "https://evidence.example.test/provider-release/dpa",
		DPASHA256: digest("c"), ProhibitedUseSummary: "Consumer login sharing and unapproved training are prohibited.",
		TerminationRunbookReference: "https://evidence.example.test/provider-release/termination",
		TerminationRunbookSHA256:    digest("d"), ReviewExpiresAt: now.Add(90 * 24 * time.Hour),
	}, "seed-provider-authorization", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	authorization, err = service.Transition(ctx, creatorID, authorization.ID, providercommercial.TransitionInput{
		ExpectedVersion: authorization.Version, TargetState: "ready_for_review",
		Reason: "Submit the exact Provider authorization for release binding review.",
	}, "seed-provider-ready", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for index, role := range []string{"legal", "privacy", "security", "product"} {
		authorization, err = service.RecordApproval(ctx, approverIDs[index], authorization.ID, providercommercial.ApprovalInput{
			Role: role, Decision: "approved", Reason: "Approved the exact Provider release authorization evidence.",
			EvidenceReference: "https://evidence.example.test/provider-release/approval/" + role,
			EvidenceSHA256:    digest(string("1234"[index])),
		}, "seed-provider-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	authorization, err = service.Transition(ctx, creatorID, authorization.ID, providercommercial.TransitionInput{
		ExpectedVersion: authorization.Version, TargetState: "active",
		Reason: "Activate the exact byte-bound Provider authorization for release candidate binding.",
	}, "seed-provider-active", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return authorization.ID
}

func seedReleaseAuthority(t *testing.T, db *gorm.DB, tenantID, ownerID, userID uuid.UUID, key string, now time.Time) {
	t.Helper()
	grant := persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: tenantID, UserID: userID, AuthorityKey: key,
		Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: ownerID,
		Reason:            "Owner assigned the exact release governance approval function.",
		EvidenceReference: "https://evidence.example.test/governance-authority/" + strings.ReplaceAll(key, ".", "/"),
		EvidenceSHA256:    stage6authority.DigestPointer("release-authority-" + key),
		CreatedAt:         now, UpdatedAt: now,
	}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
}

func (f releaseGovernanceFixture) createCandidate(t *testing.T) Candidate {
	t.Helper()
	candidate, err := f.service.Create(
		f.ctx, f.creatorID,
		newCandidateCreateInput("v0.6.3-stage6.1", "stage6/production-like", []string{"code_change"}),
		"candidate-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func (f releaseGovernanceFixture) transition(
	t *testing.T,
	actorID uuid.UUID,
	candidate Candidate,
	target string,
	override *TransitionInput,
) Candidate {
	t.Helper()
	input := TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: target,
		Reason: "Advance the exact candidate after completing the required bounded gate.",
	}
	if override != nil {
		input.DecisionSummary = override.DecisionSummary
		input.ResidualRiskDisposition = override.ResidualRiskDisposition
		input.ResidualRisks = override.ResidualRisks
	}
	result, err := f.service.Transition(f.ctx, actorID, candidate.ID, input, "transition-"+target, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}

func testReleaseApprovalDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}

func newCandidateCreateInput(candidateID, environmentID string, impactDomains []string) CreateInput {
	_, capacityDigest, _ := stage6capacity.Receipt(environmentID)
	_, incidentDigest, _ := stage6incident.Receipt(environmentID)
	_, operationsDigest, _ := stage6operations.Receipt(candidateID, environmentID)
	_, penetrationDigest, _ := stage6penetration.Receipt(environmentID)
	_, internalCostDigest, _ := stage6internalcost.Receipt(
		candidateID, environmentID,
		"000153_stage6_internal_self_hosted_release_gate.sql",
		"sha256:"+repeat("f", 64),
	)
	receipts := make(map[string]any, len(candidateEvidenceReceiptNames))
	for _, name := range candidateEvidenceReceiptNames {
		spec := candidateEvidenceReceiptSpecs[name]
		receipts[name] = map[string]any{
			"path": name + "-receipt.json", "sha256": testReceiptSHA(name),
			"schemaVersion": spec[0], "assessment": spec[1],
			"readyForCandidateReview": true, "validatedAt": "2026-07-31T11:00:00Z",
		}
	}
	receipts["penetration"].(map[string]any)["sha256"] = penetrationDigest
	receipts["capacity"].(map[string]any)["sha256"] = capacityDigest
	receipts["incident"].(map[string]any)["sha256"] = incidentDigest
	receipts["operations"].(map[string]any)["sha256"] = operationsDigest
	receipts["internalCost"].(map[string]any)["sha256"] = internalCostDigest
	receipts["desktop"].(map[string]any)["desktopArtifactSetSha256"] = "sha256:" + repeat("e", 64)
	receipts["recovery"].(map[string]any)["candidateBindingSha256"] = "sha256:" + repeat("a", 64)
	receipts["recovery"].(map[string]any)["recoverySubjectSha256"] = "sha256:" + repeat("c", 64)
	receipts["recovery"].(map[string]any)["cryptographicSignaturesVerified"] = false
	receipts["recovery"].(map[string]any)["realBackupRestoreAndApproverAuthorityVerificationRequired"] = true
	receipts["residency"].(map[string]any)["candidateBindingSha256"] = "sha256:" + repeat("a", 64)
	receipts["residency"].(map[string]any)["evidenceSetSha256"] = "sha256:" + repeat("c", 64)
	receipts["residency"].(map[string]any)["policyDigest"] = "sha256:" + repeat("d", 64)
	receipts["residency"].(map[string]any)["policyVersion"] = 1
	receipts["residency"].(map[string]any)["homeRegion"] = "region-one"
	receipts["residency"].(map[string]any)["allowedRegions"] = []string{"region-one", "region-two"}
	receipts["residency"].(map[string]any)["cryptographicSignaturesVerified"] = false
	receipts["residency"].(map[string]any)["externalSignatureIdentityAndAuthorityVerificationRequired"] = true
	receipts["workerSupplyChain"].(map[string]any)["registryReportSha256"] = "sha256:" + repeat("1", 64)
	receipts["workerSupplyChain"].(map[string]any)["admissionReportSha256"] = "sha256:" + repeat("2", 64)
	receipt := map[string]any{
		"schemaVersion": candidateEvidenceReceiptSchema,
		"assessment":    candidateEvidenceReceiptAssessment,
		"candidate": map[string]any{
			"candidateId": candidateID, "sourceCommit": repeat("a", 40),
			"lockfileSha256":           repeat("b", 64),
			"desktopArtifactSetSha256": "sha256:" + repeat("e", 64),
			"environmentClass":         "production-like", "environmentId": environmentID,
			"artifacts": map[string]any{
				"controlPlaneImage": "sha256:" + repeat("1", 64), "workerImage": "sha256:" + repeat("2", 64),
				"providerHostImage": "sha256:" + repeat("3", 64), "webArtifact": "sha256:" + repeat("4", 64),
				"adminArtifact": "sha256:" + repeat("5", 64),
				"desktopArtifacts": map[string]any{
					"linux-x64": "sha256:" + repeat("6", 64), "macos-arm64": "sha256:" + repeat("7", 64),
					"macos-x64": "sha256:" + repeat("8", 64), "windows-x64": "sha256:" + repeat("9", 64),
				},
			},
			"migrationTail": map[string]any{"name": "000153_stage6_internal_self_hosted_release_gate.sql", "sha256": "sha256:" + repeat("f", 64)},
			"origins": map[string]any{
				"controlPlaneBaseUrl": "https://control.example.test/v1", "webBaseUrl": "https://app.example.test", "adminBaseUrl": "https://admin.example.test",
			},
			"regions": []string{"region-one", "region-two"},
		},
		"candidateConsistencyValidated":              true,
		"environmentEligible":                        true,
		"allRequiredReceiptsReadyForCandidateReview": true,
		"eligibleForCandidateEvidenceReview":         true,
		"requiredReceiptCount":                       len(candidateEvidenceReceiptNames),
		"receipts":                                   receipts,
		"validatedAt":                                "2026-07-31T12:00:00Z",
		"manifest":                                   map[string]any{"path": "bundle.json", "sha256": testReceiptSHA("manifest")},
		"compatibilityMatrix": map[string]any{
			"path": "compatibility-matrix.json", "sha256": testReceiptSHA("compatibility-matrix"),
			"schemaVersion": "synara.release-compatibility-matrix.v1", "matrixVersion": 1,
			"assessment": "source-compatible-not-release-approved", "sourceFileCount": 172,
			"sourceByteCount": 1048576,
			"migrationTail": map[string]any{
				"name": "000153_stage6_internal_self_hosted_release_gate.sql", "sha256": "sha256:" + repeat("f", 64),
			},
		},
		"releaseEvidence": map[string]any{
			"path": "release-evidence.json", "sha256": testReceiptSHA("release-evidence"),
			"schemaVersion": "synara-stage6-release-evidence-v2", "assessment": "evidence-collected-not-control-passed",
			"generatedAt": "2026-07-31T10:00:00Z",
		},
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return CreateInput{
		CandidateID: candidateID, SourceCommit: repeat("a", 40),
		LockfileSHA256:              "sha256:" + repeat("b", 64),
		EvidenceBundleSHA256:        "sha256:" + hex.EncodeToString(digest[:]),
		EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded),
		FinalAssetSetSHA256:         "sha256:" + repeat("d", 64), EnvironmentID: environmentID,
		ImpactDomains: impactDomains,
	}
}

func newFinalReviewInput(
	t *testing.T,
	candidate Candidate,
	decisionSummary, disposition string,
	risks []ResidualRisk,
) FinalReviewInput {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	startedAt := now.Add(-4 * time.Hour)
	protectedApprovedAt := now.Add(-3 * time.Hour)
	controlDecidedAt := now.Add(-2 * time.Hour)
	approvalAt := now.Add(-time.Hour)
	completedAt := now.Add(-30 * time.Minute)
	validatedAt := now.Add(-20 * time.Minute)
	reference := func(name string) finalReviewReference {
		return finalReviewReference{Path: "final-review/" + name + ".json", SHA256: testReceiptSHA(name)}
	}
	controls := make([]finalReviewControlDecision, 0, len(finalReviewControlOwners))
	controlIDs := make([]string, 0, len(finalReviewControlOwners))
	for id := range finalReviewControlOwners {
		controlIDs = append(controlIDs, id)
	}
	slices.Sort(controlIDs)
	for index, id := range controlIDs {
		controls = append(controls, finalReviewControlDecision{
			ID: id, OwnerRole: finalReviewControlOwners[id], ApproverID: "control-approver-" + id,
			Status: "passed", DecidedAt: controlDecidedAt.Format(time.RFC3339),
			Evidence: []finalReviewReference{reference("control-" + string(rune('a'+index)))},
		})
	}
	approvals := make([]finalReviewApproval, 0, len(candidate.RequiredApprovalRoles))
	for index, role := range candidate.RequiredApprovalRoles {
		approvals = append(approvals, finalReviewApproval{
			Role: role, ApproverID: "final-approver-" + role,
			Decision: "approved-for-ga-authority-review", ApprovedAt: approvalAt.Format(time.RFC3339),
			Evidence: reference("approval-" + string(rune('a'+index))),
		})
	}
	canonicalOwners, err := json.Marshal(finalReviewControlOwners)
	if err != nil {
		t.Fatal(err)
	}
	ownerDigest := sha256.Sum256(canonicalOwners)
	document := finalReviewReceipt{
		SchemaVersion: finalReviewReceiptSchema,
		Candidate: finalReviewCandidate{
			CandidateID: candidate.CandidateID, ReleaseTag: candidate.CandidateID,
			SourceCommit: candidate.SourceCommit, EnvironmentID: candidate.EnvironmentID,
			CandidateBundleReceipt:   reference("candidate-bundle"),
			ProtectedReleaseApproval: reference("protected-release-approval"),
			FinalAssetSet:            reference("final-asset-set"), FinalAssetSetSHA256: candidate.FinalAssetSetSHA256,
			LockfileSHA256:           strings.TrimPrefix(candidate.LockfileSHA256, "sha256:"),
			DesktopArtifactSetSHA256: candidate.DesktopArtifactSetSHA256,
			CopiedChecklist:          reference("copied-checklist"), ChangeNotice: reference("change-notice"),
			PlatformAuditExport:        reference("platform-audit-export"),
			CandidateValidatedAt:       candidate.EvidenceBundleValidatedAt,
			ProtectedReleaseApprovedAt: protectedApprovedAt.Format(time.RFC3339),
		},
		ReleaseManagerID: "release-manager-primary", ImpactDomains: slices.Clone(candidate.ImpactDomains),
		ControlInventorySHA256: "sha256:" + hex.EncodeToString(ownerDigest[:]),
		ControlCount:           len(finalReviewControlOwners),
		ControlStatusCounts:    finalReviewStatusCounts{Passed: len(finalReviewControlOwners)},
		ControlDecisions:       controls, FinalApprovals: approvals,
		PlatformAuditRequestIDs: []string{uuid.NewString()}, DecisionSummary: decisionSummary,
		ResidualRiskDisposition: disposition, ResidualRisks: append([]ResidualRisk{}, risks...),
		StartedAt: startedAt.Format(time.RFC3339), CompletedAt: completedAt.Format(time.RFC3339),
		ValidatedAt: validatedAt.Format(time.RFC3339), AllRequiredControlsPassed: true,
		AllRequiredFinalApprovalsApproved: true, EligibleForExternalGAAuthorityReview: true,
		VerificationBoundary: finalReviewBoundary{}, Assessment: finalReviewReceiptAssessment,
	}
	document.Candidate.CandidateBundleReceipt.SHA256 = candidate.EvidenceBundleSHA256
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return FinalReviewInput{
		ReceiptSHA256: "sha256:" + hex.EncodeToString(digest[:]),
		ReceiptBase64: base64.StdEncoding.EncodeToString(encoded),
	}
}

func mutatedFinalReviewModel(
	t *testing.T,
	db *gorm.DB,
	candidateRecordID, actorID uuid.UUID,
	input FinalReviewInput,
	mutate func(*finalReviewReceipt),
) persistence.Stage6ReleaseFinalReview {
	t.Helper()
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.Where("id = ?", candidateRecordID).Take(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	model, err := bindFinalReviewReceipt(input, candidate, actorID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var receipt finalReviewReceipt
	if err := json.Unmarshal(model.Receipt, &receipt); err != nil {
		t.Fatal(err)
	}
	mutate(&receipt)
	model.Receipt, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(model.Receipt)
	model.ReceiptSHA256 = digest[:]
	model.ReceiptSizeBytes = int64(len(model.Receipt))
	return model
}

func mutateFinalReviewInput(
	t *testing.T,
	input FinalReviewInput,
	mutate func(map[string]any),
) FinalReviewInput {
	t.Helper()
	encoded, err := base64.StdEncoding.DecodeString(input.ReceiptBase64)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		t.Fatal(err)
	}
	mutate(receipt)
	encoded, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	input.ReceiptBase64 = base64.StdEncoding.EncodeToString(encoded)
	input.ReceiptSHA256 = "sha256:" + hex.EncodeToString(digest[:])
	return input
}

func testReceiptSHA(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func mutateCandidateEvidenceInput(t *testing.T, input CreateInput, mutate func(map[string]any)) CreateInput {
	t.Helper()
	encoded, err := base64.StdEncoding.DecodeString(input.EvidenceBundleReceiptBase64)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		t.Fatal(err)
	}
	mutate(receipt)
	encoded, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	input.EvidenceBundleReceiptBase64 = base64.StdEncoding.EncodeToString(encoded)
	input.EvidenceBundleSHA256 = "sha256:" + hex.EncodeToString(digest[:])
	return input
}
