CREATE TABLE worker_pool_warm_capacity (
  worker_pool_id UUID NOT NULL REFERENCES worker_pools(id) ON DELETE RESTRICT,
  worker_pool_version BIGINT NOT NULL,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  capacity_class TEXT NOT NULL CHECK (capacity_class IN ('standard', 'interactive')),
  warm_supported BOOLEAN NOT NULL DEFAULT FALSE,
  worker_release_revision_id UUID REFERENCES worker_release_revisions(id) ON DELETE RESTRICT,
  worker_release_channel TEXT,
  desired_idle_units INTEGER NOT NULL CHECK (desired_idle_units >= 0),
  max_active_units INTEGER NOT NULL CHECK (max_active_units >= 0),
  desired_total_units INTEGER NOT NULL CHECK (desired_total_units >= 0),
  claimed_units INTEGER NOT NULL CHECK (claimed_units >= 0),
  ready_idle_units INTEGER NOT NULL CHECK (ready_idle_units >= 0),
  source TEXT NOT NULL,
  reason TEXT,
  observed_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (worker_pool_id, worker_pool_version),
  CHECK (worker_pool_version > 0),
  CHECK (
    (worker_release_revision_id IS NULL AND worker_release_channel IS NULL)
    OR
    (worker_release_revision_id IS NOT NULL AND worker_release_channel IN ('promoted', 'canary'))
  ),
  CHECK (length(btrim(source)) BETWEEN 1 AND 160),
  CHECK (reason IS NULL OR length(reason) <= 2000),
  CHECK (expires_at > observed_at),
  CHECK (desired_idle_units <= max_active_units),
  CHECK (desired_total_units <= max_active_units),
  CHECK (ready_idle_units <= desired_total_units),
  CHECK (
    warm_supported
    OR (desired_total_units = 0 AND ready_idle_units = 0)
  )
);

CREATE INDEX idx_worker_pool_warm_capacity_target
  ON worker_pool_warm_capacity (tenant_id, execution_target_id, worker_pool_id, worker_pool_version);

CREATE INDEX idx_worker_pool_warm_capacity_expiry
  ON worker_pool_warm_capacity (observed_at DESC, execution_target_id, expires_at, worker_pool_id, worker_pool_version);

CREATE INDEX idx_worker_pool_warm_capacity_release_pair
  ON worker_pool_warm_capacity (worker_pool_id, worker_pool_version, worker_release_revision_id, worker_release_channel);

CREATE OR REPLACE FUNCTION assert_worker_pool_warm_capacity_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  pool worker_pools%ROWTYPE;
  target execution_targets%ROWTYPE;
  release worker_release_revisions%ROWTYPE;
BEGIN
  SELECT * INTO pool FROM worker_pools WHERE id = NEW.worker_pool_id;
  IF NOT FOUND
     OR pool.execution_target_id <> NEW.execution_target_id
     OR pool.tenant_id IS DISTINCT FROM NEW.tenant_id
     OR pool.version <> NEW.worker_pool_version
     OR pool.mode <> 'warm'
     OR pool.status = 'disabled'
     OR pool.capacity_class <> NEW.capacity_class
     OR pool.desired_idle_units <> NEW.desired_idle_units
     OR pool.max_active_units <> NEW.max_active_units THEN
    RAISE EXCEPTION 'Worker pool warm capacity scope is invalid' USING ERRCODE = '23514';
  END IF;

  SELECT * INTO target FROM execution_targets WHERE id = NEW.execution_target_id;
  IF NOT FOUND
     OR target.tenant_id IS NULL
     OR target.tenant_id IS DISTINCT FROM NEW.tenant_id
     OR target.kind <> 'kubernetes'
     OR target.status <> 'active' THEN
    RAISE EXCEPTION 'Worker pool warm capacity scope is invalid' USING ERRCODE = '23514';
  END IF;

  IF NEW.worker_release_revision_id IS NOT NULL THEN
    SELECT * INTO release FROM worker_release_revisions WHERE id = NEW.worker_release_revision_id;
    IF NOT FOUND
       OR release.execution_target_id <> NEW.execution_target_id
       OR release.tenant_id <> NEW.tenant_id THEN
      RAISE EXCEPTION 'Worker pool warm capacity scope is invalid' USING ERRCODE = '23514';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_pool_warm_capacity_scope ON worker_pool_warm_capacity;
CREATE CONSTRAINT TRIGGER trg_worker_pool_warm_capacity_scope
AFTER INSERT OR UPDATE
ON worker_pool_warm_capacity
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_pool_warm_capacity_scope();

CREATE OR REPLACE FUNCTION enforce_worker_pool_warm_capacity_cas()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Worker pool warm capacity observations cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.worker_pool_id <> OLD.worker_pool_id
     OR NEW.worker_pool_version <> OLD.worker_pool_version
     OR NEW.tenant_id <> OLD.tenant_id
     OR NEW.execution_target_id <> OLD.execution_target_id THEN
    RAISE EXCEPTION 'Worker pool warm capacity authority ownership is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version + 1 OR NEW.observed_at <= OLD.observed_at THEN
    RAISE EXCEPTION 'Worker pool warm capacity version must advance exactly once and observed_at must advance'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_pool_warm_capacity_cas ON worker_pool_warm_capacity;
CREATE TRIGGER trg_worker_pool_warm_capacity_cas
BEFORE UPDATE OR DELETE ON worker_pool_warm_capacity
FOR EACH ROW EXECUTE FUNCTION enforce_worker_pool_warm_capacity_cas();
