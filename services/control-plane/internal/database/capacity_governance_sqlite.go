package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateCapacityGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_approvals_no_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_runs_update`,
		`UPDATE stage6_capacity_approvals
		 SET superseded_at = COALESCE(superseded_at, CURRENT_TIMESTAMP),
		     superseded_reason = COALESCE(superseded_reason, 'Automatically superseded during Migration 000145 because Capacity approval evidence was not byte-bound.')
		 WHERE evidence_sha256 IS NULL AND superseded_at IS NULL`,
		`UPDATE stage6_capacity_runs
		 SET state = 'recorded', version = version + 1, approved_at = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE state = 'approved' AND EXISTS (
		   SELECT 1 FROM stage6_capacity_approvals AS approval
		   WHERE approval.capacity_run_id = stage6_capacity_runs.id AND approval.superseded_at IS NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_capacity_runs_candidate ON stage6_capacity_runs (candidate_record_id, created_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_capacity_phases_name ON stage6_capacity_phases (capacity_run_id, phase_name)`,
		`DROP INDEX IF EXISTS uq_stage6_capacity_approvals_role`,
		`DROP INDEX IF EXISTS uq_stage6_capacity_approvals_user`,
		`DROP INDEX IF EXISTS uq_stage6_capacity_approvals_role_active`,
		`DROP INDEX IF EXISTS uq_stage6_capacity_approvals_user_active`,
		`CREATE UNIQUE INDEX uq_stage6_capacity_approvals_role_active ON stage6_capacity_approvals (capacity_run_id, approval_role) WHERE superseded_at IS NULL`,
		`CREATE UNIQUE INDEX uq_stage6_capacity_approvals_user_active ON stage6_capacity_approvals (capacity_run_id, approver_user_id) WHERE superseded_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_runs_insert`,
		`CREATE TRIGGER trg_stage6_capacity_runs_insert BEFORE INSERT ON stage6_capacity_runs BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Capacity run') WHERE
		    length(NEW.receipt) NOT BETWEEN 1 AND 524288 OR length(NEW.receipt_sha256) <> 32
		    OR NEW.receipt_size_bytes <> length(NEW.receipt)
		    OR NEW.receipt_schema <> 'synara.capacity-soak-evidence-receipt.v1'
		    OR NEW.assessment <> 'evidence-validated-not-capacity-passed'
		    OR json_valid(CAST(NEW.receipt AS TEXT)) <> 1
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.schemaVersion') <> NEW.receipt_schema
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.assessment') <> NEW.assessment
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.runId') <> NEW.run_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.releaseCommit') <> NEW.release_commit
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.environmentClass') <> NEW.environment_class
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.environmentId') <> NEW.environment_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.durationSeconds') <> NEW.duration_seconds
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.minimumDurationSeconds') <> NEW.minimum_duration_seconds
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.sampleIntervalSeconds') <> NEW.sample_interval_seconds
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.externalProbeRegions') <> NEW.external_probe_regions
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.externalProbeCoverageRatio') <> NEW.external_probe_coverage_ratio
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.forecastHeadroomCovered') IS NOT NEW.forecast_headroom_covered
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.declaredMeasurementsWithinObjectives') IS NOT NEW.measurements_within_objectives
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.releaseEligibleEnvironment') IS NOT NEW.release_eligible_environment
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.eligibleForHumanGateReview') IS NOT NEW.eligible_for_human_gate_review
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.phases')) <> 5
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.evidence')) <> 7
		    OR NEW.forecast_headroom_covered <> 1 OR NEW.phase_coverage_complete <> 1
		    OR NEW.exercise_coverage_complete <> 1 OR NEW.measurements_within_objectives <> 1
		    OR NEW.release_eligible_environment <> 1 OR NEW.eligible_for_human_gate_review <> 1
		    OR NEW.cryptographic_signatures_verified <> 0 OR NEW.external_authority_verification_required <> 1
		    OR NEW.state <> 'recorded' OR NEW.version <> 1 OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_release_candidates candidate
		      WHERE candidate.id = NEW.candidate_record_id AND candidate.operator_tenant_id = NEW.operator_tenant_id
		        AND candidate.created_by = NEW.created_by AND candidate.source_commit = NEW.release_commit
		        AND candidate.environment_id = NEW.environment_id
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.candidate.environmentClass') = NEW.environment_class
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.receipts.capacity.sha256') = 'sha256:' || lower(hex(NEW.receipt_sha256))
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_runs_update`,
		`CREATE TRIGGER trg_stage6_capacity_runs_update BEFORE UPDATE ON stage6_capacity_runs BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Capacity run update') WHERE
		    NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		    OR NEW.candidate_record_id IS NOT OLD.candidate_record_id OR NEW.run_id IS NOT OLD.run_id
		    OR NEW.receipt IS NOT OLD.receipt OR NEW.receipt_sha256 IS NOT OLD.receipt_sha256
		    OR NEW.receipt_size_bytes <> OLD.receipt_size_bytes OR NEW.receipt_schema <> OLD.receipt_schema
		    OR NEW.assessment <> OLD.assessment OR NEW.release_commit <> OLD.release_commit
		    OR NEW.environment_class <> OLD.environment_class OR NEW.environment_id <> OLD.environment_id
		    OR NEW.started_at <> OLD.started_at OR NEW.completed_at <> OLD.completed_at OR NEW.validated_at <> OLD.validated_at
		    OR NEW.duration_seconds <> OLD.duration_seconds OR NEW.minimum_duration_seconds <> OLD.minimum_duration_seconds
		    OR NEW.sample_interval_seconds <> OLD.sample_interval_seconds OR NEW.external_probe_regions <> OLD.external_probe_regions
		    OR NEW.external_probe_coverage_ratio <> OLD.external_probe_coverage_ratio
		    OR NEW.forecast_headroom_covered <> OLD.forecast_headroom_covered
		    OR NEW.phase_coverage_complete <> OLD.phase_coverage_complete OR NEW.exercise_coverage_complete <> OLD.exercise_coverage_complete
		    OR NEW.measurements_within_objectives <> OLD.measurements_within_objectives
		    OR NEW.release_eligible_environment <> OLD.release_eligible_environment
		    OR NEW.eligible_for_human_gate_review <> OLD.eligible_for_human_gate_review
		    OR NEW.cryptographic_signatures_verified <> OLD.cryptographic_signatures_verified
		    OR NEW.external_authority_verification_required <> OLD.external_authority_verification_required
		    OR NEW.created_by IS NOT OLD.created_by OR NEW.created_at <> OLD.created_at OR NEW.version <> OLD.version + 1
		    OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected')
		    OR (NEW.state = 'approved' AND (
		      NEW.eligible_for_human_gate_review <> 1 OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL
		      OR 2 <> (SELECT count(*) FROM stage6_capacity_approvals
		               WHERE capacity_run_id = NEW.id AND decision = 'approved'
		                 AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
		                 AND evidence_sha256 <> 'sha256:' || printf('%064d', 0))
		    ))
		    OR (NEW.state = 'rejected' AND (NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL));
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_phases_insert`,
		`CREATE TRIGGER trg_stage6_capacity_phases_insert BEFORE INSERT ON stage6_capacity_phases BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Capacity phase') WHERE NOT EXISTS (
		    SELECT 1 FROM stage6_capacity_runs parent
		    JOIN json_each(CAST(parent.receipt AS TEXT), '$.phases') phase
		    WHERE parent.id = NEW.capacity_run_id AND parent.operator_tenant_id = NEW.operator_tenant_id AND parent.state = 'recorded'
		      AND json_extract(phase.value, '$.name') = NEW.phase_name
		      AND json_extract(phase.value, '$.durationSeconds') = NEW.duration_seconds
		      AND json_extract(phase.value, '$.loadMultiplier') = NEW.load_multiplier
		  );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_approvals_insert`,
		`CREATE TRIGGER trg_stage6_capacity_approvals_insert BEFORE INSERT ON stage6_capacity_approvals BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Capacity approval') WHERE
		    NEW.approval_role NOT IN ('engineering', 'operations')
		    OR NEW.decision NOT IN ('approved', 'rejected') OR length(trim(NEW.reason)) NOT BETWEEN 20 AND 2000
		    OR substr(NEW.evidence_reference, 1, 8) <> 'https://'
		    OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		    OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		    OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		    OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		    OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_capacity_runs parent
		      JOIN stage6_governance_authority_grants authority ON authority.operator_tenant_id = parent.operator_tenant_id
		        AND authority.user_id = NEW.approver_user_id AND authority.authority_key = 'capacity.' || NEW.approval_role
		        AND authority.status = 'active' AND unixepoch(authority.expires_at) > unixepoch('now')
		      JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
		        AND membership.user_id = authority.user_id AND membership.status = 'active'
		        AND membership.role IN ('owner', 'admin', 'security_admin')
		      JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		        AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		      JOIN users governed_user ON governed_user.id = authority.user_id
		        AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		      WHERE parent.id = NEW.capacity_run_id AND parent.operator_tenant_id = NEW.operator_tenant_id
		        AND parent.state = 'recorded' AND parent.created_by <> NEW.approver_user_id
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_phases_no_update`,
		`CREATE TRIGGER trg_stage6_capacity_phases_no_update BEFORE UPDATE ON stage6_capacity_phases BEGIN SELECT RAISE(ABORT, 'Stage 6 Capacity evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_phases_no_delete`,
		`CREATE TRIGGER trg_stage6_capacity_phases_no_delete BEFORE DELETE ON stage6_capacity_phases BEGIN SELECT RAISE(ABORT, 'Stage 6 Capacity evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_approvals_no_update`,
		`CREATE TRIGGER trg_stage6_capacity_approvals_no_update BEFORE UPDATE ON stage6_capacity_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 Capacity evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_approvals_no_delete`,
		`CREATE TRIGGER trg_stage6_capacity_approvals_no_delete BEFORE DELETE ON stage6_capacity_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 Capacity evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_capacity_runs_no_delete`,
		`CREATE TRIGGER trg_stage6_capacity_runs_no_delete BEFORE DELETE ON stage6_capacity_runs BEGIN SELECT RAISE(ABORT, 'Stage 6 Capacity evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_capacity_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_capacity_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state IN ('approved', 'deploying', 'observing', 'released')
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release transition requires an approved Capacity run with two byte-bound decisions')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_capacity_runs AS capacity
		     WHERE capacity.candidate_record_id = NEW.id AND capacity.operator_tenant_id = NEW.operator_tenant_id
		       AND capacity.state = 'approved' AND capacity.forecast_headroom_covered = 1
		       AND capacity.phase_coverage_complete = 1 AND capacity.exercise_coverage_complete = 1
		       AND capacity.measurements_within_objectives = 1 AND capacity.release_eligible_environment = 1
		       AND capacity.eligible_for_human_gate_review = 1
		       AND capacity.cryptographic_signatures_verified = 0
		       AND capacity.external_authority_verification_required = 1
		       AND 2 = (
		         SELECT count(*) FROM stage6_capacity_approvals AS approval
		         WHERE approval.capacity_run_id = capacity.id AND approval.decision = 'approved'
		           AND approval.superseded_at IS NULL AND approval.evidence_sha256 IS NOT NULL
		           AND approval.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		       )
		   );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 Capacity governance safety migration: %w", err)
		}
	}
	return nil
}
