package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateComplianceGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_evidence_reviews_no_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_program_decisions_no_update`,
		`UPDATE stage6_compliance_evidence_reviews
		 SET superseded_at = COALESCE(superseded_at, CURRENT_TIMESTAMP),
		     superseded_reason = COALESCE(superseded_reason, 'Automatically superseded during Migration 000141 because review evidence was not byte-bound.')
		 WHERE evidence_sha256 IS NULL AND superseded_at IS NULL`,
		`UPDATE stage6_compliance_program_decisions
		 SET superseded_at = COALESCE(superseded_at, CURRENT_TIMESTAMP),
		     superseded_reason = COALESCE(superseded_reason, 'Automatically superseded during Migration 000141 because decision evidence was not byte-bound.')
		 WHERE evidence_sha256 IS NULL AND superseded_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_compliance_controls_program_control
		 ON stage6_compliance_controls (program_id, control_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_compliance_evidence_program_evidence
		 ON stage6_compliance_evidence (program_id, evidence_id)`,
		`DROP INDEX IF EXISTS idx_stage6_compliance_evidence_reviews_evidence_record_id`,
		`DROP INDEX IF EXISTS uq_stage6_compliance_reviews_evidence`,
		`DROP INDEX IF EXISTS uq_stage6_compliance_decisions_role`,
		`DROP INDEX IF EXISTS uq_stage6_compliance_decisions_user`,
		`DROP INDEX IF EXISTS uq_stage6_compliance_reviews_evidence_active`,
		`DROP INDEX IF EXISTS uq_stage6_compliance_decisions_role_active`,
		`DROP INDEX IF EXISTS uq_stage6_compliance_decisions_user_active`,
		`CREATE UNIQUE INDEX uq_stage6_compliance_reviews_evidence_active
		 ON stage6_compliance_evidence_reviews (evidence_record_id) WHERE superseded_at IS NULL`,
		`CREATE UNIQUE INDEX uq_stage6_compliance_decisions_role_active
		 ON stage6_compliance_program_decisions (program_id, decision_role) WHERE superseded_at IS NULL`,
		`CREATE UNIQUE INDEX uq_stage6_compliance_decisions_user_active
		 ON stage6_compliance_program_decisions (program_id, decider_user_id) WHERE superseded_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_programs_insert`,
		`CREATE TRIGGER trg_stage6_compliance_programs_insert
		 BEFORE INSERT ON stage6_compliance_programs
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 compliance program')
		   WHERE NEW.framework NOT IN ('soc2_type2', 'iso27001')
		      OR length(trim(NEW.scope_summary)) NOT BETWEEN 20 AND 4000
		      OR NEW.observation_end <= NEW.observation_start
		      OR NEW.evidence_retention_days NOT BETWEEN 365 AND 3650
		      OR NEW.state <> 'draft' OR NEW.version <> 1 OR NEW.record_completed_at IS NOT NULL
		      OR NOT EXISTS (
		        SELECT 1 FROM tenant_memberships AS membership
		        JOIN tenants AS tenant ON tenant.id = membership.tenant_id
		        JOIN users AS actor ON actor.id = membership.user_id
		        WHERE membership.tenant_id = NEW.operator_tenant_id
		          AND membership.user_id = NEW.created_by AND membership.status = 'active'
		          AND membership.role IN ('owner', 'admin')
		          AND tenant.status = 'active' AND tenant.deleted_at IS NULL
		          AND actor.status = 'active' AND actor.deleted_at IS NULL
		      )
		      OR NOT EXISTS (
		        SELECT 1 FROM tenant_memberships AS membership
		        WHERE membership.tenant_id = NEW.operator_tenant_id
		          AND membership.user_id = NEW.executive_sponsor_user_id
		          AND membership.status = 'active' AND membership.role IN ('owner', 'admin')
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_programs_update`,
		`CREATE TRIGGER trg_stage6_compliance_programs_update
		 BEFORE UPDATE ON stage6_compliance_programs
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 compliance program update')
		   WHERE NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		      OR NEW.program_key <> OLD.program_key OR NEW.framework <> OLD.framework
		      OR NEW.scope_version <> OLD.scope_version OR NEW.scope_summary <> OLD.scope_summary
		      OR NEW.executive_sponsor_user_id IS NOT OLD.executive_sponsor_user_id
		      OR NEW.auditor_organization <> OLD.auditor_organization
		      OR NEW.auditor_engagement_reference <> OLD.auditor_engagement_reference
		      OR NEW.observation_start <> OLD.observation_start OR NEW.observation_end <> OLD.observation_end
		      OR NEW.evidence_repository_reference <> OLD.evidence_repository_reference
		      OR NEW.evidence_access_policy_reference <> OLD.evidence_access_policy_reference
		      OR NEW.evidence_retention_days <> OLD.evidence_retention_days
		      OR NEW.vendor_register_reference <> OLD.vendor_register_reference
		      OR NEW.risk_register_reference <> OLD.risk_register_reference
		      OR NEW.created_by IS NOT OLD.created_by OR NEW.created_at <> OLD.created_at
		      OR NEW.version <> OLD.version + 1
		      OR (OLD.state = 'draft' AND NEW.state <> 'ready_for_review')
		      OR (OLD.state = 'ready_for_review' AND NEW.state <> 'record_complete')
		      OR OLD.state = 'record_complete'
		      OR (NEW.state = 'record_complete' AND (
		        NEW.record_completed_at IS NULL
		        OR 7 <> (SELECT count(DISTINCT control_family) FROM stage6_compliance_controls WHERE program_id = NEW.id)
		        OR 4 <> (SELECT count(*) FROM stage6_compliance_program_decisions
		                   WHERE program_id = NEW.id AND decision = 'approved'
		                     AND decision_role IN ('security', 'operations', 'legal_privacy', 'executive')
		                     AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
		                     AND evidence_sha256 <> 'sha256:' || printf('%064d', 0))
		        OR 1 > (SELECT count(*) FROM stage6_compliance_evidence AS evidence
		                JOIN stage6_compliance_evidence_reviews AS review
		                  ON review.evidence_record_id = evidence.id AND review.decision = 'accepted'
		                 AND review.superseded_at IS NULL AND review.evidence_sha256 IS NOT NULL
		                 AND review.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		                WHERE evidence.program_id = NEW.id AND evidence.evidence_type = 'release_manifest')
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_controls_insert`,
		`CREATE TRIGGER trg_stage6_compliance_controls_insert
		 BEFORE INSERT ON stage6_compliance_controls
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 compliance control')
		   WHERE NEW.control_family NOT IN ('logical_access', 'change_release', 'operations', 'data_governance', 'resilience', 'vendor_provider', 'security_testing')
		      OR NEW.cadence NOT IN ('continuous', 'daily', 'monthly', 'quarterly', 'annual', 'per_release', 'per_incident')
		      OR NOT EXISTS (SELECT 1 FROM stage6_compliance_programs AS program
		                     WHERE program.id = NEW.program_id AND program.operator_tenant_id = NEW.operator_tenant_id
		                       AND program.state <> 'record_complete')
		      OR NOT EXISTS (SELECT 1 FROM tenant_memberships AS membership
		                     WHERE membership.tenant_id = NEW.operator_tenant_id
		                       AND membership.user_id = NEW.owner_user_id AND membership.status = 'active'
		                       AND membership.role IN ('owner', 'admin', 'security_admin'))
		      OR NOT EXISTS (SELECT 1 FROM tenant_memberships AS membership
		                     WHERE membership.tenant_id = NEW.operator_tenant_id
		                       AND membership.user_id = NEW.created_by AND membership.status = 'active'
		                       AND membership.role IN ('owner', 'admin'));
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_evidence_insert`,
		`CREATE TRIGGER trg_stage6_compliance_evidence_insert
		 BEFORE INSERT ON stage6_compliance_evidence
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 compliance evidence')
		   WHERE length(NEW.sha256) <> 32 OR NEW.period_end < NEW.period_start
		      OR NEW.evidence_type NOT IN ('release_manifest', 'access_review', 'release', 'incident', 'recovery', 'vendor_review', 'security_test', 'data_governance', 'other')
		      OR NEW.classification NOT IN ('internal', 'confidential', 'restricted')
		      OR NOT EXISTS (SELECT 1 FROM stage6_compliance_controls AS control
		                     JOIN stage6_compliance_programs AS program ON program.id = control.program_id
		                     WHERE control.id = NEW.control_record_id AND control.program_id = NEW.program_id
		                       AND control.operator_tenant_id = NEW.operator_tenant_id AND program.state <> 'record_complete'
		                       AND NEW.retention_until >= datetime(NEW.collected_at, '+' || program.evidence_retention_days || ' days'))
		      OR NOT EXISTS (SELECT 1 FROM tenant_memberships AS membership
		                     WHERE membership.tenant_id = NEW.operator_tenant_id
		                       AND membership.user_id = NEW.submitted_by AND membership.status = 'active'
		                       AND membership.role IN ('owner', 'admin', 'security_admin'));
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_reviews_insert`,
		`CREATE TRIGGER trg_stage6_compliance_reviews_insert
		 BEFORE INSERT ON stage6_compliance_evidence_reviews
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 compliance evidence review')
		   WHERE NEW.decision NOT IN ('accepted', 'rejected')
		      OR NEW.review_role NOT IN ('security', 'operations', 'legal_privacy', 'auditor')
		      OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		      OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		      OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		      OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		      OR NOT EXISTS (SELECT 1 FROM stage6_compliance_evidence AS evidence
		                     JOIN stage6_compliance_programs AS program ON program.id = evidence.program_id
		                     WHERE evidence.id = NEW.evidence_record_id AND evidence.program_id = NEW.program_id
		                       AND evidence.operator_tenant_id = NEW.operator_tenant_id
		                       AND evidence.submitted_by <> NEW.reviewer_user_id AND program.state <> 'record_complete')
		      OR NOT EXISTS (SELECT 1 FROM tenant_memberships AS membership
		                     WHERE membership.tenant_id = NEW.operator_tenant_id
		                       AND membership.user_id = NEW.reviewer_user_id AND membership.status = 'active'
		                       AND membership.role IN ('owner', 'admin', 'security_admin'));
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_decisions_insert`,
		`CREATE TRIGGER trg_stage6_compliance_decisions_insert
		 BEFORE INSERT ON stage6_compliance_program_decisions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 compliance decision')
		   WHERE NEW.decision_role NOT IN ('security', 'operations', 'legal_privacy', 'executive')
		      OR NEW.decision NOT IN ('approved', 'rejected')
		      OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		      OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		      OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		      OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		      OR NOT EXISTS (SELECT 1 FROM stage6_compliance_programs AS program
		                     WHERE program.id = NEW.program_id AND program.operator_tenant_id = NEW.operator_tenant_id
		                       AND program.state = 'ready_for_review' AND program.created_by <> NEW.decider_user_id)
		      OR NOT EXISTS (SELECT 1 FROM tenant_memberships AS membership
		                     WHERE membership.tenant_id = NEW.operator_tenant_id
		                       AND membership.user_id = NEW.decider_user_id AND membership.status = 'active'
		                       AND membership.role IN ('owner', 'admin', 'security_admin'));
		 END`,
	}
	for _, table := range []string{
		"stage6_compliance_controls", "stage6_compliance_evidence",
		"stage6_compliance_evidence_reviews", "stage6_compliance_program_decisions",
	} {
		statements = append(statements,
			fmt.Sprintf(`DROP TRIGGER IF EXISTS trg_%s_no_update`, table),
			fmt.Sprintf(`CREATE TRIGGER trg_%s_no_update BEFORE UPDATE ON %s BEGIN SELECT RAISE(ABORT, 'Stage 6 compliance record is append-only'); END`, table, table),
			fmt.Sprintf(`DROP TRIGGER IF EXISTS trg_%s_no_delete`, table),
			fmt.Sprintf(`CREATE TRIGGER trg_%s_no_delete BEFORE DELETE ON %s BEGIN SELECT RAISE(ABORT, 'Stage 6 compliance record is append-only'); END`, table, table),
		)
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 compliance governance safety migration: %w", err)
		}
	}
	return nil
}
