CREATE TABLE IF NOT EXISTS worker_instances (
  id UUID PRIMARY KEY,
  pool_id TEXT NOT NULL,
  cluster_id TEXT NOT NULL,
  namespace TEXT NOT NULL,
  pod_name TEXT NOT NULL,
  version TEXT NOT NULL,
  capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
  auth_token_hash BYTEA NOT NULL UNIQUE,
  status TEXT NOT NULL DEFAULT 'online'
    CHECK (status IN ('online', 'draining', 'offline', 'terminated')),
  registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  draining_at TIMESTAMPTZ,
  terminated_at TIMESTAMPTZ,
  CHECK (length(pool_id) BETWEEN 1 AND 160),
  CHECK (length(cluster_id) BETWEEN 1 AND 160),
  CHECK (length(namespace) BETWEEN 1 AND 253),
  CHECK (length(pod_name) BETWEEN 1 AND 253),
  CHECK (length(version) BETWEEN 1 AND 160),
  CHECK ((status = 'terminated' AND terminated_at IS NOT NULL) OR status <> 'terminated'),
  CHECK ((status = 'draining' AND draining_at IS NOT NULL) OR status <> 'draining')
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_instances_active_pod
  ON worker_instances (cluster_id, namespace, pod_name)
  WHERE status <> 'terminated';

CREATE INDEX IF NOT EXISTS idx_worker_instances_pool_status_heartbeat
  ON worker_instances (pool_id, status, last_heartbeat_at, id);

CREATE TABLE IF NOT EXISTS agent_executions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  session_id UUID NOT NULL,
  turn_id UUID NOT NULL,
  attempt INTEGER NOT NULL CHECK (attempt > 0),
  status TEXT NOT NULL DEFAULT 'queued'
    CHECK (status IN ('queued', 'leased', 'running', 'recovering', 'completed', 'failed', 'cancelled')),
  target_type TEXT NOT NULL DEFAULT 'shared_pool'
    CHECK (target_type IN ('local', 'shared_pool', 'dedicated_pool')),
  worker_id UUID REFERENCES worker_instances(id) ON DELETE SET NULL,
  generation BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0),
  requested_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  queued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  failure_code TEXT,
  failure_message TEXT,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, turn_id, attempt),
  FOREIGN KEY (tenant_id, session_id, turn_id)
    REFERENCES agent_turns(tenant_id, session_id, id) ON DELETE RESTRICT,
  CHECK (failure_code IS NULL OR length(failure_code) BETWEEN 1 AND 160),
  CHECK (failure_message IS NULL OR length(failure_message) <= 10000),
  CHECK ((status IN ('completed', 'failed', 'cancelled') AND finished_at IS NOT NULL) OR
         status NOT IN ('completed', 'failed', 'cancelled')),
  CHECK ((status = 'failed' AND failure_code IS NOT NULL) OR status <> 'failed'),
  CHECK ((status IN ('leased', 'running') AND worker_id IS NOT NULL AND generation > 0) OR
         status NOT IN ('leased', 'running'))
);

CREATE INDEX IF NOT EXISTS idx_agent_executions_claimable
  ON agent_executions (target_type, queued_at, id)
  WHERE status IN ('queued', 'recovering');

CREATE INDEX IF NOT EXISTS idx_agent_executions_session_status
  ON agent_executions (tenant_id, session_id, status, queued_at DESC, id);

CREATE INDEX IF NOT EXISTS idx_agent_executions_worker_status
  ON agent_executions (worker_id, status, id)
  WHERE worker_id IS NOT NULL AND status IN ('leased', 'running');

CREATE TABLE IF NOT EXISTS worker_leases (
  execution_id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  worker_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE RESTRICT,
  generation BIGINT NOT NULL CHECK (generation > 0),
  lease_token_hash BYTEA NOT NULL UNIQUE,
  acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  UNIQUE (tenant_id, execution_id),
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE CASCADE,
  CHECK (expires_at > acquired_at),
  CHECK (heartbeat_at >= acquired_at)
);

CREATE INDEX IF NOT EXISTS idx_worker_leases_expiry
  ON worker_leases (expires_at, execution_id);

CREATE INDEX IF NOT EXISTS idx_worker_leases_worker_expiry
  ON worker_leases (worker_id, expires_at, execution_id);

CREATE TABLE IF NOT EXISTS worker_request_receipts (
  worker_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE CASCADE,
  request_id TEXT NOT NULL,
  operation TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  status_code INTEGER NOT NULL CHECK (status_code BETWEEN 100 AND 599),
  response JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (worker_id, request_id),
  CHECK (length(request_id) BETWEEN 1 AND 160),
  CHECK (length(operation) BETWEEN 1 AND 160),
  CHECK (length(request_hash) = 64),
  CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS idx_worker_request_receipts_expiry
  ON worker_request_receipts (expires_at, worker_id, request_id);

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
      AND execution.status IN ('leased', 'running')
  ) THEN
    RAISE EXCEPTION 'worker lease does not match the current execution generation'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_lease_matches_execution ON worker_leases;
CREATE CONSTRAINT TRIGGER trg_worker_lease_matches_execution
AFTER INSERT OR UPDATE ON worker_leases
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_lease_matches_execution();

CREATE OR REPLACE FUNCTION assert_terminal_execution_has_no_lease()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.status IN ('completed', 'failed', 'cancelled') AND EXISTS (
    SELECT 1 FROM worker_leases lease WHERE lease.execution_id = NEW.id
  ) THEN
    RAISE EXCEPTION 'terminal execution cannot retain a worker lease'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_terminal_execution_has_no_lease ON agent_executions;
CREATE CONSTRAINT TRIGGER trg_terminal_execution_has_no_lease
AFTER UPDATE OF status ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_terminal_execution_has_no_lease();

DROP TRIGGER IF EXISTS trg_agent_executions_tenant_immutable ON agent_executions;
CREATE TRIGGER trg_agent_executions_tenant_immutable
BEFORE UPDATE OF tenant_id ON agent_executions
FOR EACH ROW EXECUTE FUNCTION reject_tenant_ownership_change();
