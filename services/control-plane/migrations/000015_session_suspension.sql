ALTER TABLE agent_sessions
  DROP CONSTRAINT IF EXISTS agent_sessions_status_check;

ALTER TABLE agent_sessions
  DROP CONSTRAINT IF EXISTS agent_sessions_check;

ALTER TABLE agent_sessions
  ADD CONSTRAINT agent_sessions_status_check
  CHECK (status IN ('active', 'suspended', 'archived'));

ALTER TABLE agent_sessions
  ADD CONSTRAINT agent_sessions_archive_state_check
  CHECK (
    (status = 'archived' AND archived_at IS NOT NULL) OR
    (status IN ('active', 'suspended') AND archived_at IS NULL)
  );
