INSERT INTO plan_entitlements (
  plan_code, entitlement_key, value_kind, integer_value
)
VALUES
  ('free', 'usage.execution_seconds_per_period', 'integer', 72000),
  ('free', 'usage.soft_warning_percent', 'integer', 80)
ON CONFLICT (plan_code, entitlement_key) DO NOTHING;

CREATE TABLE tenant_usage_quota_alerts (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  subscription_version BIGINT NOT NULL CHECK (subscription_version > 0),
  metric_key TEXT NOT NULL CHECK (metric_key = 'execution_seconds'),
  period_start TIMESTAMPTZ NOT NULL,
  period_end TIMESTAMPTZ NOT NULL,
  threshold_percent INTEGER NOT NULL CHECK (threshold_percent BETWEEN 1 AND 100),
  severity TEXT NOT NULL CHECK (severity IN ('warning', 'limit_reached')),
  limit_units BIGINT NOT NULL CHECK (limit_units > 0),
  observed_units BIGINT NOT NULL CHECK (observed_units >= 0),
  first_observed_at TIMESTAMPTZ NOT NULL,
  last_observed_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, subscription_version, metric_key, period_start, threshold_percent),
  CHECK (period_end > period_start),
  CHECK (last_observed_at >= first_observed_at),
  CHECK (
    (threshold_percent = 100 AND severity = 'limit_reached')
    OR (threshold_percent < 100 AND severity = 'warning')
  )
);

CREATE INDEX idx_tenant_usage_quota_alerts_period
  ON tenant_usage_quota_alerts (tenant_id, period_start, period_end, threshold_percent);

CREATE TRIGGER trg_tenant_usage_quota_alerts_updated_at
BEFORE UPDATE ON tenant_usage_quota_alerts
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE tenant_usage_quota_alerts IS
  'Idempotent soft-limit threshold crossings. These alerts never participate in Execution admission.';
