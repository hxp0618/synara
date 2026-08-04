ALTER TABLE agent_sessions
  ADD COLUMN IF NOT EXISTS settled_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_agent_sessions_settled
  ON agent_sessions (tenant_id, organization_id, settled_at DESC, id)
  WHERE settled_at IS NOT NULL AND archived_at IS NULL;
