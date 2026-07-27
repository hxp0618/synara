ALTER TABLE worker_pools
  ADD COLUMN min_idle_units INTEGER NOT NULL DEFAULT 0;

ALTER TABLE worker_pools
  ADD CONSTRAINT chk_worker_pools_min_idle_units CHECK (
    min_idle_units >= 0 AND min_idle_units <= desired_idle_units
  );

ALTER TABLE worker_pool_warm_capacity
  ADD COLUMN min_idle_units INTEGER NOT NULL DEFAULT 0;

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
     OR pool.min_idle_units <> NEW.min_idle_units
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
