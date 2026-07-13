ALTER TABLE agent_sessions
  ADD COLUMN provider_resume_cursor_state TEXT,
  ADD COLUMN provider_resume_cursor_source_execution_id UUID,
  ADD COLUMN provider_resume_cursor_source_generation BIGINT,
  ADD COLUMN provider_resume_cursor_history_sequence BIGINT;

UPDATE agent_sessions
SET
  provider_resume_cursor_encrypted = CASE
    WHEN COALESCE(octet_length(provider_resume_cursor_encrypted), 0) = 0 THEN NULL
    ELSE provider_resume_cursor_encrypted
  END,
  provider_resume_cursor_state = CASE
    WHEN COALESCE(octet_length(provider_resume_cursor_encrypted), 0) > 0 THEN 'quarantined'
    ELSE 'absent'
  END,
  provider_resume_cursor_source_execution_id = NULL,
  provider_resume_cursor_source_generation = NULL,
  provider_resume_cursor_history_sequence = NULL;

UPDATE provider_runtime_bindings AS binding
SET
  resume_strategy = 'authoritative-history',
  cursor_compatibility_key = NULL,
  cursor_updated_at = NULL,
  updated_at = now()
FROM agent_sessions AS session
WHERE session.tenant_id = binding.tenant_id
  AND session.id = binding.session_id
  AND session.provider_resume_cursor_state = 'quarantined'
  AND binding.status <> 'released';

ALTER TABLE agent_sessions
  ALTER COLUMN provider_resume_cursor_state SET DEFAULT 'absent',
  ALTER COLUMN provider_resume_cursor_state SET NOT NULL,
  ADD CONSTRAINT chk_agent_sessions_provider_resume_cursor_state
    CHECK (provider_resume_cursor_state IN ('absent', 'usable', 'quarantined')),
  ADD CONSTRAINT chk_agent_sessions_provider_resume_cursor_metadata_all_or_none
    CHECK (
      (
        provider_resume_cursor_source_execution_id IS NULL
        AND provider_resume_cursor_source_generation IS NULL
        AND provider_resume_cursor_history_sequence IS NULL
      )
      OR
      (
        provider_resume_cursor_source_execution_id IS NOT NULL
        AND provider_resume_cursor_source_generation IS NOT NULL
        AND provider_resume_cursor_source_generation > 0
        AND provider_resume_cursor_history_sequence IS NOT NULL
        AND provider_resume_cursor_history_sequence >= 0
        AND provider_resume_cursor_history_sequence <= last_event_sequence
      )
    ),
  ADD CONSTRAINT chk_agent_sessions_provider_resume_cursor_shape
    CHECK (
      (
        provider_resume_cursor_state = 'absent'
        AND COALESCE(octet_length(provider_resume_cursor_encrypted), 0) = 0
        AND provider_resume_cursor_source_execution_id IS NULL
      )
      OR
      (
        provider_resume_cursor_state = 'usable'
        AND COALESCE(octet_length(provider_resume_cursor_encrypted), 0) > 0
        AND provider_resume_cursor_source_execution_id IS NOT NULL
      )
      OR
      (
        provider_resume_cursor_state = 'quarantined'
        AND COALESCE(octet_length(provider_resume_cursor_encrypted), 0) > 0
      )
    ),
  ADD CONSTRAINT fk_agent_sessions_provider_resume_cursor_source_execution
    FOREIGN KEY (tenant_id, provider_resume_cursor_source_execution_id)
    REFERENCES agent_executions(tenant_id, id)
    DEFERRABLE INITIALLY DEFERRED;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM agent_executions
    WHERE status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering')
    GROUP BY tenant_id, session_id
    HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'cannot enforce one active Execution per Session while duplicate active rows exist'
      USING ERRCODE = 'P0001';
  END IF;
END;
$$;

CREATE UNIQUE INDEX uq_agent_executions_session_active
  ON agent_executions (tenant_id, session_id)
  WHERE status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering');

CREATE INDEX idx_agent_sessions_provider_resume_cursor_source
  ON agent_sessions (tenant_id, provider_resume_cursor_source_execution_id)
  WHERE provider_resume_cursor_source_execution_id IS NOT NULL;

CREATE OR REPLACE FUNCTION assert_session_provider_resume_cursor_source()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.provider_resume_cursor_source_execution_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM agent_executions AS execution
    WHERE execution.tenant_id = NEW.tenant_id
      AND execution.id = NEW.provider_resume_cursor_source_execution_id
      AND execution.session_id = NEW.id
      AND execution.generation >= NEW.provider_resume_cursor_source_generation
  ) THEN
    RAISE EXCEPTION 'Provider resume Cursor source does not match the Session or generation'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_sessions_provider_resume_cursor_source ON agent_sessions;
CREATE CONSTRAINT TRIGGER trg_agent_sessions_provider_resume_cursor_source
AFTER INSERT OR UPDATE OF
  tenant_id,
  id,
  provider_resume_cursor_state,
  provider_resume_cursor_source_execution_id,
  provider_resume_cursor_source_generation,
  provider_resume_cursor_history_sequence
ON agent_sessions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_session_provider_resume_cursor_source();

COMMENT ON COLUMN agent_sessions.provider_resume_cursor_state IS
  'Controls whether encrypted Provider Cursor bytes may be used. Quarantine preserves bytes while forcing authoritative-history recovery.';
COMMENT ON COLUMN agent_sessions.provider_resume_cursor_source_execution_id IS
  'Execution that produced the currently stored Provider Cursor; NULL for absent and legacy quarantined ciphertext.';
COMMENT ON COLUMN agent_sessions.provider_resume_cursor_history_sequence IS
  'Authoritative Session sequence observed when the current Provider Cursor was persisted.';
