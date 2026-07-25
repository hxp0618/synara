ALTER TABLE worker_instances
  ADD COLUMN registration_trust_mode TEXT NOT NULL DEFAULT 'shared-token',
  ADD CONSTRAINT chk_worker_instances_registration_trust_mode
    CHECK (registration_trust_mode IN ('shared-token', 'kubernetes-pod-bound-v1'));

COMMENT ON COLUMN worker_instances.registration_trust_mode IS
  'Authoritative Worker registration trust provenance; kubernetes-pod-bound-v1 is required for pod-terminal suspension proof.';

ALTER TABLE execution_suspend_attempts
  ADD COLUMN execution_target_id UUID REFERENCES execution_targets(id) ON DELETE RESTRICT,
  ADD COLUMN completion_mode TEXT NOT NULL DEFAULT 'worker-attested-v1',
  ADD COLUMN worker_incarnation BIGINT,
  ADD COLUMN worker_instance_uid TEXT,
  ADD COLUMN worker_cluster_id TEXT,
  ADD COLUMN worker_namespace TEXT,
  ADD COLUMN worker_pod_name TEXT,
  ADD COLUMN checkpoint_status TEXT,
  ADD COLUMN checkpoint_ready_at TIMESTAMPTZ,
  ADD COLUMN pod_terminal_observed_at TIMESTAMPTZ,
  ADD COLUMN pod_terminal_phase TEXT;

UPDATE execution_suspend_attempts AS attempt
SET execution_target_id = execution.execution_target_id
FROM agent_executions AS execution
WHERE attempt.execution_target_id IS NULL
  AND execution.tenant_id = attempt.tenant_id
  AND execution.id = attempt.execution_id;

ALTER TABLE execution_suspend_attempts
  ADD CONSTRAINT chk_execution_suspend_attempts_completion_mode
    CHECK (completion_mode IN ('worker-attested-v1', 'kubernetes-pod-terminal-v1')),
  ADD CONSTRAINT chk_execution_suspend_attempts_checkpoint
    CHECK (
      (checkpoint_status IS NULL AND checkpoint_ready_at IS NULL) OR (
        checkpoint_status IN ('ready', 'unchanged')
        AND checkpoint_ready_at IS NOT NULL
        AND provider_quiesced_at IS NOT NULL
        AND checkpoint_ready_at >= provider_quiesced_at
        AND checkpoint_ready_at <= checkpoint_deadline_at
        AND (completed_at IS NULL OR checkpoint_ready_at <= completed_at)
        AND (aborted_at IS NULL OR checkpoint_ready_at <= aborted_at)
      )
    ),
  ADD CONSTRAINT chk_execution_suspend_attempts_pod_terminal
    CHECK (
      (
        pod_terminal_phase IS NULL
        AND pod_terminal_observed_at IS NULL
        AND NOT (completion_mode = 'kubernetes-pod-terminal-v1' AND status = 'completed')
      ) OR (
        completion_mode = 'kubernetes-pod-terminal-v1'
        AND status = 'completed'
        AND checkpoint_status IN ('ready', 'unchanged')
        AND checkpoint_ready_at IS NOT NULL
        AND provider_quiesced_at IS NOT NULL
        AND pod_terminal_phase = 'Succeeded'
        AND pod_terminal_observed_at IS NOT NULL
        AND pod_terminal_observed_at >= provider_quiesced_at
        AND pod_terminal_observed_at >= checkpoint_ready_at
        AND completed_at IS NOT NULL
        AND pod_terminal_observed_at <= completed_at
      )
    ),
  ADD CONSTRAINT chk_execution_suspend_attempts_kubernetes_identity
    CHECK (
      completion_mode <> 'kubernetes-pod-terminal-v1' OR (
        execution_target_id IS NOT NULL
        AND execution_target_id <> '00000000-0000-0000-0000-000000000000'::uuid
        AND worker_incarnation IS NOT NULL
        AND worker_incarnation > 0
        AND worker_instance_uid IS NOT NULL
        AND worker_instance_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
        AND worker_cluster_id IS NOT NULL
        AND length(trim(worker_cluster_id)) BETWEEN 1 AND 160
        AND worker_namespace IS NOT NULL
        AND length(trim(worker_namespace)) BETWEEN 1 AND 160
        AND worker_pod_name IS NOT NULL
        AND length(trim(worker_pod_name)) BETWEEN 1 AND 253
      )
    );

CREATE OR REPLACE FUNCTION enforce_execution_suspend_attempt_quiesce_immutability()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF NEW.id IS DISTINCT FROM OLD.id
      OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
      OR NEW.session_id IS DISTINCT FROM OLD.session_id
      OR NEW.turn_id IS DISTINCT FROM OLD.turn_id
      OR NEW.execution_id IS DISTINCT FROM OLD.execution_id
      OR NEW.worker_id IS DISTINCT FROM OLD.worker_id
      OR NEW.execution_target_id IS DISTINCT FROM OLD.execution_target_id
      OR NEW.generation IS DISTINCT FROM OLD.generation
      OR NEW.reason IS DISTINCT FROM OLD.reason
      OR NEW.completion_mode IS DISTINCT FROM OLD.completion_mode
      OR NEW.worker_incarnation IS DISTINCT FROM OLD.worker_incarnation
      OR NEW.worker_instance_uid IS DISTINCT FROM OLD.worker_instance_uid
      OR NEW.worker_cluster_id IS DISTINCT FROM OLD.worker_cluster_id
      OR NEW.worker_namespace IS DISTINCT FROM OLD.worker_namespace
      OR NEW.worker_pod_name IS DISTINCT FROM OLD.worker_pod_name
      OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
      OR NEW.checkpoint_deadline_at IS DISTINCT FROM OLD.checkpoint_deadline_at THEN
      RAISE EXCEPTION 'Execution suspend attempt lineage and Pod identity are immutable'
        USING ERRCODE = '23514';
    END IF;

    IF OLD.provider_quiesced_at IS NOT NULL
      AND NEW.provider_quiesced_at IS DISTINCT FROM OLD.provider_quiesced_at THEN
      RAISE EXCEPTION 'Execution suspend attempt provider quiesce can only be recorded once'
        USING ERRCODE = '23514';
    END IF;

    IF (OLD.checkpoint_status IS NOT NULL OR OLD.checkpoint_ready_at IS NOT NULL)
      AND (
        NEW.checkpoint_status IS DISTINCT FROM OLD.checkpoint_status
        OR NEW.checkpoint_ready_at IS DISTINCT FROM OLD.checkpoint_ready_at
      ) THEN
      RAISE EXCEPTION 'Execution suspend attempt checkpoint readiness can only be recorded once'
        USING ERRCODE = '23514';
    END IF;

    IF (OLD.pod_terminal_phase IS NOT NULL OR OLD.pod_terminal_observed_at IS NOT NULL)
      AND (
        NEW.pod_terminal_phase IS DISTINCT FROM OLD.pod_terminal_phase
        OR NEW.pod_terminal_observed_at IS DISTINCT FROM OLD.pod_terminal_observed_at
      ) THEN
      RAISE EXCEPTION 'Execution suspend attempt Pod terminal proof can only be recorded once'
        USING ERRCODE = '23514';
    END IF;

    IF OLD.status IN ('completed', 'aborted', 'superseded') AND NEW IS DISTINCT FROM OLD THEN
      RAISE EXCEPTION 'Finished Execution suspend attempts are immutable'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION validate_execution_suspend_attempt_kubernetes_identity()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.completion_mode = 'kubernetes-pod-terminal-v1' THEN
    IF NOT EXISTS (
      SELECT 1
      FROM agent_executions AS execution
      JOIN worker_instances AS worker
        ON worker.id = NEW.worker_id
      WHERE execution.tenant_id = NEW.tenant_id
        AND execution.id = NEW.execution_id
        AND execution.session_id = NEW.session_id
        AND execution.turn_id = NEW.turn_id
        AND execution.generation = NEW.generation
        AND execution.worker_id = NEW.worker_id
        AND execution.execution_target_id = NEW.execution_target_id
        AND execution.target_kind = 'kubernetes'
        AND worker.execution_target_id = NEW.execution_target_id
        AND worker.target_kind = 'kubernetes'
        AND worker.registration_trust_mode = 'kubernetes-pod-bound-v1'
        AND worker.incarnation = NEW.worker_incarnation
        AND worker.instance_uid = NEW.worker_instance_uid
        AND worker.cluster_id = NEW.worker_cluster_id
        AND worker.namespace = NEW.worker_namespace
        AND worker.pod_name = NEW.worker_pod_name
    ) THEN
      RAISE EXCEPTION 'Kubernetes suspend attempt must capture the exact Pod-bound Worker snapshot'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_suspend_attempts_kubernetes_identity ON execution_suspend_attempts;
CREATE TRIGGER trg_execution_suspend_attempts_kubernetes_identity
BEFORE INSERT OR UPDATE OF
  session_id,
  turn_id,
  execution_id,
  worker_id,
  execution_target_id,
  generation,
  completion_mode,
  worker_incarnation,
  worker_instance_uid,
  worker_cluster_id,
  worker_namespace,
  worker_pod_name
ON execution_suspend_attempts
FOR EACH ROW EXECUTE FUNCTION validate_execution_suspend_attempt_kubernetes_identity();

COMMENT ON COLUMN execution_suspend_attempts.completion_mode IS
  'worker-attested-v1 trusts signed containment; kubernetes-pod-terminal-v1 requires exact Pod identity, checkpoint handoff, and Succeeded Pod terminal proof.';

COMMENT ON COLUMN execution_suspend_attempts.checkpoint_status IS
  'A durable Worker-authored checkpoint handoff recorded exactly once before Pod-terminal completion.';

COMMENT ON COLUMN execution_suspend_attempts.pod_terminal_phase IS
  'The only terminal Kubernetes phase accepted for Pod-terminal suspend proof is Succeeded.';
