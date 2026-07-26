CREATE TABLE execution_generation_metric_rollups (
  bucket_day DATE NOT NULL,
  metric_kind TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  recovery_reason TEXT NOT NULL,
  outcome TEXT NOT NULL,
  warm_pool_mode TEXT NOT NULL,
  warm_pool_result TEXT NOT NULL,
  failure_class TEXT NOT NULL,
  histogram_bucket INTEGER NOT NULL,
  sample_count BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_execution_generation_metric_rollups PRIMARY KEY (
    bucket_day, metric_kind, target_kind, recovery_reason, outcome,
    warm_pool_mode, warm_pool_result, failure_class, histogram_bucket
  ),
  CONSTRAINT chk_execution_generation_metric_rollup_dimensions CHECK (
    target_kind IN ('local', 'ssh', 'docker', 'kubernetes', 'other')
    AND (
      (
        metric_kind = 'outcome'
        AND recovery_reason IN ('initial-claim', 'legacy-adoption', 'execution-recovery', 'suspend-resume', 'other')
        AND outcome IN ('completed', 'failed', 'cancelled', 'interrupted', 'recovering', 'other')
        AND warm_pool_mode = 'none' AND warm_pool_result = 'none'
        AND failure_class = 'none' AND histogram_bucket = -1
      )
      OR (
        metric_kind = 'warm-acquisition'
        AND recovery_reason = 'none' AND outcome = 'none'
        AND warm_pool_mode IN ('balanced', 'low-latency')
        AND warm_pool_result IN ('pending', 'not-requested', 'hit', 'fallback')
        AND failure_class = 'none' AND histogram_bucket = -1
      )
      OR (
        metric_kind = 'cold-start-duration'
        AND recovery_reason IN ('initial-claim', 'legacy-adoption', 'execution-recovery', 'suspend-resume', 'other')
        AND outcome = 'none'
        AND warm_pool_mode IN ('disabled', 'balanced', 'low-latency')
        AND warm_pool_result IN ('pending', 'not-requested', 'hit', 'fallback')
        AND failure_class = 'none' AND histogram_bucket BETWEEN 0 AND 4095
      )
      OR (
        metric_kind IN ('pod-queue-duration', 'pod-provisioning-duration')
        AND recovery_reason IN ('initial-claim', 'legacy-adoption', 'execution-recovery', 'suspend-resume', 'other')
        AND outcome = 'none' AND warm_pool_mode = 'none' AND warm_pool_result = 'none'
        AND failure_class = 'none' AND histogram_bucket BETWEEN 0 AND 4095
      )
      OR (
        metric_kind = 'pod-failure'
        AND recovery_reason = 'none' AND outcome = 'none'
        AND warm_pool_mode = 'none' AND warm_pool_result = 'none'
        AND failure_class IN (
          'pod-apply-failed', 'pending-timeout', 'unschedulable', 'image-pull',
          'container-start', 'evicted', 'oom-killed', 'pod-failed', 'other'
        )
        AND histogram_bucket = -1
      )
    )
  ),
  CONSTRAINT chk_execution_generation_metric_rollup_totals CHECK (
    sample_count > 0 AND created_at <= updated_at
  )
);

CREATE INDEX idx_execution_generation_metric_rollups_window
  ON execution_generation_metric_rollups (bucket_day, metric_kind, target_kind);

CREATE TABLE execution_generation_metric_rollup_entries (
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL,
  dispatch_requested_at TIMESTAMPTZ NOT NULL,
  terminal_at TIMESTAMPTZ NOT NULL,
  bucket_day DATE NOT NULL,
  rolled_up_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_execution_generation_metric_rollup_entries PRIMARY KEY (
    tenant_id, execution_id, generation
  ),
  CONSTRAINT fk_execution_generation_metric_rollup_entry_fact
    FOREIGN KEY (tenant_id, execution_id, generation)
    REFERENCES execution_generation_facts(tenant_id, execution_id, generation) ON DELETE RESTRICT,
  CONSTRAINT chk_execution_generation_metric_rollup_entry_generation CHECK (generation > 0),
  CONSTRAINT chk_execution_generation_metric_rollup_entry_bucket CHECK (
    bucket_day = (dispatch_requested_at AT TIME ZONE 'UTC')::date
    AND terminal_at >= dispatch_requested_at
  ),
  CONSTRAINT chk_execution_generation_metric_rollup_entry_timeline CHECK (
    created_at <= updated_at
    AND (rolled_up_at IS NULL OR rolled_up_at >= created_at)
  )
);

CREATE INDEX idx_execution_generation_metric_rollup_entries_pending
  ON execution_generation_metric_rollup_entries (terminal_at, tenant_id, execution_id, generation)
  WHERE rolled_up_at IS NULL;

CREATE TABLE execution_generation_pod_failure_metric_rollup_entries (
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL,
  failure_class TEXT NOT NULL,
  first_observed_at TIMESTAMPTZ NOT NULL,
  bucket_day DATE NOT NULL,
  rolled_up_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_execution_generation_pod_failure_metric_rollup_entries PRIMARY KEY (
    tenant_id, execution_id, generation, failure_class
  ),
  CONSTRAINT fk_execution_generation_pod_failure_metric_rollup_entry_fact
    FOREIGN KEY (tenant_id, execution_id, generation, failure_class)
    REFERENCES execution_generation_pod_failure_facts(tenant_id, execution_id, generation, failure_class)
    ON DELETE RESTRICT,
  CONSTRAINT chk_execution_generation_pod_failure_metric_rollup_entry_generation CHECK (generation > 0),
  CONSTRAINT chk_execution_generation_pod_failure_metric_rollup_entry_bucket CHECK (
    bucket_day = (first_observed_at AT TIME ZONE 'UTC')::date
  ),
  CONSTRAINT chk_execution_generation_pod_failure_metric_rollup_entry_timeline CHECK (
    created_at <= updated_at
    AND (rolled_up_at IS NULL OR rolled_up_at >= created_at)
  )
);

CREATE INDEX idx_execution_generation_pod_failure_metric_rollup_entries_pending
  ON execution_generation_pod_failure_metric_rollup_entries (
    first_observed_at, tenant_id, execution_id, generation, failure_class
  ) WHERE rolled_up_at IS NULL;

INSERT INTO execution_generation_metric_rollup_entries (
  tenant_id, execution_id, generation, dispatch_requested_at, terminal_at,
  bucket_day, rolled_up_at, created_at, updated_at
)
SELECT
  tenant_id, execution_id, generation, dispatch_requested_at, terminal_at,
  (dispatch_requested_at AT TIME ZONE 'UTC')::date, NULL, terminal_at, terminal_at
FROM execution_generation_facts
WHERE dispatch_requested_at IS NOT NULL
  AND terminal_at IS NOT NULL
  AND terminal_outcome IS NOT NULL
ON CONFLICT (tenant_id, execution_id, generation) DO NOTHING;

INSERT INTO execution_generation_pod_failure_metric_rollup_entries (
  tenant_id, execution_id, generation, failure_class, first_observed_at,
  bucket_day, rolled_up_at, created_at, updated_at
)
SELECT
  tenant_id, execution_id, generation, failure_class, first_observed_at,
  (first_observed_at AT TIME ZONE 'UTC')::date, NULL, first_observed_at, first_observed_at
FROM execution_generation_pod_failure_facts
ON CONFLICT (tenant_id, execution_id, generation, failure_class) DO NOTHING;

CREATE OR REPLACE FUNCTION enforce_execution_generation_metric_rollup()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution generation metric rollups cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.bucket_day <> OLD.bucket_day
     OR NEW.metric_kind <> OLD.metric_kind
     OR NEW.target_kind <> OLD.target_kind
     OR NEW.recovery_reason <> OLD.recovery_reason
     OR NEW.outcome <> OLD.outcome
     OR NEW.warm_pool_mode <> OLD.warm_pool_mode
     OR NEW.warm_pool_result <> OLD.warm_pool_result
     OR NEW.failure_class <> OLD.failure_class
     OR NEW.histogram_bucket <> OLD.histogram_bucket
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'Execution generation metric rollup identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.sample_count < OLD.sample_count OR NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'Execution generation metric rollup totals cannot regress' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollups ON execution_generation_metric_rollups;
CREATE TRIGGER trg_execution_generation_metric_rollups
BEFORE UPDATE OR DELETE ON execution_generation_metric_rollups
FOR EACH ROW EXECUTE FUNCTION enforce_execution_generation_metric_rollup();

CREATE OR REPLACE FUNCTION enforce_execution_generation_metric_rollup_entry()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  fact_dispatch_requested_at TIMESTAMPTZ;
  fact_terminal_at TIMESTAMPTZ;
  fact_terminal_outcome TEXT;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution generation metric rollup entries cannot be deleted' USING ERRCODE = '23514';
  END IF;
  SELECT dispatch_requested_at, terminal_at, terminal_outcome
  INTO fact_dispatch_requested_at, fact_terminal_at, fact_terminal_outcome
  FROM execution_generation_facts
  WHERE tenant_id = NEW.tenant_id
    AND execution_id = NEW.execution_id
    AND generation = NEW.generation;
  IF fact_dispatch_requested_at IS NULL
     OR fact_terminal_at IS NULL
     OR fact_terminal_outcome IS NULL
     OR NEW.dispatch_requested_at <> fact_dispatch_requested_at
     OR NEW.terminal_at <> fact_terminal_at
     OR NEW.bucket_day <> (fact_dispatch_requested_at AT TIME ZONE 'UTC')::date THEN
    RAISE EXCEPTION 'Execution generation metric rollup entry source is invalid' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.tenant_id <> OLD.tenant_id
       OR NEW.execution_id <> OLD.execution_id
       OR NEW.generation <> OLD.generation
       OR NEW.dispatch_requested_at <> OLD.dispatch_requested_at
       OR NEW.terminal_at <> OLD.terminal_at
       OR NEW.bucket_day <> OLD.bucket_day
       OR NEW.created_at <> OLD.created_at THEN
      RAISE EXCEPTION 'Execution generation metric rollup entry identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.rolled_up_at IS NOT NULL AND NEW.rolled_up_at IS DISTINCT FROM OLD.rolled_up_at THEN
      RAISE EXCEPTION 'Execution generation metric rollup entry completion is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
      RAISE EXCEPTION 'Execution generation metric rollup entry timeline cannot regress' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollup_entries ON execution_generation_metric_rollup_entries;
CREATE TRIGGER trg_execution_generation_metric_rollup_entries
BEFORE INSERT OR UPDATE OR DELETE ON execution_generation_metric_rollup_entries
FOR EACH ROW EXECUTE FUNCTION enforce_execution_generation_metric_rollup_entry();

CREATE OR REPLACE FUNCTION enqueue_execution_generation_metric_rollup()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.dispatch_requested_at IS NOT NULL
     AND NEW.terminal_at IS NOT NULL
     AND NEW.terminal_outcome IS NOT NULL THEN
    INSERT INTO execution_generation_metric_rollup_entries (
      tenant_id, execution_id, generation, dispatch_requested_at, terminal_at,
      bucket_day, rolled_up_at, created_at, updated_at
    ) VALUES (
      NEW.tenant_id, NEW.execution_id, NEW.generation, NEW.dispatch_requested_at, NEW.terminal_at,
      (NEW.dispatch_requested_at AT TIME ZONE 'UTC')::date, NULL, NEW.terminal_at, NEW.terminal_at
    ) ON CONFLICT (tenant_id, execution_id, generation) DO NOTHING;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_generation_metric_rollup_enqueue ON execution_generation_facts;
CREATE TRIGGER trg_execution_generation_metric_rollup_enqueue
AFTER INSERT OR UPDATE OF dispatch_requested_at, terminal_at, terminal_outcome ON execution_generation_facts
FOR EACH ROW EXECUTE FUNCTION enqueue_execution_generation_metric_rollup();

CREATE OR REPLACE FUNCTION enforce_execution_generation_metric_source_sealed()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM execution_generation_metric_rollup_entries
    WHERE tenant_id = OLD.tenant_id
      AND execution_id = OLD.execution_id
      AND generation = OLD.generation
  ) AND (
    NEW.target_kind IS DISTINCT FROM OLD.target_kind
    OR NEW.recovery_reason IS DISTINCT FROM OLD.recovery_reason
    OR NEW.warm_pool_mode IS DISTINCT FROM OLD.warm_pool_mode
    OR NEW.warm_pool_result IS DISTINCT FROM OLD.warm_pool_result
    OR NEW.dispatch_requested_at IS DISTINCT FROM OLD.dispatch_requested_at
    OR NEW.provider_ready_at IS DISTINCT FROM OLD.provider_ready_at
    OR NEW.pod_provisioning_started_at IS DISTINCT FROM OLD.pod_provisioning_started_at
    OR NEW.pod_running_at IS DISTINCT FROM OLD.pod_running_at
    OR NEW.terminal_at IS DISTINCT FROM OLD.terminal_at
    OR NEW.terminal_outcome IS DISTINCT FROM OLD.terminal_outcome
  ) THEN
    RAISE EXCEPTION 'Execution generation metric source is sealed' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_generation_metric_source_sealed ON execution_generation_facts;
CREATE TRIGGER trg_execution_generation_metric_source_sealed
BEFORE UPDATE ON execution_generation_facts
FOR EACH ROW EXECUTE FUNCTION enforce_execution_generation_metric_source_sealed();

CREATE OR REPLACE FUNCTION enforce_execution_generation_pod_failure_metric_rollup_entry()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  fact_first_observed_at TIMESTAMPTZ;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution generation Pod failure metric rollup entries cannot be deleted' USING ERRCODE = '23514';
  END IF;
  SELECT first_observed_at INTO fact_first_observed_at
  FROM execution_generation_pod_failure_facts
  WHERE tenant_id = NEW.tenant_id
    AND execution_id = NEW.execution_id
    AND generation = NEW.generation
    AND failure_class = NEW.failure_class;
  IF fact_first_observed_at IS NULL
     OR NEW.first_observed_at <> fact_first_observed_at
     OR NEW.bucket_day <> (fact_first_observed_at AT TIME ZONE 'UTC')::date THEN
    RAISE EXCEPTION 'Execution generation Pod failure metric rollup entry source is invalid' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.tenant_id <> OLD.tenant_id
       OR NEW.execution_id <> OLD.execution_id
       OR NEW.generation <> OLD.generation
       OR NEW.failure_class <> OLD.failure_class
       OR NEW.first_observed_at <> OLD.first_observed_at
       OR NEW.bucket_day <> OLD.bucket_day
       OR NEW.created_at <> OLD.created_at THEN
      RAISE EXCEPTION 'Execution generation Pod failure metric rollup entry identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.rolled_up_at IS NOT NULL AND NEW.rolled_up_at IS DISTINCT FROM OLD.rolled_up_at THEN
      RAISE EXCEPTION 'Execution generation Pod failure metric rollup entry completion is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
      RAISE EXCEPTION 'Execution generation Pod failure metric rollup entry timeline cannot regress' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_metric_rollup_entries
  ON execution_generation_pod_failure_metric_rollup_entries;
CREATE TRIGGER trg_execution_generation_pod_failure_metric_rollup_entries
BEFORE INSERT OR UPDATE OR DELETE ON execution_generation_pod_failure_metric_rollup_entries
FOR EACH ROW EXECUTE FUNCTION enforce_execution_generation_pod_failure_metric_rollup_entry();

CREATE OR REPLACE FUNCTION enqueue_execution_generation_pod_failure_metric_rollup()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO execution_generation_pod_failure_metric_rollup_entries (
    tenant_id, execution_id, generation, failure_class, first_observed_at,
    bucket_day, rolled_up_at, created_at, updated_at
  ) VALUES (
    NEW.tenant_id, NEW.execution_id, NEW.generation, NEW.failure_class, NEW.first_observed_at,
    (NEW.first_observed_at AT TIME ZONE 'UTC')::date, NULL, NEW.first_observed_at, NEW.first_observed_at
  ) ON CONFLICT (tenant_id, execution_id, generation, failure_class) DO NOTHING;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_generation_pod_failure_metric_rollup_enqueue
  ON execution_generation_pod_failure_facts;
CREATE TRIGGER trg_execution_generation_pod_failure_metric_rollup_enqueue
AFTER INSERT ON execution_generation_pod_failure_facts
FOR EACH ROW EXECUTE FUNCTION enqueue_execution_generation_pod_failure_metric_rollup();

COMMENT ON TABLE execution_generation_metric_rollups IS
  'Mergeable daily categorical counts and integer 2-percent duration histograms for sealed terminal Execution generations.';

COMMENT ON TABLE execution_generation_metric_rollup_entries IS
  'Append-only membership cursor proving each sealed terminal Execution generation contributes at most once.';

COMMENT ON TABLE execution_generation_pod_failure_metric_rollup_entries IS
  'Append-only membership cursor proving each immutable Generation Pod failure class contributes at most once.';
