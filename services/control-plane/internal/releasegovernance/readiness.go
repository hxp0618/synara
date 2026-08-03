package releasegovernance

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const externalGAStatus = "required_not_verified_by_synara"

type ReadinessGate struct {
	ID                           string `json:"id"`
	Label                        string `json:"label"`
	Satisfied                    bool   `json:"satisfied"`
	InternalAssessment           string `json:"internalAssessment"`
	ExternalVerificationRequired bool   `json:"externalVerificationRequired"`
	ExternalBoundary             string `json:"externalBoundary"`
}

type Readiness struct {
	CandidateRecordID          uuid.UUID       `json:"candidateRecordId"`
	CandidateID                string          `json:"candidateId"`
	CandidateState             string          `json:"candidateState"`
	InternalGatesSatisfied     bool            `json:"internalGatesSatisfied"`
	ApprovalTransitionEligible bool            `json:"approvalTransitionEligible"`
	FinalReviewGate            ReadinessGate   `json:"finalReviewGate"`
	ReleaseTransitionEligible  bool            `json:"releaseTransitionEligible"`
	ExternalGAStatus           string          `json:"externalGaStatus"`
	Gates                      []ReadinessGate `json:"gates"`
}

type approvalGateResult struct {
	readiness ReadinessGate
	code      string
	message   string
}

func (s *Service) GetReadiness(ctx context.Context, candidateRecordID uuid.UUID) (Readiness, error) {
	var readiness Readiness
	txOptions := &sql.TxOptions{ReadOnly: true}
	if s.db.Dialector.Name() == "postgres" {
		txOptions.Isolation = sql.LevelRepeatableRead
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate, err := s.loadCandidate(ctx, tx, candidateRecordID)
		if err != nil {
			return err
		}
		gates, err := s.evaluateApprovalGates(ctx, tx, candidate)
		if err != nil {
			return err
		}
		allSatisfied := true
		items := make([]ReadinessGate, 0, len(gates))
		for _, gate := range gates {
			items = append(items, gate.readiness)
			allSatisfied = allSatisfied && gate.readiness.Satisfied
		}
		finalReviewGate, err := s.evaluateFinalReviewGate(ctx, tx, candidate)
		if err != nil {
			return err
		}
		readiness = Readiness{
			CandidateRecordID: candidate.ID, CandidateID: candidate.CandidateID, CandidateState: candidate.State,
			InternalGatesSatisfied: allSatisfied, ApprovalTransitionEligible: allSatisfied && candidate.State == "ready_for_review",
			FinalReviewGate: finalReviewGate, ReleaseTransitionEligible: allSatisfied && finalReviewGate.Satisfied && candidate.State == "observing",
			ExternalGAStatus: externalGAStatus, Gates: items,
		}
		return nil
	}, txOptions)
	return readiness, err
}

func (s *Service) evaluateFinalReviewGate(
	ctx context.Context,
	db *gorm.DB,
	candidate persistence.Stage6ReleaseCandidate,
) (ReadinessGate, error) {
	var review persistence.Stage6ReleaseFinalReview
	err := db.WithContext(ctx).Where(
		"candidate_record_id = ? AND operator_tenant_id = ?",
		candidate.ID, candidate.OperatorTenantID,
	).Take(&review).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return newApprovalGate(
			"final_review", "Final Review release binding", false,
			"eligible-exact-final-review-bound", "release_final_review_unbound",
			"Released state requires an eligible exact-candidate Final Review receipt.",
			"Receipt eligibility does not authenticate external evidence, signatures, corporate authority or GA approval.",
		).readiness, nil
	}
	if err != nil {
		return ReadinessGate{}, problem.Wrap(500, "release_final_review_load_failed", "Final Review readiness could not be verified.", err)
	}
	satisfied := review.AllRequiredControlsPassed && review.AllRequiredFinalApprovalsApproved &&
		review.EligibleForExternalGAAuthorityReview && !review.ExternalEvidenceAuthorityVerified &&
		!review.ApproverCorporateAuthorityVerified && !review.ExternalSignaturesVerified &&
		!review.PublicationDeliveryVerifiedBySynara
	return newApprovalGate(
		"final_review", "Final Review release binding", satisfied,
		"eligible-exact-final-review-bound", "release_final_review_invalid",
		"Released state requires an eligible exact-candidate Final Review receipt.",
		"Receipt eligibility does not authenticate external evidence, signatures, corporate authority or GA approval.",
	).readiness, nil
}

func (s *Service) loadCandidate(
	ctx context.Context,
	db *gorm.DB,
	candidateRecordID uuid.UUID,
) (persistence.Stage6ReleaseCandidate, error) {
	var candidate persistence.Stage6ReleaseCandidate
	err := db.WithContext(ctx).
		Where("id = ? AND operator_tenant_id = ?", candidateRecordID, s.operatorTenantID).
		Take(&candidate).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return candidate, problem.New(404, "release_candidate_not_found", "Release candidate not found.")
	}
	if err != nil {
		return candidate, problem.Wrap(500, "release_candidate_load_failed", "Release candidate could not be loaded.", err)
	}
	return candidate, nil
}

func (s *Service) requireApprovalGates(ctx context.Context, db *gorm.DB, candidate persistence.Stage6ReleaseCandidate) error {
	gates, err := s.evaluateApprovalGates(ctx, db, candidate)
	if err != nil {
		return err
	}
	for _, gate := range gates {
		if !gate.readiness.Satisfied {
			return problem.New(409, gate.code, gate.message)
		}
	}
	return nil
}

func (s *Service) evaluateApprovalGates(
	ctx context.Context,
	db *gorm.DB,
	candidate persistence.Stage6ReleaseCandidate,
) ([]approvalGateResult, error) {
	requiredRoles := requiredApprovalRolesFor(candidate)
	gates := []approvalGateResult{
		newApprovalGate(
			"candidate_evidence", "Candidate evidence binding", candidate.EvidenceReceiptBound,
			"exact-candidate-evidence-bound", "release_candidate_evidence_unbound",
			"Release candidate review requires an exact eligible evidence receipt.",
			"Receipt consistency does not authenticate any underlying control, deployment, signer or reviewer.",
		),
	}
	providerAuthorizationBindingsValid, err := s.evaluateProviderCommercialAuthorizationBindings(ctx, db, candidate, s.now())
	if err != nil {
		return nil, err
	}
	providerCommercialRequired := containsImpactDomain(candidate.ImpactDomains, "provider_commercial")
	providerAssessment := "not-applicable-no-provider-commercial-impact"
	if providerCommercialRequired {
		providerAssessment = "active-exact-byte-bound-provider-authorizations"
	}
	gates = append(gates, newApprovalGate(
		"provider_commercial_authorizations", "Provider commercial authorization bindings", providerAuthorizationBindingsValid,
		providerAssessment, "release_provider_authorization_gate_incomplete",
		"Provider commercial impact requires active exact authorization versions with byte-bound documents and four decisions.",
		"The binding does not verify contract signatures, legal interpretation, evidence-repository authority or corporate delegation.",
	))

	releaseApprovals, err := countRows(ctx, db.Model(&persistence.Stage6ReleaseApproval{}).
		Where(
			"candidate_record_id = ? AND decision = ? AND approval_role IN ? AND evidence_sha256 IS NOT NULL AND evidence_sha256 <> ?",
			candidate.ID, "approved", requiredRoles, "sha256:"+strings.Repeat("0", 64),
		))
	if err != nil {
		return nil, problem.Wrap(500, "release_approvals_load_failed", "Release approvals could not be verified.", err)
	}
	gates = append(gates, newApprovalGate(
		"release_approvals", "Impact-derived Release decisions", releaseApprovals == int64(len(requiredRoles)),
		"separated-internal-decisions-recorded", "release_approvals_incomplete",
		"All impact-derived release approvals are required.",
		"Product authority does not prove employment, corporate delegation or evidence-repository authority.",
	))

	sloCount, err := countRows(ctx, db.Model(&persistence.Stage6SLOWindow{}).Where(
		`candidate_record_id = ? AND operator_tenant_id = ? AND state = ?
		 AND eligible_for_human_gate_review = ? AND all_objectives_assessable = ? AND all_objectives_met = ?
		 AND 4 = (SELECT count(*) FROM stage6_slo_approvals AS approval
		   WHERE approval.slo_window_record_id = stage6_slo_windows.id
		     AND approval.decision = 'approved' AND approval.superseded_at IS NULL
		     AND approval.evidence_sha256 IS NOT NULL AND approval.evidence_sha256 <> ?)`,
		candidate.ID, candidate.OperatorTenantID, "approved", true, true, true, "sha256:"+strings.Repeat("0", 64),
	))
	if err != nil {
		return nil, problem.Wrap(500, "release_slo_gate_load_failed", "The candidate SLO approval gate could not be verified.", err)
	}
	gates = append(gates, newApprovalGate(
		"slo", "SLO window", sloCount > 0, "approved-eligible-slo-window", "release_slo_gate_incomplete",
		"Release approval requires an approved eligible SLO window bound to this candidate.",
		"Real production telemetry, external probes and the complete observation window require external verification.",
	))

	recoveryCount, err := countRows(ctx, db.Model(&persistence.Stage6RecoveryDrill{}).Where(
		`candidate_record_id = ? AND operator_tenant_id = ? AND state = ?
		 AND eligible_for_human_gate_review = ? AND measurements_within_objectives = ?
		 AND all_restore_canaries_passed = ? AND all_source_approvals_approved = ?
		 AND cryptographic_signatures_verified = ? AND external_authority_verification_required = ?
		 AND 5 = (SELECT count(*) FROM stage6_recovery_approvals AS approval
		   WHERE approval.recovery_drill_record_id = stage6_recovery_drills.id
		     AND approval.decision = 'approved' AND approval.superseded_at IS NULL
		     AND approval.evidence_sha256 IS NOT NULL AND approval.evidence_sha256 <> ?)`,
		candidate.ID, candidate.OperatorTenantID, "approved", true, true, true, true, false, true,
		"sha256:"+strings.Repeat("0", 64),
	))
	if err != nil {
		return nil, problem.Wrap(500, "release_recovery_gate_load_failed", "The candidate Recovery approval gate could not be verified.", err)
	}
	gates = append(gates, newApprovalGate(
		"recovery", "Recovery drill", recoveryCount > 0, "approved-eligible-recovery-drill", "release_recovery_gate_incomplete",
		"Release approval requires an approved eligible Recovery drill bound to this candidate.",
		"Backup authority, production restore execution, approver authority and signatures require external verification.",
	))

	penetrationCount, err := countRows(ctx, db.Model(&persistence.Stage6PenetrationEngagement{}).Where(
		`candidate_record_id = ? AND operator_tenant_id = ? AND state = ?
		 AND third_party_independence_declared = ? AND stage5_dependency_satisfied = ?
		 AND asset_coverage_complete = ? AND scope_coverage_complete = ? AND methodology_coverage_complete = ?
		 AND no_unaccepted_high_or_critical_findings = ? AND eligible_for_human_gate_review = ?
		 AND cryptographic_signatures_verified = ? AND external_authority_verification_required = ?
		 AND 3 = (SELECT count(*) FROM stage6_penetration_approvals AS approval
		   WHERE approval.penetration_engagement_id = stage6_penetration_engagements.id
		     AND approval.decision = 'approved' AND approval.superseded_at IS NULL
		     AND approval.evidence_sha256 IS NOT NULL AND approval.evidence_sha256 <> ?)`,
		candidate.ID, candidate.OperatorTenantID, "approved", true, true, true, true, true, true, true, false, true,
		"sha256:"+strings.Repeat("0", 64),
	))
	if err != nil {
		return nil, problem.Wrap(500, "release_penetration_gate_load_failed", "The candidate Penetration approval gate could not be verified.", err)
	}
	gates = append(gates, newApprovalGate(
		"penetration", "Penetration engagement", penetrationCount > 0, "approved-eligible-penetration-engagement", "release_penetration_gate_incomplete",
		"Release approval requires an approved eligible Penetration engagement bound to this candidate.",
		"Third-party identity, actual execution, signed report and independent Security review require external verification.",
	))

	capacityCount, err := countRows(ctx, db.Model(&persistence.Stage6CapacityRun{}).Where(
		`candidate_record_id = ? AND operator_tenant_id = ? AND state = ?
		 AND forecast_headroom_covered = ? AND phase_coverage_complete = ? AND exercise_coverage_complete = ?
		 AND measurements_within_objectives = ? AND release_eligible_environment = ?
		 AND eligible_for_human_gate_review = ? AND cryptographic_signatures_verified = ?
		 AND external_authority_verification_required = ?
		 AND 2 = (SELECT count(*) FROM stage6_capacity_approvals AS approval
		   WHERE approval.capacity_run_id = stage6_capacity_runs.id
		     AND approval.decision = 'approved' AND approval.superseded_at IS NULL
		     AND approval.evidence_sha256 IS NOT NULL AND approval.evidence_sha256 <> ?)`,
		candidate.ID, candidate.OperatorTenantID, "approved", true, true, true, true, true, true, false, true,
		"sha256:"+strings.Repeat("0", 64),
	))
	if err != nil {
		return nil, problem.Wrap(500, "release_capacity_gate_load_failed", "The candidate Capacity approval gate could not be verified.", err)
	}
	gates = append(gates, newApprovalGate(
		"capacity", "Capacity and soak run", capacityCount > 0, "approved-eligible-capacity-run", "release_capacity_gate_incomplete",
		"Release approval requires an approved eligible Capacity run bound to this candidate.",
		"Production-like environment, telemetry authority, signatures and long-duration execution require external verification.",
	))

	incidentCount, err := countRows(ctx, db.Model(&persistence.Stage6IncidentExercise{}).Where(
		`candidate_record_id = ? AND operator_tenant_id = ? AND state = ?
		 AND independent_status_page_declared = ? AND role_separation_complete = ?
		 AND paging_exercise_complete = ? AND status_page_components_complete = ?
		 AND public_timeline_within_targets = ? AND subscriber_delivery_complete = ?
		 AND recovery_verification_complete = ? AND review_complete = ?
		 AND release_eligible_environment = ? AND eligible_for_human_gate_review = ?
		 AND cryptographic_signatures_verified = ? AND external_authority_verification_required = ?
		 AND 2 = (SELECT count(*) FROM stage6_incident_exercise_approvals AS approval
		   WHERE approval.incident_exercise_id = stage6_incident_exercises.id
		     AND approval.decision = 'approved' AND approval.superseded_at IS NULL
		     AND approval.evidence_sha256 IS NOT NULL AND approval.evidence_sha256 <> ?)`,
		candidate.ID, candidate.OperatorTenantID, "approved", true, true, true, true, true, true, true, true, true, true, false, true,
		"sha256:"+strings.Repeat("0", 64),
	))
	if err != nil {
		return nil, problem.Wrap(500, "release_incident_exercise_gate_load_failed", "The candidate Incident exercise approval gate could not be verified.", err)
	}
	gates = append(gates, newApprovalGate(
		"incident_exercise", "Incident communication exercise", incidentCount > 0, "approved-eligible-incident-exercise", "release_incident_exercise_gate_incomplete",
		"Release approval requires an approved eligible Incident exercise bound to this candidate.",
		"Internal Status Board, paging, employee notification delivery, signatures and actual execution require deployment verification.",
	))

	operationsCount, err := countRows(ctx, db.Model(&persistence.Stage6OperationsExercise{}).Where(
		`candidate_record_id = ? AND operator_tenant_id = ? AND state = ?
		 AND all_operations_passed = ? AND all_negative_authorizations_denied = ?
		 AND no_developer_fallbacks = ? AND production_authentication_declared = ?
		 AND support_lifecycle_complete = ? AND receipt_approvals_complete = ?
		 AND release_eligible_environment = ? AND eligible_for_human_gate_review = ?
		 AND cryptographic_signatures_verified = ? AND external_authority_verification_required = ?
		 AND 2 = (SELECT count(*) FROM stage6_operations_exercise_approvals AS approval
		   WHERE approval.operations_exercise_id = stage6_operations_exercises.id
		     AND approval.decision = 'approved' AND approval.superseded_at IS NULL
		     AND approval.evidence_sha256 IS NOT NULL AND approval.evidence_sha256 <> ?)`,
		candidate.ID, candidate.OperatorTenantID, "approved", true, true, true, true, true, true, true, true, false, true,
		"sha256:"+strings.Repeat("0", 64),
	))
	if err != nil {
		return nil, problem.Wrap(500, "release_operations_exercise_gate_load_failed", "The candidate Operations exercise approval gate could not be verified.", err)
	}
	gates = append(gates, newApprovalGate(
		"operations_exercise", "Operations browser exercise", operationsCount > 0, "approved-eligible-operations-exercise", "release_operations_exercise_gate_incomplete",
		"Release approval requires an approved eligible Operations exercise bound to this candidate.",
		"Deployed hosts, browser sessions, Audit/evidence authority, signatures and actual execution require external verification.",
	))

	internalCostCount, err := countRows(ctx, db.Model(&persistence.Stage6InternalCostReview{}).Where(
		`candidate_record_id = ? AND operator_tenant_id = ? AND state = ?
		 AND execution_count > 0
		 AND provider_cost_reported_execution_count + provider_cost_unavailable_execution_count = execution_count
		 AND actual_platform_allocation_count + estimated_platform_allocation_count = execution_count
		 AND token_totals_reconciled = ? AND provider_coverage_complete = ?
		 AND actual_overrides_estimate = ? AND currency_safe_aggregation = ?
		 AND tenant_isolation_validated = ? AND no_payment_data_present = ?
		 AND eligible_for_human_gate_review = ? AND cryptographic_signatures_verified = ?
		 AND external_source_authority_verification_required = ?
		 AND 2 = (SELECT count(*) FROM stage6_internal_cost_review_approvals AS approval
		   WHERE approval.internal_cost_review_id = stage6_internal_cost_reviews.id
		     AND approval.decision = 'approved' AND approval.superseded_at IS NULL
		     AND approval.evidence_sha256 IS NOT NULL AND approval.evidence_sha256 <> ?)`,
		candidate.ID, candidate.OperatorTenantID, "approved", true, true, true, true, true, true, true, false, true,
		"sha256:"+strings.Repeat("0", 64),
	))
	if err != nil {
		return nil, problem.Wrap(500, "release_internal_cost_gate_load_failed", "The candidate internal cost approval gate could not be verified.", err)
	}
	gates = append(gates, newApprovalGate(
		"internal_cost", "Internal usage and cost review", internalCostCount > 0, "approved-eligible-internal-cost-review", "release_internal_cost_gate_incomplete",
		"Release approval requires an approved internal Token, Provider cost and platform allocation review bound to this candidate.",
		"Source exports, allocation completeness, signatures, execution and organizational authority require external verification.",
	))

	return gates, nil
}

func containsImpactDomain(domains []string, expected string) bool {
	for _, domain := range domains {
		if domain == expected {
			return true
		}
	}
	return false
}

func newApprovalGate(
	id, label string,
	satisfied bool,
	assessment, code, message, externalBoundary string,
) approvalGateResult {
	return approvalGateResult{
		readiness: ReadinessGate{
			ID: id, Label: label, Satisfied: satisfied, InternalAssessment: assessment,
			ExternalVerificationRequired: true, ExternalBoundary: externalBoundary,
		},
		code: code, message: message,
	}
}

func countRows(ctx context.Context, query *gorm.DB) (int64, error) {
	var count int64
	if err := query.WithContext(ctx).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
