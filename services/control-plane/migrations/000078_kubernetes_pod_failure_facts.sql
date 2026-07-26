ALTER TABLE execution_generation_facts
  ADD COLUMN pod_provisioning_started_at TIMESTAMPTZ,
  ADD COLUMN pod_pending_since_at TIMESTAMPTZ,
  ADD COLUMN pod_running_at TIMESTAMPTZ,
  ADD COLUMN pod_last_observed_at TIMESTAMPTZ,
  ADD CONSTRAINT chk_execution_generation_facts_pod_timeline CHECK (
    (pod_provisioning_started_at IS NULL OR dispatch_requested_at IS NULL
      OR pod_provisioning_started_at >= dispatch_requested_at)
    AND (pod_pending_since_at IS NULL OR pod_provisioning_started_at IS NULL
      OR pod_pending_since_at >= pod_provisioning_started_at)
    AND (pod_running_at IS NULL OR pod_provisioning_started_at IS NULL
      OR pod_running_at >= pod_provisioning_started_at)
    AND (pod_last_observed_at IS NULL OR pod_provisioning_started_at IS NULL
      OR pod_last_observed_at >= pod_provisioning_started_at)
    AND (pod_last_observed_at IS NULL OR pod_pending_since_at IS NULL
      OR pod_last_observed_at >= pod_pending_since_at)
    AND (pod_last_observed_at IS NULL OR pod_running_at IS NULL
      OR pod_last_observed_at >= pod_running_at)
  );

CREATE INDEX idx_execution_generation_facts_pod_provisioning
  ON execution_generation_facts (
    target_kind,
    pod_provisioning_started_at,
    pod_running_at,
    pod_pending_since_at
  );

CREATE TABLE execution_generation_pod_failure_facts (
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL,
  failure_class TEXT NOT NULL,
  execution_target_id UUID NOT NULL,
  namespace TEXT NOT NULL,
  pod_name TEXT NOT NULL,
  pod_uid TEXT,
  reason_code TEXT NOT NULL,
  first_observed_at TIMESTAMPTZ NOT NULL,
  last_observed_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_execution_generation_pod_failure_facts
    PRIMARY KEY (tenant_id, execution_id, generation, failure_class),
  CONSTRAINT fk_execution_generation_pod_failure_generation
    FOREIGN KEY (tenant_id, execution_id, generation)
    REFERENCES execution_generation_facts(tenant_id, execution_id, generation) ON DELETE RESTRICT,
  CONSTRAINT chk_execution_generation_pod_failure_generation CHECK (generation > 0),
  CONSTRAINT chk_execution_generation_pod_failure_class CHECK (
    failure_class IN (
      'pod-apply-failed',
      'pending-timeout',
      'unschedulable',
      'image-pull',
      'container-start',
      'evicted',
      'oom-killed',
      'pod-failed'
    )
  ),
  CONSTRAINT chk_execution_generation_pod_failure_identity CHECK (
    length(namespace) BETWEEN 1 AND 253
    AND length(pod_name) BETWEEN 1 AND 253
    AND (pod_uid IS NULL OR length(pod_uid) BETWEEN 1 AND 160)
    AND length(reason_code) BETWEEN 1 AND 160
    AND ((failure_class = 'pod-apply-failed' AND pod_uid IS NULL)
      OR (failure_class <> 'pod-apply-failed' AND pod_uid IS NOT NULL))
  ),
  CONSTRAINT chk_execution_generation_pod_failure_timeline CHECK (
    last_observed_at >= first_observed_at
  )
);

CREATE INDEX idx_execution_generation_pod_failure_metrics
  ON execution_generation_pod_failure_facts (
    failure_class,
    first_observed_at,
    execution_target_id
  );

CREATE OR REPLACE FUNCTION enforce_execution_generation_fact_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution generation facts cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.tenant_id <> OLD.tenant_id
     OR NEW.execution_id <> OLD.execution_id
     OR NEW.generation <> OLD.generation
     OR NEW.session_id <> OLD.session_id
     OR NEW.turn_id <> OLD.turn_id
     OR NEW.execution_target_id <> OLD.execution_target_id
     OR NEW.target_kind <> OLD.target_kind
     OR NEW.provider <> OLD.provider
     OR NEW.recovery_reason <> OLD.recovery_reason
     OR NEW.warm_pool_mode <> OLD.warm_pool_mode THEN
    RAISE EXCEPTION 'Execution generation fact identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.warm_pool_result <> 'pending' AND NEW.warm_pool_result <> OLD.warm_pool_result THEN
    RAISE EXCEPTION 'Execution generation warm-pool result is immutable after decision' USING ERRCODE = '23514';
  END IF;
  IF OLD.terminal_outcome IS NOT NULL AND (
       NEW.terminal_outcome IS DISTINCT FROM OLD.terminal_outcome
       OR NEW.terminal_at IS DISTINCT FROM OLD.terminal_at
     ) THEN
    RAISE EXCEPTION 'Execution generation terminal outcome is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.pod_provisioning_started_at IS NOT NULL
       AND NEW.pod_provisioning_started_at IS DISTINCT FROM OLD.pod_provisioning_started_at THEN
    RAISE EXCEPTION 'Execution generation Pod provisioning start is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.pod_pending_since_at IS NOT NULL
       AND NEW.pod_pending_since_at IS DISTINCT FROM OLD.pod_pending_since_at THEN
    RAISE EXCEPTION 'Execution generation Pod Pending start is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.pod_running_at IS NOT NULL
       AND NEW.pod_running_at IS DISTINCT FROM OLD.pod_running_at THEN
    RAISE EXCEPTION 'Execution generation Pod Running time is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.pod_last_observed_at IS NOT NULL AND (
       NEW.pod_last_observed_at IS NULL
       OR NEW.pod_last_observed_at < OLD.pod_last_observed_at
     ) THEN
    RAISE EXCEPTION 'Execution generation Pod observation time cannot move backwards' USING ERRCODE = '23514';
  END IF;
  IF (NEW.dispatch_requested_at IS NOT NULL AND NEW.leased_at IS NOT NULL
       AND NEW.dispatch_requested_at > NEW.leased_at)
     OR (NEW.leased_at IS NOT NULL AND NEW.execution_started_at IS NOT NULL
       AND NEW.leased_at > NEW.execution_started_at)
     OR (NEW.execution_started_at IS NOT NULL AND NEW.provider_ready_at IS NOT NULL
       AND NEW.execution_started_at > NEW.provider_ready_at)
     OR (NEW.dispatch_requested_at IS NOT NULL AND NEW.terminal_at IS NOT NULL
       AND NEW.dispatch_requested_at > NEW.terminal_at) THEN
    RAISE EXCEPTION 'Execution generation fact timeline is invalid' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_execution_generation_pod_failure_fact()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  generation_target_id UUID;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution generation Pod failure facts cannot be deleted' USING ERRCODE = '23514';
  END IF;

  SELECT execution_target_id INTO generation_target_id
  FROM execution_generation_facts
  WHERE tenant_id = NEW.tenant_id
    AND execution_id = NEW.execution_id
    AND generation = NEW.generation;
  IF generation_target_id IS NULL OR generation_target_id <> NEW.execution_target_id THEN
    RAISE EXCEPTION 'Execution generation Pod failure fact scope is invalid' USING ERRCODE = '23514';
  END IF;

  IF TG_OP = 'UPDATE' THEN
    IF NEW.tenant_id <> OLD.tenant_id
       OR NEW.execution_id <> OLD.execution_id
       OR NEW.generation <> OLD.generation
       OR NEW.failure_class <> OLD.failure_class
       OR NEW.execution_target_id <> OLD.execution_target_id
       OR NEW.namespace <> OLD.namespace
       OR NEW.pod_name <> OLD.pod_name
       OR NEW.pod_uid IS DISTINCT FROM OLD.pod_uid
       OR NEW.reason_code <> OLD.reason_code
       OR NEW.first_observed_at <> OLD.first_observed_at THEN
      RAISE EXCEPTION 'Execution generation Pod failure fact identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.last_observed_at < OLD.last_observed_at THEN
      RAISE EXCEPTION 'Execution generation Pod failure observation cannot move backwards' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_facts ON execution_generation_pod_failure_facts;
CREATE TRIGGER trg_execution_generation_pod_failure_facts
BEFORE INSERT OR UPDATE OR DELETE ON execution_generation_pod_failure_facts
FOR EACH ROW EXECUTE FUNCTION enforce_execution_generation_pod_failure_fact();
