ALTER TABLE agent_executions
  ADD COLUMN traceparent TEXT;

ALTER TABLE agent_executions
  ADD CONSTRAINT agent_executions_traceparent_check CHECK (
    traceparent IS NULL OR (
      traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'
      AND substring(traceparent FROM 4 FOR 32) <> repeat('0', 32)
      AND substring(traceparent FROM 37 FOR 16) <> repeat('0', 16)
    )
  );

CREATE OR REPLACE FUNCTION reject_execution_traceparent_mutation()
RETURNS trigger AS $$
BEGIN
  IF NEW.traceparent IS DISTINCT FROM OLD.traceparent THEN
    RAISE EXCEPTION 'Execution trace context is immutable';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_agent_executions_traceparent_immutable
BEFORE UPDATE OF traceparent ON agent_executions
FOR EACH ROW EXECUTE FUNCTION reject_execution_traceparent_mutation();

COMMENT ON COLUMN agent_executions.traceparent IS
  'Immutable W3C diagnostic parent captured at Execution creation; never an authorization or idempotency authority.';
