ALTER TABLE agent_executions
  DROP CONSTRAINT IF EXISTS agent_executions_status_check;

ALTER TABLE agent_executions
  ADD CONSTRAINT agent_executions_status_check
  CHECK (status IN (
    'queued', 'leased', 'running', 'waiting-for-approval', 'recovering',
    'completed', 'failed', 'cancelled'
  ));

DROP INDEX IF EXISTS idx_agent_executions_worker_status;
CREATE INDEX idx_agent_executions_worker_status
  ON agent_executions (worker_id, status, id)
  WHERE worker_id IS NOT NULL AND status IN ('leased', 'running', 'waiting-for-approval');

CREATE OR REPLACE FUNCTION assert_worker_lease_matches_execution()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM agent_executions execution
    WHERE execution.id = NEW.execution_id
      AND execution.tenant_id = NEW.tenant_id
      AND execution.worker_id = NEW.worker_id
      AND execution.generation = NEW.generation
      AND execution.status IN ('leased', 'running', 'waiting-for-approval')
  ) THEN
    RAISE EXCEPTION 'worker lease does not match the current execution generation'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TABLE IF NOT EXISTS execution_interactions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  session_id UUID NOT NULL,
  worker_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE RESTRICT,
  generation BIGINT NOT NULL CHECK (generation > 0),
  request_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('approval', 'user-input')),
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'resolved', 'expired')),
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  resolution JSONB,
  requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at TIMESTAMPTZ,
  resolved_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  UNIQUE (tenant_id, execution_id, request_id),
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, session_id)
    REFERENCES agent_sessions(tenant_id, id) ON DELETE CASCADE,
  CHECK (length(request_id) BETWEEN 1 AND 200),
  CHECK (
    (status = 'pending' AND resolution IS NULL AND resolved_at IS NULL AND resolved_by IS NULL) OR
    (status = 'resolved' AND resolution IS NOT NULL AND resolved_at IS NOT NULL AND resolved_by IS NOT NULL) OR
    status = 'expired'
  )
);

CREATE INDEX IF NOT EXISTS idx_execution_interactions_pending
  ON execution_interactions (tenant_id, session_id, requested_at, id)
  WHERE status = 'pending';
