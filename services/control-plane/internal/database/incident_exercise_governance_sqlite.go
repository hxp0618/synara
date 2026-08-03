package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateIncidentExerciseGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_incident_exercise_approvals_no_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_exercises_update`,
		`UPDATE stage6_incident_exercise_approvals
		 SET superseded_at = COALESCE(superseded_at, CURRENT_TIMESTAMP),
		     superseded_reason = COALESCE(superseded_reason, 'Automatically superseded during Migration 000146 because Incident exercise approval evidence was not byte-bound.')
		 WHERE evidence_sha256 IS NULL AND superseded_at IS NULL`,
		`UPDATE stage6_incident_exercises
		 SET state = 'recorded', version = version + 1, approved_at = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE state = 'approved' AND EXISTS (
		   SELECT 1 FROM stage6_incident_exercise_approvals AS approval
		   WHERE approval.incident_exercise_id = stage6_incident_exercises.id AND approval.superseded_at IS NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_incident_exercises_candidate ON stage6_incident_exercises (candidate_record_id, created_at DESC, id)`,
		`DROP INDEX IF EXISTS uq_stage6_incident_exercise_approvals_role`,
		`DROP INDEX IF EXISTS uq_stage6_incident_exercise_approvals_user`,
		`DROP INDEX IF EXISTS uq_stage6_incident_exercise_approvals_role_active`,
		`DROP INDEX IF EXISTS uq_stage6_incident_exercise_approvals_user_active`,
		`CREATE UNIQUE INDEX uq_stage6_incident_exercise_approvals_role_active ON stage6_incident_exercise_approvals (incident_exercise_id, approval_role) WHERE superseded_at IS NULL`,
		`CREATE UNIQUE INDEX uq_stage6_incident_exercise_approvals_user_active ON stage6_incident_exercise_approvals (incident_exercise_id, approver_user_id) WHERE superseded_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_exercises_insert`,
		`CREATE TRIGGER trg_stage6_incident_exercises_insert BEFORE INSERT ON stage6_incident_exercises BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Incident exercise') WHERE
		    length(NEW.receipt) NOT BETWEEN 1 AND 524288 OR length(NEW.receipt_sha256) <> 32
		    OR NEW.receipt_size_bytes <> length(NEW.receipt)
		    OR NEW.receipt_schema <> 'synara.incident-communication-exercise-evidence-receipt.v2'
		    OR NEW.assessment <> 'evidence-validated-not-operations-ready'
		    OR json_valid(CAST(NEW.receipt AS TEXT)) <> 1
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.schemaVersion') <> NEW.receipt_schema
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.assessment') <> NEW.assessment
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.exerciseId') <> NEW.exercise_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.releaseCommit') <> NEW.release_commit
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.environmentClass') <> NEW.environment_class
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.environmentId') <> NEW.environment_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.exerciseMode') <> NEW.exercise_mode
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.severity') <> NEW.severity
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.serviceOrigin') <> NEW.service_origin
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.internalStatusBoardOrigin') <> NEW.status_page_origin
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.independentInternalStatusBoardDeclared') IS NOT NEW.independent_status_page_declared
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.roleSeparationComplete') IS NOT NEW.role_separation_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.pagingExerciseComplete') IS NOT NEW.paging_exercise_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.internalStatusBoardComponentsComplete') IS NOT NEW.status_page_components_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.internalTimelineWithinTargets') IS NOT NEW.public_timeline_within_targets
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.employeeNotificationDeliveryComplete') IS NOT NEW.subscriber_delivery_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.recoveryVerificationComplete') IS NOT NEW.recovery_verification_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.reviewComplete') IS NOT NEW.review_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.releaseEligibleEnvironment') IS NOT NEW.release_eligible_environment
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.eligibleForHumanGateReview') IS NOT NEW.eligible_for_human_gate_review
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.internalStatusBoardComponents')) <> 6
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.evidence')) <> 7
		    OR NEW.environment_class NOT IN ('production', 'production-like')
		    OR NEW.exercise_mode <> 'live-internal-communication' OR NEW.severity NOT IN ('SEV-0', 'SEV-1', 'SEV-2')
		    OR NEW.independent_status_page_declared <> 1 OR NEW.role_separation_complete <> 1
		    OR NEW.paging_exercise_complete <> 1 OR NEW.status_page_components_complete <> 1
		    OR NEW.public_timeline_within_targets <> 1 OR NEW.subscriber_delivery_complete <> 1
		    OR NEW.recovery_verification_complete <> 1 OR NEW.review_complete <> 1
		    OR NEW.release_eligible_environment <> 1 OR NEW.eligible_for_human_gate_review <> 1
		    OR NEW.cryptographic_signatures_verified <> 0 OR NEW.external_authority_verification_required <> 1
		    OR NEW.state <> 'recorded' OR NEW.version <> 1 OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_release_candidates candidate
		      WHERE candidate.id = NEW.candidate_record_id AND candidate.operator_tenant_id = NEW.operator_tenant_id
		        AND candidate.created_by = NEW.created_by AND candidate.source_commit = NEW.release_commit
		        AND candidate.environment_id = NEW.environment_id
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.candidate.environmentClass') = NEW.environment_class
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.receipts.incident.sha256') = 'sha256:' || lower(hex(NEW.receipt_sha256))
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_exercises_update`,
		`CREATE TRIGGER trg_stage6_incident_exercises_update BEFORE UPDATE ON stage6_incident_exercises BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Incident exercise update') WHERE
		    NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		    OR NEW.candidate_record_id IS NOT OLD.candidate_record_id OR NEW.exercise_id IS NOT OLD.exercise_id
		    OR NEW.receipt IS NOT OLD.receipt OR NEW.receipt_sha256 IS NOT OLD.receipt_sha256
		    OR NEW.receipt_size_bytes <> OLD.receipt_size_bytes OR NEW.receipt_schema <> OLD.receipt_schema
		    OR NEW.assessment <> OLD.assessment OR NEW.release_commit <> OLD.release_commit
		    OR NEW.environment_class <> OLD.environment_class OR NEW.environment_id <> OLD.environment_id
		    OR NEW.exercise_mode <> OLD.exercise_mode OR NEW.severity <> OLD.severity
		    OR NEW.service_origin <> OLD.service_origin OR NEW.status_page_origin <> OLD.status_page_origin
		    OR NEW.started_at <> OLD.started_at OR NEW.completed_at <> OLD.completed_at OR NEW.validated_at <> OLD.validated_at
		    OR NEW.independent_status_page_declared <> OLD.independent_status_page_declared
		    OR NEW.role_separation_complete <> OLD.role_separation_complete
		    OR NEW.paging_exercise_complete <> OLD.paging_exercise_complete
		    OR NEW.status_page_components_complete <> OLD.status_page_components_complete
		    OR NEW.public_timeline_within_targets <> OLD.public_timeline_within_targets
		    OR NEW.subscriber_delivery_complete <> OLD.subscriber_delivery_complete
		    OR NEW.recovery_verification_complete <> OLD.recovery_verification_complete
		    OR NEW.review_complete <> OLD.review_complete
		    OR NEW.release_eligible_environment <> OLD.release_eligible_environment
		    OR NEW.eligible_for_human_gate_review <> OLD.eligible_for_human_gate_review
		    OR NEW.cryptographic_signatures_verified <> OLD.cryptographic_signatures_verified
		    OR NEW.external_authority_verification_required <> OLD.external_authority_verification_required
		    OR NEW.created_by IS NOT OLD.created_by OR NEW.created_at <> OLD.created_at OR NEW.version <> OLD.version + 1
		    OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected')
		    OR (NEW.state = 'approved' AND (
		      NEW.eligible_for_human_gate_review <> 1 OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL
		      OR 2 <> (SELECT count(*) FROM stage6_incident_exercise_approvals
		               WHERE incident_exercise_id = NEW.id AND decision = 'approved'
		                 AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
		                 AND evidence_sha256 <> 'sha256:' || printf('%064d', 0))
		    ))
		    OR (NEW.state = 'rejected' AND (NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL));
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_exercise_approvals_insert`,
		`CREATE TRIGGER trg_stage6_incident_exercise_approvals_insert BEFORE INSERT ON stage6_incident_exercise_approvals BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Incident exercise approval') WHERE
		    NEW.approval_role NOT IN ('operations', 'communications')
		    OR NEW.decision NOT IN ('approved', 'rejected') OR length(trim(NEW.reason)) NOT BETWEEN 20 AND 2000
		    OR substr(NEW.evidence_reference, 1, 8) <> 'https://'
		    OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		    OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		    OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		    OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		    OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_incident_exercises parent
		      JOIN stage6_governance_authority_grants authority ON authority.operator_tenant_id = parent.operator_tenant_id
		        AND authority.user_id = NEW.approver_user_id AND authority.authority_key = 'incident_exercise.' || NEW.approval_role
		        AND authority.status = 'active' AND unixepoch(authority.expires_at) > unixepoch('now')
		      JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
		        AND membership.user_id = authority.user_id AND membership.status = 'active'
		        AND membership.role IN ('owner', 'admin', 'security_admin')
		      JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		        AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		      JOIN users governed_user ON governed_user.id = authority.user_id
		        AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		      WHERE parent.id = NEW.incident_exercise_id AND parent.operator_tenant_id = NEW.operator_tenant_id
		        AND parent.state = 'recorded' AND parent.created_by <> NEW.approver_user_id
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_exercise_approvals_no_update`,
		`CREATE TRIGGER trg_stage6_incident_exercise_approvals_no_update BEFORE UPDATE ON stage6_incident_exercise_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 Incident exercise evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_exercise_approvals_no_delete`,
		`CREATE TRIGGER trg_stage6_incident_exercise_approvals_no_delete BEFORE DELETE ON stage6_incident_exercise_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 Incident exercise evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_exercises_no_delete`,
		`CREATE TRIGGER trg_stage6_incident_exercises_no_delete BEFORE DELETE ON stage6_incident_exercises BEGIN SELECT RAISE(ABORT, 'Stage 6 Incident exercise evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_incident_exercise_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_incident_exercise_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state IN ('approved', 'deploying', 'observing', 'released')
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release transition requires an approved Incident exercise with two byte-bound decisions')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_incident_exercises AS exercise
		     WHERE exercise.candidate_record_id = NEW.id AND exercise.operator_tenant_id = NEW.operator_tenant_id
		       AND exercise.state = 'approved' AND exercise.independent_status_page_declared = 1
		       AND exercise.role_separation_complete = 1 AND exercise.paging_exercise_complete = 1
		       AND exercise.status_page_components_complete = 1 AND exercise.public_timeline_within_targets = 1
		       AND exercise.subscriber_delivery_complete = 1 AND exercise.recovery_verification_complete = 1
		       AND exercise.review_complete = 1 AND exercise.release_eligible_environment = 1
		       AND exercise.eligible_for_human_gate_review = 1
		       AND exercise.cryptographic_signatures_verified = 0
		       AND exercise.external_authority_verification_required = 1
		       AND 2 = (
		         SELECT count(*) FROM stage6_incident_exercise_approvals AS approval
		         WHERE approval.incident_exercise_id = exercise.id AND approval.decision = 'approved'
		           AND approval.superseded_at IS NULL AND approval.evidence_sha256 IS NOT NULL
		           AND approval.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		       )
		   );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 Incident exercise governance safety migration: %w", err)
		}
	}
	return nil
}
