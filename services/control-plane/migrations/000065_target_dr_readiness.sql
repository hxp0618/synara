CREATE TABLE execution_target_dr_readiness (
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE CASCADE,
  source_dr_domain TEXT NOT NULL,
  dr_domain TEXT NOT NULL,
  replicated_through_at TIMESTAMPTZ NOT NULL,
  artifacts_ready BOOLEAN NOT NULL DEFAULT false,
  checkpoints_ready BOOLEAN NOT NULL DEFAULT false,
  memory_ready BOOLEAN NOT NULL DEFAULT false,
  publisher_identity TEXT NOT NULL,
  reason TEXT,
  observed_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (execution_target_id, source_dr_domain),
  CHECK (expires_at > observed_at),
  CHECK (replicated_through_at <= observed_at),
  CHECK (length(btrim(source_dr_domain)) BETWEEN 1 AND 200),
  CHECK (length(btrim(dr_domain)) BETWEEN 1 AND 200),
  CHECK (length(btrim(publisher_identity)) BETWEEN 1 AND 200),
  CHECK (reason IS NULL OR length(reason) <= 2000)
);

CREATE INDEX idx_execution_target_dr_readiness_route
  ON execution_target_dr_readiness (execution_target_id, source_dr_domain, expires_at);

CREATE OR REPLACE FUNCTION enforce_execution_target_dr_readiness_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution Target DR readiness observations cannot be deleted'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.execution_target_id <> OLD.execution_target_id
     OR NEW.source_dr_domain <> OLD.source_dr_domain THEN
    RAISE EXCEPTION 'Execution Target DR readiness identity is immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version + 1 OR NEW.observed_at <= OLD.observed_at THEN
    RAISE EXCEPTION 'Execution Target DR readiness update is stale or not monotonic'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_target_dr_readiness_update
  ON execution_target_dr_readiness;
CREATE TRIGGER trg_execution_target_dr_readiness_update
BEFORE UPDATE OR DELETE ON execution_target_dr_readiness
FOR EACH ROW EXECUTE FUNCTION enforce_execution_target_dr_readiness_update();
