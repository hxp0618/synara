ALTER TABLE agent_sessions
  ADD COLUMN IF NOT EXISTS provider_credential_id UUID;

ALTER TABLE agent_sessions
  DROP CONSTRAINT IF EXISTS fk_agent_sessions_provider_credential;

ALTER TABLE agent_sessions
  ADD CONSTRAINT fk_agent_sessions_provider_credential
  FOREIGN KEY (tenant_id, provider_credential_id)
  REFERENCES provider_credentials(tenant_id, id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_agent_sessions_provider_credential
  ON agent_sessions (tenant_id, provider_credential_id, updated_at DESC, id)
  WHERE provider_credential_id IS NOT NULL;
