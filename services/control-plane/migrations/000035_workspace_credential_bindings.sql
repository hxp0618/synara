ALTER TABLE provider_credentials
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_purpose,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_git_shape,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_workspace_shape,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_workspace_selectors;

ALTER TABLE provider_credentials
  ADD CONSTRAINT chk_provider_credentials_purpose
    CHECK (purpose IN ('provider', 'git', 'registry', 'package')),
  ADD CONSTRAINT chk_provider_credentials_workspace_shape
    CHECK (
      purpose = 'provider' OR
      (purpose = 'git' AND provider = 'git' AND credential_type IN ('https_token', 'ssh_key')) OR
      (purpose = 'registry' AND provider = 'oci' AND credential_type IN ('basic', 'bearer_token')) OR
      (purpose = 'package' AND (
        (provider = 'npm' AND credential_type = 'npm_token') OR
        (provider = 'pypi' AND credential_type = 'pypi_token')
      ))
    ),
  ADD CONSTRAINT chk_provider_credentials_workspace_selectors
    CHECK (
      purpose = 'provider' OR
      (scope IN ('organization', 'tenant') AND selector_organization_id IS NULL AND selector_model IS NULL)
    );

CREATE TABLE IF NOT EXISTS credential_bindings (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  organization_id UUID,
  project_id UUID,
  execution_target_id UUID,
  credential_id UUID NOT NULL,
  binding_kind TEXT NOT NULL,
  selector_value TEXT NOT NULL,
  created_by UUID NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  disabled_at TIMESTAMPTZ,
  disabled_by UUID,
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, project_id)
    REFERENCES projects(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, execution_target_id)
    REFERENCES execution_targets(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, credential_id)
    REFERENCES provider_credentials(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, created_by)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, disabled_by)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE RESTRICT,
  CHECK ((project_id IS NOT NULL)::integer + (execution_target_id IS NOT NULL)::integer = 1),
  CHECK (binding_kind IN (
    'git_fetch', 'git_push', 'registry_pull', 'registry_push',
    'package_read', 'package_publish', 'worker_image_pull'
  )),
  CHECK (
    (binding_kind = 'worker_image_pull' AND execution_target_id IS NOT NULL) OR
    (binding_kind <> 'worker_image_pull' AND project_id IS NOT NULL)
  ),
  CHECK (length(btrim(selector_value)) BETWEEN 1 AND 2048),
  CHECK (selector_value !~ '[[:cntrl:]]'),
  CHECK ((disabled_at IS NULL AND disabled_by IS NULL) OR (disabled_at IS NOT NULL AND disabled_by IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_credential_bindings_active_project_selector
  ON credential_bindings (tenant_id, project_id, binding_kind, selector_value)
  WHERE project_id IS NOT NULL AND disabled_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_credential_bindings_active_target_selector
  ON credential_bindings (tenant_id, execution_target_id, binding_kind, selector_value)
  WHERE execution_target_id IS NOT NULL AND disabled_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_credential_bindings_project_lookup
  ON credential_bindings (tenant_id, project_id, binding_kind, selector_value, id)
  WHERE project_id IS NOT NULL AND disabled_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_credential_bindings_target_lookup
  ON credential_bindings (tenant_id, execution_target_id, binding_kind, selector_value, id)
  WHERE execution_target_id IS NOT NULL AND disabled_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_credential_bindings_credential
  ON credential_bindings (tenant_id, credential_id, created_at DESC, id);

CREATE INDEX IF NOT EXISTS idx_credential_bindings_organization
  ON credential_bindings (tenant_id, organization_id, binding_kind, created_at DESC, id)
  WHERE organization_id IS NOT NULL;

INSERT INTO credential_bindings (
  id, tenant_id, organization_id, project_id, credential_id,
  binding_kind, selector_value, created_by, created_at
)
SELECT
  (
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:' || project.git_credential_id::text), 1, 8) || '-' ||
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:' || project.git_credential_id::text), 9, 4) || '-' ||
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:' || project.git_credential_id::text), 13, 4) || '-' ||
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:' || project.git_credential_id::text), 17, 4) || '-' ||
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:' || project.git_credential_id::text), 21, 12)
  )::uuid,
  project.tenant_id,
  project.organization_id,
  project.id,
  project.git_credential_id,
  'git_fetch',
  project.repository_url,
  project.created_by,
  project.updated_at
FROM projects AS project
WHERE project.git_credential_id IS NOT NULL
  AND project.repository_url IS NOT NULL
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION enforce_credential_binding()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  credential provider_credentials%ROWTYPE;
  owner_organization_id UUID;
  expected_purpose TEXT;
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM tenant_memberships AS membership
    WHERE membership.tenant_id = NEW.tenant_id
      AND membership.user_id = NEW.created_by
      AND membership.status = 'active'
  ) THEN
    RAISE EXCEPTION 'Credential Binding creator must be an active Tenant member'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.project_id IS NOT NULL THEN
    SELECT project.organization_id INTO owner_organization_id
    FROM projects AS project
    WHERE project.tenant_id = NEW.tenant_id AND project.id = NEW.project_id;
  ELSE
    SELECT target.organization_id INTO owner_organization_id
    FROM execution_targets AS target
    WHERE target.tenant_id = NEW.tenant_id AND target.id = NEW.execution_target_id;
  END IF;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Credential Binding owner does not exist in the Tenant'
      USING ERRCODE = '23503';
  END IF;
  IF NEW.organization_id IS DISTINCT FROM owner_organization_id THEN
    RAISE EXCEPTION 'Credential Binding Organization does not match its owner'
      USING ERRCODE = '23514';
  END IF;

  SELECT * INTO credential
  FROM provider_credentials
  WHERE tenant_id = NEW.tenant_id AND id = NEW.credential_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Credential Binding Credential does not exist in the Tenant'
      USING ERRCODE = '23503';
  END IF;

  expected_purpose := CASE
    WHEN NEW.binding_kind IN ('git_fetch', 'git_push') THEN 'git'
    WHEN NEW.binding_kind IN ('registry_pull', 'registry_push', 'worker_image_pull') THEN 'registry'
    WHEN NEW.binding_kind IN ('package_read', 'package_publish') THEN 'package'
    ELSE NULL
  END;
  IF credential.purpose <> expected_purpose THEN
    RAISE EXCEPTION 'Credential Binding kind does not match Credential purpose'
      USING ERRCODE = '23514';
  END IF;
  IF credential.scope = 'organization' AND (
    owner_organization_id IS NULL OR credential.organization_id <> owner_organization_id
  ) THEN
    RAISE EXCEPTION 'Organization Credential does not match Credential Binding owner'
      USING ERRCODE = '23514';
  END IF;
  IF credential.scope NOT IN ('organization', 'tenant') THEN
    RAISE EXCEPTION 'Workspace Credentials must use Organization or Tenant scope'
      USING ERRCODE = '23514';
  END IF;
  IF credential.revoked_at IS NOT NULL OR
     (credential.expires_at IS NOT NULL AND credential.expires_at <= clock_timestamp()) THEN
    RAISE EXCEPTION 'Credential Binding cannot use a revoked or expired Credential'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_credential_bindings_validate ON credential_bindings;
CREATE TRIGGER trg_credential_bindings_validate
BEFORE INSERT ON credential_bindings
FOR EACH ROW EXECUTE FUNCTION enforce_credential_binding();

CREATE OR REPLACE FUNCTION enforce_credential_binding_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.tenant_id <> OLD.tenant_id OR
     NEW.organization_id IS DISTINCT FROM OLD.organization_id OR
     NEW.project_id IS DISTINCT FROM OLD.project_id OR
     NEW.execution_target_id IS DISTINCT FROM OLD.execution_target_id OR
     NEW.credential_id <> OLD.credential_id OR
     NEW.binding_kind <> OLD.binding_kind OR
     NEW.selector_value <> OLD.selector_value OR
     NEW.created_by <> OLD.created_by OR
     NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'Credential Binding identity is immutable'
      USING ERRCODE = '23514';
  END IF;
  IF OLD.disabled_at IS NOT NULL OR
     NEW.disabled_at IS NULL OR NEW.disabled_by IS NULL THEN
    RAISE EXCEPTION 'Credential Binding may only transition once from active to disabled'
      USING ERRCODE = '23514';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM tenant_memberships AS membership
    WHERE membership.tenant_id = NEW.tenant_id
      AND membership.user_id = NEW.disabled_by
      AND membership.status = 'active'
  ) THEN
    RAISE EXCEPTION 'Credential Binding disabler must be an active Tenant member'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_credential_bindings_immutable ON credential_bindings;
CREATE TRIGGER trg_credential_bindings_immutable
BEFORE UPDATE ON credential_bindings
FOR EACH ROW EXECUTE FUNCTION enforce_credential_binding_immutable();

CREATE OR REPLACE FUNCTION reject_credential_binding_delete()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Credential Binding history cannot be deleted'
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_credential_bindings_no_delete ON credential_bindings;
CREATE TRIGGER trg_credential_bindings_no_delete
BEFORE DELETE ON credential_bindings
FOR EACH ROW EXECUTE FUNCTION reject_credential_binding_delete();

CREATE TABLE IF NOT EXISTS execution_credential_grants (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  binding_id UUID NOT NULL,
  credential_id UUID NOT NULL,
  credential_version INTEGER NOT NULL CHECK (credential_version > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, execution_id, generation, binding_id),
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, binding_id)
    REFERENCES credential_bindings(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, credential_id)
    REFERENCES provider_credentials(tenant_id, id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_execution_credential_grants_execution
  ON execution_credential_grants (tenant_id, execution_id, generation, id);

CREATE INDEX IF NOT EXISTS idx_execution_credential_grants_binding
  ON execution_credential_grants (tenant_id, binding_id, generation, id);

CREATE INDEX IF NOT EXISTS idx_execution_credential_grants_credential
  ON execution_credential_grants (tenant_id, credential_id, credential_version, id);

CREATE OR REPLACE FUNCTION enforce_execution_credential_grant()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  execution_generation BIGINT;
  binding credential_bindings%ROWTYPE;
  credential provider_credentials%ROWTYPE;
BEGIN
  SELECT generation INTO execution_generation
  FROM agent_executions
  WHERE tenant_id = NEW.tenant_id AND id = NEW.execution_id
  FOR SHARE;
  IF NOT FOUND OR execution_generation <> NEW.generation THEN
    RAISE EXCEPTION 'Execution Credential Grant generation is fenced'
      USING ERRCODE = '23514';
  END IF;

  SELECT * INTO binding
  FROM credential_bindings
  WHERE tenant_id = NEW.tenant_id AND id = NEW.binding_id
  FOR SHARE;
  IF NOT FOUND OR binding.disabled_at IS NOT NULL OR binding.credential_id <> NEW.credential_id THEN
    RAISE EXCEPTION 'Execution Credential Grant Binding is unavailable'
      USING ERRCODE = '23514';
  END IF;

  SELECT * INTO credential
  FROM provider_credentials
  WHERE tenant_id = NEW.tenant_id AND id = NEW.credential_id
  FOR SHARE;
  IF NOT FOUND OR credential.version <> NEW.credential_version OR credential.revoked_at IS NOT NULL OR
     (credential.expires_at IS NOT NULL AND credential.expires_at <= clock_timestamp()) THEN
    RAISE EXCEPTION 'Execution Credential Grant Credential is unavailable or rotated'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_credential_grants_validate ON execution_credential_grants;
CREATE TRIGGER trg_execution_credential_grants_validate
BEFORE INSERT ON execution_credential_grants
FOR EACH ROW EXECUTE FUNCTION enforce_execution_credential_grant();

CREATE OR REPLACE FUNCTION reject_execution_credential_grant_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Execution Credential Grants are immutable'
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_credential_grants_no_update ON execution_credential_grants;
CREATE TRIGGER trg_execution_credential_grants_no_update
BEFORE UPDATE ON execution_credential_grants
FOR EACH ROW EXECUTE FUNCTION reject_execution_credential_grant_mutation();

DROP TRIGGER IF EXISTS trg_execution_credential_grants_no_delete ON execution_credential_grants;
CREATE TRIGGER trg_execution_credential_grants_no_delete
BEFORE DELETE ON execution_credential_grants
FOR EACH ROW EXECUTE FUNCTION reject_execution_credential_grant_mutation();

COMMENT ON TABLE credential_bindings IS
  'Immutable Project/Execution Target bindings for explicit Git, Registry, and Package Credential stages.';
COMMENT ON TABLE execution_credential_grants IS
  'Generation-fenced immutable Credential snapshots created during Execution Claim.';
