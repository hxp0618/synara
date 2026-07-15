package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateCredentialScopeSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`UPDATE provider_credentials
		 SET scope = CASE WHEN organization_id IS NULL THEN 'tenant' ELSE 'organization' END
		 WHERE scope IS NULL OR scope = ''`,
		`UPDATE provider_credentials
		 SET aad_version = CASE WHEN purpose = 'git' THEN 2 ELSE 1 END
		 WHERE aad_version IS NULL OR aad_version = 0`,
		`UPDATE provider_credentials
		 SET auto_select_enabled = false
		 WHERE auto_select_enabled IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_provider_credentials_user_scope
		 ON provider_credentials (tenant_id, purpose, provider, scope, scope_user_id, id)
		 WHERE scope = 'user' AND auto_select_enabled = 1 AND revoked_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_provider_credentials_organization_scope
		 ON provider_credentials (tenant_id, purpose, provider, scope, organization_id, id)
		 WHERE scope = 'organization' AND auto_select_enabled = 1 AND revoked_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_provider_credentials_tenant_platform_scope
		 ON provider_credentials (tenant_id, purpose, provider, scope, id)
		 WHERE scope IN ('tenant', 'platform') AND auto_select_enabled = 1 AND revoked_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_provider_credential_scope_policy_insert`,
		`CREATE TRIGGER trg_provider_credential_scope_policy_insert
		 BEFORE INSERT ON provider_credential_scope_policies
		 WHEN NOT EXISTS (
		     SELECT 1 FROM tenant_memberships AS membership
		     WHERE membership.tenant_id = NEW.tenant_id
		       AND membership.user_id = NEW.updated_by
		       AND membership.status = 'active'
		   )
		   OR (NEW.platform_credential_auto_select = 1 AND NEW.platform_credentials_enabled = 0)
		   OR (
		     (NEW.platform_credentials_enabled = 1 OR NEW.platform_credential_auto_select = 1)
		     AND NOT EXISTS (
		       SELECT 1
		       FROM tenants AS tenant
		       JOIN platform_installations AS installation
		         ON installation.key = 'control-plane' AND installation.profile = 'enterprise'
		       WHERE tenant.id = NEW.tenant_id
		         AND tenant.deleted_at IS NULL
		         AND tenant.plan_code = 'enterprise'
		     )
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Platform Credentials require an enterprise installation and Tenant entitlement');
		 END`,
		`DROP TRIGGER IF EXISTS trg_provider_credential_scope_policy_update`,
		`CREATE TRIGGER trg_provider_credential_scope_policy_update
		 BEFORE UPDATE OF tenant_id, platform_credentials_enabled, platform_credential_auto_select, updated_by
		 ON provider_credential_scope_policies
		 WHEN NOT EXISTS (
		     SELECT 1 FROM tenant_memberships AS membership
		     WHERE membership.tenant_id = NEW.tenant_id
		       AND membership.user_id = NEW.updated_by
		       AND membership.status = 'active'
		   )
		   OR (NEW.platform_credential_auto_select = 1 AND NEW.platform_credentials_enabled = 0)
		   OR (
		     (NEW.platform_credentials_enabled = 1 OR NEW.platform_credential_auto_select = 1)
		     AND NOT EXISTS (
		       SELECT 1
		       FROM tenants AS tenant
		       JOIN platform_installations AS installation
		         ON installation.key = 'control-plane' AND installation.profile = 'enterprise'
		       WHERE tenant.id = NEW.tenant_id
		         AND tenant.deleted_at IS NULL
		         AND tenant.plan_code = 'enterprise'
		     )
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Platform Credential policy');
		 END`,
		`DROP TRIGGER IF EXISTS trg_provider_credentials_scope_shape_insert`,
		`CREATE TRIGGER trg_provider_credentials_scope_shape_insert
		 BEFORE INSERT ON provider_credentials
		 WHEN NEW.scope IS NULL
		   OR NEW.scope NOT IN ('user', 'organization', 'tenant', 'platform')
		   OR NOT (
		     (NEW.scope = 'user' AND NEW.scope_user_id IS NOT NULL AND NEW.organization_id IS NULL
		       AND NEW.selector_organization_id IS NULL AND NEW.selector_model IS NULL)
		     OR (NEW.scope = 'organization' AND NEW.scope_user_id IS NULL AND NEW.organization_id IS NOT NULL
		       AND NEW.selector_organization_id IS NULL AND NEW.selector_model IS NULL)
		     OR (NEW.scope = 'tenant' AND NEW.scope_user_id IS NULL AND NEW.organization_id IS NULL)
		     OR (NEW.scope = 'platform' AND NEW.scope_user_id IS NULL AND NEW.organization_id IS NULL
		       AND NEW.selector_organization_id IS NULL AND NEW.selector_model IS NULL)
		   )
		   OR (NEW.purpose <> 'provider' AND NEW.scope NOT IN ('organization', 'tenant'))
		   OR (NEW.purpose <> 'provider' AND NEW.auto_select_enabled = 1)
		   OR (NEW.selector_model IS NOT NULL AND (
		     NEW.scope <> 'tenant' OR NEW.purpose <> 'provider' OR
		     length(trim(NEW.selector_model)) NOT BETWEEN 1 AND 200 OR
		     instr(NEW.selector_model, char(10)) > 0 OR instr(NEW.selector_model, char(13)) > 0 OR
		     instr(NEW.selector_model, char(9)) > 0
		   ))
		   OR NOT (
		     (NEW.aad_version = 1 AND NEW.purpose = 'provider') OR
		     (NEW.aad_version = 2 AND NEW.purpose = 'git') OR
		     NEW.aad_version = 3
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Provider Credential scope shape');
		 END`,
		`DROP TRIGGER IF EXISTS trg_provider_credentials_scope_membership_insert`,
		`CREATE TRIGGER trg_provider_credentials_scope_membership_insert
		 BEFORE INSERT ON provider_credentials
		 WHEN (NEW.organization_id IS NOT NULL AND NOT EXISTS (
		   SELECT 1 FROM organizations AS organization
		   WHERE organization.tenant_id = NEW.tenant_id AND organization.id = NEW.organization_id
		 )) OR (NEW.selector_organization_id IS NOT NULL AND NOT EXISTS (
		   SELECT 1 FROM organizations AS organization
		   WHERE organization.tenant_id = NEW.tenant_id AND organization.id = NEW.selector_organization_id
		 )) OR (NEW.scope = 'user' AND NOT EXISTS (
		   SELECT 1 FROM tenant_memberships AS membership
		   WHERE membership.tenant_id = NEW.tenant_id
		     AND membership.user_id = NEW.scope_user_id
		     AND membership.status = 'active'
		 )) OR (NEW.scope = 'platform' AND NOT EXISTS (
		   SELECT 1
		   FROM tenants AS tenant
		   JOIN platform_installations AS installation
		     ON installation.key = 'control-plane' AND installation.profile = 'enterprise'
		   JOIN provider_credential_scope_policies AS policy
		     ON policy.tenant_id = tenant.id
		    AND policy.platform_credentials_enabled = 1
		    AND (NEW.auto_select_enabled = 0 OR policy.platform_credential_auto_select = 1)
		   WHERE tenant.id = NEW.tenant_id
		     AND tenant.deleted_at IS NULL
		     AND tenant.plan_code = 'enterprise'
		 ))
		 BEGIN
		   SELECT RAISE(ABORT, 'Provider Credential scope is not entitled');
		 END`,
		`DROP TRIGGER IF EXISTS trg_provider_credentials_scope_shape_update`,
		`CREATE TRIGGER trg_provider_credentials_scope_shape_update
		 BEFORE UPDATE OF tenant_id, purpose, provider, credential_type, scope, scope_user_id,
		   organization_id, selector_organization_id, selector_model, auto_select_enabled, aad_version,
		   version, encrypted_payload, encrypted_data_key, kms_provider, kms_key_id
		 ON provider_credentials
		 WHEN NEW.tenant_id IS NOT OLD.tenant_id
		   OR NEW.purpose IS NOT OLD.purpose
		   OR NEW.provider IS NOT OLD.provider
		   OR NEW.credential_type IS NOT OLD.credential_type
		   OR NEW.scope IS NOT OLD.scope
		   OR NEW.scope_user_id IS NOT OLD.scope_user_id
		   OR NEW.organization_id IS NOT OLD.organization_id
		   OR NEW.selector_organization_id IS NOT OLD.selector_organization_id
		   OR NEW.selector_model IS NOT OLD.selector_model
		   OR (NEW.aad_version IS NOT OLD.aad_version AND NOT (
		     OLD.aad_version IN (1, 2) AND NEW.aad_version = 3
		   ))
		   OR (NEW.version IS NOT OLD.version AND NOT (
		     NEW.version = OLD.version + 1
		     AND NEW.encrypted_payload IS NOT OLD.encrypted_payload
		     AND NEW.encrypted_data_key IS NOT OLD.encrypted_data_key
		   ))
		   OR (NEW.version IS OLD.version AND (
		     NEW.aad_version IS NOT OLD.aad_version
		     OR NEW.encrypted_payload IS NOT OLD.encrypted_payload
		     OR NEW.encrypted_data_key IS NOT OLD.encrypted_data_key
		     OR NEW.kms_provider IS NOT OLD.kms_provider
		     OR NEW.kms_key_id IS NOT OLD.kms_key_id
		   ))
		 BEGIN
		   SELECT RAISE(ABORT, 'Provider Credential scope identity is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_provider_credential_scope_insert`,
		`CREATE TRIGGER trg_agent_sessions_provider_credential_scope_insert
		 BEFORE INSERT ON agent_sessions
		 WHEN NEW.provider_credential_id IS NOT NULL AND NOT EXISTS (
		   SELECT 1
		   FROM provider_credentials AS credential
		   WHERE credential.tenant_id = NEW.tenant_id
		     AND credential.id = NEW.provider_credential_id
		     AND credential.purpose = 'provider'
		     AND credential.provider = NEW.provider
		     AND credential.revoked_at IS NULL
		     AND (credential.expires_at IS NULL OR credential.expires_at > CURRENT_TIMESTAMP)
		     AND (
		       (credential.scope = 'user' AND credential.scope_user_id = NEW.created_by AND EXISTS (
		         SELECT 1 FROM tenant_memberships AS membership
		         WHERE membership.tenant_id = NEW.tenant_id
		           AND membership.user_id = NEW.created_by
		           AND membership.status = 'active'
		       ))
		       OR (credential.scope = 'organization' AND credential.organization_id = NEW.organization_id)
		       OR (credential.scope = 'tenant'
		         AND (credential.selector_organization_id IS NULL OR credential.selector_organization_id = NEW.organization_id)
		         AND (credential.selector_model IS NULL OR credential.selector_model IS NEW.model))
		       OR (credential.scope = 'platform' AND EXISTS (
		         SELECT 1
		         FROM tenants AS tenant
		         JOIN platform_installations AS installation
		           ON installation.key = 'control-plane' AND installation.profile = 'enterprise'
		         JOIN provider_credential_scope_policies AS policy
		           ON policy.tenant_id = tenant.id AND policy.platform_credentials_enabled = 1
		         WHERE tenant.id = NEW.tenant_id
		           AND tenant.deleted_at IS NULL
		           AND tenant.plan_code = 'enterprise'
		       ))
		     )
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Agent Session Provider Credential violates Credential scope');
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_provider_credential_scope_update`,
		`CREATE TRIGGER trg_agent_sessions_provider_credential_scope_update
		 BEFORE UPDATE OF tenant_id, organization_id, created_by, provider, model, provider_credential_id
		 ON agent_sessions
		 WHEN NEW.provider_credential_id IS NOT NULL AND NOT EXISTS (
		   SELECT 1
		   FROM provider_credentials AS credential
		   WHERE credential.tenant_id = NEW.tenant_id
		     AND credential.id = NEW.provider_credential_id
		     AND credential.purpose = 'provider'
		     AND credential.provider = NEW.provider
		     AND credential.revoked_at IS NULL
		     AND (credential.expires_at IS NULL OR credential.expires_at > CURRENT_TIMESTAMP)
		     AND (
		       (credential.scope = 'user' AND credential.scope_user_id = NEW.created_by AND EXISTS (
		         SELECT 1 FROM tenant_memberships AS membership
		         WHERE membership.tenant_id = NEW.tenant_id
		           AND membership.user_id = NEW.created_by
		           AND membership.status = 'active'
		       ))
		       OR (credential.scope = 'organization' AND credential.organization_id = NEW.organization_id)
		       OR (credential.scope = 'tenant'
		         AND (credential.selector_organization_id IS NULL OR credential.selector_organization_id = NEW.organization_id)
		         AND (credential.selector_model IS NULL OR credential.selector_model IS NEW.model))
		       OR (credential.scope = 'platform' AND EXISTS (
		         SELECT 1
		         FROM tenants AS tenant
		         JOIN platform_installations AS installation
		           ON installation.key = 'control-plane' AND installation.profile = 'enterprise'
		         JOIN provider_credential_scope_policies AS policy
		           ON policy.tenant_id = tenant.id AND policy.platform_credentials_enabled = 1
		         WHERE tenant.id = NEW.tenant_id
		           AND tenant.deleted_at IS NULL
		           AND tenant.plan_code = 'enterprise'
		       ))
		     )
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Agent Session Provider Credential violates Credential scope');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Credential scope safety migration: %w", err)
		}
	}
	return nil
}
