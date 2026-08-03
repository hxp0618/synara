package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateSLOGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_slo_approvals_no_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_windows_update`,
		`UPDATE stage6_slo_approvals
		 SET superseded_at = COALESCE(superseded_at, CURRENT_TIMESTAMP),
		     superseded_reason = COALESCE(superseded_reason, 'Automatically superseded during Migration 000142 because SLO approval evidence was not byte-bound.')
		 WHERE evidence_sha256 IS NULL AND superseded_at IS NULL`,
		`UPDATE stage6_slo_windows
		 SET state = 'recorded', version = version + 1, approved_at = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE state = 'approved' AND EXISTS (
		   SELECT 1 FROM stage6_slo_approvals AS approval
		   WHERE approval.slo_window_record_id = stage6_slo_windows.id AND approval.superseded_at IS NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_slo_windows_candidate ON stage6_slo_windows (candidate_record_id, created_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_slo_objectives_key ON stage6_slo_objectives (slo_window_record_id, objective_key)`,
		`DROP INDEX IF EXISTS uq_stage6_slo_approvals_role`,
		`DROP INDEX IF EXISTS uq_stage6_slo_approvals_user`,
		`DROP INDEX IF EXISTS uq_stage6_slo_approvals_role_active`,
		`DROP INDEX IF EXISTS uq_stage6_slo_approvals_user_active`,
		`CREATE UNIQUE INDEX uq_stage6_slo_approvals_role_active ON stage6_slo_approvals (slo_window_record_id, approval_role) WHERE superseded_at IS NULL`,
		`CREATE UNIQUE INDEX uq_stage6_slo_approvals_user_active ON stage6_slo_approvals (slo_window_record_id, approver_user_id) WHERE superseded_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_windows_insert`,
		`CREATE TRIGGER trg_stage6_slo_windows_insert BEFORE INSERT ON stage6_slo_windows BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 SLO window') WHERE
		    length(NEW.receipt) NOT BETWEEN 1 AND 262144 OR length(NEW.receipt_sha256) <> 32
		    OR NEW.receipt_size_bytes <> length(NEW.receipt)
		    OR NEW.receipt_schema <> 'synara.slo-window-evidence-receipt.v1'
		    OR NEW.assessment <> 'evidence-validated-not-slo-passed'
		    OR NEW.environment_class NOT IN ('production', 'production-like')
		    OR NEW.state <> 'recorded' OR NEW.version <> 1 OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
		    OR unixepoch(NEW.window_completed_at) - unixepoch(NEW.window_started_at) < 2592000
		    OR unixepoch(NEW.validated_at) < unixepoch(NEW.window_completed_at)
		    OR json_valid(CAST(NEW.receipt AS TEXT)) <> 1
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.schemaVersion') <> NEW.receipt_schema
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.assessment') <> NEW.assessment
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.windowId') <> NEW.window_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.releaseCommit') <> NEW.release_commit
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.environmentId') <> NEW.environment_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.publicOrigin') <> NEW.public_origin
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.queryRevision') <> NEW.query_revision
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.allObjectivesAssessable') IS NOT NEW.all_objectives_assessable
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.declaredMeasurementsWithinObjectives') IS NOT NEW.all_objectives_met
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.eligibleForHumanGateReview') IS NOT NEW.eligible_for_human_gate_review
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.objectives')) <> 4
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_release_candidates candidate
		      WHERE candidate.id = NEW.candidate_record_id AND candidate.operator_tenant_id = NEW.operator_tenant_id
		        AND candidate.source_commit = NEW.release_commit AND candidate.environment_id = NEW.environment_id
		        AND candidate.created_by = NEW.created_by
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_windows_update`,
		`CREATE TRIGGER trg_stage6_slo_windows_update BEFORE UPDATE ON stage6_slo_windows BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 SLO window update') WHERE
		    NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		    OR NEW.candidate_record_id IS NOT OLD.candidate_record_id OR NEW.window_id IS NOT OLD.window_id
		    OR NEW.receipt IS NOT OLD.receipt OR NEW.receipt_sha256 IS NOT OLD.receipt_sha256
		    OR NEW.receipt_size_bytes <> OLD.receipt_size_bytes OR NEW.receipt_schema <> OLD.receipt_schema
		    OR NEW.assessment <> OLD.assessment OR NEW.release_commit <> OLD.release_commit
		    OR NEW.environment_class <> OLD.environment_class OR NEW.environment_id <> OLD.environment_id
		    OR NEW.public_origin <> OLD.public_origin OR NEW.window_started_at <> OLD.window_started_at
		    OR NEW.window_completed_at <> OLD.window_completed_at OR NEW.validated_at <> OLD.validated_at
		    OR NEW.query_revision <> OLD.query_revision
		    OR NEW.all_objectives_assessable <> OLD.all_objectives_assessable OR NEW.all_objectives_met <> OLD.all_objectives_met
		    OR NEW.eligible_for_human_gate_review <> OLD.eligible_for_human_gate_review
		    OR NEW.worst_budget_remaining_ratio <> OLD.worst_budget_remaining_ratio OR NEW.budget_policy_state <> OLD.budget_policy_state
		    OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at OR NEW.version <> OLD.version + 1
		    OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected')
		    OR (NEW.state = 'approved' AND (
		      NEW.eligible_for_human_gate_review <> 1 OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL
		      OR 4 <> (SELECT count(*) FROM stage6_slo_approvals
		               WHERE slo_window_record_id = NEW.id AND decision = 'approved'
		                 AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
		                 AND evidence_sha256 <> 'sha256:' || printf('%064d', 0))
		    ))
		    OR (NEW.state = 'rejected' AND (NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL));
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_objectives_insert`,
		`CREATE TRIGGER trg_stage6_slo_objectives_insert BEFORE INSERT ON stage6_slo_objectives BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 SLO objective') WHERE
		    NEW.objective_key NOT IN ('availability', 'apiLatency', 'executionStartDelay', 'eventDelay')
		    OR NEW.target_ratio NOT BETWEEN 0 AND 1 OR NEW.good_ratio NOT BETWEEN 0 AND 1 OR NEW.sample_count < 0
		    OR NEW.error_budget_remaining_ratio NOT BETWEEN 0 AND 1
		    OR NEW.policy_state NOT IN ('normal-delivery', 'risk-note-required', 'risky-rollout-paused', 'reliability-freeze')
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_slo_windows parent WHERE parent.id = NEW.slo_window_record_id
		        AND parent.operator_tenant_id = NEW.operator_tenant_id AND parent.state = 'recorded'
		        AND json_extract(CAST(parent.receipt AS TEXT), '$.objectives.' || NEW.objective_key || '.targetRatio') = NEW.target_ratio
		        AND json_extract(CAST(parent.receipt AS TEXT), '$.objectives.' || NEW.objective_key || '.goodRatio') = NEW.good_ratio
		        AND json_extract(CAST(parent.receipt AS TEXT), '$.objectives.' || NEW.objective_key || '.sampleCount') = NEW.sample_count
		        AND json_extract(CAST(parent.receipt AS TEXT), '$.objectives.' || NEW.objective_key || '.errorBudgetPolicyState') = NEW.policy_state
		        AND json_extract(CAST(parent.receipt AS TEXT), '$.objectives.' || NEW.objective_key || '.assessable') IS NEW.assessable
		        AND json_extract(CAST(parent.receipt AS TEXT), '$.objectives.' || NEW.objective_key || '.objectiveMet') IS NEW.objective_met
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_approvals_insert`,
		`CREATE TRIGGER trg_stage6_slo_approvals_insert BEFORE INSERT ON stage6_slo_approvals BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 SLO approval') WHERE
		    NEW.approval_role NOT IN ('engineering', 'operations', 'security', 'product')
		    OR NEW.decision NOT IN ('approved', 'rejected') OR length(trim(NEW.reason)) NOT BETWEEN 20 AND 2000
		    OR substr(NEW.evidence_reference, 1, 8) <> 'https://'
		    OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		    OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		    OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		    OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		    OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_slo_windows parent
		      JOIN stage6_governance_authority_grants authority ON authority.operator_tenant_id = parent.operator_tenant_id
		        AND authority.user_id = NEW.approver_user_id AND authority.authority_key = 'release.' || NEW.approval_role
		        AND authority.status = 'active' AND unixepoch(authority.expires_at) > unixepoch('now')
		      JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
		        AND membership.user_id = authority.user_id AND membership.status = 'active'
		        AND membership.role IN ('owner', 'admin', 'security_admin')
		      JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		        AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		      JOIN users governed_user ON governed_user.id = authority.user_id
		        AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		      WHERE parent.id = NEW.slo_window_record_id AND parent.operator_tenant_id = NEW.operator_tenant_id
		        AND parent.state = 'recorded' AND parent.created_by <> NEW.approver_user_id
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_objectives_no_update`,
		`CREATE TRIGGER trg_stage6_slo_objectives_no_update BEFORE UPDATE ON stage6_slo_objectives BEGIN SELECT RAISE(ABORT, 'Stage 6 SLO evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_objectives_no_delete`,
		`CREATE TRIGGER trg_stage6_slo_objectives_no_delete BEFORE DELETE ON stage6_slo_objectives BEGIN SELECT RAISE(ABORT, 'Stage 6 SLO evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_approvals_no_update`,
		`CREATE TRIGGER trg_stage6_slo_approvals_no_update BEFORE UPDATE ON stage6_slo_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 SLO evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_approvals_no_delete`,
		`CREATE TRIGGER trg_stage6_slo_approvals_no_delete BEFORE DELETE ON stage6_slo_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 SLO evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_slo_windows_no_delete`,
		`CREATE TRIGGER trg_stage6_slo_windows_no_delete BEFORE DELETE ON stage6_slo_windows BEGIN SELECT RAISE(ABORT, 'Stage 6 SLO evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_slo_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_slo_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state IN ('approved', 'deploying', 'observing', 'released')
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release transition requires an approved SLO window with four byte-bound decisions')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_slo_windows AS slo
		     WHERE slo.candidate_record_id = NEW.id AND slo.operator_tenant_id = NEW.operator_tenant_id
		       AND slo.state = 'approved' AND slo.eligible_for_human_gate_review = 1
		       AND slo.all_objectives_assessable = 1 AND slo.all_objectives_met = 1
		       AND 4 = (
		         SELECT count(*) FROM stage6_slo_approvals AS approval
		         WHERE approval.slo_window_record_id = slo.id AND approval.decision = 'approved'
		           AND approval.superseded_at IS NULL AND approval.evidence_sha256 IS NOT NULL
		           AND approval.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		       )
		   );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 SLO governance safety migration: %w", err)
		}
	}
	return nil
}
