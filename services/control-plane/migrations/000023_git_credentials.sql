ALTER TABLE provider_credentials
  ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'provider';

ALTER TABLE provider_credentials
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_purpose;

ALTER TABLE provider_credentials
  ADD CONSTRAINT chk_provider_credentials_purpose
  CHECK (purpose IN ('provider', 'git'));

ALTER TABLE provider_credentials
  DROP CONSTRAINT IF EXISTS chk_provider_credentials_git_shape;

ALTER TABLE provider_credentials
  ADD CONSTRAINT chk_provider_credentials_git_shape
  CHECK (purpose <> 'git' OR (provider = 'git' AND credential_type = 'https_token'));

CREATE INDEX IF NOT EXISTS idx_provider_credentials_tenant_purpose
  ON provider_credentials (tenant_id, purpose, created_at DESC, id)
  WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_provider_credentials_organization_purpose
  ON provider_credentials (tenant_id, organization_id, purpose, created_at DESC, id)
  WHERE organization_id IS NOT NULL AND revoked_at IS NULL;

CREATE OR REPLACE FUNCTION reject_credential_identity_change()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
	IF NEW.tenant_id <> OLD.tenant_id OR
	   NEW.purpose <> OLD.purpose OR
     NEW.provider <> OLD.provider OR
     NEW.credential_type <> OLD.credential_type OR
     NEW.organization_id IS DISTINCT FROM OLD.organization_id THEN
		RAISE EXCEPTION 'credential tenant, purpose, provider, type, and organization scope are immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_provider_credentials_identity_immutable ON provider_credentials;
CREATE TRIGGER trg_provider_credentials_identity_immutable
BEFORE UPDATE OF tenant_id, purpose, provider, credential_type, organization_id
ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION reject_credential_identity_change();

ALTER TABLE projects
  ADD COLUMN IF NOT EXISTS git_credential_id UUID;

ALTER TABLE projects
  DROP CONSTRAINT IF EXISTS fk_projects_git_credential;

ALTER TABLE projects
  ADD CONSTRAINT fk_projects_git_credential
  FOREIGN KEY (tenant_id, git_credential_id)
  REFERENCES provider_credentials(tenant_id, id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_projects_git_credential
  ON projects (tenant_id, git_credential_id, updated_at DESC, id)
  WHERE git_credential_id IS NOT NULL;

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
        (credential.organization_id IS NOT NULL AND credential.organization_id <> session.organization_id)
      )
  ) THEN
    RAISE EXCEPTION 'existing agent session Provider Credential bindings violate purpose, provider, or organization scope'
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
    RAISE EXCEPTION 'agent sessions can only bind provider credentials'
      USING ERRCODE = '23514';
  END IF;
  IF credential.organization_id IS NOT NULL AND credential.organization_id <> NEW.organization_id THEN
    RAISE EXCEPTION 'provider credential organization does not match the agent session'
      USING ERRCODE = '23514';
  END IF;
  IF credential.provider <> NEW.provider THEN
    RAISE EXCEPTION 'provider credential does not match the agent session provider'
      USING ERRCODE = '23514';
  END IF;
  IF credential.revoked_at IS NOT NULL OR
     (credential.expires_at IS NOT NULL AND credential.expires_at <= clock_timestamp()) THEN
    RAISE EXCEPTION 'provider credential is revoked or expired'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_sessions_provider_credential_binding ON agent_sessions;
CREATE TRIGGER trg_agent_sessions_provider_credential_binding
BEFORE INSERT OR UPDATE OF tenant_id, organization_id, provider, provider_credential_id
ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION enforce_provider_credential_binding();

CREATE OR REPLACE FUNCTION enforce_project_git_credential_binding()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  credential provider_credentials%ROWTYPE;
BEGIN
  IF NEW.git_credential_id IS NULL THEN
    RETURN NEW;
  END IF;

  IF NEW.repository_url IS NULL OR length(btrim(NEW.repository_url)) = 0 THEN
    RAISE EXCEPTION 'a project Git credential requires a repository URL'
      USING ERRCODE = '23514';
  END IF;

  SELECT * INTO credential
  FROM provider_credentials
  WHERE tenant_id = NEW.tenant_id AND id = NEW.git_credential_id;

  IF NOT FOUND THEN
    RAISE EXCEPTION 'Git credential does not exist in the project tenant'
      USING ERRCODE = '23503';
  END IF;
  IF credential.purpose <> 'git' THEN
    RAISE EXCEPTION 'projects can only bind Git credentials'
      USING ERRCODE = '23514';
  END IF;
	IF credential.provider <> 'git' OR credential.credential_type <> 'https_token' THEN
		RAISE EXCEPTION 'project Git credential must use provider git and type https_token'
			USING ERRCODE = '23514';
	END IF;
  IF credential.organization_id IS NOT NULL AND credential.organization_id <> NEW.organization_id THEN
    RAISE EXCEPTION 'Git credential organization does not match the project'
      USING ERRCODE = '23514';
  END IF;
  IF credential.revoked_at IS NOT NULL OR
     (credential.expires_at IS NOT NULL AND credential.expires_at <= clock_timestamp()) THEN
    RAISE EXCEPTION 'Git credential is revoked or expired'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_projects_git_credential_binding ON projects;
CREATE TRIGGER trg_projects_git_credential_binding
BEFORE INSERT OR UPDATE OF tenant_id, organization_id, repository_url, git_credential_id
ON projects
FOR EACH ROW EXECUTE FUNCTION enforce_project_git_credential_binding();
