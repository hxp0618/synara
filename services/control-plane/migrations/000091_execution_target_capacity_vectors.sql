CREATE TABLE execution_target_capacities (
  execution_target_id UUID PRIMARY KEY REFERENCES execution_targets(id) ON DELETE CASCADE,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  target_kind TEXT NOT NULL CHECK (target_kind IN ('kubernetes')),
  source TEXT NOT NULL CHECK (length(source) BETWEEN 1 AND 160),

  total_pods BIGINT NOT NULL CHECK (total_pods >= 0),
  allocated_pods BIGINT NOT NULL CHECK (allocated_pods >= 0),
  available_pods BIGINT NOT NULL CHECK (
    available_pods = CASE WHEN total_pods > allocated_pods THEN total_pods - allocated_pods ELSE 0 END
  ),
  schedulable_units BIGINT NOT NULL CHECK (schedulable_units >= 0 AND schedulable_units <= available_pods),

  pod_request_cpu_millicores BIGINT CHECK (pod_request_cpu_millicores IS NULL OR pod_request_cpu_millicores > 0),
  total_cpu_millicores BIGINT,
  allocated_cpu_millicores BIGINT,
  available_cpu_millicores BIGINT,
  pod_request_memory_bytes BIGINT CHECK (pod_request_memory_bytes IS NULL OR pod_request_memory_bytes > 0),
  total_memory_bytes BIGINT,
  allocated_memory_bytes BIGINT,
  available_memory_bytes BIGINT,
  pod_request_ephemeral_storage_bytes BIGINT
    CHECK (pod_request_ephemeral_storage_bytes IS NULL OR pod_request_ephemeral_storage_bytes > 0),
  total_ephemeral_storage_bytes BIGINT,
  allocated_ephemeral_storage_bytes BIGINT,
  available_ephemeral_storage_bytes BIGINT,
  gpu_resource_name TEXT,
  pod_request_gpu_units BIGINT CHECK (pod_request_gpu_units IS NULL OR pod_request_gpu_units > 0),
  total_gpu_units BIGINT,
  allocated_gpu_units BIGINT,
  available_gpu_units BIGINT,

  observed_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at > observed_at),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  CHECK (
    (total_cpu_millicores IS NULL AND allocated_cpu_millicores IS NULL AND available_cpu_millicores IS NULL)
    OR (total_cpu_millicores >= 0 AND allocated_cpu_millicores >= 0 AND
        available_cpu_millicores = CASE WHEN total_cpu_millicores > allocated_cpu_millicores
          THEN total_cpu_millicores - allocated_cpu_millicores ELSE 0 END)
  ),
  CHECK (
    (total_memory_bytes IS NULL AND allocated_memory_bytes IS NULL AND available_memory_bytes IS NULL)
    OR (total_memory_bytes >= 0 AND allocated_memory_bytes >= 0 AND
        available_memory_bytes = CASE WHEN total_memory_bytes > allocated_memory_bytes
          THEN total_memory_bytes - allocated_memory_bytes ELSE 0 END)
  ),
  CHECK (
    (total_ephemeral_storage_bytes IS NULL AND allocated_ephemeral_storage_bytes IS NULL AND available_ephemeral_storage_bytes IS NULL)
    OR (total_ephemeral_storage_bytes >= 0 AND allocated_ephemeral_storage_bytes >= 0 AND
        available_ephemeral_storage_bytes = CASE WHEN total_ephemeral_storage_bytes > allocated_ephemeral_storage_bytes
          THEN total_ephemeral_storage_bytes - allocated_ephemeral_storage_bytes ELSE 0 END)
  ),
  CHECK (
    (gpu_resource_name IS NULL AND pod_request_gpu_units IS NULL AND total_gpu_units IS NULL AND
      allocated_gpu_units IS NULL AND available_gpu_units IS NULL)
    OR (length(gpu_resource_name) BETWEEN 3 AND 253 AND pod_request_gpu_units > 0 AND
        total_gpu_units >= 0 AND allocated_gpu_units >= 0 AND
        available_gpu_units = CASE WHEN total_gpu_units > allocated_gpu_units
          THEN total_gpu_units - allocated_gpu_units ELSE 0 END)
  )
);

CREATE INDEX idx_execution_target_capacity_expiry
  ON execution_target_capacities (target_kind, tenant_id, expires_at, execution_target_id);

CREATE OR REPLACE FUNCTION enforce_execution_target_capacity_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM execution_targets target
    WHERE target.id = NEW.execution_target_id
      AND target.tenant_id = NEW.tenant_id
      AND target.kind = NEW.target_kind
      AND target.status = 'active'
  ) THEN
    RAISE EXCEPTION 'Execution Target capacity scope is invalid' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.execution_target_id <> OLD.execution_target_id
       OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.target_kind <> OLD.target_kind
       OR NEW.version <> OLD.version + 1
       OR NEW.observed_at <= OLD.observed_at THEN
      RAISE EXCEPTION 'Execution Target capacity authority must advance exactly once' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_target_capacity_scope ON execution_target_capacities;
CREATE TRIGGER trg_execution_target_capacity_scope
BEFORE INSERT OR UPDATE ON execution_target_capacities
FOR EACH ROW EXECUTE FUNCTION enforce_execution_target_capacity_scope();
