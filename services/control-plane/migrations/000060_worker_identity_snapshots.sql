ALTER TABLE worker_instances
  ADD COLUMN assigned_execution_id UUID REFERENCES agent_executions(id) ON DELETE RESTRICT;

ALTER TABLE worker_instances
  ADD COLUMN worker_pool_id UUID REFERENCES worker_pools(id) ON DELETE RESTRICT;

ALTER TABLE worker_instances
  ADD COLUMN worker_pool_version BIGINT;

ALTER TABLE worker_instances
  ADD COLUMN capacity_class TEXT;

CREATE INDEX idx_worker_instances_assigned_execution
  ON worker_instances (execution_target_id, assigned_execution_id, id)
  WHERE assigned_execution_id IS NOT NULL;

CREATE INDEX idx_worker_instances_pool_identity
  ON worker_instances (execution_target_id, worker_pool_id, worker_pool_version, capacity_class, id)
  WHERE worker_pool_id IS NOT NULL;

CREATE OR REPLACE FUNCTION assert_worker_instance_identity_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  assigned_execution agent_executions%ROWTYPE;
  pool worker_pools%ROWTYPE;
BEGIN
  IF NEW.worker_mode = 'execution-pinned' THEN
    IF NEW.assigned_execution_id IS NULL
       OR NEW.worker_pool_id IS NOT NULL
       OR NEW.worker_pool_version IS NOT NULL
       OR NEW.capacity_class IS NOT NULL THEN
      RAISE EXCEPTION 'execution-pinned Worker identity is invalid' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.worker_mode = 'warm-pool' THEN
    IF NEW.assigned_execution_id IS NOT NULL
       OR NEW.worker_pool_id IS NULL
       OR NEW.worker_pool_version IS NULL
       OR NEW.worker_pool_version <= 0
       OR COALESCE(NEW.capacity_class, '') NOT IN ('standard', 'interactive') THEN
      RAISE EXCEPTION 'warm-pool Worker identity is invalid' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.worker_mode = 'general-pool' THEN
    IF NEW.assigned_execution_id IS NOT NULL
       OR NEW.worker_pool_id IS NOT NULL
       OR NEW.worker_pool_version IS NOT NULL
       OR NEW.capacity_class IS NOT NULL THEN
      RAISE EXCEPTION 'general-pool Worker identity is invalid' USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.assigned_execution_id IS NOT NULL THEN
    SELECT * INTO assigned_execution
    FROM agent_executions
    WHERE id = NEW.assigned_execution_id;
    IF NOT FOUND
       OR assigned_execution.execution_target_id <> NEW.execution_target_id
       OR assigned_execution.target_kind <> NEW.target_kind THEN
      RAISE EXCEPTION 'worker assigned execution does not match its execution target'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.worker_pool_id IS NOT NULL THEN
    SELECT * INTO pool
    FROM worker_pools
    WHERE id = NEW.worker_pool_id;
    IF NOT FOUND
       OR pool.execution_target_id <> NEW.execution_target_id
       OR pool.mode <> 'warm'
       OR pool.status <> 'active'
       OR pool.version <> NEW.worker_pool_version
       OR pool.capacity_class <> NEW.capacity_class THEN
      RAISE EXCEPTION 'worker pool identity does not match an active warm-pool snapshot'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_instances_identity_scope ON worker_instances;
CREATE CONSTRAINT TRIGGER trg_worker_instances_identity_scope
AFTER INSERT OR UPDATE OF
  worker_mode, assigned_execution_id, worker_pool_id, worker_pool_version,
  capacity_class, execution_target_id, target_kind
ON worker_instances
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_instance_identity_scope();

ALTER TABLE agent_executions
  ADD COLUMN warm_pool_mode_snapshot TEXT;

UPDATE agent_executions AS execution
SET warm_pool_mode_snapshot = CASE
  WHEN lower(btrim(session.warm_pool_mode)) IN ('disabled', 'balanced', 'low-latency')
    THEN lower(btrim(session.warm_pool_mode))
  ELSE 'disabled'
END
FROM agent_sessions AS session
WHERE session.tenant_id = execution.tenant_id
  AND session.id = execution.session_id;

UPDATE agent_executions
SET warm_pool_mode_snapshot = 'disabled'
WHERE warm_pool_mode_snapshot IS NULL
   OR btrim(warm_pool_mode_snapshot) = '';

ALTER TABLE agent_executions
  ALTER COLUMN warm_pool_mode_snapshot SET DEFAULT 'disabled';

ALTER TABLE agent_executions
  ALTER COLUMN warm_pool_mode_snapshot SET NOT NULL;

ALTER TABLE agent_executions
  ADD CONSTRAINT chk_agent_executions_warm_pool_mode_snapshot
    CHECK (warm_pool_mode_snapshot IN ('disabled', 'balanced', 'low-latency'));

CREATE OR REPLACE FUNCTION enforce_execution_warm_pool_mode_snapshot_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.warm_pool_mode_snapshot IS DISTINCT FROM OLD.warm_pool_mode_snapshot THEN
    RAISE EXCEPTION 'Execution warm-pool demand snapshot is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_executions_warm_pool_mode_snapshot_immutable ON agent_executions;
CREATE TRIGGER trg_agent_executions_warm_pool_mode_snapshot_immutable
BEFORE UPDATE OF warm_pool_mode_snapshot ON agent_executions
FOR EACH ROW EXECUTE FUNCTION enforce_execution_warm_pool_mode_snapshot_immutable();
