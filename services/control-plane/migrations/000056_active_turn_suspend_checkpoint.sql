ALTER TABLE execution_control_commands
  DROP CONSTRAINT IF EXISTS execution_control_commands_command_type_check,
  ADD CONSTRAINT execution_control_commands_command_type_check
    CHECK (command_type IN (
      'SteerTurn', 'InterruptTurn', 'CompactSession', 'RollbackSession',
      'ForkSession', 'StartReview', 'StopSession', 'SuspendTurn'
    ));

ALTER TABLE execution_suspend_attempts
  DROP CONSTRAINT IF EXISTS execution_suspend_attempts_reason_check;

ALTER TABLE execution_suspend_attempts
  ADD COLUMN control_command_id UUID,
  ADD COLUMN boundary_meaningful_activity_sequence BIGINT,
  ADD COLUMN active_command_id TEXT,
  ADD COLUMN checkpoint_history_sequence BIGINT,
  ADD COLUMN current_turn_sequence BIGINT,
  ADD COLUMN provider_runtime_binding_id UUID,
  ADD COLUMN provider_checkpoint_protocol TEXT,
  ADD COLUMN provider_cursor_source_execution_id UUID,
  ADD COLUMN provider_cursor_source_generation BIGINT,
  ADD COLUMN provider_cursor_history_sequence BIGINT,
  ADD COLUMN provider_cursor_binding_version INTEGER,
  ADD COLUMN provider_cursor_binding_digest BYTEA,
  ADD COLUMN provider_cursor_sha256 BYTEA,
  ADD COLUMN checkpoint_receipt_sha256 BYTEA,
  ADD COLUMN resume_bundle_id UUID,
  ADD COLUMN resume_generation BIGINT,
  ADD COLUMN resume_bound_at TIMESTAMPTZ,
  ADD COLUMN resume_outcome_unknown_at TIMESTAMPTZ,
  ADD CONSTRAINT fk_execution_suspend_attempts_provider_runtime_binding
    FOREIGN KEY (tenant_id, provider_runtime_binding_id)
    REFERENCES provider_runtime_bindings(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT fk_execution_suspend_attempts_resume_bundle
    FOREIGN KEY (tenant_id, resume_bundle_id)
    REFERENCES execution_recovery_bundles(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT execution_suspend_attempts_reason_check
    CHECK (reason IN ('waiting-keepalive', 'active-idle-timeout')),
  ADD CONSTRAINT chk_execution_suspend_attempts_active_suspend_waiting
    CHECK (
      reason <> 'waiting-keepalive' OR (
        control_command_id IS NULL
        AND boundary_meaningful_activity_sequence IS NULL
        AND active_command_id IS NULL
        AND checkpoint_history_sequence IS NULL
        AND current_turn_sequence IS NULL
        AND provider_runtime_binding_id IS NULL
        AND provider_checkpoint_protocol IS NULL
        AND provider_cursor_source_execution_id IS NULL
        AND provider_cursor_source_generation IS NULL
        AND provider_cursor_history_sequence IS NULL
        AND provider_cursor_binding_version IS NULL
        AND provider_cursor_binding_digest IS NULL
        AND provider_cursor_sha256 IS NULL
        AND checkpoint_receipt_sha256 IS NULL
        AND resume_bundle_id IS NULL
        AND resume_generation IS NULL
        AND resume_bound_at IS NULL
        AND resume_outcome_unknown_at IS NULL
      )
    ),
  ADD CONSTRAINT chk_execution_suspend_attempts_active_suspend_receipt
    CHECK (
      reason <> 'active-idle-timeout' OR (
        control_command_id IS NOT NULL
        AND boundary_meaningful_activity_sequence IS NOT NULL
        AND boundary_meaningful_activity_sequence > 0
        AND (
          provider_quiesced_at IS NULL
          AND active_command_id IS NULL
          AND checkpoint_history_sequence IS NULL
          AND current_turn_sequence IS NULL
          AND provider_runtime_binding_id IS NULL
          AND provider_checkpoint_protocol IS NULL
          AND provider_cursor_source_execution_id IS NULL
          AND provider_cursor_source_generation IS NULL
          AND provider_cursor_history_sequence IS NULL
          AND provider_cursor_binding_version IS NULL
          AND provider_cursor_binding_digest IS NULL
          AND provider_cursor_sha256 IS NULL
          AND checkpoint_receipt_sha256 IS NULL
        )
        OR
        (
          provider_quiesced_at IS NOT NULL
          AND active_command_id IS NOT NULL
          AND length(active_command_id) BETWEEN 1 AND 240
          AND checkpoint_history_sequence IS NOT NULL
          AND checkpoint_history_sequence > 0
          AND current_turn_sequence IS NOT NULL
          AND current_turn_sequence > 0
          AND provider_runtime_binding_id IS NOT NULL
          AND provider_checkpoint_protocol = 'provider-host-suspend-terminal-v1'
          AND provider_cursor_source_execution_id = execution_id
          AND provider_cursor_source_generation = generation
          AND provider_cursor_history_sequence = checkpoint_history_sequence
          AND provider_cursor_binding_version IS NOT NULL
          AND provider_cursor_binding_version > 0
          AND provider_cursor_binding_digest IS NOT NULL
          AND octet_length(provider_cursor_binding_digest) = 32
          AND provider_cursor_sha256 IS NOT NULL
          AND octet_length(provider_cursor_sha256) = 32
          AND checkpoint_receipt_sha256 IS NOT NULL
          AND octet_length(checkpoint_receipt_sha256) = 32
        )
      )
    ),
  ADD CONSTRAINT chk_execution_suspend_attempts_resume_binding
    CHECK (
      ((resume_bundle_id IS NULL) = (resume_generation IS NULL))
      AND ((resume_bundle_id IS NULL) = (resume_bound_at IS NULL))
      AND (resume_bundle_id IS NULL OR (
        reason = 'active-idle-timeout'
        AND status = 'completed'
        AND resume_generation > generation
      ))
      AND (
        resume_outcome_unknown_at IS NULL
        OR (
          resume_bundle_id IS NOT NULL
          AND resume_bound_at IS NOT NULL
          AND resume_outcome_unknown_at >= resume_bound_at
        )
      )
    );

CREATE OR REPLACE FUNCTION enforce_execution_suspend_attempt_quiesce_immutability()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  allow_resume_binding BOOLEAN;
  allow_outcome_unknown BOOLEAN;
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
      OR NEW.control_command_id IS DISTINCT FROM OLD.control_command_id
      OR NEW.boundary_meaningful_activity_sequence IS DISTINCT FROM OLD.boundary_meaningful_activity_sequence
      OR NEW.worker_incarnation IS DISTINCT FROM OLD.worker_incarnation
      OR NEW.worker_instance_uid IS DISTINCT FROM OLD.worker_instance_uid
      OR NEW.worker_cluster_id IS DISTINCT FROM OLD.worker_cluster_id
      OR NEW.worker_namespace IS DISTINCT FROM OLD.worker_namespace
      OR NEW.worker_pod_name IS DISTINCT FROM OLD.worker_pod_name
      OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
      OR NEW.checkpoint_deadline_at IS DISTINCT FROM OLD.checkpoint_deadline_at THEN
      RAISE EXCEPTION 'Execution suspend attempt lineage, Control command, and Pod identity are immutable'
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

    IF (OLD.active_command_id IS NOT NULL
        OR OLD.checkpoint_history_sequence IS NOT NULL
        OR OLD.current_turn_sequence IS NOT NULL
        OR OLD.provider_runtime_binding_id IS NOT NULL
        OR OLD.provider_checkpoint_protocol IS NOT NULL
        OR OLD.provider_cursor_source_execution_id IS NOT NULL
        OR OLD.provider_cursor_source_generation IS NOT NULL
        OR OLD.provider_cursor_history_sequence IS NOT NULL
        OR OLD.provider_cursor_binding_version IS NOT NULL
        OR OLD.provider_cursor_binding_digest IS NOT NULL
        OR OLD.provider_cursor_sha256 IS NOT NULL
        OR OLD.checkpoint_receipt_sha256 IS NOT NULL)
      AND (
        NEW.active_command_id IS DISTINCT FROM OLD.active_command_id
        OR NEW.checkpoint_history_sequence IS DISTINCT FROM OLD.checkpoint_history_sequence
        OR NEW.current_turn_sequence IS DISTINCT FROM OLD.current_turn_sequence
        OR NEW.provider_runtime_binding_id IS DISTINCT FROM OLD.provider_runtime_binding_id
        OR NEW.provider_checkpoint_protocol IS DISTINCT FROM OLD.provider_checkpoint_protocol
        OR NEW.provider_cursor_source_execution_id IS DISTINCT FROM OLD.provider_cursor_source_execution_id
        OR NEW.provider_cursor_source_generation IS DISTINCT FROM OLD.provider_cursor_source_generation
        OR NEW.provider_cursor_history_sequence IS DISTINCT FROM OLD.provider_cursor_history_sequence
        OR NEW.provider_cursor_binding_version IS DISTINCT FROM OLD.provider_cursor_binding_version
        OR NEW.provider_cursor_binding_digest IS DISTINCT FROM OLD.provider_cursor_binding_digest
        OR NEW.provider_cursor_sha256 IS DISTINCT FROM OLD.provider_cursor_sha256
        OR NEW.checkpoint_receipt_sha256 IS DISTINCT FROM OLD.checkpoint_receipt_sha256
      ) THEN
      RAISE EXCEPTION 'Execution suspend attempt active receipt can only be recorded once'
        USING ERRCODE = '23514';
    END IF;

    IF (OLD.resume_bundle_id IS NOT NULL OR OLD.resume_generation IS NOT NULL OR OLD.resume_bound_at IS NOT NULL)
      AND (
        NEW.resume_bundle_id IS DISTINCT FROM OLD.resume_bundle_id
        OR NEW.resume_generation IS DISTINCT FROM OLD.resume_generation
        OR NEW.resume_bound_at IS DISTINCT FROM OLD.resume_bound_at
      ) THEN
      RAISE EXCEPTION 'Execution suspend attempt resume binding can only be recorded once'
        USING ERRCODE = '23514';
    END IF;

    IF OLD.resume_outcome_unknown_at IS NOT NULL
      AND NEW.resume_outcome_unknown_at IS DISTINCT FROM OLD.resume_outcome_unknown_at THEN
      RAISE EXCEPTION 'Execution suspend attempt resume outcome state can only be recorded once'
        USING ERRCODE = '23514';
    END IF;

    allow_resume_binding :=
      OLD.reason = 'active-idle-timeout'
      AND OLD.status = 'completed'
      AND OLD.resume_bundle_id IS NULL
      AND OLD.resume_generation IS NULL
      AND OLD.resume_bound_at IS NULL
      AND OLD.resume_outcome_unknown_at IS NULL
      AND NEW.resume_bundle_id IS NOT NULL
      AND NEW.resume_generation IS NOT NULL
      AND NEW.resume_bound_at IS NOT NULL
      AND NEW.resume_outcome_unknown_at IS NULL
      AND NEW.status IS NOT DISTINCT FROM OLD.status
      AND NEW.provider_quiesced_at IS NOT DISTINCT FROM OLD.provider_quiesced_at
      AND NEW.checkpoint_status IS NOT DISTINCT FROM OLD.checkpoint_status
      AND NEW.checkpoint_ready_at IS NOT DISTINCT FROM OLD.checkpoint_ready_at
      AND NEW.pod_terminal_observed_at IS NOT DISTINCT FROM OLD.pod_terminal_observed_at
      AND NEW.pod_terminal_phase IS NOT DISTINCT FROM OLD.pod_terminal_phase
      AND NEW.completed_at IS NOT DISTINCT FROM OLD.completed_at
      AND NEW.aborted_at IS NOT DISTINCT FROM OLD.aborted_at
      AND NEW.failure_code IS NOT DISTINCT FROM OLD.failure_code
      AND NEW.failure_message IS NOT DISTINCT FROM OLD.failure_message;

    allow_outcome_unknown :=
      OLD.reason = 'active-idle-timeout'
      AND OLD.status = 'completed'
      AND OLD.resume_bundle_id IS NOT NULL
      AND OLD.resume_generation IS NOT NULL
      AND OLD.resume_bound_at IS NOT NULL
      AND OLD.resume_outcome_unknown_at IS NULL
      AND NEW.resume_bundle_id IS NOT DISTINCT FROM OLD.resume_bundle_id
      AND NEW.resume_generation IS NOT DISTINCT FROM OLD.resume_generation
      AND NEW.resume_bound_at IS NOT DISTINCT FROM OLD.resume_bound_at
      AND NEW.resume_outcome_unknown_at IS NOT NULL
      AND NEW.status IS NOT DISTINCT FROM OLD.status
      AND NEW.provider_quiesced_at IS NOT DISTINCT FROM OLD.provider_quiesced_at
      AND NEW.checkpoint_status IS NOT DISTINCT FROM OLD.checkpoint_status
      AND NEW.checkpoint_ready_at IS NOT DISTINCT FROM OLD.checkpoint_ready_at
      AND NEW.pod_terminal_observed_at IS NOT DISTINCT FROM OLD.pod_terminal_observed_at
      AND NEW.pod_terminal_phase IS NOT DISTINCT FROM OLD.pod_terminal_phase
      AND NEW.completed_at IS NOT DISTINCT FROM OLD.completed_at
      AND NEW.aborted_at IS NOT DISTINCT FROM OLD.aborted_at
      AND NEW.failure_code IS NOT DISTINCT FROM OLD.failure_code
      AND NEW.failure_message IS NOT DISTINCT FROM OLD.failure_message;

    IF OLD.status IN ('completed', 'aborted', 'superseded') AND NEW IS DISTINCT FROM OLD THEN
      IF allow_resume_binding OR allow_outcome_unknown THEN
        RETURN NEW;
      END IF;
      RAISE EXCEPTION 'Finished Execution suspend attempts are immutable'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION validate_execution_suspend_attempt_active_suspend()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'INSERT' AND (
    NEW.resume_bundle_id IS NOT NULL
    OR NEW.resume_generation IS NOT NULL
    OR NEW.resume_bound_at IS NOT NULL
    OR NEW.resume_outcome_unknown_at IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'Execution suspend attempt resume binding must be recorded after completion'
      USING ERRCODE = '23514';
  END IF;

	IF NEW.reason = 'active-idle-timeout' THEN
		IF TG_OP = 'INSERT' AND NOT EXISTS (
      SELECT 1
      FROM agent_sessions AS session
      WHERE session.tenant_id = NEW.tenant_id
        AND session.id = NEW.session_id
        AND session.meaningful_activity_sequence = NEW.boundary_meaningful_activity_sequence
    ) THEN
      RAISE EXCEPTION 'Active suspend attempt must freeze the Session meaningful activity sequence'
        USING ERRCODE = '23514';
    END IF;

    IF NOT EXISTS (
      SELECT 1
      FROM execution_control_commands AS command
      WHERE command.tenant_id = NEW.tenant_id
        AND command.id = NEW.control_command_id
        AND command.execution_id = NEW.execution_id
        AND command.session_id = NEW.session_id
        AND command.turn_id = NEW.turn_id
        AND command.command_type = 'SuspendTurn'
        AND command.delivery_worker_id = NEW.worker_id
        AND command.delivery_generation = NEW.generation
    ) THEN
      RAISE EXCEPTION 'Active suspend attempt must reference the exact bound SuspendTurn Control command'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.resume_bundle_id IS NOT NULL AND NOT EXISTS (
    SELECT 1
    FROM execution_recovery_bundles AS bundle
    WHERE bundle.tenant_id = NEW.tenant_id
      AND bundle.id = NEW.resume_bundle_id
      AND bundle.execution_id = NEW.execution_id
      AND bundle.generation = NEW.resume_generation
      AND bundle.recovery_reason = 'suspend-resume'
  ) THEN
    RAISE EXCEPTION 'Recovery-bound suspend attempt must reference a matching suspend-resume Recovery Bundle'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_suspend_attempts_active_suspend_validate ON execution_suspend_attempts;
CREATE TRIGGER trg_execution_suspend_attempts_active_suspend_validate
BEFORE INSERT OR UPDATE OF
  reason,
  session_id,
  turn_id,
  execution_id,
  worker_id,
  generation,
  control_command_id,
  resume_bundle_id,
  resume_generation
ON execution_suspend_attempts
FOR EACH ROW EXECUTE FUNCTION validate_execution_suspend_attempt_active_suspend();
