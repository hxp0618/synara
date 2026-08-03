package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateProviderCommercialAuthorizationSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_provider_commercial_authorizations_insert`,
		`DROP TRIGGER IF EXISTS trg_provider_commercial_authorizations_update`,
		`UPDATE provider_commercial_authorizations
		 SET state = 'revoked', version = version + 1,
		     revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP), updated_at = CURRENT_TIMESTAMP
		 WHERE state NOT IN ('rejected', 'revoked')
		   AND (
		     terms_sha256 IS NULL OR agreement_sha256 IS NULL OR dpa_sha256 IS NULL OR termination_runbook_sha256 IS NULL
		     OR EXISTS (
		       SELECT 1 FROM provider_commercial_authorization_approvals AS approval
		       WHERE approval.authorization_id = provider_commercial_authorizations.id
		         AND approval.evidence_sha256 IS NULL
		     )
		   )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_provider_commercial_approval_role
		 ON provider_commercial_authorization_approvals (authorization_id, approval_role)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_provider_commercial_approval_user
		 ON provider_commercial_authorization_approvals (authorization_id, approver_user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_provider_commercial_authorization_lookup
		 ON provider_commercial_authorizations (operator_tenant_id, provider, state, review_expires_at DESC)`,
		`CREATE TRIGGER trg_provider_commercial_authorizations_insert
		 BEFORE INSERT ON provider_commercial_authorizations
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Provider commercial authorization')
		   WHERE NEW.provider NOT IN ('codex', 'claude_agent')
		      OR NEW.credential_mode NOT IN ('customer_byok', 'platform_managed')
		      OR NEW.data_use_policy NOT IN ('no_training', 'tenant_explicit_opt_in')
		      OR json_array_length(NEW.allowed_credential_scopes) NOT BETWEEN 1 AND 4
		      OR json_array_length(NEW.allowed_regions) NOT BETWEEN 1 AND 32
		      OR EXISTS (SELECT 1 FROM json_each(NEW.allowed_credential_scopes) WHERE value NOT IN ('user', 'organization', 'tenant', 'platform'))
		      OR EXISTS (SELECT value FROM json_each(NEW.allowed_credential_scopes) GROUP BY value HAVING count(*) > 1)
		      OR EXISTS (SELECT value FROM json_each(NEW.allowed_regions) GROUP BY value HAVING count(*) > 1)
		      OR NEW.state <> 'draft' OR NEW.version <> 1
		      OR NEW.activated_at IS NOT NULL OR NEW.rejected_at IS NOT NULL OR NEW.revoked_at IS NOT NULL
		      OR NEW.review_expires_at <= NEW.created_at
		      OR NEW.review_expires_at > datetime(NEW.created_at, '+180 days')
		      OR NEW.terms_sha256 IS NULL OR length(NEW.terms_sha256) <> 71
		      OR substr(NEW.terms_sha256, 1, 7) <> 'sha256:' OR substr(NEW.terms_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR substr(NEW.terms_sha256, 8) = printf('%064d', 0)
		      OR NEW.agreement_sha256 IS NULL OR length(NEW.agreement_sha256) <> 71
		      OR substr(NEW.agreement_sha256, 1, 7) <> 'sha256:' OR substr(NEW.agreement_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR substr(NEW.agreement_sha256, 8) = printf('%064d', 0)
		      OR NEW.dpa_sha256 IS NULL OR length(NEW.dpa_sha256) <> 71
		      OR substr(NEW.dpa_sha256, 1, 7) <> 'sha256:' OR substr(NEW.dpa_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR substr(NEW.dpa_sha256, 8) = printf('%064d', 0)
		      OR NEW.termination_runbook_sha256 IS NULL OR length(NEW.termination_runbook_sha256) <> 71
		      OR substr(NEW.termination_runbook_sha256, 1, 7) <> 'sha256:' OR substr(NEW.termination_runbook_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR substr(NEW.termination_runbook_sha256, 8) = printf('%064d', 0)
		      OR NOT EXISTS (SELECT 1 FROM tenant_memberships AS membership
		                     JOIN tenants AS tenant ON tenant.id = membership.tenant_id
		                     JOIN users AS actor ON actor.id = membership.user_id
		                     WHERE membership.tenant_id = NEW.operator_tenant_id
		                       AND membership.user_id = NEW.created_by AND membership.status = 'active'
		                       AND membership.role IN ('owner', 'admin')
		                       AND tenant.status = 'active' AND tenant.deleted_at IS NULL
		                       AND actor.status = 'active' AND actor.deleted_at IS NULL);
		 END`,
		`CREATE TRIGGER trg_provider_commercial_authorizations_update
		 BEFORE UPDATE ON provider_commercial_authorizations
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Provider commercial authorization update')
		   WHERE NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		      OR NEW.authorization_key <> OLD.authorization_key OR NEW.provider <> OLD.provider
		      OR NEW.provider_product <> OLD.provider_product OR NEW.account_type <> OLD.account_type
		      OR NEW.contracting_entity <> OLD.contracting_entity OR NEW.credential_mode <> OLD.credential_mode
		      OR NEW.allowed_credential_scopes <> OLD.allowed_credential_scopes OR NEW.allowed_regions <> OLD.allowed_regions
		      OR NEW.data_use_policy <> OLD.data_use_policy OR NEW.retention_policy <> OLD.retention_policy
		      OR NEW.terms_effective_at <> OLD.terms_effective_at OR NEW.terms_reference <> OLD.terms_reference
		      OR NEW.terms_sha256 IS NOT OLD.terms_sha256
		      OR NEW.agreement_reference <> OLD.agreement_reference OR NEW.agreement_sha256 IS NOT OLD.agreement_sha256
		      OR NEW.dpa_reference <> OLD.dpa_reference OR NEW.dpa_sha256 IS NOT OLD.dpa_sha256
		      OR NEW.prohibited_use_summary <> OLD.prohibited_use_summary
		      OR NEW.termination_runbook_reference <> OLD.termination_runbook_reference
		      OR NEW.termination_runbook_sha256 IS NOT OLD.termination_runbook_sha256
		      OR NEW.review_expires_at <> OLD.review_expires_at OR NEW.created_by IS NOT OLD.created_by
		      OR NEW.created_at <> OLD.created_at OR NEW.version <> OLD.version + 1
		      OR (OLD.state = 'draft' AND NEW.state <> 'ready_for_review')
		      OR (OLD.state = 'ready_for_review' AND NEW.state NOT IN ('active', 'rejected'))
		      OR (OLD.state = 'active' AND NEW.state <> 'revoked')
		      OR OLD.state IN ('rejected', 'revoked')
		      OR (NEW.state = 'active' AND (
		        NEW.activated_at IS NULL OR NEW.review_expires_at <= NEW.activated_at
		        OR NEW.terms_sha256 IS NULL OR NEW.agreement_sha256 IS NULL
		        OR NEW.dpa_sha256 IS NULL OR NEW.termination_runbook_sha256 IS NULL
		        OR 4 <> (SELECT count(*) FROM provider_commercial_authorization_approvals
		                   WHERE authorization_id = NEW.id AND decision = 'approved'
		                     AND approval_role IN ('legal', 'privacy', 'security', 'product')
		                     AND evidence_sha256 IS NOT NULL)
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_provider_commercial_authorization_approvals_insert`,
		`CREATE TRIGGER trg_provider_commercial_authorization_approvals_insert
		 BEFORE INSERT ON provider_commercial_authorization_approvals
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Provider commercial authorization approval')
		   WHERE NEW.approval_role NOT IN ('legal', 'privacy', 'security', 'product')
		      OR NEW.decision NOT IN ('approved', 'rejected')
		      OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		      OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:' OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR substr(NEW.evidence_sha256, 8) = printf('%064d', 0)
		      OR NOT EXISTS (SELECT 1 FROM provider_commercial_authorizations AS authorization
		                     JOIN tenant_memberships AS membership
		                       ON membership.tenant_id = authorization.operator_tenant_id
		                      AND membership.user_id = NEW.approver_user_id
		                      AND membership.status = 'active'
		                      AND membership.role IN ('owner', 'admin', 'security_admin')
		                     WHERE authorization.id = NEW.authorization_id
		                       AND authorization.operator_tenant_id = NEW.operator_tenant_id
		                       AND authorization.state = 'ready_for_review'
		                       AND authorization.created_by <> NEW.approver_user_id
		                       AND authorization.review_expires_at > NEW.created_at);
		 END`,
		`DROP TRIGGER IF EXISTS trg_provider_commercial_authorization_approvals_no_update`,
		`CREATE TRIGGER trg_provider_commercial_authorization_approvals_no_update
		 BEFORE UPDATE ON provider_commercial_authorization_approvals
		 BEGIN SELECT RAISE(ABORT, 'Provider commercial authorization approvals are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_provider_commercial_authorization_approvals_no_delete`,
		`CREATE TRIGGER trg_provider_commercial_authorization_approvals_no_delete
		 BEFORE DELETE ON provider_commercial_authorization_approvals
		 BEGIN SELECT RAISE(ABORT, 'Provider commercial authorization approvals are immutable'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Provider commercial authorization safety migration: %w", err)
		}
	}
	return nil
}
