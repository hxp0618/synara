package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migratePenetrationGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_approvals_no_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_engagements_update`,
		`UPDATE stage6_penetration_approvals
		 SET superseded_at = COALESCE(superseded_at, CURRENT_TIMESTAMP),
		     superseded_reason = COALESCE(superseded_reason, 'Automatically superseded during Migration 000144 because Penetration approval evidence was not byte-bound.')
		 WHERE evidence_sha256 IS NULL AND superseded_at IS NULL`,
		`UPDATE stage6_penetration_engagements
		 SET state = 'recorded', version = version + 1, approved_at = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE state = 'approved' AND EXISTS (
		   SELECT 1 FROM stage6_penetration_approvals AS approval
		   WHERE approval.penetration_engagement_id = stage6_penetration_engagements.id AND approval.superseded_at IS NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_penetration_engagements_candidate ON stage6_penetration_engagements (candidate_record_id, created_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_penetration_assets_type ON stage6_penetration_assets (penetration_engagement_id, asset_type)`,
		`DROP INDEX IF EXISTS uq_stage6_penetration_approvals_role`,
		`DROP INDEX IF EXISTS uq_stage6_penetration_approvals_user`,
		`DROP INDEX IF EXISTS uq_stage6_penetration_approvals_role_active`,
		`DROP INDEX IF EXISTS uq_stage6_penetration_approvals_user_active`,
		`CREATE UNIQUE INDEX uq_stage6_penetration_approvals_role_active ON stage6_penetration_approvals (penetration_engagement_id, approval_role) WHERE superseded_at IS NULL`,
		`CREATE UNIQUE INDEX uq_stage6_penetration_approvals_user_active ON stage6_penetration_approvals (penetration_engagement_id, approver_user_id) WHERE superseded_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_engagements_insert`,
		`CREATE TRIGGER trg_stage6_penetration_engagements_insert BEFORE INSERT ON stage6_penetration_engagements BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Penetration engagement') WHERE
		    length(NEW.receipt) NOT BETWEEN 1 AND 524288 OR length(NEW.receipt_sha256) <> 32
		    OR NEW.receipt_size_bytes <> length(NEW.receipt)
		    OR NEW.receipt_schema <> 'synara.third-party-penetration-evidence-receipt.v1'
		    OR NEW.assessment <> 'evidence-validated-not-penetration-passed'
		    OR json_valid(CAST(NEW.receipt AS TEXT)) <> 1
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.schemaVersion') <> NEW.receipt_schema
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.assessment') <> NEW.assessment
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.engagementId') <> NEW.engagement_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.releaseCommit') <> NEW.release_commit
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.environmentClass') <> NEW.environment_class
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.environmentId') <> NEW.environment_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.deploymentProfile') <> NEW.deployment_profile
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.thirdPartyIndependenceDeclared') IS NOT NEW.third_party_independence_declared
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.stage5Dependency.declaredSatisfied') IS NOT NEW.stage5_dependency_satisfied
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.assetCoverageComplete') IS NOT NEW.asset_coverage_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.scopeCoverageComplete') IS NOT NEW.scope_coverage_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.methodologyCoverageComplete') IS NOT NEW.methodology_coverage_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.declaredNoUnacceptedHighOrCriticalFindings') IS NOT NEW.no_unaccepted_high_or_critical_findings
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.eligibleForHumanGateReview') IS NOT NEW.eligible_for_human_gate_review
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.releaseEligibleEnvironment') IS NOT 1
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.assets')) <> 4
		    OR NEW.third_party_independence_declared <> 1 OR NEW.stage5_dependency_satisfied <> 1
		    OR NEW.asset_coverage_complete <> 1 OR NEW.scope_coverage_complete <> 1
		    OR NEW.methodology_coverage_complete <> 1 OR NEW.no_unaccepted_high_or_critical_findings <> 1
		    OR NEW.eligible_for_human_gate_review <> 1 OR NEW.cryptographic_signatures_verified <> 0
		    OR NEW.external_authority_verification_required <> 1
		    OR NEW.state <> 'recorded' OR NEW.version <> 1 OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_release_candidates candidate
		      WHERE candidate.id = NEW.candidate_record_id AND candidate.operator_tenant_id = NEW.operator_tenant_id
		        AND candidate.created_by = NEW.created_by AND candidate.source_commit = NEW.release_commit
		        AND candidate.environment_id = NEW.environment_id
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.candidate.environmentClass') = NEW.environment_class
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.receipts.penetration.sha256') = 'sha256:' || lower(hex(NEW.receipt_sha256))
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_engagements_update`,
		`CREATE TRIGGER trg_stage6_penetration_engagements_update BEFORE UPDATE ON stage6_penetration_engagements BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Penetration engagement update') WHERE
		    NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		    OR NEW.candidate_record_id IS NOT OLD.candidate_record_id OR NEW.engagement_id IS NOT OLD.engagement_id
		    OR NEW.receipt IS NOT OLD.receipt OR NEW.receipt_sha256 IS NOT OLD.receipt_sha256
		    OR NEW.receipt_size_bytes <> OLD.receipt_size_bytes OR NEW.receipt_schema <> OLD.receipt_schema
		    OR NEW.assessment <> OLD.assessment OR NEW.release_commit <> OLD.release_commit
		    OR NEW.environment_class <> OLD.environment_class OR NEW.environment_id <> OLD.environment_id
		    OR NEW.deployment_profile <> OLD.deployment_profile OR NEW.started_at <> OLD.started_at
		    OR NEW.completed_at <> OLD.completed_at OR NEW.report_issued_at <> OLD.report_issued_at
		    OR NEW.validated_at <> OLD.validated_at
		    OR NEW.third_party_independence_declared <> OLD.third_party_independence_declared
		    OR NEW.stage5_dependency_satisfied <> OLD.stage5_dependency_satisfied
		    OR NEW.asset_coverage_complete <> OLD.asset_coverage_complete
		    OR NEW.scope_coverage_complete <> OLD.scope_coverage_complete
		    OR NEW.methodology_coverage_complete <> OLD.methodology_coverage_complete
		    OR NEW.no_unaccepted_high_or_critical_findings <> OLD.no_unaccepted_high_or_critical_findings
		    OR NEW.eligible_for_human_gate_review <> OLD.eligible_for_human_gate_review
		    OR NEW.cryptographic_signatures_verified <> OLD.cryptographic_signatures_verified
		    OR NEW.external_authority_verification_required <> OLD.external_authority_verification_required
		    OR NEW.created_by IS NOT OLD.created_by OR NEW.created_at <> OLD.created_at OR NEW.version <> OLD.version + 1
		    OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected')
		    OR (NEW.state = 'approved' AND (
		      NEW.eligible_for_human_gate_review <> 1 OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL
		      OR 3 <> (SELECT count(*) FROM stage6_penetration_approvals
		               WHERE penetration_engagement_id = NEW.id AND decision = 'approved'
		                 AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
		                 AND evidence_sha256 <> 'sha256:' || printf('%064d', 0))
		    ))
		    OR (NEW.state = 'rejected' AND (NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL));
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_assets_insert`,
		`CREATE TRIGGER trg_stage6_penetration_assets_insert BEFORE INSERT ON stage6_penetration_assets BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Penetration asset') WHERE NOT EXISTS (
		    SELECT 1 FROM stage6_penetration_engagements parent
		    JOIN json_each(CAST(parent.receipt AS TEXT), '$.assets') asset
		    WHERE parent.id = NEW.penetration_engagement_id AND parent.operator_tenant_id = NEW.operator_tenant_id
		      AND parent.state = 'recorded' AND NEW.tested = 1
		      AND json_extract(asset.value, '$.assetType') = NEW.asset_type
		      AND json_extract(asset.value, '$.tested') IS NEW.tested
		      AND json_extract(asset.value, '$.artifactDigest') = 'sha256:' || lower(hex(NEW.artifact_sha256))
		  );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_approvals_insert`,
		`CREATE TRIGGER trg_stage6_penetration_approvals_insert BEFORE INSERT ON stage6_penetration_approvals BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 Penetration approval') WHERE
		    NEW.approval_role NOT IN ('engineering', 'product', 'security')
		    OR NEW.decision NOT IN ('approved', 'rejected') OR length(trim(NEW.reason)) NOT BETWEEN 20 AND 2000
		    OR substr(NEW.evidence_reference, 1, 8) <> 'https://'
		    OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		    OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		    OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		    OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		    OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_penetration_engagements parent
		      JOIN stage6_governance_authority_grants authority ON authority.operator_tenant_id = parent.operator_tenant_id
		        AND authority.user_id = NEW.approver_user_id AND authority.authority_key = 'penetration.' || NEW.approval_role
		        AND authority.status = 'active' AND unixepoch(authority.expires_at) > unixepoch('now')
		      JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
		        AND membership.user_id = authority.user_id AND membership.status = 'active'
		        AND membership.role IN ('owner', 'admin', 'security_admin')
		      JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		        AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		      JOIN users governed_user ON governed_user.id = authority.user_id
		        AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		      WHERE parent.id = NEW.penetration_engagement_id AND parent.operator_tenant_id = NEW.operator_tenant_id
		        AND parent.state = 'recorded' AND parent.created_by <> NEW.approver_user_id
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_assets_no_update`,
		`CREATE TRIGGER trg_stage6_penetration_assets_no_update BEFORE UPDATE ON stage6_penetration_assets BEGIN SELECT RAISE(ABORT, 'Stage 6 Penetration evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_assets_no_delete`,
		`CREATE TRIGGER trg_stage6_penetration_assets_no_delete BEFORE DELETE ON stage6_penetration_assets BEGIN SELECT RAISE(ABORT, 'Stage 6 Penetration evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_approvals_no_update`,
		`CREATE TRIGGER trg_stage6_penetration_approvals_no_update BEFORE UPDATE ON stage6_penetration_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 Penetration evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_approvals_no_delete`,
		`CREATE TRIGGER trg_stage6_penetration_approvals_no_delete BEFORE DELETE ON stage6_penetration_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 Penetration evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_penetration_engagements_no_delete`,
		`CREATE TRIGGER trg_stage6_penetration_engagements_no_delete BEFORE DELETE ON stage6_penetration_engagements BEGIN SELECT RAISE(ABORT, 'Stage 6 Penetration evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_penetration_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_penetration_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state IN ('approved', 'deploying', 'observing', 'released')
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release transition requires an approved Penetration engagement with three byte-bound decisions')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_penetration_engagements AS penetration
		     WHERE penetration.candidate_record_id = NEW.id AND penetration.operator_tenant_id = NEW.operator_tenant_id
		       AND penetration.state = 'approved' AND penetration.third_party_independence_declared = 1
		       AND penetration.stage5_dependency_satisfied = 1 AND penetration.asset_coverage_complete = 1
		       AND penetration.scope_coverage_complete = 1 AND penetration.methodology_coverage_complete = 1
		       AND penetration.no_unaccepted_high_or_critical_findings = 1
		       AND penetration.eligible_for_human_gate_review = 1
		       AND penetration.cryptographic_signatures_verified = 0
		       AND penetration.external_authority_verification_required = 1
		       AND 3 = (
		         SELECT count(*) FROM stage6_penetration_approvals AS approval
		         WHERE approval.penetration_engagement_id = penetration.id AND approval.decision = 'approved'
		           AND approval.superseded_at IS NULL AND approval.evidence_sha256 IS NOT NULL
		           AND approval.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		       )
		   );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 Penetration governance safety migration: %w", err)
		}
	}
	return nil
}
