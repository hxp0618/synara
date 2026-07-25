CREATE TABLE IF NOT EXISTS execution_provider_credential_grants (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  credential_id UUID NOT NULL,
  credential_version INTEGER NOT NULL CHECK (credential_version > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, execution_id, generation),
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, credential_id)
    REFERENCES provider_credentials(tenant_id, id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_execution_provider_credential_grants_execution
  ON execution_provider_credential_grants (tenant_id, execution_id, generation, id);

CREATE INDEX IF NOT EXISTS idx_execution_provider_credential_grants_credential
  ON execution_provider_credential_grants (tenant_id, credential_id, credential_version, id);

CREATE OR REPLACE FUNCTION enforce_execution_provider_credential_grant()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  execution agent_executions%ROWTYPE;
  credential provider_credentials%ROWTYPE;
BEGIN
  SELECT * INTO execution
  FROM agent_executions
  WHERE tenant_id = NEW.tenant_id AND id = NEW.execution_id
  FOR SHARE;
  IF NOT FOUND OR execution.generation <> NEW.generation OR
     execution.provider_credential_id_snapshot IS NULL OR
     execution.provider_credential_id_snapshot <> NEW.credential_id OR
     execution.provider_credential_version_snapshot IS NULL OR
     execution.provider_credential_version_snapshot <> NEW.credential_version THEN
    RAISE EXCEPTION 'Execution Provider Credential Grant generation is fenced'
      USING ERRCODE = '23514';
  END IF;

  SELECT * INTO credential
  FROM provider_credentials
  WHERE tenant_id = NEW.tenant_id AND id = NEW.credential_id
  FOR SHARE;
  IF NOT FOUND OR credential.version <> NEW.credential_version OR credential.revoked_at IS NOT NULL OR
     (credential.expires_at IS NOT NULL AND credential.expires_at <= clock_timestamp()) THEN
    RAISE EXCEPTION 'Execution Provider Credential Grant Credential is unavailable or rotated'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_provider_credential_grants_validate ON execution_provider_credential_grants;
CREATE TRIGGER trg_execution_provider_credential_grants_validate
BEFORE INSERT ON execution_provider_credential_grants
FOR EACH ROW EXECUTE FUNCTION enforce_execution_provider_credential_grant();

CREATE OR REPLACE FUNCTION reject_execution_provider_credential_grant_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Execution Provider Credential Grants are immutable'
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_provider_credential_grants_no_update ON execution_provider_credential_grants;
CREATE TRIGGER trg_execution_provider_credential_grants_no_update
BEFORE UPDATE ON execution_provider_credential_grants
FOR EACH ROW EXECUTE FUNCTION reject_execution_provider_credential_grant_mutation();

DROP TRIGGER IF EXISTS trg_execution_provider_credential_grants_no_delete ON execution_provider_credential_grants;
CREATE TRIGGER trg_execution_provider_credential_grants_no_delete
BEFORE DELETE ON execution_provider_credential_grants
FOR EACH ROW EXECUTE FUNCTION reject_execution_provider_credential_grant_mutation();

COMMENT ON TABLE execution_provider_credential_grants IS
  'Generation-fenced immutable Provider Credential snapshots created during Execution Claim.';
