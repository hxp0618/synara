CREATE TABLE project_cost_allocations (
  tenant_id UUID NOT NULL,
  project_id UUID NOT NULL,
  cost_center_code TEXT NOT NULL,
  department_code TEXT NOT NULL,
  version BIGINT NOT NULL DEFAULT 1,
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, project_id),
  FOREIGN KEY (tenant_id, project_id) REFERENCES projects(tenant_id, id) ON DELETE RESTRICT,
  CHECK (cost_center_code ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$'),
  CHECK (department_code ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$'),
  CHECK (version > 0)
);

CREATE INDEX idx_project_cost_allocations_dimensions
  ON project_cost_allocations (tenant_id, cost_center_code, department_code, project_id);

CREATE OR REPLACE FUNCTION enforce_project_cost_allocation_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
     OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1
     OR NEW.updated_at <= OLD.updated_at THEN
    RAISE EXCEPTION 'invalid Project internal cost allocation update' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_project_cost_allocations_update
BEFORE UPDATE ON project_cost_allocations
FOR EACH ROW EXECUTE FUNCTION enforce_project_cost_allocation_update();

CREATE OR REPLACE FUNCTION reject_project_cost_allocation_delete()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Project internal cost allocations cannot be deleted; assign the unallocated dimension instead'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_project_cost_allocations_no_delete
BEFORE DELETE ON project_cost_allocations
FOR EACH ROW EXECUTE FUNCTION reject_project_cost_allocation_delete();
