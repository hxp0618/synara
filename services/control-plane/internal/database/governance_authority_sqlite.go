package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateGovernanceAuthoritySQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_governance_authority_update`,
		`UPDATE stage6_governance_authority_grants
		 SET status = 'revoked', version = version + 1,
		     revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP),
		     revoked_by = COALESCE(revoked_by, granted_by),
		     revocation_reason = COALESCE(
		       revocation_reason,
		       'Automatically revoked during Migration 000140 because authority evidence was not byte-bound.'
		     ),
		     updated_at = CURRENT_TIMESTAMP
		 WHERE status = 'active' AND evidence_sha256 IS NULL`,
		`UPDATE stage6_governance_authority_grants
		 SET status = 'revoked', version = version + 1,
		     revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP),
		     revoked_by = COALESCE(revoked_by, granted_by),
		     revocation_reason = COALESCE(
		       revocation_reason,
		       'Automatically revoked during Migration 000160 because internal self-hosted products do not support billing exercises.'
		     ),
		     updated_at = CURRENT_TIMESTAMP
		 WHERE status = 'active' AND authority_key LIKE 'billing_exercise.%'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_governance_authority_active
		 ON stage6_governance_authority_grants (operator_tenant_id, user_id, authority_key)
		 WHERE status = 'active'`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_governance_authority_lookup
		 ON stage6_governance_authority_grants (operator_tenant_id, user_id, authority_key, status, expires_at DESC)`,
		`DROP TRIGGER IF EXISTS trg_stage6_governance_authority_insert`,
		`CREATE TRIGGER trg_stage6_governance_authority_insert
		 BEFORE INSERT ON stage6_governance_authority_grants
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 governance authority grant')
		   WHERE NEW.authority_key NOT IN (
		     'release.engineering', 'release.operations', 'release.security', 'release.product', 'release.privacy_legal',
		     'compliance.security', 'compliance.operations', 'compliance.legal_privacy', 'compliance.executive',
		     'compliance.evidence.security', 'compliance.evidence.operations', 'compliance.evidence.legal_privacy', 'compliance.evidence.auditor',
		     'provider_commercial.legal', 'provider_commercial.privacy', 'provider_commercial.security', 'provider_commercial.product',
		     'recovery.database', 'recovery.kms', 'recovery.operations', 'recovery.security', 'recovery.storage',
		     'penetration.engineering', 'penetration.product', 'penetration.security',
		     'capacity.engineering', 'capacity.operations',
		     'incident_exercise.operations', 'incident_exercise.communications',
		     'operations_exercise.operations', 'operations_exercise.security',
		     'internal_cost.operations', 'internal_cost.owner'
		   ) OR NEW.status <> 'active' OR NEW.version <> 1
		      OR NEW.user_id = NEW.granted_by
		      OR NEW.evidence_sha256 IS NULL
		      OR length(NEW.evidence_sha256) <> 71
		      OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		      OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		      OR NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revocation_reason IS NOT NULL
		      OR abs((julianday(NEW.created_at) - julianday('now')) * 86400.0) > 300
		      OR julianday(NEW.expires_at) <= julianday(NEW.created_at)
		      OR julianday(NEW.expires_at) > julianday(NEW.created_at, '+366 days')
		      OR julianday(NEW.expires_at) > julianday('now', '+366 days')
		      OR NOT EXISTS (
		        SELECT 1 FROM tenant_memberships AS membership
		        JOIN tenants AS tenant ON tenant.id = membership.tenant_id
		        JOIN users AS target_user ON target_user.id = membership.user_id
		        WHERE membership.tenant_id = NEW.operator_tenant_id AND membership.user_id = NEW.user_id
		          AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
		          AND tenant.status = 'active' AND tenant.deleted_at IS NULL
		          AND target_user.status = 'active' AND target_user.deleted_at IS NULL
		      ) OR NOT EXISTS (
		        SELECT 1 FROM tenant_memberships AS membership
		        JOIN users AS grantor ON grantor.id = membership.user_id
		        WHERE membership.tenant_id = NEW.operator_tenant_id AND membership.user_id = NEW.granted_by
		          AND membership.status = 'active' AND membership.role = 'owner'
		          AND grantor.status = 'active' AND grantor.deleted_at IS NULL
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_governance_authority_update`,
		`CREATE TRIGGER trg_stage6_governance_authority_update
		 BEFORE UPDATE ON stage6_governance_authority_grants
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 governance authority revocation')
		   WHERE NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		      OR NEW.user_id IS NOT OLD.user_id OR NEW.authority_key <> OLD.authority_key
		      OR NEW.expires_at <> OLD.expires_at OR NEW.granted_by IS NOT OLD.granted_by
		      OR NEW.reason <> OLD.reason OR NEW.evidence_reference <> OLD.evidence_reference
		      OR NEW.evidence_sha256 IS NOT OLD.evidence_sha256
		      OR NEW.created_at <> OLD.created_at OR OLD.status <> 'active' OR NEW.status <> 'revoked'
		      OR NEW.version <> OLD.version + 1 OR NEW.revoked_at IS NULL OR NEW.revoked_by IS NULL
		      OR NEW.revocation_reason IS NULL OR NEW.revoked_by = OLD.user_id
		      OR NOT EXISTS (
		        SELECT 1 FROM tenant_memberships AS membership
		        JOIN users AS revoker ON revoker.id = membership.user_id
		        WHERE membership.tenant_id = OLD.operator_tenant_id AND membership.user_id = NEW.revoked_by
		          AND membership.status = 'active' AND membership.role = 'owner'
		          AND revoker.status = 'active' AND revoker.deleted_at IS NULL
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_governance_authority_delete`,
		`CREATE TRIGGER trg_stage6_governance_authority_delete
		 BEFORE DELETE ON stage6_governance_authority_grants
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 governance authority history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_approval_authority`,
		`CREATE TRIGGER trg_stage6_release_approval_authority
		 BEFORE INSERT ON stage6_release_approvals
		 BEGIN
		   SELECT RAISE(ABORT, 'matching active Stage 6 governance authority is required')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_governance_authority_grants AS authority
		     JOIN tenant_memberships AS membership ON membership.tenant_id = authority.operator_tenant_id AND membership.user_id = authority.user_id
		     JOIN tenants AS operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		     JOIN users AS governed_user ON governed_user.id = authority.user_id
		     WHERE authority.operator_tenant_id = NEW.operator_tenant_id AND authority.user_id = NEW.approver_user_id
		       AND authority.authority_key = 'release.' || NEW.approval_role AND authority.status = 'active'
		       AND authority.evidence_sha256 IS NOT NULL
		       AND authority.expires_at > CURRENT_TIMESTAMP AND membership.status = 'active'
		       AND membership.role IN ('owner', 'admin', 'security_admin')
		       AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		       AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_decision_authority`,
		`CREATE TRIGGER trg_stage6_compliance_decision_authority
		 BEFORE INSERT ON stage6_compliance_program_decisions
		 BEGIN
		   SELECT RAISE(ABORT, 'matching active Stage 6 governance authority is required')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_governance_authority_grants AS authority
		     JOIN tenant_memberships AS membership ON membership.tenant_id = authority.operator_tenant_id AND membership.user_id = authority.user_id
		     JOIN tenants AS operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		     JOIN users AS governed_user ON governed_user.id = authority.user_id
		     WHERE authority.operator_tenant_id = NEW.operator_tenant_id AND authority.user_id = NEW.decider_user_id
		       AND authority.authority_key = 'compliance.' || NEW.decision_role AND authority.status = 'active'
		       AND authority.evidence_sha256 IS NOT NULL
		       AND authority.expires_at > CURRENT_TIMESTAMP AND membership.status = 'active'
		       AND membership.role IN ('owner', 'admin', 'security_admin')
		       AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		       AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_compliance_evidence_review_authority`,
		`CREATE TRIGGER trg_stage6_compliance_evidence_review_authority
		 BEFORE INSERT ON stage6_compliance_evidence_reviews
		 BEGIN
		   SELECT RAISE(ABORT, 'matching active Stage 6 governance authority is required')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_governance_authority_grants AS authority
		     JOIN tenant_memberships AS membership ON membership.tenant_id = authority.operator_tenant_id AND membership.user_id = authority.user_id
		     JOIN tenants AS operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		     JOIN users AS governed_user ON governed_user.id = authority.user_id
		     WHERE authority.operator_tenant_id = NEW.operator_tenant_id AND authority.user_id = NEW.reviewer_user_id
		       AND authority.authority_key = 'compliance.evidence.' || NEW.review_role AND authority.status = 'active'
		       AND authority.evidence_sha256 IS NOT NULL
		       AND authority.expires_at > CURRENT_TIMESTAMP AND membership.status = 'active'
		       AND membership.role IN ('owner', 'admin', 'security_admin')
		       AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		       AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_provider_commercial_approval_authority`,
		`CREATE TRIGGER trg_provider_commercial_approval_authority
		 BEFORE INSERT ON provider_commercial_authorization_approvals
		 BEGIN
		   SELECT RAISE(ABORT, 'matching active Stage 6 governance authority is required')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_governance_authority_grants AS authority
		     JOIN tenant_memberships AS membership ON membership.tenant_id = authority.operator_tenant_id AND membership.user_id = authority.user_id
		     JOIN tenants AS operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		     JOIN users AS governed_user ON governed_user.id = authority.user_id
		     WHERE authority.operator_tenant_id = NEW.operator_tenant_id AND authority.user_id = NEW.approver_user_id
		       AND authority.authority_key = 'provider_commercial.' || NEW.approval_role AND authority.status = 'active'
		       AND authority.evidence_sha256 IS NOT NULL
		       AND authority.expires_at > CURRENT_TIMESTAMP AND membership.status = 'active'
		       AND membership.role IN ('owner', 'admin', 'security_admin')
		       AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		       AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		   );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 governance authority safety migration: %w", err)
		}
	}
	return nil
}
