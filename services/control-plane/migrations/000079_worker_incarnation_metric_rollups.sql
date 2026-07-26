CREATE TABLE worker_incarnation_metric_rollups (
  bucket_day DATE NOT NULL,
  target_kind TEXT NOT NULL,
  pool_mode TEXT NOT NULL,
  capacity_class TEXT NOT NULL,
  fact_count BIGINT NOT NULL,
  run_seconds DOUBLE PRECISION NOT NULL,
  active_seconds DOUBLE PRECISION NOT NULL,
  idle_seconds DOUBLE PRECISION NOT NULL,
  requested_cpu_seconds DOUBLE PRECISION NOT NULL,
  requested_memory_byte_seconds DOUBLE PRECISION NOT NULL,
  requested_ephemeral_storage_byte_seconds DOUBLE PRECISION NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_worker_incarnation_metric_rollups PRIMARY KEY (
    bucket_day, target_kind, pool_mode, capacity_class
  ),
  CONSTRAINT chk_worker_incarnation_metric_rollup_dimensions CHECK (
    target_kind IN ('local', 'ssh', 'docker', 'kubernetes')
    AND pool_mode IN ('resident', 'per-execution', 'warm', 'unassigned')
    AND capacity_class IN ('standard', 'interactive', 'unassigned')
  ),
  CONSTRAINT chk_worker_incarnation_metric_rollup_totals CHECK (
    fact_count > 0
    AND run_seconds >= 0
    AND active_seconds >= 0
    AND idle_seconds >= 0
    AND requested_cpu_seconds >= 0
    AND requested_memory_byte_seconds >= 0
    AND requested_ephemeral_storage_byte_seconds >= 0
    AND run_seconds < 'Infinity'::DOUBLE PRECISION
    AND active_seconds < 'Infinity'::DOUBLE PRECISION
    AND idle_seconds < 'Infinity'::DOUBLE PRECISION
    AND requested_cpu_seconds < 'Infinity'::DOUBLE PRECISION
    AND requested_memory_byte_seconds < 'Infinity'::DOUBLE PRECISION
    AND requested_ephemeral_storage_byte_seconds < 'Infinity'::DOUBLE PRECISION
    AND created_at <= updated_at
  )
);

CREATE INDEX idx_worker_incarnation_metric_rollups_day
  ON worker_incarnation_metric_rollups (bucket_day, target_kind);

CREATE TABLE worker_incarnation_metric_rollup_entries (
  worker_id UUID NOT NULL,
  worker_incarnation BIGINT NOT NULL,
  terminal_at TIMESTAMPTZ NOT NULL,
  bucket_day DATE NOT NULL,
  rolled_up_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_worker_incarnation_metric_rollup_entries PRIMARY KEY (worker_id, worker_incarnation),
  CONSTRAINT fk_worker_incarnation_metric_rollup_entry_fact
    FOREIGN KEY (worker_id, worker_incarnation)
    REFERENCES worker_incarnation_facts(worker_id, worker_incarnation) ON DELETE RESTRICT,
  CONSTRAINT chk_worker_incarnation_metric_rollup_entry_incarnation CHECK (worker_incarnation > 0),
  CONSTRAINT chk_worker_incarnation_metric_rollup_entry_bucket CHECK (
    bucket_day = (terminal_at AT TIME ZONE 'UTC')::date
  ),
  CONSTRAINT chk_worker_incarnation_metric_rollup_entry_timeline CHECK (
    created_at <= updated_at
    AND (rolled_up_at IS NULL OR rolled_up_at >= created_at)
  )
);

CREATE INDEX idx_worker_incarnation_metric_rollup_entries_pending
  ON worker_incarnation_metric_rollup_entries (terminal_at, worker_id, worker_incarnation)
  WHERE rolled_up_at IS NULL;

INSERT INTO worker_incarnation_metric_rollup_entries (
  worker_id, worker_incarnation, terminal_at, bucket_day,
  rolled_up_at, created_at, updated_at
)
SELECT
  worker_id,
  worker_incarnation,
  terminated_at,
  (terminated_at AT TIME ZONE 'UTC')::date,
  NULL,
  terminated_at,
  terminated_at
FROM worker_incarnation_facts
WHERE current_state = 'terminated'
  AND terminated_at IS NOT NULL
ON CONFLICT (worker_id, worker_incarnation) DO NOTHING;

CREATE OR REPLACE FUNCTION enforce_worker_incarnation_metric_rollup()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Worker incarnation metric rollups cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.bucket_day <> OLD.bucket_day
     OR NEW.target_kind <> OLD.target_kind
     OR NEW.pool_mode <> OLD.pool_mode
     OR NEW.capacity_class <> OLD.capacity_class
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'Worker incarnation metric rollup identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.fact_count < OLD.fact_count
     OR NEW.run_seconds < OLD.run_seconds
     OR NEW.active_seconds < OLD.active_seconds
     OR NEW.idle_seconds < OLD.idle_seconds
     OR NEW.requested_cpu_seconds < OLD.requested_cpu_seconds
     OR NEW.requested_memory_byte_seconds < OLD.requested_memory_byte_seconds
     OR NEW.requested_ephemeral_storage_byte_seconds < OLD.requested_ephemeral_storage_byte_seconds
     OR NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'Worker incarnation metric rollup totals cannot regress' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollups_update ON worker_incarnation_metric_rollups;
CREATE TRIGGER trg_worker_incarnation_metric_rollups_update
BEFORE UPDATE OR DELETE ON worker_incarnation_metric_rollups
FOR EACH ROW EXECUTE FUNCTION enforce_worker_incarnation_metric_rollup();

CREATE OR REPLACE FUNCTION enforce_worker_incarnation_metric_rollup_entry()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  fact_terminal_at TIMESTAMPTZ;
  fact_state TEXT;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Worker incarnation metric rollup entries cannot be deleted' USING ERRCODE = '23514';
  END IF;
  SELECT current_state, terminated_at
  INTO fact_state, fact_terminal_at
  FROM worker_incarnation_facts
  WHERE worker_id = NEW.worker_id
    AND worker_incarnation = NEW.worker_incarnation;
  IF fact_state IS DISTINCT FROM 'terminated'
     OR fact_terminal_at IS NULL
     OR NEW.terminal_at <> fact_terminal_at
     OR NEW.bucket_day <> (fact_terminal_at AT TIME ZONE 'UTC')::date THEN
    RAISE EXCEPTION 'Worker incarnation metric rollup entry source is invalid' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.worker_id <> OLD.worker_id
       OR NEW.worker_incarnation <> OLD.worker_incarnation
       OR NEW.terminal_at <> OLD.terminal_at
       OR NEW.bucket_day <> OLD.bucket_day
       OR NEW.created_at <> OLD.created_at THEN
      RAISE EXCEPTION 'Worker incarnation metric rollup entry identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.rolled_up_at IS NOT NULL AND NEW.rolled_up_at IS DISTINCT FROM OLD.rolled_up_at THEN
      RAISE EXCEPTION 'Worker incarnation metric rollup entry completion is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
      RAISE EXCEPTION 'Worker incarnation metric rollup entry timeline cannot regress' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollup_entries ON worker_incarnation_metric_rollup_entries;
CREATE TRIGGER trg_worker_incarnation_metric_rollup_entries
BEFORE INSERT OR UPDATE OR DELETE ON worker_incarnation_metric_rollup_entries
FOR EACH ROW EXECUTE FUNCTION enforce_worker_incarnation_metric_rollup_entry();

CREATE OR REPLACE FUNCTION enqueue_worker_incarnation_metric_rollup()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.current_state = 'terminated'
     AND NEW.terminated_at IS NOT NULL
     AND (TG_OP = 'INSERT' OR OLD.current_state <> 'terminated') THEN
    INSERT INTO worker_incarnation_metric_rollup_entries (
      worker_id, worker_incarnation, terminal_at, bucket_day,
      rolled_up_at, created_at, updated_at
    ) VALUES (
      NEW.worker_id,
      NEW.worker_incarnation,
      NEW.terminated_at,
      (NEW.terminated_at AT TIME ZONE 'UTC')::date,
      NULL,
      NEW.terminated_at,
      NEW.terminated_at
    )
    ON CONFLICT (worker_id, worker_incarnation) DO NOTHING;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_incarnation_metric_rollup_enqueue ON worker_incarnation_facts;
CREATE TRIGGER trg_worker_incarnation_metric_rollup_enqueue
AFTER INSERT OR UPDATE OF current_state, terminated_at ON worker_incarnation_facts
FOR EACH ROW EXECUTE FUNCTION enqueue_worker_incarnation_metric_rollup();

COMMENT ON TABLE worker_incarnation_metric_rollups IS
  'Exact daily aggregates for immutable terminal Worker incarnation facts; active and pending-rollup facts remain raw.';

COMMENT ON TABLE worker_incarnation_metric_rollup_entries IS
  'Append-only membership and completion cursor proving each terminal Worker fact contributes to a rollup at most once.';
