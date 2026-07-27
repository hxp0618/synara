CREATE TABLE worker_pool_autoscaling_policies (
  worker_pool_id UUID NOT NULL REFERENCES worker_pools(id) ON DELETE RESTRICT,
  worker_pool_version BIGINT NOT NULL CHECK (worker_pool_version > 0),
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  enabled BOOLEAN NOT NULL DEFAULT FALSE,
  min_idle_units INTEGER NOT NULL DEFAULT 0 CHECK (min_idle_units >= 0),
  max_idle_units INTEGER NOT NULL DEFAULT 0 CHECK (max_idle_units >= min_idle_units),
  target_queue_delay_seconds INTEGER NOT NULL DEFAULT 30
    CHECK (target_queue_delay_seconds BETWEEN 1 AND 3600),
  interactive_cold_start_max_seconds INTEGER
    CHECK (interactive_cold_start_max_seconds IS NULL OR interactive_cold_start_max_seconds BETWEEN 5 AND 3600),
  scale_up_step INTEGER NOT NULL DEFAULT 1 CHECK (scale_up_step BETWEEN 1 AND 10000),
  scale_down_step INTEGER NOT NULL DEFAULT 1 CHECK (scale_down_step BETWEEN 1 AND 10000),
  cooldown_seconds INTEGER NOT NULL DEFAULT 30 CHECK (cooldown_seconds BETWEEN 1 AND 3600),
  scale_down_stabilization_seconds INTEGER NOT NULL DEFAULT 300
    CHECK (scale_down_stabilization_seconds BETWEEN 1 AND 86400),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (worker_pool_id, worker_pool_version),
  CHECK (interactive_cold_start_max_seconds IS NULL OR interactive_cold_start_max_seconds >= target_queue_delay_seconds)
);

CREATE TABLE worker_pool_autoscaling_state (
  worker_pool_id UUID NOT NULL,
  worker_pool_version BIGINT NOT NULL,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  policy_version BIGINT NOT NULL CHECK (policy_version > 0),
  desired_idle_units INTEGER NOT NULL CHECK (desired_idle_units >= 0),
  queue_depth BIGINT NOT NULL CHECK (queue_depth >= 0),
  oldest_queued_at TIMESTAMPTZ,
  ready_idle_units INTEGER NOT NULL CHECK (ready_idle_units >= 0),
  decision_reason TEXT NOT NULL CHECK (decision_reason IN (
    'initialized', 'disabled', 'queue-pressure', 'target-delay', 'stable', 'scale-down-stabilizing', 'scale-down'
  )),
  cold_start_gate_status TEXT NOT NULL DEFAULT 'unknown'
    CHECK (cold_start_gate_status IN ('unknown', 'healthy', 'violated')),
  last_queue_active_at TIMESTAMPTZ,
  last_scaled_at TIMESTAMPTZ,
  decision_version BIGINT NOT NULL DEFAULT 1 CHECK (decision_version > 0),
  observed_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (worker_pool_id, worker_pool_version),
  FOREIGN KEY (worker_pool_id, worker_pool_version)
    REFERENCES worker_pool_autoscaling_policies(worker_pool_id, worker_pool_version)
    ON DELETE CASCADE
);

CREATE INDEX idx_worker_pool_autoscaling_policies_target
  ON worker_pool_autoscaling_policies (execution_target_id, enabled, worker_pool_id);

CREATE INDEX idx_worker_pool_autoscaling_state_target
  ON worker_pool_autoscaling_state (execution_target_id, cold_start_gate_status, worker_pool_id);

DROP TRIGGER IF EXISTS trg_worker_pool_autoscaling_policies_updated_at ON worker_pool_autoscaling_policies;
CREATE TRIGGER trg_worker_pool_autoscaling_policies_updated_at
BEFORE UPDATE ON worker_pool_autoscaling_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE OR REPLACE FUNCTION enforce_worker_pool_autoscaling_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  pool worker_pools%ROWTYPE;
BEGIN
  SELECT * INTO pool FROM worker_pools WHERE id = NEW.worker_pool_id;
  IF NOT FOUND
     OR pool.version <> NEW.worker_pool_version
     OR pool.tenant_id IS DISTINCT FROM NEW.tenant_id
     OR pool.execution_target_id <> NEW.execution_target_id
     OR pool.mode <> 'warm'
     OR pool.status = 'disabled'
     OR NEW.min_idle_units < pool.min_idle_units
     OR NEW.max_idle_units > pool.max_active_units THEN
    RAISE EXCEPTION 'Worker Pool autoscaling scope is invalid' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.worker_pool_id <> OLD.worker_pool_id
       OR NEW.worker_pool_version <> OLD.worker_pool_version
       OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.execution_target_id <> OLD.execution_target_id THEN
      RAISE EXCEPTION 'Worker Pool autoscaling scope is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
      RAISE EXCEPTION 'Worker Pool autoscaling policy version must advance exactly once' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_pool_autoscaling_scope ON worker_pool_autoscaling_policies;
CREATE TRIGGER trg_worker_pool_autoscaling_scope
BEFORE INSERT OR UPDATE ON worker_pool_autoscaling_policies
FOR EACH ROW EXECUTE FUNCTION enforce_worker_pool_autoscaling_scope();

CREATE OR REPLACE FUNCTION enforce_worker_pool_autoscaling_state()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  policy worker_pool_autoscaling_policies%ROWTYPE;
BEGIN
  SELECT * INTO policy
  FROM worker_pool_autoscaling_policies
  WHERE worker_pool_id = NEW.worker_pool_id AND worker_pool_version = NEW.worker_pool_version;
  IF NOT FOUND
     OR policy.tenant_id <> NEW.tenant_id
     OR policy.execution_target_id <> NEW.execution_target_id
     OR policy.version <> NEW.policy_version
     OR NEW.desired_idle_units < policy.min_idle_units
     OR NEW.desired_idle_units > policy.max_idle_units THEN
    RAISE EXCEPTION 'Worker Pool autoscaling state is outside its policy' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.decision_version <> OLD.decision_version + 1
       OR NEW.observed_at <= OLD.observed_at THEN
      RAISE EXCEPTION 'Worker Pool autoscaling decision must advance exactly once' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_pool_autoscaling_state ON worker_pool_autoscaling_state;
CREATE TRIGGER trg_worker_pool_autoscaling_state
BEFORE INSERT OR UPDATE ON worker_pool_autoscaling_state
FOR EACH ROW EXECUTE FUNCTION enforce_worker_pool_autoscaling_state();

ALTER TABLE worker_pool_warm_capacity
  ADD COLUMN effective_desired_idle_units INTEGER NOT NULL DEFAULT 0;

UPDATE worker_pool_warm_capacity
SET effective_desired_idle_units = desired_idle_units;

ALTER TABLE worker_pool_warm_capacity
  ADD CONSTRAINT chk_worker_pool_warm_capacity_effective_desired
  CHECK (
    effective_desired_idle_units >= min_idle_units
    AND effective_desired_idle_units <= max_active_units
  );

CREATE OR REPLACE FUNCTION enforce_worker_pool_warm_capacity_effective_desired()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  policy worker_pool_autoscaling_policies%ROWTYPE;
  state worker_pool_autoscaling_state%ROWTYPE;
BEGIN
  SELECT * INTO policy
  FROM worker_pool_autoscaling_policies
  WHERE worker_pool_id = NEW.worker_pool_id AND worker_pool_version = NEW.worker_pool_version;
  IF NOT FOUND OR NOT policy.enabled THEN
    IF NEW.effective_desired_idle_units <> NEW.desired_idle_units THEN
      RAISE EXCEPTION 'Warm capacity effective desired units lack an enabled autoscaling authority' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  SELECT * INTO state
  FROM worker_pool_autoscaling_state
  WHERE worker_pool_id = NEW.worker_pool_id AND worker_pool_version = NEW.worker_pool_version;
  IF NOT FOUND
     OR state.policy_version <> policy.version
     OR state.desired_idle_units <> NEW.effective_desired_idle_units THEN
    RAISE EXCEPTION 'Warm capacity effective desired units do not match autoscaling authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_pool_warm_capacity_effective_desired ON worker_pool_warm_capacity;
CREATE TRIGGER trg_worker_pool_warm_capacity_effective_desired
BEFORE INSERT OR UPDATE OF effective_desired_idle_units ON worker_pool_warm_capacity
FOR EACH ROW EXECUTE FUNCTION enforce_worker_pool_warm_capacity_effective_desired();
