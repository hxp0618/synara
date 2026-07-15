ALTER TABLE provider_credentials
  ADD COLUMN IF NOT EXISTS scope TEXT,
  ADD COLUMN IF NOT EXISTS scope_user_id UUID,
  ADD COLUMN IF NOT EXISTS selector_organization_id UUID,
  ADD COLUMN IF NOT EXISTS selector_model TEXT,
  ADD COLUMN IF NOT EXISTS auto_select_enabled BOOLEAN,
  ADD COLUMN IF NOT EXISTS aad_version INTEGER;

UPDATE provider_credentials
SET scope = CASE WHEN organization_id IS NULL THEN 'tenant' ELSE 'organization' END
WHERE scope IS NULL;

UPDATE provider_credentials
SET aad_version = CASE WHEN purpose = 'git' THEN 2 ELSE 1 END
WHERE aad_version IS NULL;

UPDATE provider_credentials
SET auto_select_enabled = false
WHERE auto_select_enabled IS NULL;

ALTER TABLE provider_credentials
  ALTER COLUMN scope SET DEFAULT 'tenant',
  ALTER COLUMN scope SET NOT NULL,
  ALTER COLUMN auto_select_enabled SET DEFAULT false,
  ALTER COLUMN auto_select_enabled SET NOT NULL,
  ALTER COLUMN aad_version SET DEFAULT 3,
  ALTER COLUMN aad_version SET NOT NULL;

ALTER TABLE provider_credentials
  DROP CONSTRAINT IF EXISTS fk_provider_credentials_scope_user,
  DROP CONSTRAINT IF EXISTS fk_provider_credentials_selector_organization,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_scope,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_scope_shape,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_scope_purpose,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_selector_model,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_auto_select,
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_aad_version;

ALTER TABLE provider_credentials
  ADD CONSTRAINT fk_provider_credentials_scope_user
    FOREIGN KEY (tenant_id, scope_user_id)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE RESTRICT,
  ADD CONSTRAINT fk_provider_credentials_selector_organization
    FOREIGN KEY (tenant_id, selector_organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_provider_credentials_scope
    CHECK (scope IN ('user', 'organization', 'tenant', 'platform')),
  ADD CONSTRAINT chk_provider_credentials_scope_shape
    CHECK (
      (scope = 'user' AND scope_user_id IS NOT NULL AND organization_id IS NULL AND
        selector_organization_id IS NULL AND selector_model IS NULL) OR
      (scope = 'organization' AND scope_user_id IS NULL AND organization_id IS NOT NULL AND
        selector_organization_id IS NULL AND selector_model IS NULL) OR
      (scope = 'tenant' AND scope_user_id IS NULL AND organization_id IS NULL) OR
      (scope = 'platform' AND scope_user_id IS NULL AND organization_id IS NULL AND
        selector_organization_id IS NULL AND selector_model IS NULL)
    ),
  ADD CONSTRAINT chk_provider_credentials_scope_purpose
    CHECK (purpose = 'provider' OR scope IN ('organization', 'tenant')),
  ADD CONSTRAINT chk_provider_credentials_selector_model
    CHECK (
      selector_model IS NULL OR
      (scope = 'tenant' AND purpose = 'provider' AND
        length(btrim(selector_model)) BETWEEN 1 AND 200 AND
        selector_model !~ E'[\\r\\n\\t]')
    ),
  ADD CONSTRAINT chk_provider_credentials_auto_select
    CHECK (purpose = 'provider' OR NOT auto_select_enabled),
  ADD CONSTRAINT chk_provider_credentials_aad_version
    CHECK (
      (aad_version = 1 AND purpose = 'provider') OR
      (aad_version = 2 AND purpose = 'git') OR
      aad_version = 3
    );

CREATE INDEX IF NOT EXISTS idx_provider_credentials_user_scope
  ON provider_credentials (tenant_id, purpose, provider, scope, scope_user_id, id)
  WHERE scope = 'user' AND auto_select_enabled AND revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_provider_credentials_organization_scope
  ON provider_credentials (tenant_id, purpose, provider, scope, organization_id, id)
  WHERE scope = 'organization' AND auto_select_enabled AND revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_provider_credentials_tenant_platform_scope
  ON provider_credentials (tenant_id, purpose, provider, scope, id)
  WHERE scope IN ('tenant', 'platform') AND auto_select_enabled AND revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS provider_credential_scope_policies (
  tenant_id UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  platform_credentials_enabled BOOLEAN NOT NULL DEFAULT false,
  platform_credential_auto_select BOOLEAN NOT NULL DEFAULT false,
  updated_by UUID NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, updated_by)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE RESTRICT,
  CHECK (NOT platform_credential_auto_select OR platform_credentials_enabled)
);

DROP TRIGGER IF EXISTS trg_provider_credential_scope_policies_updated_at
ON provider_credential_scope_policies;
CREATE TRIGGER trg_provider_credential_scope_policies_updated_at
BEFORE UPDATE ON provider_credential_scope_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE OR REPLACE FUNCTION provider_credential_platform_policy_enabled(
  requested_tenant_id UUID,
  require_auto_select BOOLEAN
)
RETURNS BOOLEAN
LANGUAGE sql
STABLE
AS $$
  SELECT EXISTS (
    SELECT 1
    FROM tenants AS tenant
    JOIN platform_installations AS installation
      ON installation.key = 'control-plane'
     AND installation.profile = 'enterprise'
    JOIN provider_credential_scope_policies AS policy
      ON policy.tenant_id = tenant.id
     AND policy.platform_credentials_enabled
     AND (NOT require_auto_select OR policy.platform_credential_auto_select)
    WHERE tenant.id = requested_tenant_id
      AND tenant.deleted_at IS NULL
      AND tenant.plan_code = 'enterprise'
  );
$$;

CREATE OR REPLACE FUNCTION enforce_provider_credential_scope_policy()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM tenant_memberships AS membership
    WHERE membership.tenant_id = NEW.tenant_id
      AND membership.user_id = NEW.updated_by
      AND membership.status = 'active'
  ) THEN
    RAISE EXCEPTION 'Provider Credential scope policy updater must be an active Tenant member'
      USING ERRCODE = '23514';
  END IF;
  IF (NEW.platform_credentials_enabled OR NEW.platform_credential_auto_select) AND
     NOT EXISTS (
       SELECT 1
       FROM tenants AS tenant
       JOIN platform_installations AS installation
         ON installation.key = 'control-plane'
        AND installation.profile = 'enterprise'
       WHERE tenant.id = NEW.tenant_id
         AND tenant.deleted_at IS NULL
         AND tenant.plan_code = 'enterprise'
     ) THEN
    RAISE EXCEPTION 'Platform Credentials require an enterprise installation and Tenant entitlement'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_provider_credential_scope_policy
ON provider_credential_scope_policies;
CREATE TRIGGER trg_provider_credential_scope_policy
BEFORE INSERT OR UPDATE OF tenant_id, platform_credentials_enabled, platform_credential_auto_select, updated_by
ON provider_credential_scope_policies
FOR EACH ROW EXECUTE FUNCTION enforce_provider_credential_scope_policy();

CREATE OR REPLACE FUNCTION enforce_provider_credential_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.scope = 'user' AND NOT EXISTS (
    SELECT 1
    FROM tenant_memberships AS membership
    WHERE membership.tenant_id = NEW.tenant_id
      AND membership.user_id = NEW.scope_user_id
      AND membership.status = 'active'
  ) THEN
    RAISE EXCEPTION 'User Credential scope requires an active Tenant member'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.scope = 'platform' AND
     NOT provider_credential_platform_policy_enabled(NEW.tenant_id, NEW.auto_select_enabled) THEN
    IF TG_OP = 'UPDATE' AND OLD.scope = 'platform' AND
       OLD.auto_select_enabled AND NOT NEW.auto_select_enabled THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'Platform Credential requires enterprise entitlement and explicit Tenant policy'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_provider_credentials_scope
ON provider_credentials;
CREATE TRIGGER trg_provider_credentials_scope
BEFORE INSERT OR UPDATE OF tenant_id, scope, scope_user_id, organization_id,
  selector_organization_id, selector_model, auto_select_enabled
ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION enforce_provider_credential_scope();

CREATE OR REPLACE FUNCTION reject_credential_identity_change()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.tenant_id <> OLD.tenant_id OR
     NEW.purpose <> OLD.purpose OR
     NEW.provider <> OLD.provider OR
     NEW.credential_type <> OLD.credential_type OR
     NEW.scope <> OLD.scope OR
     NEW.scope_user_id IS DISTINCT FROM OLD.scope_user_id OR
     NEW.organization_id IS DISTINCT FROM OLD.organization_id OR
     NEW.selector_organization_id IS DISTINCT FROM OLD.selector_organization_id OR
     NEW.selector_model IS DISTINCT FROM OLD.selector_model THEN
    RAISE EXCEPTION 'credential tenant, purpose, provider, type, and scope identity are immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.aad_version <> OLD.aad_version AND NOT (
    OLD.aad_version IN (1, 2) AND NEW.aad_version = 3
  ) THEN
    RAISE EXCEPTION 'credential AAD version may only upgrade from legacy v1/v2 to v3 during rotation'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version AND NOT (
    NEW.version = OLD.version + 1 AND
    NEW.encrypted_payload IS DISTINCT FROM OLD.encrypted_payload AND
    NEW.encrypted_data_key IS DISTINCT FROM OLD.encrypted_data_key
  ) THEN
    RAISE EXCEPTION 'credential rotation must advance one version with new ciphertext and wrapped data key'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.version = OLD.version AND (
    NEW.aad_version <> OLD.aad_version OR
    NEW.encrypted_payload IS DISTINCT FROM OLD.encrypted_payload OR
    NEW.encrypted_data_key IS DISTINCT FROM OLD.encrypted_data_key OR
    NEW.kms_provider <> OLD.kms_provider OR
    NEW.kms_key_id <> OLD.kms_key_id
  ) THEN
    RAISE EXCEPTION 'credential envelope cannot change without rotation'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_provider_credentials_identity_immutable ON provider_credentials;
CREATE TRIGGER trg_provider_credentials_identity_immutable
BEFORE UPDATE OF tenant_id, purpose, provider, credential_type, scope, scope_user_id,
  organization_id, selector_organization_id, selector_model, aad_version, version,
  encrypted_payload, encrypted_data_key, kms_provider, kms_key_id
ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION reject_credential_identity_change();

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM agent_sessions AS session
    JOIN provider_credentials AS credential
      ON credential.tenant_id = session.tenant_id
     AND credential.id = session.provider_credential_id
    WHERE session.provider_credential_id IS NOT NULL
      AND (
        credential.purpose <> 'provider' OR
        credential.provider <> session.provider OR
        (credential.scope = 'user' AND credential.scope_user_id <> session.created_by) OR
        (credential.scope = 'organization' AND credential.organization_id <> session.organization_id) OR
        (credential.scope = 'tenant' AND (
          (credential.selector_organization_id IS NOT NULL AND credential.selector_organization_id <> session.organization_id) OR
          (credential.selector_model IS NOT NULL AND credential.selector_model IS DISTINCT FROM session.model)
        )) OR
        (credential.scope = 'platform' AND
          NOT provider_credential_platform_policy_enabled(session.tenant_id, false))
      )
  ) THEN
    RAISE EXCEPTION 'existing Agent Session Provider Credential bindings violate Credential scope'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_provider_credential_binding()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  credential provider_credentials%ROWTYPE;
BEGIN
  IF NEW.provider_credential_id IS NULL THEN
    RETURN NEW;
  END IF;

  SELECT * INTO credential
  FROM provider_credentials
  WHERE tenant_id = NEW.tenant_id AND id = NEW.provider_credential_id;

  IF NOT FOUND THEN
    RAISE EXCEPTION 'provider credential does not exist in the session tenant'
      USING ERRCODE = '23503';
  END IF;
  IF credential.purpose <> 'provider' THEN
    RAISE EXCEPTION 'agent sessions can only bind Provider Credentials'
      USING ERRCODE = '23514';
  END IF;
  IF credential.provider <> NEW.provider THEN
    RAISE EXCEPTION 'Provider Credential does not match the Agent Session provider'
      USING ERRCODE = '23514';
  END IF;
  IF credential.scope = 'user' AND (
    credential.scope_user_id <> NEW.created_by OR
    NOT EXISTS (
      SELECT 1 FROM tenant_memberships AS membership
      WHERE membership.tenant_id = NEW.tenant_id
        AND membership.user_id = NEW.created_by
        AND membership.status = 'active'
    )
  ) THEN
    RAISE EXCEPTION 'User Credential belongs to another Session owner'
      USING ERRCODE = '23514';
  END IF;
  IF credential.scope = 'organization' AND credential.organization_id <> NEW.organization_id THEN
    RAISE EXCEPTION 'Organization Credential does not match the Agent Session'
      USING ERRCODE = '23514';
  END IF;
  IF credential.scope = 'tenant' AND (
    (credential.selector_organization_id IS NOT NULL AND credential.selector_organization_id <> NEW.organization_id) OR
    (credential.selector_model IS NOT NULL AND credential.selector_model IS DISTINCT FROM NEW.model)
  ) THEN
    RAISE EXCEPTION 'Tenant Credential policy does not match the Agent Session'
      USING ERRCODE = '23514';
  END IF;
  IF credential.scope = 'platform' AND
     NOT provider_credential_platform_policy_enabled(NEW.tenant_id, false) THEN
    RAISE EXCEPTION 'Platform Credential is not enabled for this Tenant'
      USING ERRCODE = '23514';
  END IF;
  IF credential.revoked_at IS NOT NULL OR
     (credential.expires_at IS NOT NULL AND credential.expires_at <= clock_timestamp()) THEN
    RAISE EXCEPTION 'Provider Credential is revoked or expired'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_sessions_provider_credential_binding ON agent_sessions;
CREATE TRIGGER trg_agent_sessions_provider_credential_binding
BEFORE INSERT OR UPDATE OF tenant_id, organization_id, created_by, provider, model, provider_credential_id
ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION enforce_provider_credential_binding();

COMMENT ON COLUMN provider_credentials.scope IS
  'Resolution scope: user, organization, tenant, or Tenant-owned platform fallback.';
COMMENT ON COLUMN provider_credentials.aad_version IS
  'Ciphertext AAD format. Legacy Provider v1 and Git v2 remain decryptable; new or rotated rows use scope-aware v3.';
COMMENT ON TABLE provider_credential_scope_policies IS
  'Explicit Tenant security policy for enterprise Platform Credential use and automatic selection.';
