package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateRecoveryGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_approvals_no_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_drills_update`,
		`UPDATE stage6_recovery_approvals
		 SET superseded_at = COALESCE(superseded_at, CURRENT_TIMESTAMP),
		     superseded_reason = COALESCE(superseded_reason, 'Automatically superseded during Migration 000143 because Recovery approval evidence was not byte-bound.')
		 WHERE evidence_sha256 IS NULL AND superseded_at IS NULL`,
		`UPDATE stage6_recovery_drills
		 SET state = 'recorded', version = version + 1, approved_at = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE state = 'approved' AND EXISTS (
		   SELECT 1 FROM stage6_recovery_approvals AS approval
		   WHERE approval.recovery_drill_record_id = stage6_recovery_drills.id AND approval.superseded_at IS NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_recovery_drills_candidate ON stage6_recovery_drills (candidate_record_id, created_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_recovery_components_key ON stage6_recovery_components (recovery_drill_record_id, component_key)`,
		`DROP INDEX IF EXISTS uq_stage6_recovery_approvals_role`,
		`DROP INDEX IF EXISTS uq_stage6_recovery_approvals_user`,
		`DROP INDEX IF EXISTS uq_stage6_recovery_approvals_role_active`,
		`DROP INDEX IF EXISTS uq_stage6_recovery_approvals_user_active`,
		`CREATE UNIQUE INDEX uq_stage6_recovery_approvals_role_active ON stage6_recovery_approvals (recovery_drill_record_id, approval_role) WHERE superseded_at IS NULL`,
		`CREATE UNIQUE INDEX uq_stage6_recovery_approvals_user_active ON stage6_recovery_approvals (recovery_drill_record_id, approver_user_id) WHERE superseded_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_drills_insert`,
		`CREATE TRIGGER trg_stage6_recovery_drills_insert BEFORE INSERT ON stage6_recovery_drills BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Recovery drill') WHERE
		    length(NEW.receipt) NOT BETWEEN 1 AND 524288 OR length(NEW.receipt_sha256) <> 32
		    OR NEW.receipt_size_bytes <> length(NEW.receipt)
		    OR NEW.receipt_schema <> 'synara.recovery-drill-evidence-receipt.v2'
		    OR NEW.assessment <> 'evidence-validated-not-control-passed'
		    OR length(NEW.candidate_binding_sha256) <> 32 OR length(NEW.recovery_subject_sha256) <> 32
		    OR NEW.state <> 'recorded' OR NEW.version <> 1 OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
		    OR json_valid(CAST(NEW.receipt AS TEXT)) <> 1
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT))) <> 20
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.schemaVersion') <> NEW.receipt_schema
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.assessment') <> NEW.assessment
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.drillId') <> NEW.drill_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.candidateBindingSha256') <> 'sha256:' || lower(hex(NEW.candidate_binding_sha256))
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.recoverySubjectSha256') <> 'sha256:' || lower(hex(NEW.recovery_subject_sha256))
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.declaredMeasurementsWithinObjectives') IS NOT NEW.measurements_within_objectives
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.allRestoreCanariesPassed') IS NOT NEW.all_restore_canaries_passed
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.allRequiredApprovalsApproved') IS NOT NEW.all_source_approvals_approved
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.eligibleForHumanGateReview') IS NOT NEW.eligible_for_human_gate_review
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.verificationBoundary.cryptographicSignaturesVerified') IS NOT NEW.cryptographic_signatures_verified
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.verificationBoundary.realBackupRestoreAndApproverAuthorityVerificationRequired') IS NOT NEW.external_authority_verification_required
		    OR NEW.cryptographic_signatures_verified <> 0 OR NEW.external_authority_verification_required <> 1
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.components')) <> 4
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.approvals')) <> 5
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_release_candidates candidate
		      WHERE candidate.id = NEW.candidate_record_id AND candidate.operator_tenant_id = NEW.operator_tenant_id
		        AND candidate.created_by = NEW.created_by
		        AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.candidateId') = candidate.candidate_id
		        AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.sourceCommit') = candidate.source_commit
		        AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.environmentId') = candidate.environment_id
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_drills_update`,
		`CREATE TRIGGER trg_stage6_recovery_drills_update BEFORE UPDATE ON stage6_recovery_drills BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Recovery drill update') WHERE
		    NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		    OR NEW.candidate_record_id IS NOT OLD.candidate_record_id OR NEW.drill_id IS NOT OLD.drill_id
		    OR NEW.receipt IS NOT OLD.receipt OR NEW.receipt_sha256 IS NOT OLD.receipt_sha256
		    OR NEW.receipt_size_bytes <> OLD.receipt_size_bytes OR NEW.receipt_schema <> OLD.receipt_schema
		    OR NEW.assessment <> OLD.assessment OR NEW.candidate_binding_sha256 IS NOT OLD.candidate_binding_sha256
		    OR NEW.recovery_subject_sha256 IS NOT OLD.recovery_subject_sha256 OR NEW.started_at <> OLD.started_at
		    OR NEW.completed_at <> OLD.completed_at OR NEW.validated_at <> OLD.validated_at
		    OR NEW.measurements_within_objectives <> OLD.measurements_within_objectives
		    OR NEW.all_restore_canaries_passed <> OLD.all_restore_canaries_passed
		    OR NEW.all_source_approvals_approved <> OLD.all_source_approvals_approved
		    OR NEW.eligible_for_human_gate_review <> OLD.eligible_for_human_gate_review
		    OR NEW.cryptographic_signatures_verified <> OLD.cryptographic_signatures_verified
		    OR NEW.external_authority_verification_required <> OLD.external_authority_verification_required
		    OR NEW.created_by IS NOT OLD.created_by OR NEW.created_at <> OLD.created_at OR NEW.version <> OLD.version + 1
		    OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected')
		    OR (NEW.state = 'approved' AND (
		      NEW.eligible_for_human_gate_review <> 1 OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL
		      OR 5 <> (SELECT count(*) FROM stage6_recovery_approvals
		               WHERE recovery_drill_record_id = NEW.id AND decision = 'approved'
		                 AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
		                 AND evidence_sha256 <> 'sha256:' || printf('%064d', 0))
		    ))
		    OR (NEW.state = 'rejected' AND (NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL));
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_components_insert`,
		`CREATE TRIGGER trg_stage6_recovery_components_insert BEFORE INSERT ON stage6_recovery_components BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Recovery component') WHERE NOT EXISTS (
		    SELECT 1 FROM stage6_recovery_drills parent
		    WHERE parent.id = NEW.recovery_drill_record_id AND parent.operator_tenant_id = NEW.operator_tenant_id
		      AND parent.state = 'recorded'
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.profile') = NEW.profile
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.sourceRegion') = NEW.source_region
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.restoreRegion') = NEW.restore_region
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.measuredRpoSeconds') = NEW.measured_rpo_seconds
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.rpoObjectiveSeconds') = NEW.rpo_objective_seconds
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.measuredRtoSeconds') = NEW.measured_rto_seconds
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.rtoObjectiveSeconds') = NEW.rto_objective_seconds
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.rpoWithinObjective') IS NEW.rpo_within_objective
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.rtoWithinObjective') IS NEW.rto_within_objective
		      AND json_extract(CAST(parent.receipt AS TEXT), '$.components.' || NEW.component_key || '.restoreServedCanary') IS NEW.restore_served_canary
		  );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_approvals_insert`,
		`CREATE TRIGGER trg_stage6_recovery_approvals_insert BEFORE INSERT ON stage6_recovery_approvals BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Recovery approval') WHERE
		    NEW.approval_role NOT IN ('database', 'kms', 'operations', 'security', 'storage')
		    OR NEW.decision NOT IN ('approved', 'rejected') OR length(trim(NEW.reason)) NOT BETWEEN 20 AND 2000
		    OR substr(NEW.evidence_reference, 1, 8) <> 'https://'
		    OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		    OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		    OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		    OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		    OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_recovery_drills parent
		      JOIN stage6_governance_authority_grants authority ON authority.operator_tenant_id = parent.operator_tenant_id
		        AND authority.user_id = NEW.approver_user_id AND authority.authority_key = 'recovery.' || NEW.approval_role
		        AND authority.status = 'active' AND unixepoch(authority.expires_at) > unixepoch('now')
		      JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
		        AND membership.user_id = authority.user_id AND membership.status = 'active'
		        AND membership.role IN ('owner', 'admin', 'security_admin')
		      JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		        AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		      JOIN users governed_user ON governed_user.id = authority.user_id
		        AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		      WHERE parent.id = NEW.recovery_drill_record_id AND parent.operator_tenant_id = NEW.operator_tenant_id
		        AND parent.state = 'recorded' AND parent.created_by <> NEW.approver_user_id
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_components_no_update`,
		`CREATE TRIGGER trg_stage6_recovery_components_no_update BEFORE UPDATE ON stage6_recovery_components BEGIN SELECT RAISE(ABORT, 'Stage 6 Recovery evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_components_no_delete`,
		`CREATE TRIGGER trg_stage6_recovery_components_no_delete BEFORE DELETE ON stage6_recovery_components BEGIN SELECT RAISE(ABORT, 'Stage 6 Recovery evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_approvals_no_update`,
		`CREATE TRIGGER trg_stage6_recovery_approvals_no_update BEFORE UPDATE ON stage6_recovery_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 Recovery evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_approvals_no_delete`,
		`CREATE TRIGGER trg_stage6_recovery_approvals_no_delete BEFORE DELETE ON stage6_recovery_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 Recovery evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_recovery_drills_no_delete`,
		`CREATE TRIGGER trg_stage6_recovery_drills_no_delete BEFORE DELETE ON stage6_recovery_drills BEGIN SELECT RAISE(ABORT, 'Stage 6 Recovery evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_recovery_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_recovery_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state IN ('approved', 'deploying', 'observing', 'released')
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release transition requires an approved Recovery drill with five byte-bound decisions')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_recovery_drills AS recovery
		     WHERE recovery.candidate_record_id = NEW.id AND recovery.operator_tenant_id = NEW.operator_tenant_id
		       AND recovery.state = 'approved' AND recovery.eligible_for_human_gate_review = 1
		       AND recovery.measurements_within_objectives = 1 AND recovery.all_restore_canaries_passed = 1
		       AND recovery.all_source_approvals_approved = 1 AND recovery.cryptographic_signatures_verified = 0
		       AND recovery.external_authority_verification_required = 1
		       AND 5 = (
		         SELECT count(*) FROM stage6_recovery_approvals AS approval
		         WHERE approval.recovery_drill_record_id = recovery.id AND approval.decision = 'approved'
		           AND approval.superseded_at IS NULL AND approval.evidence_sha256 IS NOT NULL
		           AND approval.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		       )
		   );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 Recovery governance safety migration: %w", err)
		}
	}
	return nil
}
