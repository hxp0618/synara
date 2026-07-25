CREATE TABLE execution_recovery_bundles (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  session_id UUID NOT NULL,
  turn_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  schema_version INTEGER NOT NULL CHECK (schema_version > 0),
  recovery_reason TEXT NOT NULL
    CHECK (recovery_reason IN ('initial-claim', 'execution-recovery', 'suspend-resume', 'legacy-adoption')),
  previous_bundle_id UUID,
  authoritative_history_sequence BIGINT NOT NULL CHECK (authoritative_history_sequence >= 0),
  payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  payload_sha256 TEXT NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, execution_id, generation),
  FOREIGN KEY (tenant_id, session_id, turn_id)
    REFERENCES agent_turns(tenant_id, session_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, previous_bundle_id)
    REFERENCES execution_recovery_bundles(tenant_id, id) ON DELETE RESTRICT
);

CREATE INDEX idx_execution_recovery_bundles_session_created
  ON execution_recovery_bundles (tenant_id, session_id, created_at DESC, id);

CREATE OR REPLACE FUNCTION enforce_execution_recovery_bundle_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  previous_generation BIGINT;
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM agent_executions AS execution
    WHERE execution.tenant_id = NEW.tenant_id
      AND execution.id = NEW.execution_id
      AND execution.session_id = NEW.session_id
      AND execution.turn_id = NEW.turn_id
      AND execution.generation = NEW.generation
      AND execution.status IN ('leased', 'running', 'waiting-for-approval')
  ) THEN
    RAISE EXCEPTION 'Recovery Bundle does not match the current leased Execution generation'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.previous_bundle_id IS NOT NULL THEN
    SELECT bundle.generation INTO previous_generation
    FROM execution_recovery_bundles AS bundle
    WHERE bundle.tenant_id = NEW.tenant_id
      AND bundle.id = NEW.previous_bundle_id
      AND bundle.execution_id = NEW.execution_id;
    IF NOT FOUND OR previous_generation >= NEW.generation THEN
      RAISE EXCEPTION 'Recovery Bundle predecessor must belong to an earlier generation of the same Execution'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.recovery_reason = 'initial-claim' AND (
    NEW.generation <> 1 OR NEW.previous_bundle_id IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'Initial Recovery Bundle must be Generation 1 without a predecessor'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.recovery_reason IN ('execution-recovery', 'suspend-resume') AND NEW.previous_bundle_id IS NULL THEN
    RAISE EXCEPTION 'Recovered Execution generations require a predecessor Recovery Bundle'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.recovery_reason = 'legacy-adoption' AND (
    NEW.generation <= 1 OR
    NEW.previous_bundle_id IS NOT NULL OR
    EXISTS (
      SELECT 1 FROM execution_recovery_bundles AS existing
      WHERE existing.tenant_id = NEW.tenant_id
        AND existing.execution_id = NEW.execution_id
    )
  ) THEN
    RAISE EXCEPTION 'Legacy Recovery Bundle adoption is allowed only once for an existing Generation without history'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_recovery_bundles_insert
BEFORE INSERT ON execution_recovery_bundles
FOR EACH ROW EXECUTE FUNCTION enforce_execution_recovery_bundle_insert();

CREATE OR REPLACE FUNCTION reject_execution_recovery_bundle_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Execution Recovery Bundles are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_execution_recovery_bundles_immutable
BEFORE UPDATE OR DELETE ON execution_recovery_bundles
FOR EACH ROW EXECUTE FUNCTION reject_execution_recovery_bundle_mutation();

COMMENT ON TABLE execution_recovery_bundles IS
  'Immutable, content-hashed recovery input frozen atomically for one Execution generation; it contains references and bounded context, never plaintext Credentials or Provider resume Cursors.';
