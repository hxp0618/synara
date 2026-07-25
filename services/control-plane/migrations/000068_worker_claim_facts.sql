CREATE TABLE worker_claim_facts (
  id UUID PRIMARY KEY,
  worker_id UUID NOT NULL,
  worker_incarnation BIGINT NOT NULL,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  target_kind TEXT NOT NULL,
  claim_kind TEXT NOT NULL,
  request_id TEXT NOT NULL,
  claimed_at TIMESTAMPTZ NOT NULL,
  execution_id UUID REFERENCES agent_executions(id) ON DELETE RESTRICT,
  execution_generation BIGINT,
  cleanup_command_id UUID REFERENCES workspace_cleanup_commands(id) ON DELETE RESTRICT,
  cleanup_dispatch_generation BIGINT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT fk_worker_claim_facts_worker
    FOREIGN KEY (worker_id, worker_incarnation)
    REFERENCES worker_incarnation_facts(worker_id, worker_incarnation)
    ON DELETE RESTRICT,
  CONSTRAINT chk_worker_claim_facts_target_kind CHECK (
    target_kind IN ('local', 'ssh', 'docker', 'kubernetes')
  ),
  CONSTRAINT chk_worker_claim_facts_claim_kind CHECK (
    claim_kind IN ('execution', 'workspace-cleanup')
  ),
  CONSTRAINT chk_worker_claim_facts_request_id CHECK (
    length(btrim(request_id)) BETWEEN 1 AND 160
  ),
  CONSTRAINT chk_worker_claim_facts_generations CHECK (
    (execution_generation IS NULL OR execution_generation > 0)
    AND (cleanup_dispatch_generation IS NULL OR cleanup_dispatch_generation > 0)
  ),
  CONSTRAINT chk_worker_claim_facts_shape CHECK (
    (
      claim_kind = 'execution'
      AND execution_id IS NOT NULL
      AND execution_generation IS NOT NULL
      AND cleanup_command_id IS NULL
      AND cleanup_dispatch_generation IS NULL
    )
    OR
    (
      claim_kind = 'workspace-cleanup'
      AND execution_id IS NULL
      AND execution_generation IS NULL
      AND cleanup_command_id IS NOT NULL
      AND cleanup_dispatch_generation IS NOT NULL
    )
  )
);

CREATE INDEX idx_worker_claim_facts_billing
  ON worker_claim_facts (worker_id, worker_incarnation, claimed_at, id);

CREATE INDEX idx_worker_claim_facts_request
  ON worker_claim_facts (worker_id, worker_incarnation, request_id);

CREATE UNIQUE INDEX uq_worker_claim_facts_execution_generation
  ON worker_claim_facts (execution_id, execution_generation)
  WHERE claim_kind = 'execution';

CREATE UNIQUE INDEX uq_worker_claim_facts_cleanup_dispatch
  ON worker_claim_facts (cleanup_command_id, cleanup_dispatch_generation)
  WHERE claim_kind = 'workspace-cleanup';

CREATE OR REPLACE FUNCTION assert_worker_claim_fact_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  worker_fact worker_incarnation_facts%ROWTYPE;
  target execution_targets%ROWTYPE;
BEGIN
  SELECT * INTO worker_fact
  FROM worker_incarnation_facts
  WHERE worker_id = NEW.worker_id
    AND worker_incarnation = NEW.worker_incarnation;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Worker claim fact scope is invalid' USING ERRCODE = '23514';
  END IF;
  IF worker_fact.execution_target_id <> NEW.execution_target_id
     OR worker_fact.target_kind <> NEW.target_kind
     OR (worker_fact.tenant_id IS NOT NULL AND worker_fact.tenant_id IS DISTINCT FROM NEW.tenant_id)
     OR NEW.claimed_at < worker_fact.registered_at
     OR (worker_fact.terminated_at IS NOT NULL AND NEW.claimed_at > worker_fact.terminated_at) THEN
    RAISE EXCEPTION 'Worker claim fact scope is invalid' USING ERRCODE = '23514';
  END IF;

  SELECT * INTO target FROM execution_targets WHERE id = NEW.execution_target_id;
  IF NOT FOUND
     OR target.kind <> NEW.target_kind
     OR (target.tenant_id IS NOT NULL AND target.tenant_id IS DISTINCT FROM NEW.tenant_id) THEN
    RAISE EXCEPTION 'Worker claim fact scope is invalid' USING ERRCODE = '23514';
  END IF;

  IF NEW.claim_kind = 'execution' THEN
    IF NOT EXISTS (
      SELECT 1
      FROM agent_executions AS execution
      JOIN worker_leases AS lease
        ON lease.tenant_id = execution.tenant_id
       AND lease.execution_id = execution.id
       AND lease.worker_id = NEW.worker_id
       AND lease.worker_incarnation = NEW.worker_incarnation
       AND lease.generation = NEW.execution_generation
      WHERE execution.id = NEW.execution_id
        AND execution.tenant_id = NEW.tenant_id
        AND execution.execution_target_id = NEW.execution_target_id
        AND execution.target_kind = NEW.target_kind
        AND execution.generation = NEW.execution_generation
        AND execution.worker_id = NEW.worker_id
    ) THEN
      RAISE EXCEPTION 'Worker claim fact scope is invalid' USING ERRCODE = '23514';
    END IF;
  ELSE
    IF NOT EXISTS (
      SELECT 1
      FROM workspace_cleanup_commands AS cleanup
      WHERE cleanup.id = NEW.cleanup_command_id
        AND cleanup.tenant_id = NEW.tenant_id
        AND cleanup.execution_target_id = NEW.execution_target_id
        AND cleanup.target_kind = NEW.target_kind
        AND cleanup.dispatch_generation = NEW.cleanup_dispatch_generation
        AND cleanup.delivery_worker_id = NEW.worker_id
        AND cleanup.delivery_worker_incarnation = NEW.worker_incarnation
    ) THEN
      RAISE EXCEPTION 'Worker claim fact scope is invalid' USING ERRCODE = '23514';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_claim_facts_scope ON worker_claim_facts;
CREATE CONSTRAINT TRIGGER trg_worker_claim_facts_scope
AFTER INSERT ON worker_claim_facts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_claim_fact_scope();

CREATE OR REPLACE FUNCTION enforce_worker_claim_fact_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Worker claim facts are immutable' USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_claim_facts_immutable ON worker_claim_facts;
CREATE TRIGGER trg_worker_claim_facts_immutable
BEFORE UPDATE OR DELETE ON worker_claim_facts
FOR EACH ROW EXECUTE FUNCTION enforce_worker_claim_fact_immutable();
