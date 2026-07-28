CREATE TABLE execution_kubernetes_allocations (
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL,
  execution_target_id UUID NOT NULL,
  backend TEXT NOT NULL,
  namespace TEXT NOT NULL,
  sandbox_template_name TEXT NOT NULL,
  sandbox_warm_pool_name TEXT NOT NULL,
  configuration_digest TEXT NOT NULL,
  claim_name TEXT NOT NULL,
  claim_uid TEXT,
  sandbox_name TEXT,
  sandbox_uid TEXT,
  pod_name TEXT,
  pod_uid TEXT,
  status TEXT NOT NULL,
  failure_reason_code TEXT,
  materialization_started_at TIMESTAMPTZ NOT NULL,
  claim_ready_at TIMESTAMPTZ,
  bound_at TIMESTAMPTZ,
  delete_requested_at TIMESTAMPTZ,
  deleted_at TIMESTAMPTZ,
  last_observed_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_execution_kubernetes_allocations
    PRIMARY KEY (tenant_id, execution_id, generation),
  CONSTRAINT fk_execution_kubernetes_allocations_generation
    FOREIGN KEY (tenant_id, execution_id, generation)
    REFERENCES execution_generation_facts(tenant_id, execution_id, generation) ON DELETE RESTRICT,
  CONSTRAINT fk_execution_kubernetes_allocations_target
    FOREIGN KEY (execution_target_id) REFERENCES execution_targets(id) ON DELETE RESTRICT,
  CONSTRAINT uq_execution_kubernetes_allocations_claim
    UNIQUE (execution_target_id, namespace, claim_name),
  CONSTRAINT chk_execution_kubernetes_allocations_generation CHECK (generation > 0),
  CONSTRAINT chk_execution_kubernetes_allocations_backend CHECK (
    backend IN ('sandbox-operator-standard', 'sandbox-operator-cocoon')
  ),
  CONSTRAINT chk_execution_kubernetes_allocations_status CHECK (
    status IN ('materializing', 'bound', 'deleting', 'deleted', 'failed')
  ),
  CONSTRAINT chk_execution_kubernetes_allocations_identity CHECK (
    length(namespace) BETWEEN 1 AND 63
    AND length(sandbox_template_name) BETWEEN 1 AND 63
    AND length(sandbox_warm_pool_name) BETWEEN 1 AND 63
    AND length(configuration_digest) = 64
    AND length(claim_name) BETWEEN 1 AND 63
    AND (claim_uid IS NULL OR length(claim_uid) BETWEEN 1 AND 160)
    AND (sandbox_name IS NULL OR length(sandbox_name) BETWEEN 1 AND 253)
    AND (sandbox_uid IS NULL OR length(sandbox_uid) BETWEEN 1 AND 160)
    AND (pod_name IS NULL OR length(pod_name) BETWEEN 1 AND 253)
    AND (pod_uid IS NULL OR length(pod_uid) BETWEEN 1 AND 160)
    AND (failure_reason_code IS NULL OR length(failure_reason_code) BETWEEN 1 AND 160)
  ),
  CONSTRAINT chk_execution_kubernetes_allocations_binding CHECK (
    (status = 'materializing' AND bound_at IS NULL AND deleted_at IS NULL)
    OR (status = 'bound' AND claim_uid IS NOT NULL AND sandbox_name IS NOT NULL
      AND sandbox_uid IS NOT NULL AND pod_name IS NOT NULL AND pod_uid IS NOT NULL
      AND claim_ready_at IS NOT NULL AND bound_at IS NOT NULL AND deleted_at IS NULL)
    OR (status = 'deleting' AND claim_uid IS NOT NULL AND delete_requested_at IS NOT NULL
      AND deleted_at IS NULL)
    OR (status = 'deleted' AND delete_requested_at IS NOT NULL
      AND deleted_at IS NOT NULL)
    OR status = 'failed'
  ),
  CONSTRAINT chk_execution_kubernetes_allocations_timeline CHECK (
    last_observed_at >= materialization_started_at
    AND (claim_ready_at IS NULL OR claim_ready_at >= materialization_started_at)
    AND (bound_at IS NULL OR bound_at >= materialization_started_at)
    AND (bound_at IS NULL OR claim_ready_at IS NULL OR bound_at >= claim_ready_at)
    AND (delete_requested_at IS NULL OR delete_requested_at >= materialization_started_at)
    AND (deleted_at IS NULL OR (delete_requested_at IS NOT NULL AND deleted_at >= delete_requested_at))
  )
);

CREATE INDEX idx_execution_kubernetes_allocations_target_status
  ON execution_kubernetes_allocations (execution_target_id, status, last_observed_at);

CREATE OR REPLACE FUNCTION enforce_execution_kubernetes_allocation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  generation_target_id UUID;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution Kubernetes allocations cannot be deleted' USING ERRCODE = '23514';
  END IF;
  SELECT execution_target_id INTO generation_target_id
  FROM execution_generation_facts
  WHERE tenant_id = NEW.tenant_id
    AND execution_id = NEW.execution_id
    AND generation = NEW.generation;
  IF generation_target_id IS NULL OR generation_target_id <> NEW.execution_target_id THEN
    RAISE EXCEPTION 'Execution Kubernetes allocation scope is invalid' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.tenant_id <> OLD.tenant_id
       OR NEW.execution_id <> OLD.execution_id
       OR NEW.generation <> OLD.generation
       OR NEW.execution_target_id <> OLD.execution_target_id
       OR NEW.backend <> OLD.backend
       OR NEW.namespace <> OLD.namespace
       OR NEW.sandbox_template_name <> OLD.sandbox_template_name
       OR NEW.sandbox_warm_pool_name <> OLD.sandbox_warm_pool_name
       OR NEW.configuration_digest <> OLD.configuration_digest
       OR NEW.claim_name <> OLD.claim_name
       OR (OLD.claim_uid IS NOT NULL AND NEW.claim_uid IS DISTINCT FROM OLD.claim_uid)
       OR (OLD.sandbox_name IS NOT NULL AND NEW.sandbox_name IS DISTINCT FROM OLD.sandbox_name)
       OR (OLD.sandbox_uid IS NOT NULL AND NEW.sandbox_uid IS DISTINCT FROM OLD.sandbox_uid)
       OR (OLD.pod_name IS NOT NULL AND NEW.pod_name IS DISTINCT FROM OLD.pod_name)
       OR (OLD.pod_uid IS NOT NULL AND NEW.pod_uid IS DISTINCT FROM OLD.pod_uid)
       OR NEW.materialization_started_at <> OLD.materialization_started_at
       OR (OLD.claim_ready_at IS NOT NULL AND NEW.claim_ready_at IS DISTINCT FROM OLD.claim_ready_at)
       OR (OLD.bound_at IS NOT NULL AND NEW.bound_at IS DISTINCT FROM OLD.bound_at)
       OR (OLD.delete_requested_at IS NOT NULL AND NEW.delete_requested_at IS DISTINCT FROM OLD.delete_requested_at)
       OR (OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS DISTINCT FROM OLD.deleted_at)
       OR NEW.created_at <> OLD.created_at THEN
      RAISE EXCEPTION 'Execution Kubernetes allocation identity is immutable after binding' USING ERRCODE = '23514';
    END IF;
    IF OLD.status = 'deleted' AND NEW.status <> 'deleted' THEN
      RAISE EXCEPTION 'Deleted Execution Kubernetes allocation cannot be revived' USING ERRCODE = '23514';
    END IF;
    IF NOT (
      (OLD.status = 'materializing' AND NEW.status IN ('materializing', 'bound', 'deleting', 'failed'))
      OR (OLD.status = 'bound' AND NEW.status IN ('bound', 'deleting', 'failed'))
      OR (OLD.status = 'deleting' AND NEW.status IN ('deleting', 'deleted'))
      OR (OLD.status = 'failed' AND NEW.status IN ('failed', 'deleting', 'deleted'))
      OR (OLD.status = 'deleted' AND NEW.status = 'deleted')
    ) THEN
      RAISE EXCEPTION 'Execution Kubernetes allocation status transition is invalid' USING ERRCODE = '23514';
    END IF;
    IF NEW.last_observed_at < OLD.last_observed_at THEN
      RAISE EXCEPTION 'Execution Kubernetes allocation observation cannot move backwards' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_kubernetes_allocations ON execution_kubernetes_allocations;
CREATE TRIGGER trg_execution_kubernetes_allocations
BEFORE INSERT OR UPDATE OR DELETE ON execution_kubernetes_allocations
FOR EACH ROW EXECUTE FUNCTION enforce_execution_kubernetes_allocation();
