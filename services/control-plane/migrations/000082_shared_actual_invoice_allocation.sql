-- Account-level provider invoices remain operator-owned external truth. This
-- graph attributes only exact invoice lines whose provider/currency/period,
-- resource key, and charge kind match a sealed shared-Target estimate graph.
-- No historical rows are inferred or backfilled.
CREATE TABLE billing_shared_actual_allocation_runs (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL,
  invoice_import_id UUID NOT NULL,
  execution_target_id UUID NOT NULL
    REFERENCES execution_targets(id) ON DELETE RESTRICT,
  ledger_coverage_id UUID NOT NULL
    REFERENCES billing_shared_target_ledger_coverages(id) ON DELETE RESTRICT,
  provider TEXT NOT NULL,
  currency_code TEXT NOT NULL,
  billing_period_start_at TIMESTAMPTZ NOT NULL,
  billing_period_end_at TIMESTAMPTZ NOT NULL,
  algorithm_version TEXT NOT NULL,
  source_checksum TEXT NOT NULL,
  source_scope_attestation_sha256 TEXT NOT NULL,
  source_line_set_sha256 TEXT NOT NULL,
  import_line_count BIGINT NOT NULL,
  import_amount_micros BIGINT NOT NULL,
  source_line_count BIGINT NOT NULL,
  source_amount_micros BIGINT NOT NULL,
  unallocated_line_count BIGINT NOT NULL,
  unallocated_amount_micros BIGINT NOT NULL,
  allocation_line_count BIGINT NOT NULL,
  allocation_slice_count BIGINT NOT NULL,
  allocated_amount_micros BIGINT NOT NULL,
  state TEXT NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  sealed_at TIMESTAMPTZ,
  CONSTRAINT fk_billing_shared_actual_allocation_runs_import
    FOREIGN KEY (operator_tenant_id, invoice_import_id)
    REFERENCES billing_actual_invoice_imports(tenant_id, id)
    ON DELETE RESTRICT,
  CONSTRAINT uq_billing_shared_actual_allocation_runs_identity UNIQUE (
    operator_tenant_id, invoice_import_id, execution_target_id, algorithm_version
  ),
  CONSTRAINT chk_billing_shared_actual_allocation_runs_provider CHECK (
    provider ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
  ),
  CONSTRAINT chk_billing_shared_actual_allocation_runs_currency CHECK (
    currency_code ~ '^[A-Z]{3}$'
  ),
  CONSTRAINT chk_billing_shared_actual_allocation_runs_algorithm CHECK (
    algorithm_version = 'proportional-shared-estimate-v1'
  ),
  CONSTRAINT chk_billing_shared_actual_allocation_runs_period CHECK (
    billing_period_end_at > billing_period_start_at
    AND created_at >= billing_period_end_at
  ),
  CONSTRAINT chk_billing_shared_actual_allocation_runs_digests CHECK (
    source_checksum ~ '^[0-9a-f]{64}$'
    AND source_scope_attestation_sha256 ~ '^[0-9a-f]{64}$'
    AND source_line_set_sha256 ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT chk_billing_shared_actual_allocation_runs_counts CHECK (
    import_line_count >= source_line_count
    AND source_line_count > 0
    AND unallocated_line_count = import_line_count - source_line_count
    AND allocation_line_count = source_line_count
    AND allocation_slice_count >= allocation_line_count
  ),
  CONSTRAINT chk_billing_shared_actual_allocation_runs_amounts CHECK (
    import_amount_micros = source_amount_micros + unallocated_amount_micros
    AND allocated_amount_micros = source_amount_micros
  ),
  CONSTRAINT chk_billing_shared_actual_allocation_runs_state CHECK (
    (state = 'building' AND sealed_at IS NULL)
    OR (state = 'sealed' AND sealed_at IS NOT NULL AND sealed_at >= created_at)
  )
);

CREATE INDEX idx_billing_shared_actual_allocation_runs_target_period
  ON billing_shared_actual_allocation_runs (
    execution_target_id, billing_period_start_at, billing_period_end_at, id
  );

CREATE TABLE billing_shared_actual_allocation_lines (
  id UUID PRIMARY KEY,
  run_id UUID NOT NULL
    REFERENCES billing_shared_actual_allocation_runs(id) ON DELETE RESTRICT,
  actual_invoice_line_id UUID NOT NULL UNIQUE
    REFERENCES billing_actual_invoice_lines(id) ON DELETE RESTRICT,
  source_amount_micros BIGINT NOT NULL,
  estimated_amount_micros BIGINT NOT NULL,
  allocated_amount_micros BIGINT NOT NULL,
  estimated_slice_count BIGINT NOT NULL,
  estimated_slice_set_sha256 TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_billing_shared_actual_allocation_lines_shape CHECK (
    estimated_amount_micros >= 0
    AND allocated_amount_micros = source_amount_micros
    AND estimated_slice_count > 0
    AND estimated_slice_set_sha256 ~ '^[0-9a-f]{64}$'
  )
);

CREATE INDEX idx_billing_shared_actual_allocation_lines_run
  ON billing_shared_actual_allocation_lines (run_id, actual_invoice_line_id);

CREATE TABLE billing_shared_actual_charge_slices (
  id UUID PRIMARY KEY,
  allocation_line_id UUID NOT NULL
    REFERENCES billing_shared_actual_allocation_lines(id) ON DELETE RESTRICT,
  estimated_slice_id UUID NOT NULL UNIQUE
    REFERENCES billing_shared_estimated_charge_slices(id) ON DELETE RESTRICT,
  tenant_id UUID REFERENCES tenants(id) ON DELETE RESTRICT,
  allocation_kind TEXT NOT NULL,
  estimate_weight_micros BIGINT NOT NULL,
  amount_micros BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_billing_shared_actual_charge_slices_semantic UNIQUE (
    allocation_line_id, estimated_slice_id
  ),
  CONSTRAINT chk_billing_shared_actual_charge_slices_kind CHECK (
    allocation_kind IN ('tenant-claim', 'platform-idle')
  ),
  CONSTRAINT chk_billing_shared_actual_charge_slices_shape CHECK (
    estimate_weight_micros >= 0
    AND (
      (allocation_kind = 'tenant-claim' AND tenant_id IS NOT NULL)
      OR (allocation_kind = 'platform-idle' AND tenant_id IS NULL)
    )
  )
);

CREATE INDEX idx_billing_shared_actual_charge_slices_line
  ON billing_shared_actual_charge_slices (allocation_line_id, estimated_slice_id);
CREATE INDEX idx_billing_shared_actual_charge_slices_tenant
  ON billing_shared_actual_charge_slices (tenant_id, allocation_kind, id);

CREATE OR REPLACE FUNCTION assert_billing_shared_actual_allocation_run_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  invoice_import billing_actual_invoice_imports%ROWTYPE;
  coverage billing_shared_target_ledger_coverages%ROWTYPE;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended(concat_ws(
    chr(31),
    'synara:billing-shared-actual-invoice-allocation',
    NEW.operator_tenant_id::text,
    NEW.invoice_import_id::text
  ), 0));
  PERFORM pg_advisory_xact_lock(hashtextextended(concat_ws(
    chr(31),
    'synara:billing-shared-estimate-period-snapshot',
    NEW.provider,
    NEW.currency_code,
    ((extract(epoch FROM NEW.billing_period_start_at) * 1000000)::bigint)::text,
    ((extract(epoch FROM NEW.billing_period_end_at) * 1000000)::bigint)::text
  ), 0));

  SELECT * INTO invoice_import
  FROM billing_actual_invoice_imports
  WHERE tenant_id = NEW.operator_tenant_id AND id = NEW.invoice_import_id;
  SELECT * INTO coverage
  FROM billing_shared_target_ledger_coverages
  WHERE id = NEW.ledger_coverage_id;

  IF invoice_import.id IS NULL
     OR coverage.id IS NULL
     OR coverage.execution_target_id <> NEW.execution_target_id
     OR NOT EXISTS (
       SELECT 1 FROM execution_targets AS target
       WHERE target.id = NEW.execution_target_id AND target.tenant_id IS NULL
     )
     OR invoice_import.provider <> NEW.provider
     OR invoice_import.currency_code <> NEW.currency_code
     OR invoice_import.billing_period_start_at <> NEW.billing_period_start_at
     OR invoice_import.billing_period_end_at <> NEW.billing_period_end_at
     OR invoice_import.source_checksum <> NEW.source_checksum THEN
    RAISE EXCEPTION 'billing shared actual allocation run scope is invalid'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_actual_allocation_runs_insert
BEFORE INSERT ON billing_shared_actual_allocation_runs
FOR EACH ROW EXECUTE FUNCTION assert_billing_shared_actual_allocation_run_insert();

CREATE OR REPLACE FUNCTION assert_billing_shared_actual_allocation_line_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  allocation_run billing_shared_actual_allocation_runs%ROWTYPE;
  actual_line billing_actual_invoice_lines%ROWTYPE;
BEGIN
  SELECT * INTO allocation_run
  FROM billing_shared_actual_allocation_runs WHERE id = NEW.run_id;
  SELECT * INTO actual_line
  FROM billing_actual_invoice_lines WHERE id = NEW.actual_invoice_line_id;

  IF allocation_run.id IS NULL
     OR allocation_run.state <> 'building'
     OR actual_line.id IS NULL
     OR actual_line.tenant_id <> allocation_run.operator_tenant_id
     OR actual_line.invoice_import_id <> allocation_run.invoice_import_id
     OR actual_line.provider <> allocation_run.provider
     OR actual_line.currency_code <> allocation_run.currency_code
     OR actual_line.billing_period_start_at <> allocation_run.billing_period_start_at
     OR actual_line.billing_period_end_at <> allocation_run.billing_period_end_at
     OR actual_line.amount_micros <> NEW.source_amount_micros
     OR NOT EXISTS (
       SELECT 1
       FROM billing_shared_estimated_charge_slices AS estimate_slice
       JOIN billing_shared_cost_allocation_runs AS estimate_run
         ON estimate_run.id = estimate_slice.run_id
       WHERE estimate_run.execution_target_id = allocation_run.execution_target_id
         AND estimate_run.provider = actual_line.provider
         AND estimate_run.currency_code = actual_line.currency_code
         AND estimate_run.billing_period_start_at = actual_line.billing_period_start_at
         AND estimate_run.billing_period_end_at = actual_line.billing_period_end_at
         AND estimate_slice.charge_kind = actual_line.charge_kind
         AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
     ) THEN
    RAISE EXCEPTION 'billing shared actual allocation line scope is invalid'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_actual_allocation_lines_insert
BEFORE INSERT ON billing_shared_actual_allocation_lines
FOR EACH ROW EXECUTE FUNCTION assert_billing_shared_actual_allocation_line_insert();

CREATE OR REPLACE FUNCTION assert_billing_shared_actual_charge_slice_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  allocation_line billing_shared_actual_allocation_lines%ROWTYPE;
  allocation_run billing_shared_actual_allocation_runs%ROWTYPE;
  actual_line billing_actual_invoice_lines%ROWTYPE;
  estimate_slice billing_shared_estimated_charge_slices%ROWTYPE;
  estimate_run billing_shared_cost_allocation_runs%ROWTYPE;
BEGIN
  SELECT * INTO allocation_line
  FROM billing_shared_actual_allocation_lines WHERE id = NEW.allocation_line_id;
  SELECT * INTO allocation_run
  FROM billing_shared_actual_allocation_runs WHERE id = allocation_line.run_id;
  SELECT * INTO actual_line
  FROM billing_actual_invoice_lines WHERE id = allocation_line.actual_invoice_line_id;
  SELECT * INTO estimate_slice
  FROM billing_shared_estimated_charge_slices WHERE id = NEW.estimated_slice_id;
  SELECT * INTO estimate_run
  FROM billing_shared_cost_allocation_runs WHERE id = estimate_slice.run_id;

  IF allocation_line.id IS NULL
     OR allocation_run.id IS NULL
     OR allocation_run.state <> 'building'
     OR actual_line.id IS NULL
     OR estimate_slice.id IS NULL
     OR estimate_run.id IS NULL
     OR estimate_run.execution_target_id <> allocation_run.execution_target_id
     OR estimate_run.provider <> actual_line.provider
     OR estimate_run.currency_code <> actual_line.currency_code
     OR estimate_run.billing_period_start_at <> actual_line.billing_period_start_at
     OR estimate_run.billing_period_end_at <> actual_line.billing_period_end_at
     OR estimate_slice.charge_kind <> actual_line.charge_kind
     OR estimate_slice.resource_correlation_key <> actual_line.resource_correlation_key
     OR estimate_slice.tenant_id IS DISTINCT FROM NEW.tenant_id
     OR estimate_slice.allocation_kind <> NEW.allocation_kind
     OR estimate_slice.amount_micros <> NEW.estimate_weight_micros THEN
    RAISE EXCEPTION 'billing shared actual charge slice scope is invalid'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_actual_charge_slices_insert
BEFORE INSERT ON billing_shared_actual_charge_slices
FOR EACH ROW EXECUTE FUNCTION assert_billing_shared_actual_charge_slice_insert();

CREATE OR REPLACE FUNCTION fence_billing_actual_invoice_line_after_shared_allocation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM billing_shared_actual_allocation_runs AS allocation_run
    WHERE allocation_run.operator_tenant_id = NEW.tenant_id
      AND allocation_run.invoice_import_id = NEW.invoice_import_id
      AND allocation_run.state = 'sealed'
  ) THEN
    RAISE EXCEPTION 'billing actual invoice import is sealed by shared allocation'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_actual_invoice_lines_shared_allocation_fence
BEFORE INSERT ON billing_actual_invoice_lines
FOR EACH ROW EXECUTE FUNCTION fence_billing_actual_invoice_line_after_shared_allocation();

CREATE OR REPLACE FUNCTION fence_billing_shared_estimate_after_actual_allocation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  estimate_run billing_shared_cost_allocation_runs%ROWTYPE;
BEGIN
  SELECT * INTO estimate_run
  FROM billing_shared_cost_allocation_runs WHERE id = NEW.run_id;
  IF estimate_run.id IS NOT NULL AND EXISTS (
    SELECT 1
    FROM billing_shared_actual_allocation_lines AS allocation_line
    JOIN billing_shared_actual_allocation_runs AS allocation_run
      ON allocation_run.id = allocation_line.run_id
     AND allocation_run.state = 'sealed'
    JOIN billing_actual_invoice_lines AS actual_line
      ON actual_line.id = allocation_line.actual_invoice_line_id
    WHERE actual_line.provider = estimate_run.provider
      AND actual_line.currency_code = estimate_run.currency_code
      AND actual_line.billing_period_start_at = NEW.billing_period_start_at
      AND actual_line.billing_period_end_at = NEW.billing_period_end_at
      AND actual_line.charge_kind = NEW.charge_kind
      AND actual_line.resource_correlation_key = NEW.resource_correlation_key
  ) THEN
    RAISE EXCEPTION 'billing shared estimate scope is sealed by actual allocation'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_estimated_charge_slices_actual_allocation_fence
BEFORE INSERT ON billing_shared_estimated_charge_slices
FOR EACH ROW EXECUTE FUNCTION fence_billing_shared_estimate_after_actual_allocation();

CREATE OR REPLACE FUNCTION seal_billing_shared_actual_allocation_run()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  actual_import_line_count BIGINT;
  actual_import_amount NUMERIC;
  retained_line_count BIGINT;
  retained_source_amount NUMERIC;
  retained_allocated_amount NUMERIC;
  retained_slice_count BIGINT;
BEGIN
  IF OLD.state <> 'building'
     OR NEW.state <> 'sealed'
     OR NEW.sealed_at IS NULL
     OR NEW.sealed_at < NEW.created_at
     OR NEW.id IS DISTINCT FROM OLD.id
     OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.invoice_import_id IS DISTINCT FROM OLD.invoice_import_id
     OR NEW.execution_target_id IS DISTINCT FROM OLD.execution_target_id
     OR NEW.ledger_coverage_id IS DISTINCT FROM OLD.ledger_coverage_id
     OR NEW.provider IS DISTINCT FROM OLD.provider
     OR NEW.currency_code IS DISTINCT FROM OLD.currency_code
     OR NEW.billing_period_start_at IS DISTINCT FROM OLD.billing_period_start_at
     OR NEW.billing_period_end_at IS DISTINCT FROM OLD.billing_period_end_at
     OR NEW.algorithm_version IS DISTINCT FROM OLD.algorithm_version
     OR NEW.source_checksum IS DISTINCT FROM OLD.source_checksum
     OR NEW.source_scope_attestation_sha256 IS DISTINCT FROM OLD.source_scope_attestation_sha256
     OR NEW.source_line_set_sha256 IS DISTINCT FROM OLD.source_line_set_sha256
     OR NEW.import_line_count IS DISTINCT FROM OLD.import_line_count
     OR NEW.import_amount_micros IS DISTINCT FROM OLD.import_amount_micros
     OR NEW.source_line_count IS DISTINCT FROM OLD.source_line_count
     OR NEW.source_amount_micros IS DISTINCT FROM OLD.source_amount_micros
     OR NEW.unallocated_line_count IS DISTINCT FROM OLD.unallocated_line_count
     OR NEW.unallocated_amount_micros IS DISTINCT FROM OLD.unallocated_amount_micros
     OR NEW.allocation_line_count IS DISTINCT FROM OLD.allocation_line_count
     OR NEW.allocation_slice_count IS DISTINCT FROM OLD.allocation_slice_count
     OR NEW.allocated_amount_micros IS DISTINCT FROM OLD.allocated_amount_micros
     OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'billing shared actual allocation run transition is invalid'
      USING ERRCODE = '23514';
  END IF;

  SELECT count(*), COALESCE(sum(amount_micros), 0)
    INTO actual_import_line_count, actual_import_amount
  FROM billing_actual_invoice_lines
  WHERE tenant_id = NEW.operator_tenant_id
    AND invoice_import_id = NEW.invoice_import_id;

  SELECT count(*), COALESCE(sum(source_amount_micros), 0),
         COALESCE(sum(allocated_amount_micros), 0)
    INTO retained_line_count, retained_source_amount, retained_allocated_amount
  FROM billing_shared_actual_allocation_lines
  WHERE run_id = NEW.id;

  SELECT count(*) INTO retained_slice_count
  FROM billing_shared_actual_charge_slices AS actual_slice
  JOIN billing_shared_actual_allocation_lines AS allocation_line
    ON allocation_line.id = actual_slice.allocation_line_id
  WHERE allocation_line.run_id = NEW.id;

  IF actual_import_line_count <> NEW.import_line_count
     OR actual_import_amount <> NEW.import_amount_micros
     OR retained_line_count <> NEW.allocation_line_count
     OR retained_line_count <> NEW.source_line_count
     OR retained_source_amount <> NEW.source_amount_micros
     OR retained_allocated_amount <> NEW.allocated_amount_micros
     OR retained_slice_count <> NEW.allocation_slice_count
     OR EXISTS (
       SELECT 1
       FROM billing_shared_actual_allocation_lines AS allocation_line
       LEFT JOIN billing_shared_actual_charge_slices AS actual_slice
         ON actual_slice.allocation_line_id = allocation_line.id
       WHERE allocation_line.run_id = NEW.id
       GROUP BY allocation_line.id
       HAVING count(actual_slice.id) <> allocation_line.estimated_slice_count
          OR COALESCE(sum(actual_slice.estimate_weight_micros), 0) <> allocation_line.estimated_amount_micros
          OR COALESCE(sum(actual_slice.amount_micros), 0) <> allocation_line.allocated_amount_micros
     )
     OR EXISTS (
       SELECT 1
       FROM billing_actual_invoice_lines AS actual_line
       JOIN billing_shared_estimated_charge_slices AS estimate_slice
         ON estimate_slice.charge_kind = actual_line.charge_kind
        AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
        AND estimate_slice.billing_period_start_at = actual_line.billing_period_start_at
        AND estimate_slice.billing_period_end_at = actual_line.billing_period_end_at
       JOIN billing_shared_cost_allocation_runs AS estimate_run
         ON estimate_run.id = estimate_slice.run_id
        AND estimate_run.provider = actual_line.provider
        AND estimate_run.currency_code = actual_line.currency_code
       WHERE actual_line.tenant_id = NEW.operator_tenant_id
         AND actual_line.invoice_import_id = NEW.invoice_import_id
         AND estimate_run.execution_target_id = NEW.execution_target_id
         AND NOT EXISTS (
           SELECT 1
           FROM billing_shared_actual_allocation_lines AS allocation_line
           WHERE allocation_line.run_id = NEW.id
             AND allocation_line.actual_invoice_line_id = actual_line.id
         )
     )
     OR EXISTS (
       SELECT 1
       FROM billing_shared_actual_allocation_lines AS allocation_line
       JOIN billing_actual_invoice_lines AS actual_line
         ON actual_line.id = allocation_line.actual_invoice_line_id
       JOIN billing_shared_estimated_charge_slices AS selected_slice
         ON selected_slice.charge_kind = actual_line.charge_kind
        AND selected_slice.resource_correlation_key = actual_line.resource_correlation_key
        AND selected_slice.billing_period_start_at = actual_line.billing_period_start_at
        AND selected_slice.billing_period_end_at = actual_line.billing_period_end_at
       JOIN billing_shared_cost_allocation_runs AS selected_run
         ON selected_run.id = selected_slice.run_id
        AND selected_run.provider = actual_line.provider
        AND selected_run.currency_code = actual_line.currency_code
       WHERE allocation_line.run_id = NEW.id
       GROUP BY actual_line.id
       HAVING count(DISTINCT selected_run.execution_target_id) > 1
     ) THEN
    RAISE EXCEPTION 'billing shared actual allocation conservation is invalid'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_actual_allocation_runs_seal
BEFORE UPDATE ON billing_shared_actual_allocation_runs
FOR EACH ROW EXECUTE FUNCTION seal_billing_shared_actual_allocation_run();

CREATE OR REPLACE FUNCTION enforce_billing_shared_actual_allocation_immutable()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'billing shared actual allocation history is immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_billing_shared_actual_allocation_runs_delete
BEFORE DELETE ON billing_shared_actual_allocation_runs
FOR EACH ROW EXECUTE FUNCTION enforce_billing_shared_actual_allocation_immutable();
CREATE TRIGGER trg_billing_shared_actual_allocation_lines_immutable
BEFORE UPDATE OR DELETE ON billing_shared_actual_allocation_lines
FOR EACH ROW EXECUTE FUNCTION enforce_billing_shared_actual_allocation_immutable();
CREATE TRIGGER trg_billing_shared_actual_charge_slices_immutable
BEFORE UPDATE OR DELETE ON billing_shared_actual_charge_slices
FOR EACH ROW EXECUTE FUNCTION enforce_billing_shared_actual_allocation_immutable();

COMMENT ON TABLE billing_shared_actual_allocation_runs IS
  'Operator-authorized immutable source-scope snapshot connecting one account invoice import to one shared Target.';
COMMENT ON TABLE billing_shared_actual_allocation_lines IS
  'One exact provider invoice line allocated once; source and allocated micros must conserve.';
COMMENT ON TABLE billing_shared_actual_charge_slices IS
  'Signed actual micros distributed over immutable shared estimated slices by deterministic cumulative proportion.';
