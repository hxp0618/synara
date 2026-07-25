CREATE TABLE worker_incarnation_facts (
  worker_id UUID NOT NULL,
  worker_incarnation BIGINT NOT NULL,
  tenant_id UUID REFERENCES tenants(id) ON DELETE CASCADE,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE CASCADE,
  target_kind TEXT NOT NULL,
  worker_mode TEXT NOT NULL,
  worker_pool_id UUID REFERENCES worker_pools(id) ON DELETE RESTRICT,
  worker_pool_version BIGINT,
  pool_mode TEXT,
  capacity_class TEXT,
  cluster_id TEXT NOT NULL,
  region TEXT NOT NULL DEFAULT '',
  namespace TEXT NOT NULL,
  pod_name TEXT NOT NULL,
  instance_uid TEXT NOT NULL,
  registered_at TIMESTAMPTZ NOT NULL,
  current_state TEXT NOT NULL,
  state_changed_at TIMESTAMPTZ NOT NULL,
  terminated_at TIMESTAMPTZ,
  terminal_reason TEXT,
  accumulated_active_seconds BIGINT NOT NULL DEFAULT 0,
  accumulated_idle_seconds BIGINT NOT NULL DEFAULT 0,
  claim_count BIGINT NOT NULL DEFAULT 0,
  requested_cpu_millicores BIGINT,
  requested_memory_bytes BIGINT,
  requested_ephemeral_storage_bytes BIGINT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_worker_incarnation_facts PRIMARY KEY (worker_id, worker_incarnation),
  CONSTRAINT chk_worker_incarnation_facts_incarnation CHECK (worker_incarnation > 0),
  CONSTRAINT chk_worker_incarnation_facts_target_kind CHECK (
    target_kind IN ('local', 'ssh', 'docker', 'kubernetes')
  ),
  CONSTRAINT chk_worker_incarnation_facts_worker_mode CHECK (
    worker_mode IN ('execution-pinned', 'warm-pool', 'general-pool')
  ),
  CONSTRAINT chk_worker_incarnation_facts_pool_mode CHECK (
    pool_mode IS NULL OR pool_mode IN ('resident', 'per-execution', 'warm')
  ),
  CONSTRAINT chk_worker_incarnation_facts_capacity_class CHECK (
    capacity_class IS NULL OR capacity_class IN ('standard', 'interactive')
  ),
  CONSTRAINT chk_worker_incarnation_facts_state CHECK (
    current_state IN ('idle', 'active', 'draining', 'offline', 'terminated')
  ),
  CONSTRAINT chk_worker_incarnation_facts_pool_snapshot CHECK (
    (worker_pool_id IS NULL AND worker_pool_version IS NULL AND pool_mode IS NULL AND capacity_class IS NULL)
    OR
    (worker_pool_id IS NOT NULL AND worker_pool_version > 0 AND pool_mode IS NOT NULL AND capacity_class IS NOT NULL)
  ),
  CONSTRAINT chk_worker_incarnation_facts_cluster_id CHECK (length(btrim(cluster_id)) BETWEEN 1 AND 160),
  CONSTRAINT chk_worker_incarnation_facts_region CHECK (length(region) <= 160),
  CONSTRAINT chk_worker_incarnation_facts_namespace CHECK (length(btrim(namespace)) BETWEEN 1 AND 253),
  CONSTRAINT chk_worker_incarnation_facts_pod_name CHECK (length(btrim(pod_name)) BETWEEN 1 AND 253),
  CONSTRAINT chk_worker_incarnation_facts_instance_uid CHECK (
    instance_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  ),
  CONSTRAINT chk_worker_incarnation_facts_terminal_reason CHECK (
    terminal_reason IS NULL OR length(btrim(terminal_reason)) BETWEEN 1 AND 160
  ),
  CONSTRAINT chk_worker_incarnation_facts_requested_resources CHECK (
    (requested_cpu_millicores IS NULL OR requested_cpu_millicores > 0)
    AND (requested_memory_bytes IS NULL OR requested_memory_bytes > 0)
    AND (requested_ephemeral_storage_bytes IS NULL OR requested_ephemeral_storage_bytes > 0)
  ),
  CONSTRAINT chk_worker_incarnation_facts_counters CHECK (
    accumulated_active_seconds >= 0
    AND accumulated_idle_seconds >= 0
    AND claim_count >= 0
  ),
  CONSTRAINT chk_worker_incarnation_facts_timeline CHECK (
    state_changed_at >= registered_at
    AND created_at <= updated_at
    AND (
      (current_state = 'terminated' AND terminated_at IS NOT NULL AND terminated_at >= state_changed_at)
      OR
      (current_state <> 'terminated' AND terminated_at IS NULL AND terminal_reason IS NULL)
    )
  )
);

CREATE INDEX idx_worker_incarnation_facts_metrics
  ON worker_incarnation_facts (target_kind, pool_mode, capacity_class, current_state);

CREATE INDEX idx_worker_incarnation_facts_target_state
  ON worker_incarnation_facts (execution_target_id, current_state, worker_id, worker_incarnation);

-- Preserve the physical incarnations already present during a rolling deploy.
-- Historical active/idle counters cannot be reconstructed exactly, so the
-- backfill starts them at zero and records only current lease/state truth.
INSERT INTO worker_incarnation_facts (
  worker_id, worker_incarnation, tenant_id, execution_target_id,
  target_kind, worker_mode, worker_pool_id, worker_pool_version,
  pool_mode, capacity_class, cluster_id, region, namespace, pod_name,
  instance_uid, registered_at, current_state, state_changed_at,
  terminated_at, terminal_reason, accumulated_active_seconds,
  accumulated_idle_seconds, claim_count, created_at, updated_at
)
SELECT
  worker.id,
  worker.incarnation,
  target.tenant_id,
  worker.execution_target_id,
  worker.target_kind,
  worker.worker_mode,
  worker.worker_pool_id,
  worker.worker_pool_version,
  pool.mode,
  worker.capacity_class,
  worker.cluster_id,
  COALESCE(pool.region, ''),
  worker.namespace,
  worker.pod_name,
  worker.instance_uid,
  worker.registered_at,
  CASE
    WHEN worker.status = 'terminated' THEN 'terminated'
    WHEN worker.status = 'draining' THEN 'draining'
    WHEN worker.status = 'offline' THEN 'offline'
    WHEN lease.execution_id IS NOT NULL THEN 'active'
    ELSE 'idle'
  END,
  CASE
    WHEN worker.status = 'terminated' THEN COALESCE(worker.terminated_at, worker.last_heartbeat_at, worker.registered_at)
    WHEN worker.status = 'draining' THEN COALESCE(worker.draining_at, worker.last_heartbeat_at, worker.registered_at)
    ELSE COALESCE(worker.last_heartbeat_at, worker.registered_at)
  END,
  CASE WHEN worker.status = 'terminated' THEN COALESCE(worker.terminated_at, worker.last_heartbeat_at, worker.registered_at) END,
  CASE WHEN worker.status = 'terminated' THEN 'legacy-observed-terminal' END,
  0,
  0,
  CASE WHEN lease.execution_id IS NULL THEN 0 ELSE 1 END,
  worker.registered_at,
  CASE
    WHEN worker.status = 'terminated' THEN COALESCE(worker.terminated_at, worker.last_heartbeat_at, worker.registered_at)
    ELSE COALESCE(worker.last_heartbeat_at, worker.registered_at)
  END
FROM worker_instances AS worker
JOIN execution_targets AS target ON target.id = worker.execution_target_id
LEFT JOIN worker_pools AS pool ON pool.id = worker.worker_pool_id
LEFT JOIN worker_leases AS lease
  ON lease.worker_id = worker.id
 AND lease.worker_incarnation = worker.incarnation
ON CONFLICT (worker_id, worker_incarnation) DO NOTHING;

CREATE OR REPLACE FUNCTION assert_worker_incarnation_fact_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  target execution_targets%ROWTYPE;
  pool worker_pools%ROWTYPE;
BEGIN
  SELECT * INTO target FROM execution_targets WHERE id = NEW.execution_target_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'worker incarnation fact target is missing' USING ERRCODE = '23514';
  END IF;
  IF target.tenant_id IS DISTINCT FROM NEW.tenant_id THEN
    RAISE EXCEPTION 'worker incarnation fact tenant does not match its execution target' USING ERRCODE = '23514';
  END IF;

  IF NOT EXISTS (
    SELECT 1
    FROM worker_instances AS worker
    WHERE worker.id = NEW.worker_id
      AND worker.incarnation = NEW.worker_incarnation
      AND worker.execution_target_id = NEW.execution_target_id
      AND worker.target_kind = NEW.target_kind
      AND worker.worker_mode = NEW.worker_mode
      AND worker.worker_pool_id IS NOT DISTINCT FROM NEW.worker_pool_id
      AND worker.worker_pool_version IS NOT DISTINCT FROM NEW.worker_pool_version
      AND worker.capacity_class IS NOT DISTINCT FROM NEW.capacity_class
      AND worker.cluster_id = NEW.cluster_id
      AND worker.namespace = NEW.namespace
      AND worker.pod_name = NEW.pod_name
      AND worker.instance_uid = NEW.instance_uid
  ) THEN
    RAISE EXCEPTION 'worker incarnation fact does not match the current Worker identity'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.worker_pool_id IS NOT NULL THEN
    SELECT * INTO pool FROM worker_pools WHERE id = NEW.worker_pool_id;
    IF NOT FOUND
       OR pool.execution_target_id <> NEW.execution_target_id
       OR pool.tenant_id IS DISTINCT FROM NEW.tenant_id
       OR pool.mode <> NEW.pool_mode
       OR pool.capacity_class <> NEW.capacity_class
       OR pool.version < NEW.worker_pool_version THEN
      RAISE EXCEPTION 'worker incarnation fact pool snapshot is invalid' USING ERRCODE = '23514';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_incarnation_facts_scope ON worker_incarnation_facts;
CREATE CONSTRAINT TRIGGER trg_worker_incarnation_facts_scope
AFTER INSERT OR UPDATE OF
  worker_id, worker_incarnation, tenant_id, execution_target_id, target_kind,
  worker_mode, worker_pool_id, worker_pool_version, pool_mode, capacity_class,
  cluster_id, region, namespace, pod_name, instance_uid
ON worker_incarnation_facts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_incarnation_fact_scope();

CREATE OR REPLACE FUNCTION enforce_worker_incarnation_fact_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Worker incarnation facts cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.worker_id <> OLD.worker_id
     OR NEW.worker_incarnation <> OLD.worker_incarnation
     OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
     OR NEW.execution_target_id <> OLD.execution_target_id
     OR NEW.target_kind <> OLD.target_kind
     OR NEW.worker_mode <> OLD.worker_mode
     OR NEW.worker_pool_id IS DISTINCT FROM OLD.worker_pool_id
     OR NEW.worker_pool_version IS DISTINCT FROM OLD.worker_pool_version
     OR NEW.pool_mode IS DISTINCT FROM OLD.pool_mode
     OR NEW.capacity_class IS DISTINCT FROM OLD.capacity_class
     OR NEW.cluster_id <> OLD.cluster_id
     OR NEW.region <> OLD.region
     OR NEW.namespace <> OLD.namespace
     OR NEW.pod_name <> OLD.pod_name
     OR NEW.instance_uid <> OLD.instance_uid
     OR NEW.registered_at <> OLD.registered_at
     OR NEW.requested_cpu_millicores IS DISTINCT FROM OLD.requested_cpu_millicores
     OR NEW.requested_memory_bytes IS DISTINCT FROM OLD.requested_memory_bytes
     OR NEW.requested_ephemeral_storage_bytes IS DISTINCT FROM OLD.requested_ephemeral_storage_bytes THEN
    RAISE EXCEPTION 'Worker incarnation fact identity is immutable' USING ERRCODE = '23514';
  END IF;

  IF OLD.current_state = 'terminated' AND (
       NEW.current_state <> OLD.current_state
       OR NEW.state_changed_at <> OLD.state_changed_at
       OR NEW.terminated_at IS DISTINCT FROM OLD.terminated_at
       OR NEW.terminal_reason IS DISTINCT FROM OLD.terminal_reason
       OR NEW.accumulated_active_seconds <> OLD.accumulated_active_seconds
       OR NEW.accumulated_idle_seconds <> OLD.accumulated_idle_seconds
       OR NEW.claim_count <> OLD.claim_count
     ) THEN
    RAISE EXCEPTION 'Worker incarnation terminal state is immutable' USING ERRCODE = '23514';
  END IF;

  IF NEW.state_changed_at < OLD.state_changed_at
     OR NEW.updated_at < OLD.updated_at
     OR NEW.accumulated_active_seconds < OLD.accumulated_active_seconds
     OR NEW.accumulated_idle_seconds < OLD.accumulated_idle_seconds
     OR NEW.claim_count < OLD.claim_count
     OR (OLD.terminated_at IS NOT NULL AND NEW.terminated_at IS DISTINCT FROM OLD.terminated_at)
     OR (OLD.terminal_reason IS NOT NULL AND NEW.terminal_reason IS DISTINCT FROM OLD.terminal_reason) THEN
    RAISE EXCEPTION 'Worker incarnation fact timeline or counters regressed' USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_incarnation_facts_update ON worker_incarnation_facts;
CREATE TRIGGER trg_worker_incarnation_facts_update
BEFORE UPDATE OR DELETE ON worker_incarnation_facts
FOR EACH ROW EXECUTE FUNCTION enforce_worker_incarnation_fact_update();

COMMENT ON TABLE worker_incarnation_facts IS
  'Immutable physical Worker/Pod lifecycle facts keyed by logical Worker ID and incarnation.';

COMMENT ON COLUMN worker_incarnation_facts.requested_cpu_millicores IS
  'Requested CPU millicores snapshot when the physical Worker resource request is known.';

COMMENT ON COLUMN worker_incarnation_facts.requested_memory_bytes IS
  'Requested memory bytes snapshot for retained resource-cost proxy metrics; not billable currency.';
