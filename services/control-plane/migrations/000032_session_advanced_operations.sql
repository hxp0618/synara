ALTER TABLE agent_turns
  ADD COLUMN turn_kind TEXT NOT NULL DEFAULT 'message';

ALTER TABLE agent_turns
  DROP CONSTRAINT IF EXISTS agent_turns_input_text_check,
  ADD CONSTRAINT chk_agent_turns_turn_kind
    CHECK (turn_kind IN ('message', 'compact', 'review', 'rollback', 'fork')),
  ADD CONSTRAINT chk_agent_turns_input_shape
    CHECK (
      (turn_kind = 'message' AND length(input_text) BETWEEN 1 AND 1000000)
      OR
      (turn_kind <> 'message' AND input_text = '')
    );

CREATE OR REPLACE FUNCTION protect_agent_turn_kind()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.turn_kind <> OLD.turn_kind THEN
    RAISE EXCEPTION 'Turn kind is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_turns_kind_immutable ON agent_turns;
CREATE TRIGGER trg_agent_turns_kind_immutable
BEFORE UPDATE OF turn_kind ON agent_turns
FOR EACH ROW EXECUTE FUNCTION protect_agent_turn_kind();

ALTER TABLE agent_sessions
  ADD COLUMN fork_source_session_id UUID,
  ADD COLUMN fork_source_turn_id UUID,
  ADD COLUMN fork_source_event_sequence BIGINT,
  ADD COLUMN fork_strategy TEXT,
  ADD CONSTRAINT chk_agent_sessions_fork_lineage_shape
    CHECK (
      (
        fork_source_session_id IS NULL
        AND fork_source_turn_id IS NULL
        AND fork_source_event_sequence IS NULL
        AND fork_strategy IS NULL
      )
      OR
      (
        fork_source_session_id IS NOT NULL
        AND fork_source_event_sequence IS NOT NULL
        AND fork_source_event_sequence >= 0
        AND fork_strategy IS NOT NULL
        AND fork_strategy IN ('emulated', 'native')
        AND last_event_sequence >= fork_source_event_sequence
        AND (fork_source_event_sequence > 0 OR fork_source_turn_id IS NULL)
      )
    ),
  ADD CONSTRAINT chk_agent_sessions_fork_not_self
    CHECK (fork_source_session_id IS NULL OR fork_source_session_id <> id),
  ADD CONSTRAINT fk_agent_sessions_fork_source_session
    FOREIGN KEY (tenant_id, project_id, fork_source_session_id)
    REFERENCES agent_sessions(tenant_id, project_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED,
  ADD CONSTRAINT fk_agent_sessions_fork_source_turn
    FOREIGN KEY (tenant_id, fork_source_turn_id)
    REFERENCES agent_turns(tenant_id, id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX idx_agent_sessions_fork_source
  ON agent_sessions (tenant_id, fork_source_session_id)
  WHERE fork_source_session_id IS NOT NULL;

CREATE OR REPLACE FUNCTION assert_agent_session_fork_lineage()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  source_last_sequence BIGINT;
BEGIN
  IF NEW.fork_source_session_id IS NULL THEN
    RETURN NEW;
  END IF;

  SELECT source.last_event_sequence
  INTO source_last_sequence
  FROM agent_sessions AS source
  WHERE source.tenant_id = NEW.tenant_id
    AND source.project_id = NEW.project_id
    AND source.id = NEW.fork_source_session_id;

  IF source_last_sequence IS NULL OR NEW.fork_source_event_sequence > source_last_sequence THEN
    RAISE EXCEPTION 'Fork source sequence is outside the source Session history'
      USING ERRCODE = '23514';
  END IF;

  IF EXISTS (
    WITH RECURSIVE source_lineage AS (
      SELECT
        source.id AS session_id,
        source.fork_source_session_id,
        ARRAY[NEW.id, source.id]::UUID[] AS path,
        source.id = NEW.id AS cycle
      FROM agent_sessions AS source
      WHERE source.tenant_id = NEW.tenant_id
        AND source.project_id = NEW.project_id
        AND source.id = NEW.fork_source_session_id

      UNION ALL

      SELECT
        parent.id AS session_id,
        parent.fork_source_session_id,
        lineage.path || parent.id,
        parent.id = ANY(lineage.path) AS cycle
      FROM source_lineage AS lineage
      JOIN agent_sessions AS parent
        ON parent.tenant_id = NEW.tenant_id
       AND parent.project_id = NEW.project_id
       AND parent.id = lineage.fork_source_session_id
      WHERE NOT lineage.cycle
    )
    SELECT 1
    FROM source_lineage
    WHERE cycle
  ) THEN
    RAISE EXCEPTION 'Fork lineage cannot contain a Session cycle'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.fork_source_turn_id IS NOT NULL AND NOT EXISTS (
    WITH RECURSIVE source_lineage AS (
      SELECT
        source.id AS session_id,
        NEW.fork_source_event_sequence AS through_sequence,
        0 AS depth,
        ARRAY[source.id]::UUID[] AS path
      FROM agent_sessions AS source
      WHERE source.tenant_id = NEW.tenant_id
        AND source.id = NEW.fork_source_session_id

      UNION ALL

      SELECT
        parent.id AS session_id,
        LEAST(lineage.through_sequence, child.fork_source_event_sequence) AS through_sequence,
        lineage.depth + 1 AS depth,
        lineage.path || parent.id
      FROM source_lineage AS lineage
      JOIN agent_sessions AS child
        ON child.tenant_id = NEW.tenant_id
       AND child.id = lineage.session_id
      JOIN agent_sessions AS parent
        ON parent.tenant_id = NEW.tenant_id
       AND parent.id = child.fork_source_session_id
      WHERE child.fork_source_session_id IS NOT NULL
        AND child.fork_source_event_sequence IS NOT NULL
        AND lineage.depth + 1 < 32
        AND NOT parent.id = ANY(lineage.path)
    )
    SELECT 1
    FROM source_lineage AS lineage
    JOIN session_events AS source_event
      ON source_event.tenant_id = NEW.tenant_id
     AND source_event.session_id = lineage.session_id
     AND source_event.sequence <= lineage.through_sequence
    WHERE source_event.event_type = 'turn.created'
      AND source_event.payload ->> 'turnId' = NEW.fork_source_turn_id::TEXT
  ) THEN
    RAISE EXCEPTION 'Fork source Turn is outside the selected logical source Session history prefix'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_sessions_fork_lineage ON agent_sessions;
CREATE CONSTRAINT TRIGGER trg_agent_sessions_fork_lineage
AFTER INSERT OR UPDATE OF
  tenant_id,
  project_id,
  fork_source_session_id,
  fork_source_turn_id,
  fork_source_event_sequence,
  fork_strategy
ON agent_sessions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_agent_session_fork_lineage();

CREATE OR REPLACE FUNCTION protect_agent_session_fork_lineage()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.fork_source_session_id IS DISTINCT FROM OLD.fork_source_session_id
    OR NEW.fork_source_turn_id IS DISTINCT FROM OLD.fork_source_turn_id
    OR NEW.fork_source_event_sequence IS DISTINCT FROM OLD.fork_source_event_sequence
    OR NEW.fork_strategy IS DISTINCT FROM OLD.fork_strategy THEN
    RAISE EXCEPTION 'Fork lineage is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_sessions_fork_lineage_immutable ON agent_sessions;
CREATE TRIGGER trg_agent_sessions_fork_lineage_immutable
BEFORE UPDATE OF
  fork_source_session_id,
  fork_source_turn_id,
  fork_source_event_sequence,
  fork_strategy
ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION protect_agent_session_fork_lineage();

ALTER TABLE execution_control_commands
  DROP CONSTRAINT IF EXISTS execution_control_commands_status_check,
  ADD CONSTRAINT execution_control_commands_status_check
    CHECK (status IN ('pending', 'delivered', 'acknowledged', 'superseded', 'outcome_unknown')),
  ADD CONSTRAINT chk_execution_control_commands_payload_object
    CHECK (COALESCE(jsonb_typeof(payload) = 'object', FALSE)),
  ADD CONSTRAINT chk_execution_control_commands_delivery_time_order
    CHECK (delivered_at IS NULL OR delivered_at >= requested_at),
  ADD CONSTRAINT chk_execution_control_commands_ack_time_order
    CHECK (
      acknowledged_at IS NULL
      OR (delivered_at IS NOT NULL AND acknowledged_at >= delivered_at)
    ),
  ADD CONSTRAINT chk_execution_control_commands_outcome_unknown
    CHECK (
      status <> 'outcome_unknown'
      OR (delivered_at IS NOT NULL AND acknowledged_at IS NULL)
    );

CREATE UNIQUE INDEX uq_execution_control_commands_primary_operation
  ON execution_control_commands (tenant_id, execution_id)
  WHERE command_type IN ('CompactSession', 'RollbackSession', 'ForkSession', 'StartReview');

CREATE OR REPLACE FUNCTION primary_command_turn_kind(command_type_value TEXT)
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT CASE command_type_value
    WHEN 'CompactSession' THEN 'compact'
    WHEN 'RollbackSession' THEN 'rollback'
    WHEN 'ForkSession' THEN 'fork'
    WHEN 'StartReview' THEN 'review'
    ELSE NULL
  END;
$$;

CREATE OR REPLACE FUNCTION assert_primary_command_matches_turn_kind()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  expected_kind TEXT;
  actual_kind TEXT;
BEGIN
  expected_kind := primary_command_turn_kind(NEW.command_type);
  IF expected_kind IS NULL THEN
    RETURN NEW;
  END IF;

  SELECT turn.turn_kind
  INTO actual_kind
  FROM agent_executions AS execution
  JOIN agent_turns AS turn
    ON turn.tenant_id = execution.tenant_id
   AND turn.session_id = execution.session_id
   AND turn.id = execution.turn_id
  WHERE execution.tenant_id = NEW.tenant_id
    AND execution.id = NEW.execution_id
    AND execution.session_id = NEW.session_id
    AND execution.turn_id = NEW.turn_id;

  IF actual_kind IS DISTINCT FROM expected_kind THEN
    RAISE EXCEPTION 'Primary Control command does not match the Execution Turn kind'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_control_commands_primary_kind ON execution_control_commands;
CREATE CONSTRAINT TRIGGER trg_execution_control_commands_primary_kind
AFTER INSERT OR UPDATE OF tenant_id, execution_id, session_id, turn_id, command_type
ON execution_control_commands
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_primary_command_matches_turn_kind();

CREATE OR REPLACE FUNCTION assert_special_turn_has_primary_command()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  actual_kind TEXT;
  expected_command_type TEXT;
BEGIN
  SELECT turn.turn_kind
  INTO actual_kind
  FROM agent_turns AS turn
  WHERE turn.tenant_id = NEW.tenant_id
    AND turn.session_id = NEW.session_id
    AND turn.id = NEW.turn_id;

  expected_command_type := CASE actual_kind
    WHEN 'compact' THEN 'CompactSession'
    WHEN 'rollback' THEN 'RollbackSession'
    WHEN 'fork' THEN 'ForkSession'
    WHEN 'review' THEN 'StartReview'
    ELSE NULL
  END;
  IF expected_command_type IS NULL THEN
    RETURN NEW;
  END IF;

  IF NOT EXISTS (
    SELECT 1
    FROM execution_control_commands AS command
    WHERE command.tenant_id = NEW.tenant_id
      AND command.execution_id = NEW.id
      AND command.session_id = NEW.session_id
      AND command.turn_id = NEW.turn_id
      AND command.command_type = expected_command_type
  ) THEN
    RAISE EXCEPTION 'Special Turn Execution requires one matching primary Control command'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_executions_primary_command ON agent_executions;
CREATE CONSTRAINT TRIGGER trg_agent_executions_primary_command
AFTER INSERT OR UPDATE OF tenant_id, session_id, turn_id ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_special_turn_has_primary_command();

CREATE OR REPLACE FUNCTION assert_primary_command_preserved()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  expected_command_type TEXT;
BEGIN
  IF primary_command_turn_kind(OLD.command_type) IS NULL THEN
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    RETURN NEW;
  END IF;

  SELECT CASE turn.turn_kind
    WHEN 'compact' THEN 'CompactSession'
    WHEN 'rollback' THEN 'RollbackSession'
    WHEN 'fork' THEN 'ForkSession'
    WHEN 'review' THEN 'StartReview'
    ELSE NULL
  END
  INTO expected_command_type
  FROM agent_executions AS execution
  JOIN agent_turns AS turn
    ON turn.tenant_id = execution.tenant_id
   AND turn.session_id = execution.session_id
   AND turn.id = execution.turn_id
  WHERE execution.tenant_id = OLD.tenant_id
    AND execution.id = OLD.execution_id;

  IF expected_command_type IS NOT NULL AND NOT EXISTS (
    SELECT 1
    FROM execution_control_commands AS command
    WHERE command.tenant_id = OLD.tenant_id
      AND command.execution_id = OLD.execution_id
      AND command.session_id = OLD.session_id
      AND command.turn_id = OLD.turn_id
      AND command.command_type = expected_command_type
  ) THEN
    RAISE EXCEPTION 'Special Turn Execution must retain its matching primary Control command'
      USING ERRCODE = '23514';
  END IF;

  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_control_commands_primary_preserved ON execution_control_commands;
CREATE CONSTRAINT TRIGGER trg_execution_control_commands_primary_preserved
AFTER UPDATE OR DELETE ON execution_control_commands
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_primary_command_preserved();

COMMENT ON COLUMN agent_turns.turn_kind IS
  'Message Turns use SendTurn; advanced kinds carry one durable primary Provider operation.';
COMMENT ON COLUMN agent_sessions.fork_source_session_id IS
  'Immutable source Session for a zero-copy logical history Fork.';
COMMENT ON COLUMN agent_sessions.fork_source_event_sequence IS
  'Inclusive source Session Event boundary captured atomically when the Fork is created.';
COMMENT ON COLUMN agent_sessions.fork_strategy IS
  'Whether the Fork lineage was created by authoritative-history emulation or a native Provider optimization.';
