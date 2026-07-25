ALTER TABLE agent_executions
  DROP CONSTRAINT IF EXISTS agent_executions_status_check;

ALTER TABLE agent_executions
  ADD COLUMN next_recovery_reason TEXT,
  ADD CONSTRAINT agent_executions_status_check
    CHECK (status IN (
      'queued', 'leased', 'running', 'waiting-for-approval', 'recovering', 'suspended',
      'completed', 'failed', 'cancelled', 'interrupted'
    )),
  ADD CONSTRAINT chk_agent_executions_next_recovery_reason
    CHECK (
      next_recovery_reason IS NULL OR
      (next_recovery_reason = 'suspend-resume' AND status IN ('suspended', 'recovering'))
    ),
  ADD CONSTRAINT chk_agent_executions_resource_suspend_shape
    CHECK (
      status <> 'suspended' OR
      (worker_id IS NULL AND next_recovery_reason = 'suspend-resume')
    );

DROP INDEX IF EXISTS uq_agent_executions_session_active;
CREATE UNIQUE INDEX uq_agent_executions_session_active
  ON agent_executions (tenant_id, session_id)
  WHERE status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering', 'suspended');

CREATE TABLE execution_suspend_attempts (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  session_id UUID NOT NULL,
  turn_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  worker_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE RESTRICT,
  generation BIGINT NOT NULL CHECK (generation > 0),
  reason TEXT NOT NULL CHECK (reason = 'waiting-keepalive'),
  status TEXT NOT NULL CHECK (status IN ('checkpointing', 'completed', 'aborted', 'superseded')),
  requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  checkpoint_deadline_at TIMESTAMPTZ NOT NULL,
  provider_quiesced_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  aborted_at TIMESTAMPTZ,
  failure_code TEXT,
  failure_message TEXT,
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, session_id, turn_id)
    REFERENCES agent_turns(tenant_id, session_id, id) ON DELETE CASCADE,
  CHECK (checkpoint_deadline_at > requested_at),
  CHECK (provider_quiesced_at IS NULL OR provider_quiesced_at >= requested_at),
  CHECK (provider_quiesced_at IS NULL OR provider_quiesced_at <= checkpoint_deadline_at),
  CHECK (failure_code IS NULL OR length(failure_code) BETWEEN 1 AND 160),
  CHECK (failure_message IS NULL OR length(failure_message) <= 2000),
  CHECK (
    (status = 'checkpointing' AND completed_at IS NULL AND aborted_at IS NULL AND failure_code IS NULL AND failure_message IS NULL) OR
    (status = 'completed' AND provider_quiesced_at IS NOT NULL AND completed_at IS NOT NULL AND provider_quiesced_at <= completed_at AND aborted_at IS NULL AND failure_code IS NULL AND failure_message IS NULL) OR
    (status IN ('aborted', 'superseded') AND completed_at IS NULL AND aborted_at IS NOT NULL AND failure_code IS NOT NULL AND (provider_quiesced_at IS NULL OR provider_quiesced_at <= aborted_at))
  )
);

CREATE UNIQUE INDEX uq_execution_suspend_attempts_active
  ON execution_suspend_attempts (tenant_id, execution_id, generation)
  WHERE status = 'checkpointing';

CREATE INDEX idx_execution_suspend_attempts_execution
  ON execution_suspend_attempts (tenant_id, execution_id, requested_at DESC, id);

CREATE INDEX idx_execution_suspend_attempts_deadline
  ON execution_suspend_attempts (checkpoint_deadline_at, tenant_id, execution_id, id)
  WHERE status = 'checkpointing';

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
      OR NEW.generation IS DISTINCT FROM OLD.generation
      OR NEW.reason IS DISTINCT FROM OLD.reason
      OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
      OR NEW.checkpoint_deadline_at IS DISTINCT FROM OLD.checkpoint_deadline_at THEN
      RAISE EXCEPTION 'Execution suspend attempt lineage is immutable'
        USING ERRCODE = '23514';
    END IF;

    IF OLD.provider_quiesced_at IS NOT NULL
      AND NEW.provider_quiesced_at IS DISTINCT FROM OLD.provider_quiesced_at THEN
      RAISE EXCEPTION 'Execution suspend attempt provider quiesce can only be recorded once'
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

CREATE TRIGGER trg_execution_suspend_attempts_quiesce_immutable
BEFORE UPDATE
ON execution_suspend_attempts
FOR EACH ROW EXECUTE FUNCTION enforce_execution_suspend_attempt_quiesce_immutability();

CREATE OR REPLACE FUNCTION assert_suspended_execution_has_no_lease()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.status = 'suspended' AND EXISTS (
    SELECT 1 FROM worker_leases AS lease
    WHERE lease.tenant_id = NEW.tenant_id AND lease.execution_id = NEW.id
  ) THEN
    RAISE EXCEPTION 'suspended Execution cannot retain a Worker lease'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_executions_suspended_no_lease
BEFORE INSERT OR UPDATE OF status ON agent_executions
FOR EACH ROW EXECUTE FUNCTION assert_suspended_execution_has_no_lease();

ALTER TABLE execution_interactions
  ADD COLUMN resume_bundle_id UUID,
  ADD COLUMN resume_generation BIGINT,
  ADD COLUMN resume_bound_at TIMESTAMPTZ,
  ADD CONSTRAINT fk_execution_interactions_resume_bundle
    FOREIGN KEY (tenant_id, resume_bundle_id)
    REFERENCES execution_recovery_bundles(tenant_id, id) ON DELETE RESTRICT,
  DROP CONSTRAINT IF EXISTS chk_execution_interactions_delivery_status,
  DROP CONSTRAINT IF EXISTS chk_execution_interactions_delivery_state,
  DROP CONSTRAINT IF EXISTS chk_execution_interactions_delivered_at,
  DROP CONSTRAINT IF EXISTS chk_execution_interactions_acknowledged_at,
  ADD CONSTRAINT chk_execution_interactions_resume_generation
    CHECK (resume_generation IS NULL OR resume_generation > 0);

ALTER TABLE execution_interactions
  ADD CONSTRAINT chk_execution_interactions_delivery_status
    CHECK (delivery_status IN (
      'not-ready', 'pending', 'delivered', 'acknowledged', 'failed', 'superseded',
      'resume-recorded', 'resume-bound', 'outcome-unknown'
    )),
  ADD CONSTRAINT chk_execution_interactions_delivery_state
    CHECK (
      (status = 'pending' AND resolution_kind IS NULL AND resolution_command_id IS NULL
        AND delivery_status = 'not-ready' AND delivery_worker_id IS NULL AND delivery_generation IS NULL
        AND delivery_available_at IS NULL AND delivered_at IS NULL AND acknowledged_at IS NULL
        AND resume_bundle_id IS NULL AND resume_generation IS NULL AND resume_bound_at IS NULL) OR
      (status = 'resolved' AND resolution_kind IS NOT NULL AND resolution_command_id IS NOT NULL
        AND delivery_status IN ('pending', 'delivered', 'acknowledged', 'failed', 'superseded')
        AND delivery_worker_id IS NOT NULL AND delivery_generation IS NOT NULL
        AND delivery_available_at IS NOT NULL
        AND resume_bundle_id IS NULL AND resume_generation IS NULL AND resume_bound_at IS NULL) OR
      (status = 'resolved' AND resolution_kind IS NOT NULL AND resolution_command_id IS NOT NULL
        AND delivery_status = 'resume-recorded'
        AND delivery_worker_id IS NULL AND delivery_generation IS NULL
        AND delivery_available_at IS NOT NULL AND delivered_at IS NULL AND acknowledged_at IS NULL
        AND resume_bundle_id IS NULL AND resume_generation IS NULL AND resume_bound_at IS NULL) OR
      (status = 'resolved' AND resolution_kind IS NOT NULL AND resolution_command_id IS NOT NULL
        AND delivery_status = 'resume-bound'
        AND delivery_worker_id IS NULL AND delivery_generation IS NULL
        AND delivery_available_at IS NOT NULL AND delivered_at IS NULL AND acknowledged_at IS NULL
        AND resume_bundle_id IS NOT NULL AND resume_generation IS NOT NULL AND resume_bound_at IS NOT NULL) OR
      (status = 'resolved' AND resolution_kind IS NOT NULL AND resolution_command_id IS NOT NULL
        AND delivery_status = 'outcome-unknown' AND delivery_error IS NOT NULL
        AND acknowledged_at IS NULL AND (
          (delivery_worker_id IS NOT NULL AND delivery_generation IS NOT NULL AND delivered_at IS NOT NULL
            AND resume_bundle_id IS NULL AND resume_generation IS NULL AND resume_bound_at IS NULL) OR
          (delivery_worker_id IS NULL AND delivery_generation IS NULL AND delivered_at IS NULL
            AND resume_bundle_id IS NOT NULL AND resume_generation IS NOT NULL AND resume_bound_at IS NOT NULL)
        )) OR
      (status = 'expired' AND resolution_kind IS NULL AND delivery_status IN ('not-ready', 'superseded')
        AND resume_bundle_id IS NULL AND resume_generation IS NULL AND resume_bound_at IS NULL)
    ),
  ADD CONSTRAINT chk_execution_interactions_delivered_at
    CHECK (delivery_status NOT IN ('delivered', 'acknowledged') OR delivered_at IS NOT NULL),
  ADD CONSTRAINT chk_execution_interactions_acknowledged_at
    CHECK (delivery_status <> 'acknowledged' OR acknowledged_at IS NOT NULL);

CREATE INDEX idx_execution_interactions_resume_unbound
  ON execution_interactions (tenant_id, execution_id, resolved_at, id)
  WHERE status = 'resolved' AND delivery_status = 'resume-recorded';

CREATE OR REPLACE FUNCTION enforce_execution_interaction_resume_binding()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF OLD.delivery_status = 'resume-bound' AND (
      NEW.delivery_status NOT IN ('resume-bound', 'outcome-unknown') OR
      NEW.resume_bundle_id IS DISTINCT FROM OLD.resume_bundle_id OR
      NEW.resume_generation IS DISTINCT FROM OLD.resume_generation OR
      NEW.resume_bound_at IS DISTINCT FROM OLD.resume_bound_at
    ) THEN
      RAISE EXCEPTION 'Recovery-bound Interaction resolution cannot be rebound or replayed'
        USING ERRCODE = '23514';
    END IF;

    IF OLD.delivery_status = 'outcome-unknown' AND (
      NEW.delivery_status <> OLD.delivery_status OR
      NEW.resume_bundle_id IS DISTINCT FROM OLD.resume_bundle_id OR
      NEW.resume_generation IS DISTINCT FROM OLD.resume_generation OR
      NEW.resume_bound_at IS DISTINCT FROM OLD.resume_bound_at
    ) THEN
      RAISE EXCEPTION 'Outcome-unknown Interaction resolution is terminal'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.delivery_status IN ('resume-bound', 'outcome-unknown')
    AND NEW.resume_bundle_id IS NOT NULL
    AND NOT EXISTS (
      SELECT 1
      FROM execution_recovery_bundles AS bundle
      WHERE bundle.tenant_id = NEW.tenant_id
        AND bundle.id = NEW.resume_bundle_id
        AND bundle.execution_id = NEW.execution_id
        AND bundle.generation = NEW.resume_generation
    ) THEN
    RAISE EXCEPTION 'Recovery-bound Interaction resolution must reference its exact Execution generation Bundle'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_interactions_resume_binding
BEFORE INSERT OR UPDATE
ON execution_interactions
FOR EACH ROW EXECUTE FUNCTION enforce_execution_interaction_resume_binding();

COMMENT ON TABLE execution_suspend_attempts IS
  'Generation-fenced resource suspend checkpoint handshakes for waiting approvals and active-turn idle reclaim. Completion atomically releases the Worker lease and makes the Execution scale-to-zero safe.';

COMMENT ON COLUMN execution_interactions.delivery_status IS
  'resume-recorded is an unbound post-fence resolution; resume-bound records the single immutable Recovery Bundle that consumed it; outcome-unknown is terminal when Provider application cannot be proven exactly once.';

COMMENT ON COLUMN execution_interactions.resume_bundle_id IS
  'The one immutable Recovery Bundle that consumed a resume-recorded resolution. A bound resolution is never eligible for another Bundle.';
