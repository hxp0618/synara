ALTER TABLE agent_sessions
  ADD COLUMN resource_state TEXT,
  ADD COLUMN meaningful_activity_at TIMESTAMPTZ,
  ADD COLUMN resource_idle_since TIMESTAMPTZ,
  ADD COLUMN absolute_expires_at TIMESTAMPTZ;

UPDATE agent_sessions AS session
SET
  resource_state = CASE
    WHEN EXISTS (
      SELECT 1 FROM agent_executions AS execution
      WHERE execution.tenant_id = session.tenant_id
        AND execution.session_id = session.id
        AND execution.status = 'recovering'
    ) THEN 'restoring'
    WHEN EXISTS (
      SELECT 1 FROM agent_executions AS execution
      WHERE execution.tenant_id = session.tenant_id
        AND execution.session_id = session.id
        AND execution.status = 'queued'
    ) THEN 'provisioning'
    WHEN EXISTS (
      SELECT 1 FROM agent_executions AS execution
      WHERE execution.tenant_id = session.tenant_id
        AND execution.session_id = session.id
        AND execution.status = 'waiting-for-approval'
    ) THEN 'waiting'
    WHEN EXISTS (
      SELECT 1 FROM agent_executions AS execution
      WHERE execution.tenant_id = session.tenant_id
        AND execution.session_id = session.id
        AND execution.status IN ('leased', 'running')
    ) THEN 'active'
    ELSE 'idle'
  END,
  meaningful_activity_at = COALESCE(session.updated_at, session.created_at),
  resource_idle_since = CASE
    WHEN EXISTS (
      SELECT 1
      FROM agent_executions AS execution
      WHERE execution.tenant_id = session.tenant_id
        AND execution.session_id = session.id
        AND execution.status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering')
    ) THEN NULL
    ELSE COALESCE(session.updated_at, session.created_at)
  END;

ALTER TABLE agent_sessions
  ALTER COLUMN resource_state SET DEFAULT 'idle',
  ALTER COLUMN resource_state SET NOT NULL,
  ALTER COLUMN meaningful_activity_at SET DEFAULT now(),
  ALTER COLUMN meaningful_activity_at SET NOT NULL,
  ADD CONSTRAINT chk_agent_sessions_resource_state
    CHECK (resource_state IN ('idle', 'provisioning', 'active', 'waiting', 'checkpointing', 'suspended', 'restoring', 'terminating')),
  ADD CONSTRAINT chk_agent_sessions_resource_idle_since
    CHECK (resource_idle_since IS NULL OR resource_idle_since >= created_at),
  ADD CONSTRAINT chk_agent_sessions_absolute_expires_at
    CHECK (absolute_expires_at IS NULL OR absolute_expires_at > created_at);

CREATE INDEX idx_agent_sessions_resource_idle
  ON agent_sessions (tenant_id, resource_idle_since, id)
  WHERE status = 'active' AND resource_idle_since IS NOT NULL;

CREATE INDEX idx_agent_sessions_absolute_expiry
  ON agent_sessions (absolute_expires_at, tenant_id, id)
  WHERE status IN ('active', 'suspended') AND absolute_expires_at IS NOT NULL;

COMMENT ON COLUMN agent_sessions.meaningful_activity_at IS
  'Server-observed user or runtime activity; browser presence and transport heartbeats never advance this timestamp.';
COMMENT ON COLUMN agent_sessions.resource_idle_since IS
  'Server-authoritative start of the current resource-idle interval; NULL means the Session currently owns or is acquiring execution resources.';
COMMENT ON COLUMN agent_sessions.absolute_expires_at IS
  'Optional hard Session resource-lifecycle boundary that sliding activity cannot extend.';
COMMENT ON COLUMN agent_sessions.resource_state IS
  'Execution-resource lifecycle state; distinct from the user-controlled Session operational status.';
