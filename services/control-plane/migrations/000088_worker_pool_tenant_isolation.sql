ALTER TABLE worker_pools
  ADD COLUMN tenant_isolation TEXT NOT NULL DEFAULT 'pinned';

ALTER TABLE worker_pools
  ADD CONSTRAINT chk_worker_pools_tenant_isolation
  CHECK (tenant_isolation IN ('pinned', 'shared'));

ALTER TABLE worker_instances
  ADD COLUMN tenant_binding_id UUID REFERENCES tenants(id) ON DELETE RESTRICT;

CREATE INDEX idx_worker_instances_tenant_binding
  ON worker_instances (execution_target_id, tenant_binding_id, status, id)
  WHERE tenant_binding_id IS NOT NULL;

CREATE OR REPLACE FUNCTION enforce_worker_tenant_binding()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  target execution_targets%ROWTYPE;
BEGIN
  IF TG_OP = 'UPDATE'
     AND OLD.tenant_binding_id IS NOT NULL
     AND NEW.tenant_binding_id IS DISTINCT FROM OLD.tenant_binding_id THEN
    RAISE EXCEPTION 'Worker Tenant binding is immutable' USING ERRCODE = '23514';
  END IF;

  IF NEW.tenant_binding_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF NEW.worker_mode <> 'general-pool' THEN
    RAISE EXCEPTION 'Only general-pool Workers may carry a Tenant binding' USING ERRCODE = '23514';
  END IF;

  SELECT * INTO target FROM execution_targets WHERE id = NEW.execution_target_id;
  IF NOT FOUND
     OR (target.tenant_id IS NOT NULL AND target.tenant_id <> NEW.tenant_binding_id) THEN
    RAISE EXCEPTION 'Worker Tenant binding is outside its Execution Target' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_instances_tenant_binding ON worker_instances;
CREATE TRIGGER trg_worker_instances_tenant_binding
BEFORE INSERT OR UPDATE OF tenant_binding_id, worker_mode, execution_target_id ON worker_instances
FOR EACH ROW EXECUTE FUNCTION enforce_worker_tenant_binding();

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
  IF NEW.mode <> OLD.mode
     OR NEW.capacity_class <> OLD.capacity_class
     OR NEW.tenant_isolation <> OLD.tenant_isolation THEN
    RAISE EXCEPTION 'worker pool mode, capacity class, and Tenant isolation are immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'worker pool version must advance exactly once' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;
