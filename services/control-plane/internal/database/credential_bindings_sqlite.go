package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateCredentialBindingsSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_provider_credentials_workspace_shape_insert`,
		`CREATE TRIGGER trg_provider_credentials_workspace_shape_insert
		 BEFORE INSERT ON provider_credentials
		 WHEN NEW.purpose NOT IN ('provider', 'git', 'registry', 'package')
		   OR NOT (
		     NEW.purpose = 'provider'
		     OR (NEW.purpose = 'git' AND NEW.provider = 'git' AND NEW.credential_type IN ('https_token', 'ssh_key'))
		     OR (NEW.purpose = 'registry' AND NEW.provider = 'oci' AND NEW.credential_type IN ('basic', 'bearer_token'))
		     OR (NEW.purpose = 'package' AND (
		       (NEW.provider = 'npm' AND NEW.credential_type = 'npm_token')
		       OR (NEW.provider = 'pypi' AND NEW.credential_type = 'pypi_token')
		     ))
		   )
		   OR (NEW.purpose <> 'provider' AND (
		     NEW.scope NOT IN ('organization', 'tenant')
		     OR NEW.selector_organization_id IS NOT NULL
		     OR NEW.selector_model IS NOT NULL
		     OR NEW.auto_select_enabled = 1
		   ))
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Workspace Credential shape');
		 END`,
		`DROP TRIGGER IF EXISTS trg_provider_credentials_workspace_shape_update`,
		`CREATE TRIGGER trg_provider_credentials_workspace_shape_update
		 BEFORE UPDATE OF purpose, provider, credential_type, scope, selector_organization_id,
		   selector_model, auto_select_enabled ON provider_credentials
		 WHEN NEW.purpose NOT IN ('provider', 'git', 'registry', 'package')
		   OR NOT (
		     NEW.purpose = 'provider'
		     OR (NEW.purpose = 'git' AND NEW.provider = 'git' AND NEW.credential_type IN ('https_token', 'ssh_key'))
		     OR (NEW.purpose = 'registry' AND NEW.provider = 'oci' AND NEW.credential_type IN ('basic', 'bearer_token'))
		     OR (NEW.purpose = 'package' AND (
		       (NEW.provider = 'npm' AND NEW.credential_type = 'npm_token')
		       OR (NEW.provider = 'pypi' AND NEW.credential_type = 'pypi_token')
		     ))
		   )
		   OR (NEW.purpose <> 'provider' AND (
		     NEW.scope NOT IN ('organization', 'tenant')
		     OR NEW.selector_organization_id IS NOT NULL
		     OR NEW.selector_model IS NOT NULL
		     OR NEW.auto_select_enabled = 1
		   ))
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Workspace Credential shape');
		 END`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_credential_bindings_active_project_selector
		 ON credential_bindings (tenant_id, project_id, binding_kind, selector_value)
		 WHERE project_id IS NOT NULL AND disabled_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_credential_bindings_active_worker_image_target
		 ON credential_bindings (tenant_id, execution_target_id)
		 WHERE execution_target_id IS NOT NULL
		   AND binding_kind = 'worker_image_pull'
		   AND disabled_at IS NULL`,
		`DROP INDEX IF EXISTS uq_credential_bindings_active_target_selector`,
		`CREATE INDEX IF NOT EXISTS idx_credential_bindings_project_lookup
		 ON credential_bindings (tenant_id, project_id, binding_kind, selector_value, id)
		 WHERE project_id IS NOT NULL AND disabled_at IS NULL`,
		`DROP INDEX IF EXISTS idx_credential_bindings_target_lookup`,
		`CREATE INDEX IF NOT EXISTS idx_credential_bindings_credential
		 ON credential_bindings (tenant_id, credential_id, created_at DESC, id)`,
		`CREATE INDEX IF NOT EXISTS idx_credential_bindings_organization
		 ON credential_bindings (tenant_id, organization_id, binding_kind, created_at DESC, id)
		 WHERE organization_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_credential_bindings_project_fk
		 ON credential_bindings (tenant_id, project_id)
		 WHERE project_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_credential_bindings_execution_target_fk
		 ON credential_bindings (tenant_id, execution_target_id)
		 WHERE execution_target_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_credential_bindings_created_by_fk
		 ON credential_bindings (tenant_id, created_by)`,
		`CREATE INDEX IF NOT EXISTS idx_credential_bindings_disabled_by_fk
		 ON credential_bindings (tenant_id, disabled_by)
		 WHERE disabled_by IS NOT NULL`,
		`DROP TRIGGER IF EXISTS trg_credential_bindings_validate_insert`,
		`INSERT INTO credential_bindings (
		   id, tenant_id, organization_id, project_id, credential_id,
		   binding_kind, selector_value, created_by, created_at
		 )
		 SELECT
		   lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-' ||
		   lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(6))),
		   project.tenant_id, project.organization_id, project.id, project.git_credential_id,
		   'git_fetch', project.repository_url, project.created_by, project.updated_at
		 FROM projects AS project
		 WHERE project.git_credential_id IS NOT NULL
		   AND project.repository_url IS NOT NULL
		   AND NOT EXISTS (
		     SELECT 1 FROM credential_bindings AS binding
		     WHERE binding.tenant_id = project.tenant_id
		       AND binding.project_id = project.id
		       AND binding.credential_id = project.git_credential_id
		       AND binding.binding_kind = 'git_fetch'
		       AND binding.selector_value = project.repository_url
		       AND binding.disabled_at IS NULL
		   )`,
		`CREATE TRIGGER trg_credential_bindings_validate_insert
		 BEFORE INSERT ON credential_bindings
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Credential Binding owner shape')
		   WHERE ((NEW.project_id IS NOT NULL) + (NEW.execution_target_id IS NOT NULL)) <> 1
		      OR NEW.binding_kind NOT IN (
		        'git_fetch', 'git_push', 'registry_pull', 'registry_push',
		        'package_read', 'package_publish', 'worker_image_pull'
		      )
		      OR (NEW.binding_kind = 'worker_image_pull' AND NEW.execution_target_id IS NULL)
		      OR (NEW.binding_kind <> 'worker_image_pull' AND NEW.project_id IS NULL)
		      OR NEW.selector_value IS NULL
		      OR length(trim(NEW.selector_value)) NOT BETWEEN 1 AND 2048
		      OR instr(NEW.selector_value, char(0)) > 0
		      OR instr(NEW.selector_value, char(9)) > 0
		      OR instr(NEW.selector_value, char(10)) > 0
		      OR instr(NEW.selector_value, char(13)) > 0
		      OR NOT ((NEW.disabled_at IS NULL AND NEW.disabled_by IS NULL)
		        OR (NEW.disabled_at IS NOT NULL AND NEW.disabled_by IS NOT NULL));

		   SELECT RAISE(ABORT, 'Credential Binding creator must be active')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM tenant_memberships AS membership
		     WHERE membership.tenant_id = NEW.tenant_id
		       AND membership.user_id = NEW.created_by
		       AND membership.status = 'active'
		   );

		   SELECT RAISE(ABORT, 'Credential Binding owner mismatch')
		   WHERE (NEW.project_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1 FROM projects AS project
		     WHERE project.tenant_id = NEW.tenant_id
		       AND project.id = NEW.project_id
		       AND project.organization_id IS NEW.organization_id
		   )) OR (NEW.execution_target_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1 FROM execution_targets AS target
		     WHERE target.tenant_id = NEW.tenant_id
		       AND target.id = NEW.execution_target_id
		       AND target.organization_id IS NEW.organization_id
		   ));

		   SELECT RAISE(ABORT, 'Credential Binding Credential mismatch')
		   WHERE NOT EXISTS (
		     SELECT 1
		     FROM provider_credentials AS credential
		     WHERE credential.tenant_id = NEW.tenant_id
		       AND credential.id = NEW.credential_id
		       AND credential.scope IN ('organization', 'tenant')
		       AND (credential.scope = 'tenant' OR credential.organization_id IS NEW.organization_id)
		       AND credential.revoked_at IS NULL
		       AND (credential.expires_at IS NULL OR credential.expires_at > CURRENT_TIMESTAMP)
		       AND (
		         (NEW.binding_kind IN ('git_fetch', 'git_push') AND credential.purpose = 'git')
		         OR (NEW.binding_kind IN ('registry_pull', 'registry_push', 'worker_image_pull') AND credential.purpose = 'registry')
		         OR (NEW.binding_kind IN ('package_read', 'package_publish') AND credential.purpose = 'package')
		       )
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_credential_bindings_immutable_update`,
		`CREATE TRIGGER trg_credential_bindings_immutable_update
		 BEFORE UPDATE ON credential_bindings
		 BEGIN
		   SELECT RAISE(ABORT, 'Credential Binding identity is immutable')
		   WHERE NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.organization_id IS NOT OLD.organization_id
		      OR NEW.project_id IS NOT OLD.project_id
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.credential_id IS NOT OLD.credential_id
		      OR NEW.binding_kind IS NOT OLD.binding_kind
		      OR NEW.selector_value IS NOT OLD.selector_value
		      OR NEW.created_by IS NOT OLD.created_by
		      OR NEW.created_at IS NOT OLD.created_at
		      OR OLD.disabled_at IS NOT NULL
		      OR NEW.disabled_at IS NULL
		      OR NEW.disabled_by IS NULL;
		   SELECT RAISE(ABORT, 'Credential Binding disabler must be active')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM tenant_memberships AS membership
		     WHERE membership.tenant_id = NEW.tenant_id
		       AND membership.user_id = NEW.disabled_by
		       AND membership.status = 'active'
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_credential_bindings_no_delete`,
		`CREATE TRIGGER trg_credential_bindings_no_delete
		 BEFORE DELETE ON credential_bindings
		 BEGIN
		   SELECT RAISE(ABORT, 'Credential Binding history cannot be deleted');
		 END`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_credential_grants_binding
		 ON execution_credential_grants (tenant_id, execution_id, generation, binding_id)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_credential_grants_execution
		 ON execution_credential_grants (tenant_id, execution_id, generation, id)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_credential_grants_binding
		 ON execution_credential_grants (tenant_id, binding_id, generation, id)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_credential_grants_credential
		 ON execution_credential_grants (tenant_id, credential_id, credential_version, id)`,
		`DROP TRIGGER IF EXISTS trg_execution_credential_grants_validate_insert`,
		`CREATE TRIGGER trg_execution_credential_grants_validate_insert
		 BEFORE INSERT ON execution_credential_grants
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Credential Grant generation is fenced')
		   WHERE NEW.generation <= 0 OR NOT EXISTS (
		     SELECT 1 FROM agent_executions AS execution
		     WHERE execution.tenant_id = NEW.tenant_id
		       AND execution.id = NEW.execution_id
		       AND execution.generation = NEW.generation
		   );
		   SELECT RAISE(ABORT, 'Execution Credential Grant Binding is unavailable')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM credential_bindings AS binding
		     WHERE binding.tenant_id = NEW.tenant_id
		       AND binding.id = NEW.binding_id
		       AND binding.disabled_at IS NULL
		       AND binding.credential_id = NEW.credential_id
		   );
		   SELECT RAISE(ABORT, 'Execution Credential Grant Credential is unavailable')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM provider_credentials AS credential
		     WHERE credential.tenant_id = NEW.tenant_id
		       AND credential.id = NEW.credential_id
		       AND credential.version = NEW.credential_version
		       AND credential.revoked_at IS NULL
		       AND (credential.expires_at IS NULL OR credential.expires_at > CURRENT_TIMESTAMP)
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_credential_grants_no_update`,
		`CREATE TRIGGER trg_execution_credential_grants_no_update
		 BEFORE UPDATE ON execution_credential_grants
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Credential Grants are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_credential_grants_no_delete`,
		`CREATE TRIGGER trg_execution_credential_grants_no_delete
		 BEFORE DELETE ON execution_credential_grants
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Credential Grants are immutable');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Workspace Credential Binding safety migration: %w", err)
		}
	}
	return nil
}
