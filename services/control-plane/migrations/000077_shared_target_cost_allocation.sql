-- Shared-target allocation is only authorized after an operator seals a
-- complete claim/release-ledger boundary. This migration intentionally creates
-- empty retained accounting tables and does not infer coverage for old data.
CREATE TABLE billing_shared_target_ledger_coverages (
  id UUID PRIMARY KEY,
  execution_target_id UUID NOT NULL UNIQUE
    REFERENCES execution_targets(id) ON DELETE RESTRICT,
  complete_from_at TIMESTAMPTZ NOT NULL,
  minimum_writer_version TEXT NOT NULL,
  deployment_attestation_sha256 TEXT NOT NULL,
  sealed_at TIMESTAMPTZ NOT NULL,
  sealed_by UUID NOT NULL,
  CONSTRAINT chk_billing_shared_target_ledger_coverages_writer_version CHECK (
    minimum_writer_version ~ '^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$'
  ),
  CONSTRAINT chk_billing_shared_target_ledger_coverages_attestation CHECK (
    deployment_attestation_sha256 ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT chk_billing_shared_target_ledger_coverages_timeline CHECK (
    sealed_at >= complete_from_at
  )
);

CREATE TABLE billing_shared_cost_allocation_runs (
  id UUID PRIMARY KEY,
  execution_target_id UUID NOT NULL
    REFERENCES execution_targets(id) ON DELETE RESTRICT,
  worker_id UUID NOT NULL,
  worker_incarnation BIGINT NOT NULL,
  ledger_coverage_id UUID NOT NULL
    REFERENCES billing_shared_target_ledger_coverages(id) ON DELETE RESTRICT,
  provider TEXT NOT NULL,
  region TEXT NOT NULL DEFAULT '',
  currency_code TEXT NOT NULL,
  billing_period_start_at TIMESTAMPTZ NOT NULL,
  billing_period_end_at TIMESTAMPTZ NOT NULL,
  algorithm_version TEXT NOT NULL,
  usage_start_at TIMESTAMPTZ NOT NULL,
  usage_end_at TIMESTAMPTZ NOT NULL,
  claim_count BIGINT NOT NULL,
  release_count BIGINT NOT NULL,
  ledger_sha256 TEXT NOT NULL,
  tenant_allocated_seconds BIGINT NOT NULL,
  platform_idle_seconds BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT fk_billing_shared_cost_allocation_runs_worker
    FOREIGN KEY (worker_id, worker_incarnation)
    REFERENCES worker_incarnation_facts(worker_id, worker_incarnation)
    ON DELETE RESTRICT,
  CONSTRAINT uq_billing_shared_cost_allocation_runs_identity UNIQUE (
    worker_id, worker_incarnation, provider, currency_code,
    billing_period_start_at, billing_period_end_at, algorithm_version
  ),
  CONSTRAINT chk_billing_shared_cost_allocation_runs_provider CHECK (
    provider ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
  ),
  CONSTRAINT chk_billing_shared_cost_allocation_runs_region CHECK (
    length(region) <= 160 AND btrim(region) = region
  ),
  CONSTRAINT chk_billing_shared_cost_allocation_runs_currency CHECK (
    currency_code ~ '^[A-Z]{3}$'
  ),
  CONSTRAINT chk_billing_shared_cost_allocation_runs_algorithm CHECK (
    algorithm_version = 'closed-claim-interval-v1'
  ),
  CONSTRAINT chk_billing_shared_cost_allocation_runs_period CHECK (
    billing_period_end_at > billing_period_start_at
    AND usage_end_at > usage_start_at
    AND usage_start_at >= billing_period_start_at
    AND usage_end_at <= billing_period_end_at
    AND created_at >= usage_end_at
  ),
  CONSTRAINT chk_billing_shared_cost_allocation_runs_counts CHECK (
    claim_count >= 0
    AND release_count = claim_count
    AND tenant_allocated_seconds >= 0
    AND platform_idle_seconds >= 0
  ),
  CONSTRAINT chk_billing_shared_cost_allocation_runs_ledger CHECK (
    ledger_sha256 ~ '^[0-9a-f]{64}$'
  )
);

CREATE INDEX idx_billing_shared_cost_allocation_runs_target_period
  ON billing_shared_cost_allocation_runs (
    execution_target_id, billing_period_start_at, billing_period_end_at, id
  );

CREATE TABLE billing_shared_estimated_charge_slices (
  id UUID PRIMARY KEY,
  run_id UUID NOT NULL
    REFERENCES billing_shared_cost_allocation_runs(id) ON DELETE RESTRICT,
  tenant_id UUID REFERENCES tenants(id) ON DELETE RESTRICT,
  claim_fact_id UUID REFERENCES worker_claim_facts(id) ON DELETE RESTRICT,
  tariff_id UUID NOT NULL REFERENCES billing_provider_tariffs(id) ON DELETE RESTRICT,
  charge_kind TEXT NOT NULL,
  allocation_kind TEXT NOT NULL,
  resource_correlation_key TEXT NOT NULL,
  billing_period_start_at TIMESTAMPTZ NOT NULL,
  billing_period_end_at TIMESTAMPTZ NOT NULL,
  usage_start_at TIMESTAMPTZ NOT NULL,
  usage_end_at TIMESTAMPTZ NOT NULL,
  billable_seconds BIGINT NOT NULL,
  claim_count BIGINT NOT NULL,
  requested_cpu_millicores BIGINT,
  requested_memory_bytes BIGINT,
  requested_ephemeral_storage_bytes BIGINT,
  rate_micros BIGINT NOT NULL,
  amount_micros BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_billing_shared_estimated_charge_slices_charge_kind CHECK (
    charge_kind IN ('cpu', 'memory', 'ephemeral-storage', 'request', 'pod')
  ),
  CONSTRAINT chk_billing_shared_estimated_charge_slices_allocation_kind CHECK (
    allocation_kind IN ('tenant-claim', 'platform-idle')
  ),
  CONSTRAINT chk_billing_shared_estimated_charge_slices_shape CHECK (
    (
      allocation_kind = 'tenant-claim'
      AND tenant_id IS NOT NULL
      AND claim_fact_id IS NOT NULL
      AND claim_count = 1
    )
    OR
    (
      allocation_kind = 'platform-idle'
      AND tenant_id IS NULL
      AND claim_fact_id IS NULL
      AND claim_count = 0
      AND charge_kind <> 'request'
    )
  ),
  CONSTRAINT chk_billing_shared_estimated_charge_slices_resource_key CHECK (
    length(btrim(resource_correlation_key)) BETWEEN 1 AND 512
  ),
  CONSTRAINT chk_billing_shared_estimated_charge_slices_period CHECK (
    billing_period_end_at > billing_period_start_at
    AND usage_end_at > usage_start_at
    AND usage_start_at >= billing_period_start_at
    AND usage_end_at <= billing_period_end_at
    AND created_at >= usage_end_at
  ),
  CONSTRAINT chk_billing_shared_estimated_charge_slices_counters CHECK (
    billable_seconds >= 0
    AND rate_micros >= 0
    AND amount_micros >= 0
    AND (requested_cpu_millicores IS NULL OR requested_cpu_millicores > 0)
    AND (requested_memory_bytes IS NULL OR requested_memory_bytes > 0)
    AND (requested_ephemeral_storage_bytes IS NULL OR requested_ephemeral_storage_bytes > 0)
  )
);

CREATE INDEX idx_billing_shared_estimated_charge_slices_run
  ON billing_shared_estimated_charge_slices (run_id, allocation_kind, usage_start_at, id);
CREATE INDEX idx_billing_shared_estimated_charge_slices_tenant_period
  ON billing_shared_estimated_charge_slices (
    tenant_id, billing_period_start_at, billing_period_end_at, charge_kind, id
  );
CREATE UNIQUE INDEX uq_billing_shared_estimated_charge_slices_semantic
  ON billing_shared_estimated_charge_slices (
    run_id,
    allocation_kind,
    COALESCE(tenant_id::text, '-'),
    COALESCE(claim_fact_id::text, '-'),
    tariff_id,
    charge_kind,
    usage_start_at,
    usage_end_at
  );

CREATE OR REPLACE FUNCTION assert_billing_shared_target_ledger_coverage_scope()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM execution_targets AS target
    WHERE target.id = NEW.execution_target_id AND target.tenant_id IS NULL
  ) THEN
    RAISE EXCEPTION 'billing shared target ledger coverage scope is invalid'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_target_ledger_coverages_scope
BEFORE INSERT ON billing_shared_target_ledger_coverages
FOR EACH ROW EXECUTE FUNCTION assert_billing_shared_target_ledger_coverage_scope();

CREATE OR REPLACE FUNCTION assert_billing_shared_cost_allocation_run_scope()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  coverage billing_shared_target_ledger_coverages%ROWTYPE;
  worker_fact worker_incarnation_facts%ROWTYPE;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended(concat_ws(
    chr(31),
    'synara:billing-shared-cost-allocation',
    NEW.worker_id::text,
    NEW.worker_incarnation::text,
    NEW.provider,
    NEW.currency_code,
    NEW.algorithm_version
  ), 0));

  SELECT * INTO coverage FROM billing_shared_target_ledger_coverages
  WHERE id = NEW.ledger_coverage_id;
  SELECT * INTO worker_fact FROM worker_incarnation_facts
  WHERE worker_id = NEW.worker_id
    AND worker_incarnation = NEW.worker_incarnation;

  IF coverage.id IS NULL
     OR coverage.execution_target_id <> NEW.execution_target_id
     OR worker_fact.worker_id IS NULL
     OR worker_fact.tenant_id IS NOT NULL
     OR worker_fact.execution_target_id <> NEW.execution_target_id
     OR worker_fact.region <> NEW.region
     OR worker_fact.current_state <> 'terminated'
     OR worker_fact.terminated_at IS NULL
     OR worker_fact.registered_at < coverage.complete_from_at
     OR NEW.usage_start_at < worker_fact.registered_at
     OR NEW.usage_end_at > worker_fact.terminated_at THEN
    RAISE EXCEPTION 'billing shared cost allocation run scope is invalid'
      USING ERRCODE = '23514';
  END IF;
  IF EXISTS (
    SELECT 1
    FROM billing_shared_cost_allocation_runs AS existing
    WHERE existing.worker_id = NEW.worker_id
      AND existing.worker_incarnation = NEW.worker_incarnation
      AND existing.provider = NEW.provider
      AND existing.currency_code = NEW.currency_code
      AND existing.algorithm_version = NEW.algorithm_version
      AND existing.billing_period_start_at < NEW.billing_period_end_at
      AND existing.billing_period_end_at > NEW.billing_period_start_at
      AND NOT (
        existing.billing_period_start_at = NEW.billing_period_start_at
        AND existing.billing_period_end_at = NEW.billing_period_end_at
      )
  ) THEN
    RAISE EXCEPTION 'billing shared cost allocation run period overlaps existing history'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_cost_allocation_runs_scope
BEFORE INSERT ON billing_shared_cost_allocation_runs
FOR EACH ROW EXECUTE FUNCTION assert_billing_shared_cost_allocation_run_scope();

CREATE OR REPLACE FUNCTION assert_billing_shared_estimated_charge_slice_scope()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  allocation_run billing_shared_cost_allocation_runs%ROWTYPE;
  claim worker_claim_facts%ROWTYPE;
  claim_release worker_claim_release_facts%ROWTYPE;
  tariff billing_provider_tariffs%ROWTYPE;
  worker_fact worker_incarnation_facts%ROWTYPE;
BEGIN
  SELECT * INTO allocation_run FROM billing_shared_cost_allocation_runs WHERE id = NEW.run_id;
  SELECT * INTO tariff FROM billing_provider_tariffs WHERE id = NEW.tariff_id;
  SELECT * INTO worker_fact FROM worker_incarnation_facts
  WHERE worker_id = allocation_run.worker_id
    AND worker_incarnation = allocation_run.worker_incarnation;

  IF allocation_run.id IS NULL
     OR tariff.id IS NULL
     OR NEW.billing_period_start_at <> allocation_run.billing_period_start_at
     OR NEW.billing_period_end_at <> allocation_run.billing_period_end_at
     OR NEW.usage_start_at < allocation_run.usage_start_at
     OR NEW.usage_end_at > allocation_run.usage_end_at
     OR tariff.provider <> allocation_run.provider
     OR NOT (
       tariff.region = allocation_run.region
       OR (allocation_run.region <> '' AND tariff.region = '')
     )
     OR tariff.currency_code <> allocation_run.currency_code
     OR worker_fact.worker_id IS NULL
     OR NEW.requested_cpu_millicores IS DISTINCT FROM worker_fact.requested_cpu_millicores
     OR NEW.requested_memory_bytes IS DISTINCT FROM worker_fact.requested_memory_bytes
     OR NEW.requested_ephemeral_storage_bytes IS DISTINCT FROM worker_fact.requested_ephemeral_storage_bytes
     OR NEW.rate_micros <> (CASE NEW.charge_kind
       WHEN 'cpu' THEN tariff.cpu_core_hour_rate_micros
       WHEN 'memory' THEN tariff.memory_gib_hour_rate_micros
       WHEN 'ephemeral-storage' THEN tariff.ephemeral_gib_hour_rate_micros
       WHEN 'request' THEN tariff.request_rate_micros
       WHEN 'pod' THEN tariff.pod_hour_rate_micros
       ELSE -1
     END)
     OR (NEW.charge_kind = 'request' AND (NEW.billable_seconds <> 0 OR NEW.amount_micros <> NEW.rate_micros)) THEN
    RAISE EXCEPTION 'billing shared estimated charge slice parent scope is invalid'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.allocation_kind = 'tenant-claim' THEN
    SELECT * INTO claim FROM worker_claim_facts WHERE id = NEW.claim_fact_id;
    SELECT * INTO claim_release FROM worker_claim_release_facts
    WHERE claim_fact_id = NEW.claim_fact_id;
    IF claim.id IS NULL
       OR claim_release.claim_fact_id IS NULL
       OR claim.tenant_id <> NEW.tenant_id
       OR claim.worker_id <> allocation_run.worker_id
       OR claim.worker_incarnation <> allocation_run.worker_incarnation
       OR claim.execution_target_id <> allocation_run.execution_target_id THEN
      RAISE EXCEPTION 'billing shared estimated charge slice claim scope is invalid'
        USING ERRCODE = '23514';
    END IF;

    IF NEW.charge_kind = 'request' THEN
      IF claim.claimed_at < NEW.usage_start_at
         OR (
           claim.claimed_at >= NEW.usage_end_at
           AND NOT (
             claim.claimed_at = NEW.usage_end_at
             AND NEW.usage_end_at = allocation_run.usage_end_at
             AND allocation_run.usage_end_at = worker_fact.terminated_at
           )
         )
         OR claim.claimed_at < tariff.effective_start_at
         OR (
           tariff.effective_end_at IS NOT NULL
           AND claim.claimed_at >= tariff.effective_end_at
           AND NOT (
             claim.claimed_at = tariff.effective_end_at
             AND claim.claimed_at = allocation_run.usage_end_at
             AND NEW.usage_end_at = allocation_run.usage_end_at
             AND allocation_run.usage_end_at = worker_fact.terminated_at
           )
         ) THEN
        RAISE EXCEPTION 'billing shared request charge slice window is invalid'
          USING ERRCODE = '23514';
      END IF;
    ELSIF NEW.usage_start_at < claim.claimed_at
       OR NEW.usage_end_at > claim_release.released_at THEN
      RAISE EXCEPTION 'billing shared time charge slice claim interval is invalid'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  IF tariff.region = '' AND allocation_run.region <> '' AND EXISTS (
    SELECT 1
    FROM billing_provider_tariffs AS regional
    WHERE regional.provider = allocation_run.provider
      AND regional.region = allocation_run.region
      AND regional.currency_code = allocation_run.currency_code
      AND (
        (
          NEW.charge_kind <> 'request'
          AND regional.effective_start_at < NEW.usage_end_at
          AND (regional.effective_end_at IS NULL OR regional.effective_end_at > NEW.usage_start_at)
        )
        OR (
          NEW.charge_kind = 'request'
          AND (
            (
              claim.claimed_at < allocation_run.usage_end_at
              AND regional.effective_start_at <= claim.claimed_at
              AND (regional.effective_end_at IS NULL OR regional.effective_end_at > claim.claimed_at)
            )
            OR (
              claim.claimed_at = allocation_run.usage_end_at
              AND allocation_run.usage_end_at = worker_fact.terminated_at
              AND regional.effective_start_at < claim.claimed_at
              AND (regional.effective_end_at IS NULL OR regional.effective_end_at >= claim.claimed_at)
            )
          )
        )
      )
  ) THEN
    RAISE EXCEPTION 'billing shared estimated charge slice bypasses regional tariff precedence'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.charge_kind <> 'request'
     AND (NEW.usage_start_at < tariff.effective_start_at
       OR (tariff.effective_end_at IS NOT NULL AND NEW.usage_end_at > tariff.effective_end_at)) THEN
    RAISE EXCEPTION 'billing shared time charge slice tariff window is invalid'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_estimated_charge_slices_scope
BEFORE INSERT ON billing_shared_estimated_charge_slices
FOR EACH ROW EXECUTE FUNCTION assert_billing_shared_estimated_charge_slice_scope();

CREATE OR REPLACE FUNCTION enforce_billing_shared_accounting_immutable()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'billing shared allocation accounting history is immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_billing_shared_target_ledger_coverages_immutable
BEFORE UPDATE OR DELETE ON billing_shared_target_ledger_coverages
FOR EACH ROW EXECUTE FUNCTION enforce_billing_shared_accounting_immutable();
CREATE TRIGGER trg_billing_shared_cost_allocation_runs_immutable
BEFORE UPDATE OR DELETE ON billing_shared_cost_allocation_runs
FOR EACH ROW EXECUTE FUNCTION enforce_billing_shared_accounting_immutable();
CREATE TRIGGER trg_billing_shared_estimated_charge_slices_immutable
BEFORE UPDATE OR DELETE ON billing_shared_estimated_charge_slices
FOR EACH ROW EXECUTE FUNCTION enforce_billing_shared_accounting_immutable();

COMMENT ON TABLE billing_shared_target_ledger_coverages IS
  'Operator-sealed boundary after which a shared Target claim/release ledger is complete; no migration backfill is inferred.';
COMMENT ON TABLE billing_shared_cost_allocation_runs IS
  'Immutable per-Worker billing-period allocation evidence. Global interval conservation is validated by the allocation core, not this table.';
COMMENT ON TABLE billing_shared_estimated_charge_slices IS
  'Immutable tenant-claim or platform-idle charge slices. Platform idle is intentionally not assigned to a synthetic Tenant.';
