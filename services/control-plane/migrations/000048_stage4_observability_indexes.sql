CREATE INDEX idx_session_events_execution_started_lookup
  ON session_events (tenant_id, session_id, execution_id, generation, occurred_at)
  WHERE event_type = 'execution.started'
    AND execution_id IS NOT NULL
    AND generation IS NOT NULL;
