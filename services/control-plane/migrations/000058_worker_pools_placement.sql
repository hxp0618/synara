CREATE TABLE worker_pools (
  id UUID PRIMARY KEY,
  tenant_id UUID REFERENCES tenants(id) ON DELETE CASCADE,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode IN ('resident', 'per-execution', 'warm')),
  capacity_class TEXT NOT NULL CHECK (capacity_class IN ('standard', 'interactive')),
  cluster_id TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '',
  namespace TEXT NOT NULL DEFAULT '',
  desired_idle_units INTEGER NOT NULL DEFAULT 0 CHECK (desired_idle_units >= 0),
  max_active_units INTEGER NOT NULL DEFAULT 1 CHECK (max_active_units >= desired_idle_units),
  scheduling_template JSONB NOT NULL DEFAULT '{}'::jsonb,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'draining', 'disabled')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (execution_target_id, name),
  CHECK (length(btrim(name)) BETWEEN 1 AND 160),
  CHECK (jsonb_typeof(scheduling_template) = 'object')
);

CREATE INDEX idx_worker_pools_target_status
  ON worker_pools (execution_target_id, status, capacity_class, name, id);

CREATE TABLE execution_placement_policies (
  execution_target_id UUID PRIMARY KEY REFERENCES execution_targets(id) ON DELETE CASCADE,
  tenant_id UUID REFERENCES tenants(id) ON DELETE CASCADE,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  default_pool_id UUID NOT NULL REFERENCES worker_pools(id) ON DELETE RESTRICT,
  balanced_pool_id UUID REFERENCES worker_pools(id) ON DELETE RESTRICT,
  low_latency_pool_id UUID REFERENCES worker_pools(id) ON DELETE RESTRICT,
  updated_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_execution_placement_policies_tenant_target
  ON execution_placement_policies (tenant_id, execution_target_id);

ALTER TABLE agent_executions
  ADD COLUMN worker_pool_id UUID REFERENCES worker_pools(id) ON DELETE RESTRICT,
  ADD COLUMN worker_pool_version BIGINT,
  ADD COLUMN capacity_class TEXT,
  ADD COLUMN placement_policy_version BIGINT,
  ADD CONSTRAINT chk_agent_executions_placement_snapshot CHECK (
    (worker_pool_id IS NULL AND worker_pool_version IS NULL AND capacity_class IS NULL AND placement_policy_version IS NULL)
    OR
    (worker_pool_id IS NOT NULL
      AND worker_pool_version > 0
      AND capacity_class IN ('standard', 'interactive')
      AND placement_policy_version > 0)
  );

CREATE INDEX idx_agent_executions_placement_claim
  ON agent_executions (execution_target_id, worker_pool_id, status, queued_at, id)
  WHERE status IN ('queued', 'recovering');

CREATE OR REPLACE FUNCTION assert_worker_pool_target_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  target execution_targets%ROWTYPE;
BEGIN
  SELECT * INTO target FROM execution_targets WHERE id = NEW.execution_target_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'worker pool target is missing' USING ERRCODE = '23514';
  END IF;
  IF target.tenant_id IS DISTINCT FROM NEW.tenant_id THEN
    RAISE EXCEPTION 'worker pool tenant does not match its execution target' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_pools_target_scope ON worker_pools;
CREATE CONSTRAINT TRIGGER trg_worker_pools_target_scope
AFTER INSERT OR UPDATE OF tenant_id, execution_target_id ON worker_pools
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_pool_target_scope();

CREATE OR REPLACE FUNCTION assert_execution_placement_policy_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  target execution_targets%ROWTYPE;
  default_pool worker_pools%ROWTYPE;
  optional_pool worker_pools%ROWTYPE;
BEGIN
  SELECT * INTO target FROM execution_targets WHERE id = NEW.execution_target_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'execution placement target is missing' USING ERRCODE = '23514';
  END IF;
  IF target.tenant_id IS DISTINCT FROM NEW.tenant_id THEN
    RAISE EXCEPTION 'execution placement tenant does not match its execution target' USING ERRCODE = '23514';
  END IF;

  SELECT * INTO default_pool FROM worker_pools WHERE id = NEW.default_pool_id;
  IF NOT FOUND
     OR default_pool.execution_target_id <> NEW.execution_target_id
     OR default_pool.tenant_id IS DISTINCT FROM NEW.tenant_id
     OR default_pool.status <> 'active' THEN
    RAISE EXCEPTION 'execution placement default pool is invalid' USING ERRCODE = '23514';
  END IF;

  IF NEW.balanced_pool_id IS NOT NULL THEN
    SELECT * INTO optional_pool FROM worker_pools WHERE id = NEW.balanced_pool_id;
    IF NOT FOUND
       OR optional_pool.execution_target_id <> NEW.execution_target_id
       OR optional_pool.tenant_id IS DISTINCT FROM NEW.tenant_id THEN
      RAISE EXCEPTION 'execution placement balanced pool is invalid' USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.low_latency_pool_id IS NOT NULL THEN
    SELECT * INTO optional_pool FROM worker_pools WHERE id = NEW.low_latency_pool_id;
    IF NOT FOUND
       OR optional_pool.execution_target_id <> NEW.execution_target_id
       OR optional_pool.tenant_id IS DISTINCT FROM NEW.tenant_id THEN
      RAISE EXCEPTION 'execution placement low-latency pool is invalid' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION assert_agent_execution_placement_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  selected_pool worker_pools%ROWTYPE;
BEGIN
  IF NEW.worker_pool_id IS NULL THEN
    RETURN NEW;
  END IF;
  SELECT * INTO selected_pool FROM worker_pools WHERE id = NEW.worker_pool_id;
  IF NOT FOUND
     OR selected_pool.execution_target_id <> NEW.execution_target_id
     OR selected_pool.tenant_id IS DISTINCT FROM NEW.tenant_id
     OR selected_pool.version <> NEW.worker_pool_version
     OR selected_pool.capacity_class <> NEW.capacity_class THEN
    RAISE EXCEPTION 'execution placement snapshot does not match its worker pool'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_executions_placement_scope ON agent_executions;
CREATE CONSTRAINT TRIGGER trg_agent_executions_placement_scope
AFTER INSERT OR UPDATE OF tenant_id, execution_target_id, worker_pool_id, capacity_class
ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_agent_execution_placement_scope();

CREATE OR REPLACE FUNCTION enforce_agent_execution_placement_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.worker_pool_id IS NOT NULL AND (
       NEW.worker_pool_id IS DISTINCT FROM OLD.worker_pool_id
       OR NEW.worker_pool_version IS DISTINCT FROM OLD.worker_pool_version
       OR NEW.capacity_class IS DISTINCT FROM OLD.capacity_class
       OR NEW.placement_policy_version IS DISTINCT FROM OLD.placement_policy_version
     ) THEN
    RAISE EXCEPTION 'execution placement snapshot is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_executions_placement_immutable ON agent_executions;
CREATE TRIGGER trg_agent_executions_placement_immutable
BEFORE UPDATE OF worker_pool_id, worker_pool_version, capacity_class, placement_policy_version ON agent_executions
FOR EACH ROW EXECUTE FUNCTION enforce_agent_execution_placement_immutable();

DROP TRIGGER IF EXISTS trg_execution_placement_policies_scope ON execution_placement_policies;
CREATE CONSTRAINT TRIGGER trg_execution_placement_policies_scope
AFTER INSERT OR UPDATE OF tenant_id, execution_target_id, default_pool_id, balanced_pool_id, low_latency_pool_id
ON execution_placement_policies
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_execution_placement_policy_scope();

CREATE OR REPLACE FUNCTION enforce_execution_placement_policy_cas()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'execution placement policies cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.execution_target_id <> OLD.execution_target_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
    RAISE EXCEPTION 'execution placement policy ownership is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'execution placement policy version must advance exactly once' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_placement_policies_cas ON execution_placement_policies;
CREATE TRIGGER trg_execution_placement_policies_cas
BEFORE UPDATE OR DELETE ON execution_placement_policies
FOR EACH ROW EXECUTE FUNCTION enforce_execution_placement_policy_cas();

CREATE OR REPLACE FUNCTION guard_execution_placement_default_pool_active()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.status <> 'active' AND EXISTS (
    SELECT 1
    FROM execution_placement_policies policy
    WHERE policy.default_pool_id = NEW.id
  ) THEN
    RAISE EXCEPTION 'execution placement default pool must remain active while selected'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_pools_default_pool_active ON worker_pools;
CREATE TRIGGER trg_worker_pools_default_pool_active
BEFORE UPDATE OF status ON worker_pools
FOR EACH ROW EXECUTE FUNCTION guard_execution_placement_default_pool_active();

CREATE OR REPLACE FUNCTION enforce_worker_pool_cas()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'worker pools cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.execution_target_id <> OLD.execution_target_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
    RAISE EXCEPTION 'worker pool ownership is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.mode <> OLD.mode OR NEW.capacity_class <> OLD.capacity_class THEN
    RAISE EXCEPTION 'worker pool mode and capacity class are immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'worker pool version must advance exactly once' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_pools_cas ON worker_pools;
CREATE TRIGGER trg_worker_pools_cas
BEFORE UPDATE OR DELETE ON worker_pools
FOR EACH ROW EXECUTE FUNCTION enforce_worker_pool_cas();

DROP TRIGGER IF EXISTS trg_worker_pools_updated_at ON worker_pools;
CREATE TRIGGER trg_worker_pools_updated_at BEFORE UPDATE ON worker_pools
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_execution_placement_policies_updated_at ON execution_placement_policies;
CREATE TRIGGER trg_execution_placement_policies_updated_at BEFORE UPDATE ON execution_placement_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

WITH backfilled_pools AS (
  INSERT INTO worker_pools (
    id, tenant_id, execution_target_id, name, mode, capacity_class,
    cluster_id, region, namespace, desired_idle_units, max_active_units,
    scheduling_template, status, version
  )
  SELECT
    (
      substr(md5(target.id::text || ':worker-pool:default:000058'), 1, 8) || '-' ||
      substr(md5(target.id::text || ':worker-pool:default:000058'), 9, 4) || '-' ||
      substr(md5(target.id::text || ':worker-pool:default:000058'), 13, 4) || '-' ||
      substr(md5(target.id::text || ':worker-pool:default:000058'), 17, 4) || '-' ||
      substr(md5(target.id::text || ':worker-pool:default:000058'), 21, 12)
    )::uuid,
    target.tenant_id,
    target.id,
    'default',
    CASE WHEN target.kind = 'kubernetes' THEN 'per-execution' ELSE 'resident' END,
    'standard',
    '',
    '',
    '',
    0,
    1,
    '{}'::jsonb,
    'active',
    1
  FROM execution_targets target
  RETURNING id, tenant_id, execution_target_id
)
INSERT INTO execution_placement_policies (
  execution_target_id, tenant_id, version, default_pool_id, balanced_pool_id, low_latency_pool_id, updated_by
)
SELECT
  pool.execution_target_id,
  pool.tenant_id,
  1,
  pool.id,
  NULL,
  NULL,
  NULL
FROM backfilled_pools pool;
