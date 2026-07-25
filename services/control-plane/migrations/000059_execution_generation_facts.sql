CREATE TABLE execution_generation_facts (
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL,
  session_id UUID NOT NULL,
  turn_id UUID NOT NULL,
  execution_target_id UUID NOT NULL,
  target_kind TEXT NOT NULL,
  provider TEXT NOT NULL,
  recovery_reason TEXT NOT NULL,
  warm_pool_mode TEXT NOT NULL DEFAULT 'disabled',
  warm_pool_result TEXT NOT NULL DEFAULT 'pending',
  dispatch_requested_at TIMESTAMPTZ,
  bundle_created_at TIMESTAMPTZ,
  leased_at TIMESTAMPTZ,
  execution_started_at TIMESTAMPTZ,
  provider_ready_at TIMESTAMPTZ,
  terminal_at TIMESTAMPTZ,
  terminal_outcome TEXT,
  provider_resume_strategy TEXT NOT NULL DEFAULT 'authoritative-history',
  resume_attempted_strategy TEXT,
  resume_selected_strategy TEXT,
  resume_fallback_outcome TEXT,
  resume_fallback_reason_code TEXT,
  resume_fallback_safety TEXT,
  resume_fallback_provider TEXT,
  resume_authoritative_history_sequence BIGINT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_execution_generation_facts PRIMARY KEY (tenant_id, execution_id, generation),
  CONSTRAINT fk_execution_generation_facts_execution
    FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE CASCADE,
  CONSTRAINT fk_execution_generation_facts_session
    FOREIGN KEY (tenant_id, session_id)
    REFERENCES agent_sessions(tenant_id, id) ON DELETE CASCADE,
  CONSTRAINT chk_execution_generation_facts_generation CHECK (generation > 0),
  CONSTRAINT chk_execution_generation_facts_recovery_reason CHECK (
    recovery_reason IN ('initial-claim', 'legacy-adoption', 'execution-recovery', 'suspend-resume')
  ),
  CONSTRAINT chk_execution_generation_facts_warm_pool_mode CHECK (
    warm_pool_mode IN ('disabled', 'balanced', 'low-latency')
  ),
  CONSTRAINT chk_execution_generation_facts_warm_pool_result CHECK (
    warm_pool_result IN ('pending', 'not-requested', 'hit', 'fallback')
  ),
  CONSTRAINT chk_execution_generation_facts_terminal_outcome CHECK (
    terminal_outcome IS NULL
    OR terminal_outcome IN ('completed', 'failed', 'cancelled', 'interrupted', 'recovering')
  )
);

CREATE INDEX idx_execution_generation_facts_metrics
  ON execution_generation_facts (target_kind, recovery_reason, warm_pool_mode, warm_pool_result);

CREATE INDEX idx_execution_generation_facts_terminal
  ON execution_generation_facts (terminal_outcome, terminal_at);

-- A rolling deploy can have leased/running generations before this table
-- exists. Seed their current durable identity and known timestamps so later
-- Start/ready/terminal hooks cannot silently update zero rows.
WITH lifecycle AS (
  SELECT
    tenant_id,
    execution_id,
    generation,
    MIN(occurred_at) FILTER (WHERE event_type = 'execution.started') AS execution_started_at,
    MIN(occurred_at) FILTER (WHERE event_type = 'session.started') AS provider_ready_at
  FROM session_events
  WHERE execution_id IS NOT NULL
    AND generation IS NOT NULL
    AND event_type IN ('execution.started', 'session.started')
  GROUP BY tenant_id, execution_id, generation
), candidates AS (
  SELECT
    execution.tenant_id,
    execution.id AS execution_id,
    execution.generation,
    execution.session_id,
    execution.turn_id,
    execution.execution_target_id,
    execution.target_kind,
    COALESCE(execution.provider, session.provider) AS provider,
    CASE
      WHEN bundle.recovery_reason IS NOT NULL THEN bundle.recovery_reason
      WHEN EXISTS (
        SELECT 1
        FROM execution_recovery_bundles AS previous
        WHERE previous.tenant_id = execution.tenant_id
          AND previous.execution_id = execution.id
          AND previous.generation < execution.generation
      ) THEN CASE
        WHEN execution.next_recovery_reason = 'suspend-resume' THEN 'suspend-resume'
        ELSE 'execution-recovery'
      END
      WHEN execution.generation > 1 THEN 'legacy-adoption'
      ELSE 'initial-claim'
    END AS recovery_reason,
    session.warm_pool_mode,
    CASE
      WHEN session.warm_pool_mode = 'disabled' THEN 'not-requested'
      WHEN worker.worker_mode = 'warm-pool' THEN 'hit'
      WHEN worker.id IS NOT NULL THEN 'fallback'
      ELSE 'pending'
    END AS warm_pool_result,
    CASE
      WHEN execution.generation = 1 THEN execution.queued_at
      ELSE COALESCE(bundle.created_at, lease.acquired_at, execution.queued_at)
    END AS dispatch_requested_at,
    bundle.created_at AS bundle_created_at,
    lease.acquired_at,
    lifecycle.execution_started_at,
    lifecycle.provider_ready_at,
    execution.provider_resume_strategy_snapshot AS provider_resume_strategy
  FROM agent_executions AS execution
  JOIN agent_sessions AS session
    ON session.tenant_id = execution.tenant_id
   AND session.id = execution.session_id
  LEFT JOIN execution_recovery_bundles AS bundle
    ON bundle.tenant_id = execution.tenant_id
   AND bundle.execution_id = execution.id
   AND bundle.generation = execution.generation
  LEFT JOIN worker_leases AS lease
    ON lease.tenant_id = execution.tenant_id
   AND lease.execution_id = execution.id
   AND lease.generation = execution.generation
  LEFT JOIN worker_instances AS worker
    ON worker.id = execution.worker_id
  LEFT JOIN lifecycle
    ON lifecycle.tenant_id = execution.tenant_id
   AND lifecycle.execution_id = execution.id
   AND lifecycle.generation = execution.generation
  WHERE execution.generation > 0
)
INSERT INTO execution_generation_facts (
  tenant_id, execution_id, generation, session_id, turn_id,
  execution_target_id, target_kind, provider, recovery_reason,
  warm_pool_mode, warm_pool_result, dispatch_requested_at,
  bundle_created_at, leased_at, execution_started_at, provider_ready_at,
  provider_resume_strategy, created_at, updated_at
)
SELECT
  tenant_id, execution_id, generation, session_id, turn_id,
  execution_target_id, target_kind, provider, recovery_reason,
  warm_pool_mode, warm_pool_result, dispatch_requested_at,
  bundle_created_at,
  CASE
    WHEN acquired_at IS NULL THEN NULL
    ELSE GREATEST(acquired_at, dispatch_requested_at)
  END,
  CASE
    WHEN execution_started_at IS NULL THEN NULL
    ELSE GREATEST(execution_started_at, acquired_at, dispatch_requested_at)
  END,
  CASE
    WHEN provider_ready_at IS NULL THEN NULL
    ELSE GREATEST(provider_ready_at, execution_started_at, acquired_at, dispatch_requested_at)
  END,
  provider_resume_strategy,
  dispatch_requested_at,
  GREATEST(
    dispatch_requested_at,
    bundle_created_at,
    acquired_at,
    execution_started_at,
    provider_ready_at
  )
FROM candidates
ON CONFLICT (tenant_id, execution_id, generation) DO NOTHING;

CREATE OR REPLACE FUNCTION enforce_execution_generation_fact_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution generation facts cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.tenant_id <> OLD.tenant_id
     OR NEW.execution_id <> OLD.execution_id
     OR NEW.generation <> OLD.generation
     OR NEW.session_id <> OLD.session_id
     OR NEW.turn_id <> OLD.turn_id
     OR NEW.execution_target_id <> OLD.execution_target_id
     OR NEW.target_kind <> OLD.target_kind
     OR NEW.provider <> OLD.provider
     OR NEW.recovery_reason <> OLD.recovery_reason
     OR NEW.warm_pool_mode <> OLD.warm_pool_mode THEN
    RAISE EXCEPTION 'Execution generation fact identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.warm_pool_result <> 'pending' AND NEW.warm_pool_result <> OLD.warm_pool_result THEN
    RAISE EXCEPTION 'Execution generation warm-pool result is immutable after decision' USING ERRCODE = '23514';
  END IF;
  IF OLD.terminal_outcome IS NOT NULL AND (
       NEW.terminal_outcome IS DISTINCT FROM OLD.terminal_outcome
       OR NEW.terminal_at IS DISTINCT FROM OLD.terminal_at
     ) THEN
    RAISE EXCEPTION 'Execution generation terminal outcome is immutable' USING ERRCODE = '23514';
  END IF;
  IF (NEW.dispatch_requested_at IS NOT NULL AND NEW.leased_at IS NOT NULL
       AND NEW.dispatch_requested_at > NEW.leased_at)
     OR (NEW.leased_at IS NOT NULL AND NEW.execution_started_at IS NOT NULL
       AND NEW.leased_at > NEW.execution_started_at)
     OR (NEW.execution_started_at IS NOT NULL AND NEW.provider_ready_at IS NOT NULL
       AND NEW.execution_started_at > NEW.provider_ready_at)
     OR (NEW.dispatch_requested_at IS NOT NULL AND NEW.terminal_at IS NOT NULL
       AND NEW.dispatch_requested_at > NEW.terminal_at) THEN
    RAISE EXCEPTION 'Execution generation fact timeline is invalid' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_generation_facts_update ON execution_generation_facts;
CREATE TRIGGER trg_execution_generation_facts_update
BEFORE UPDATE OR DELETE ON execution_generation_facts
FOR EACH ROW EXECUTE FUNCTION enforce_execution_generation_fact_update();
