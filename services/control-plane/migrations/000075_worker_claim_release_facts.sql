-- First-phase claim release ledger rollout: new control-plane processes double-write
-- releases when they remove an active claim. Deletion guards are intentionally
-- deferred until migration-era claims have been backfilled and every writer is gated.
CREATE TABLE worker_claim_release_facts (
  claim_fact_id UUID PRIMARY KEY REFERENCES worker_claim_facts(id) ON DELETE RESTRICT,
  released_at TIMESTAMPTZ NOT NULL,
  recorded_at TIMESTAMPTZ NOT NULL,
  release_reason TEXT NOT NULL,
  authority_kind TEXT NOT NULL,
  authority_id TEXT,
  request_id TEXT,
  metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
  CONSTRAINT chk_worker_claim_release_facts_reason CHECK (
    release_reason IN (
      'execution_completed',
      'execution_failed',
      'worker_released',
      'orphan_lease_expired',
      'lease_expired',
      'user_cancelled',
      'tenant_deleted',
      'session_absolute_expired',
      'interaction_expired',
      'resource_suspended_worker_attested',
      'resource_suspended_pod_terminal',
      'control_interrupted',
      'control_operation_completed',
      'worker_revoked',
      'cleanup_acknowledged',
      'cleanup_failed_retryable',
      'cleanup_failed_terminal',
      'cleanup_worker_released',
      'cleanup_lease_expired',
      'cleanup_attempts_exhausted',
      'cleanup_worker_revoked',
      'cleanup_pod_confirmed_absent'
    )
  ),
  CONSTRAINT chk_worker_claim_release_facts_authority CHECK (
    authority_kind IN ('worker', 'user', 'control-plane', 'kubernetes')
  ),
  CONSTRAINT chk_worker_claim_release_facts_authority_id CHECK (
    authority_id IS NULL OR length(btrim(authority_id)) BETWEEN 1 AND 160
  ),
  CONSTRAINT chk_worker_claim_release_facts_request_id CHECK (
    request_id IS NULL OR length(btrim(request_id)) BETWEEN 1 AND 160
  ),
  CONSTRAINT chk_worker_claim_release_facts_timeline CHECK (
    recorded_at >= released_at
  ),
  CONSTRAINT chk_worker_claim_release_facts_metadata CHECK (
    jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 4096
  )
);

CREATE INDEX idx_worker_claim_release_facts_released
  ON worker_claim_release_facts (released_at, claim_fact_id);

CREATE OR REPLACE FUNCTION assert_worker_claim_release_fact_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM worker_claim_facts AS claim
    WHERE claim.id = NEW.claim_fact_id
      AND NEW.released_at >= claim.claimed_at
  ) THEN
    RAISE EXCEPTION 'Worker claim release fact scope is invalid' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_claim_release_facts_scope ON worker_claim_release_facts;
CREATE TRIGGER trg_worker_claim_release_facts_scope
BEFORE INSERT ON worker_claim_release_facts
FOR EACH ROW EXECUTE FUNCTION assert_worker_claim_release_fact_scope();

CREATE OR REPLACE FUNCTION enforce_worker_claim_release_fact_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Worker claim release facts are immutable' USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_claim_release_facts_immutable ON worker_claim_release_facts;
CREATE TRIGGER trg_worker_claim_release_facts_immutable
BEFORE UPDATE OR DELETE ON worker_claim_release_facts
FOR EACH ROW EXECUTE FUNCTION enforce_worker_claim_release_fact_immutable();
