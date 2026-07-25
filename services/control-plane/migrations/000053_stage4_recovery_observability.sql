CREATE INDEX idx_session_events_execution_generation_lifecycle
  ON session_events (tenant_id, execution_id, generation, event_type, occurred_at, event_id)
  WHERE execution_id IS NOT NULL
    AND generation IS NOT NULL
    AND event_type IN (
      'execution.started',
      'session.started',
      'execution.completed',
      'execution.failed',
      'execution.cancelled',
      'execution.interrupted'
    );

CREATE INDEX idx_session_events_provider_resume_claim_decision
  ON session_events (occurred_at, tenant_id, event_id)
  WHERE event_type = 'execution.leased';

CREATE INDEX idx_session_events_provider_resume_runtime_fallback
  ON session_events (occurred_at, tenant_id, event_id)
  WHERE event_type = 'runtime.warning';

COMMENT ON INDEX idx_session_events_execution_generation_lifecycle IS
  'Supports durable Stage 4 recovery start, Provider-ready, and terminal-before-ready outcome metrics.';
