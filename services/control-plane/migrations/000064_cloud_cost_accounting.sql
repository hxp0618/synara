CREATE TABLE billing_provider_tariffs (
  id UUID PRIMARY KEY,
  provider TEXT NOT NULL,
  region TEXT NOT NULL DEFAULT '',
  currency_code TEXT NOT NULL,
  version BIGINT NOT NULL,
  effective_start_at TIMESTAMPTZ NOT NULL,
  effective_end_at TIMESTAMPTZ,
  cpu_core_hour_rate_micros BIGINT NOT NULL DEFAULT 0,
  memory_gib_hour_rate_micros BIGINT NOT NULL DEFAULT 0,
  ephemeral_gib_hour_rate_micros BIGINT NOT NULL DEFAULT 0,
  request_rate_micros BIGINT NOT NULL DEFAULT 0,
  pod_hour_rate_micros BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_billing_provider_tariffs_version UNIQUE (provider, region, currency_code, version),
  CONSTRAINT chk_billing_provider_tariffs_provider CHECK (
    provider ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
  ),
  CONSTRAINT chk_billing_provider_tariffs_region CHECK (length(region) <= 160),
  CONSTRAINT chk_billing_provider_tariffs_currency CHECK (
    currency_code ~ '^[A-Z]{3}$'
  ),
  CONSTRAINT chk_billing_provider_tariffs_version CHECK (version > 0),
  CONSTRAINT chk_billing_provider_tariffs_effective_interval CHECK (
    effective_end_at IS NULL OR effective_end_at > effective_start_at
  ),
  CONSTRAINT chk_billing_provider_tariffs_rates CHECK (
    cpu_core_hour_rate_micros >= 0
    AND memory_gib_hour_rate_micros >= 0
    AND ephemeral_gib_hour_rate_micros >= 0
    AND request_rate_micros >= 0
    AND pod_hour_rate_micros >= 0
  )
);

CREATE INDEX idx_billing_provider_tariffs_lookup
  ON billing_provider_tariffs (provider, region, currency_code, effective_start_at);

CREATE OR REPLACE FUNCTION assert_billing_provider_tariff_non_overlapping()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM billing_provider_tariffs AS existing
    WHERE existing.id <> NEW.id
      AND existing.provider = NEW.provider
      AND existing.region = NEW.region
      AND existing.currency_code = NEW.currency_code
      AND existing.effective_start_at < COALESCE(NEW.effective_end_at, 'infinity'::timestamptz)
      AND COALESCE(existing.effective_end_at, 'infinity'::timestamptz) > NEW.effective_start_at
  ) THEN
    RAISE EXCEPTION 'billing provider tariff effective interval overlaps another tariff'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_billing_provider_tariffs_non_overlapping ON billing_provider_tariffs;
CREATE CONSTRAINT TRIGGER trg_billing_provider_tariffs_non_overlapping
AFTER INSERT OR UPDATE OF provider, region, currency_code, effective_start_at, effective_end_at
ON billing_provider_tariffs
DEFERRABLE INITIALLY IMMEDIATE
FOR EACH ROW EXECUTE FUNCTION assert_billing_provider_tariff_non_overlapping();

CREATE TABLE billing_estimated_usage_charges (
  id UUID PRIMARY KEY,
  tenant_id UUID REFERENCES tenants(id) ON DELETE CASCADE,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE CASCADE,
  target_kind TEXT NOT NULL,
  worker_id UUID NOT NULL,
  worker_incarnation BIGINT NOT NULL,
  tariff_id UUID NOT NULL REFERENCES billing_provider_tariffs(id) ON DELETE RESTRICT,
  provider TEXT NOT NULL,
  region TEXT NOT NULL DEFAULT '',
  currency_code TEXT NOT NULL,
  charge_kind TEXT NOT NULL,
  resource_correlation_key TEXT NOT NULL,
  billing_period_start_at TIMESTAMPTZ NOT NULL,
  billing_period_end_at TIMESTAMPTZ NOT NULL,
  usage_start_at TIMESTAMPTZ NOT NULL,
  usage_end_at TIMESTAMPTZ NOT NULL,
  billable_seconds BIGINT NOT NULL,
  claim_count BIGINT NOT NULL DEFAULT 0,
  requested_cpu_millicores BIGINT,
  requested_memory_bytes BIGINT,
  requested_ephemeral_storage_bytes BIGINT,
  rate_micros BIGINT NOT NULL,
  amount_micros BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT fk_billing_estimated_usage_charges_execution_target_scope
    FOREIGN KEY (tenant_id, execution_target_id)
    REFERENCES execution_targets(tenant_id, id)
    ON DELETE CASCADE,
  CONSTRAINT fk_billing_estimated_usage_charges_worker
    FOREIGN KEY (worker_id, worker_incarnation)
    REFERENCES worker_incarnation_facts(worker_id, worker_incarnation)
    ON DELETE CASCADE,
  CONSTRAINT uq_billing_estimated_usage_charges_identity UNIQUE (
    tenant_id, worker_id, worker_incarnation, tariff_id, charge_kind,
    billing_period_start_at, billing_period_end_at, usage_start_at, usage_end_at
  ),
  CONSTRAINT chk_billing_estimated_usage_charges_target_kind CHECK (
    target_kind IN ('local', 'ssh', 'docker', 'kubernetes')
  ),
  CONSTRAINT chk_billing_estimated_usage_charges_provider CHECK (
    provider ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
  ),
  CONSTRAINT chk_billing_estimated_usage_charges_region CHECK (length(region) <= 160),
  CONSTRAINT chk_billing_estimated_usage_charges_currency CHECK (
    currency_code ~ '^[A-Z]{3}$'
  ),
  CONSTRAINT chk_billing_estimated_usage_charges_charge_kind CHECK (
    charge_kind IN ('cpu', 'memory', 'ephemeral-storage', 'request', 'pod')
  ),
  CONSTRAINT chk_billing_estimated_usage_charges_resource_key CHECK (
    length(btrim(resource_correlation_key)) BETWEEN 1 AND 512
  ),
  CONSTRAINT chk_billing_estimated_usage_charges_period CHECK (
    billing_period_end_at > billing_period_start_at
    AND usage_end_at > usage_start_at
    AND usage_start_at >= billing_period_start_at
    AND usage_end_at <= billing_period_end_at
  ),
  CONSTRAINT chk_billing_estimated_usage_charges_counters CHECK (
    billable_seconds >= 0
    AND claim_count >= 0
    AND rate_micros >= 0
    AND amount_micros >= 0
    AND (requested_cpu_millicores IS NULL OR requested_cpu_millicores > 0)
    AND (requested_memory_bytes IS NULL OR requested_memory_bytes > 0)
    AND (requested_ephemeral_storage_bytes IS NULL OR requested_ephemeral_storage_bytes > 0)
  )
);

CREATE INDEX idx_billing_estimated_usage_charges_worker
  ON billing_estimated_usage_charges (worker_id, worker_incarnation);

CREATE INDEX idx_billing_estimated_usage_charges_reconcile
  ON billing_estimated_usage_charges (
    tenant_id, provider, currency_code, resource_correlation_key, charge_kind,
    billing_period_start_at, billing_period_end_at
  );

CREATE OR REPLACE FUNCTION enforce_billing_estimated_usage_charge_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'billing estimated usage charges are immutable'
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_billing_estimated_usage_charges_immutable ON billing_estimated_usage_charges;
CREATE TRIGGER trg_billing_estimated_usage_charges_immutable
BEFORE UPDATE OR DELETE ON billing_estimated_usage_charges
FOR EACH ROW EXECUTE FUNCTION enforce_billing_estimated_usage_charge_immutable();

CREATE TABLE billing_actual_invoice_imports (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  external_import_id TEXT NOT NULL,
  billing_period_start_at TIMESTAMPTZ NOT NULL,
  billing_period_end_at TIMESTAMPTZ NOT NULL,
  currency_code TEXT NOT NULL,
  source_checksum TEXT NOT NULL,
  imported_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_billing_actual_invoice_imports_tenant_id UNIQUE (tenant_id, id),
  CONSTRAINT uq_billing_actual_invoice_imports_external UNIQUE (tenant_id, provider, external_import_id),
  CONSTRAINT chk_billing_actual_invoice_imports_provider CHECK (
    provider ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
  ),
  CONSTRAINT chk_billing_actual_invoice_imports_external_id CHECK (
    length(btrim(external_import_id)) BETWEEN 1 AND 200
  ),
  CONSTRAINT chk_billing_actual_invoice_imports_period CHECK (
    billing_period_end_at > billing_period_start_at
  ),
  CONSTRAINT chk_billing_actual_invoice_imports_currency CHECK (
    currency_code ~ '^[A-Z]{3}$'
  ),
  CONSTRAINT chk_billing_actual_invoice_imports_checksum CHECK (
    source_checksum ~ '^[0-9a-f]{64}$'
  )
);

CREATE INDEX idx_billing_actual_invoice_imports_period
  ON billing_actual_invoice_imports (tenant_id, provider, billing_period_start_at, billing_period_end_at);

CREATE OR REPLACE FUNCTION enforce_billing_actual_invoice_import_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'billing invoice imports are immutable' USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_billing_actual_invoice_imports_immutable ON billing_actual_invoice_imports;
CREATE TRIGGER trg_billing_actual_invoice_imports_immutable
BEFORE UPDATE OR DELETE ON billing_actual_invoice_imports
FOR EACH ROW EXECUTE FUNCTION enforce_billing_actual_invoice_import_immutable();

CREATE TABLE billing_actual_invoice_lines (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  invoice_import_id UUID NOT NULL,
  external_line_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  currency_code TEXT NOT NULL,
  charge_kind TEXT NOT NULL,
  resource_correlation_key TEXT NOT NULL,
  billing_period_start_at TIMESTAMPTZ NOT NULL,
  billing_period_end_at TIMESTAMPTZ NOT NULL,
  amount_micros BIGINT NOT NULL,
  reconciliation_state TEXT NOT NULL DEFAULT 'pending',
  matched_estimate_count INTEGER NOT NULL DEFAULT 0,
  matched_estimate_amount_micros BIGINT NOT NULL DEFAULT 0,
  reconciled_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_billing_actual_invoice_lines_tenant_id UNIQUE (tenant_id, id),
  CONSTRAINT fk_billing_actual_invoice_lines_import
    FOREIGN KEY (tenant_id, invoice_import_id)
    REFERENCES billing_actual_invoice_imports(tenant_id, id)
    ON DELETE CASCADE,
  CONSTRAINT uq_billing_actual_invoice_lines_external UNIQUE (tenant_id, invoice_import_id, external_line_id),
  CONSTRAINT uq_billing_actual_invoice_lines_resource UNIQUE (
    tenant_id, invoice_import_id, resource_correlation_key, charge_kind, billing_period_start_at, billing_period_end_at, currency_code
  ),
  CONSTRAINT chk_billing_actual_invoice_lines_external_id CHECK (
    length(btrim(external_line_id)) BETWEEN 1 AND 200
  ),
  CONSTRAINT chk_billing_actual_invoice_lines_provider CHECK (
    provider ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
  ),
  CONSTRAINT chk_billing_actual_invoice_lines_currency CHECK (
    currency_code ~ '^[A-Z]{3}$'
  ),
  CONSTRAINT chk_billing_actual_invoice_lines_charge_kind CHECK (
    charge_kind IN ('cpu', 'memory', 'ephemeral-storage', 'request', 'pod')
  ),
  CONSTRAINT chk_billing_actual_invoice_lines_resource_key CHECK (
    length(btrim(resource_correlation_key)) BETWEEN 1 AND 512
  ),
  CONSTRAINT chk_billing_actual_invoice_lines_period CHECK (
    billing_period_end_at > billing_period_start_at
  ),
  CONSTRAINT chk_billing_actual_invoice_lines_reconciliation CHECK (
    matched_estimate_count >= 0
    AND (
      (reconciliation_state = 'pending' AND reconciled_at IS NULL AND matched_estimate_count = 0 AND matched_estimate_amount_micros = 0)
      OR
      (reconciliation_state IN ('matched', 'variance', 'unmatched-actual') AND reconciled_at IS NOT NULL)
    )
  )
);

CREATE INDEX idx_billing_actual_invoice_lines_reconcile
  ON billing_actual_invoice_lines (
    tenant_id, provider, currency_code, resource_correlation_key, charge_kind,
    billing_period_start_at, billing_period_end_at, reconciliation_state
  );

CREATE OR REPLACE FUNCTION validate_billing_actual_invoice_line()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent_import billing_actual_invoice_imports%ROWTYPE;
BEGIN
  SELECT * INTO parent_import
  FROM billing_actual_invoice_imports
  WHERE tenant_id = NEW.tenant_id
    AND id = NEW.invoice_import_id;

  IF NOT FOUND THEN
    RAISE EXCEPTION 'billing invoice line import is missing' USING ERRCODE = '23514';
  END IF;
  IF parent_import.provider <> NEW.provider
     OR parent_import.currency_code <> NEW.currency_code
     OR parent_import.billing_period_start_at <> NEW.billing_period_start_at
     OR parent_import.billing_period_end_at <> NEW.billing_period_end_at THEN
    RAISE EXCEPTION 'billing invoice line does not match its import scope'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_billing_actual_invoice_lines_validate ON billing_actual_invoice_lines;
CREATE CONSTRAINT TRIGGER trg_billing_actual_invoice_lines_validate
AFTER INSERT OR UPDATE OF
  tenant_id, invoice_import_id, provider, currency_code, billing_period_start_at, billing_period_end_at
ON billing_actual_invoice_lines
DEFERRABLE INITIALLY IMMEDIATE
FOR EACH ROW EXECUTE FUNCTION validate_billing_actual_invoice_line();

CREATE OR REPLACE FUNCTION enforce_billing_actual_invoice_line_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'billing invoice lines cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.id <> OLD.id
     OR NEW.tenant_id <> OLD.tenant_id
     OR NEW.invoice_import_id <> OLD.invoice_import_id
     OR NEW.external_line_id <> OLD.external_line_id
     OR NEW.provider <> OLD.provider
     OR NEW.currency_code <> OLD.currency_code
     OR NEW.charge_kind <> OLD.charge_kind
     OR NEW.resource_correlation_key <> OLD.resource_correlation_key
     OR NEW.billing_period_start_at <> OLD.billing_period_start_at
     OR NEW.billing_period_end_at <> OLD.billing_period_end_at
     OR NEW.amount_micros <> OLD.amount_micros
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'billing invoice line identity is immutable' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_billing_actual_invoice_lines_update ON billing_actual_invoice_lines;
CREATE TRIGGER trg_billing_actual_invoice_lines_update
BEFORE UPDATE OR DELETE ON billing_actual_invoice_lines
FOR EACH ROW EXECUTE FUNCTION enforce_billing_actual_invoice_line_update();
