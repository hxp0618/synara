CREATE TABLE saas_plans (
  code TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('draft', 'active', 'retired')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  trial_days INTEGER NOT NULL DEFAULT 0 CHECK (trial_days BETWEEN 0 AND 365),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  retired_at TIMESTAMPTZ,
  CHECK (code ~ '^[a-z0-9][a-z0-9._-]{0,79}$'),
  CHECK (length(btrim(display_name)) BETWEEN 1 AND 160),
  CHECK ((status = 'retired' AND retired_at IS NOT NULL) OR (status <> 'retired' AND retired_at IS NULL))
);

CREATE TRIGGER trg_saas_plans_updated_at
BEFORE UPDATE ON saas_plans
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO saas_plans (code, display_name, status, version, trial_days)
SELECT DISTINCT plan_code, initcap(replace(plan_code, '-', ' ')), 'active', 1,
  CASE WHEN plan_code = 'free' THEN 14 ELSE 0 END
FROM tenants
ON CONFLICT (code) DO NOTHING;

INSERT INTO saas_plans (code, display_name, status, version, trial_days)
VALUES
  ('free', 'Free', 'active', 1, 14),
  ('enterprise', 'Enterprise', 'active', 1, 0),
  ('personal', 'Personal', 'active', 1, 0)
ON CONFLICT (code) DO NOTHING;

ALTER TABLE tenants
  ADD CONSTRAINT fk_tenants_plan_code FOREIGN KEY (plan_code) REFERENCES saas_plans(code) ON DELETE RESTRICT;

CREATE TABLE plan_entitlements (
  plan_code TEXT NOT NULL REFERENCES saas_plans(code) ON DELETE CASCADE,
  entitlement_key TEXT NOT NULL,
  value_kind TEXT NOT NULL CHECK (value_kind IN ('boolean', 'integer', 'string')),
  boolean_value BOOLEAN,
  integer_value BIGINT,
  string_value TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (plan_code, entitlement_key),
  CHECK (
    entitlement_key ~ '^[a-z][a-z0-9._-]*[a-z0-9]$'
    AND position('.' IN entitlement_key) > 1
    AND entitlement_key NOT LIKE '%..%'
  ),
  CHECK (
    (value_kind = 'boolean' AND boolean_value IS NOT NULL AND integer_value IS NULL AND string_value IS NULL)
    OR (value_kind = 'integer' AND boolean_value IS NULL AND integer_value IS NOT NULL AND string_value IS NULL)
    OR (value_kind = 'string' AND boolean_value IS NULL AND integer_value IS NULL AND string_value IS NOT NULL)
  )
);

CREATE TRIGGER trg_plan_entitlements_updated_at
BEFORE UPDATE ON plan_entitlements
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE tenant_subscriptions (
  tenant_id UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  plan_code TEXT NOT NULL REFERENCES saas_plans(code) ON DELETE RESTRICT,
  status TEXT NOT NULL CHECK (status IN ('trialing', 'active', 'past_due', 'suspended', 'cancelled')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  trial_ends_at TIMESTAMPTZ,
  current_period_start TIMESTAMPTZ NOT NULL,
  current_period_end TIMESTAMPTZ NOT NULL,
  assignment_source TEXT NOT NULL CHECK (assignment_source IN ('migration', 'self_service', 'platform_admin', 'billing_provider')),
  external_customer_id TEXT,
  external_subscription_id TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (current_period_end > current_period_start),
  CHECK ((status = 'trialing' AND trial_ends_at IS NOT NULL) OR status <> 'trialing'),
  CHECK ((external_customer_id IS NULL) = (external_subscription_id IS NULL)),
  CHECK (external_customer_id IS NULL OR length(external_customer_id) BETWEEN 1 AND 500),
  CHECK (external_subscription_id IS NULL OR length(external_subscription_id) BETWEEN 1 AND 500)
);

CREATE INDEX idx_tenant_subscriptions_status_period
  ON tenant_subscriptions (status, current_period_end, tenant_id);

CREATE TRIGGER trg_tenant_subscriptions_updated_at
BEFORE UPDATE ON tenant_subscriptions
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO tenant_subscriptions (
  tenant_id, plan_code, status, version, trial_ends_at,
  current_period_start, current_period_end, assignment_source
)
SELECT
  id, plan_code,
  CASE
    WHEN status = 'trialing' THEN 'trialing'
    WHEN status = 'active' THEN 'active'
    WHEN status = 'suspended' THEN 'suspended'
    ELSE 'cancelled'
  END,
  1,
  CASE WHEN status = 'trialing' THEN trial_expires_at ELSE NULL END,
  now(),
  now() + interval '1 month',
  'migration'
FROM tenants;

CREATE OR REPLACE FUNCTION enforce_tenant_subscription_plan_projection()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  projected_plan_code TEXT;
BEGIN
  SELECT plan_code INTO projected_plan_code FROM tenants WHERE id = NEW.tenant_id;
  IF projected_plan_code IS NULL OR projected_plan_code <> NEW.plan_code THEN
    RAISE EXCEPTION 'Tenant Subscription Plan does not match Tenant projection' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_tenant_subscriptions_plan_projection
AFTER INSERT OR UPDATE OF plan_code ON tenant_subscriptions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION enforce_tenant_subscription_plan_projection();

CREATE OR REPLACE FUNCTION enforce_tenant_plan_subscription_projection()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM tenant_subscriptions WHERE tenant_id = NEW.id)
     AND NOT EXISTS (
       SELECT 1 FROM tenant_subscriptions
       WHERE tenant_id = NEW.id AND plan_code = NEW.plan_code
     ) THEN
    RAISE EXCEPTION 'Tenant Plan projection does not match Subscription' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_tenants_subscription_plan_projection
AFTER UPDATE OF plan_code ON tenants
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION enforce_tenant_plan_subscription_projection();

CREATE TABLE feature_flags (
  key TEXT PRIMARY KEY,
  description TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
  default_enabled BOOLEAN NOT NULL DEFAULT false,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  retired_at TIMESTAMPTZ,
  CHECK (
    key ~ '^[a-z][a-z0-9._-]*[a-z0-9]$'
    AND position('.' IN key) > 1
    AND key NOT LIKE '%..%'
  ),
  CHECK (length(description) BETWEEN 1 AND 1000),
  CHECK ((status = 'retired' AND retired_at IS NOT NULL) OR (status = 'active' AND retired_at IS NULL))
);

CREATE TRIGGER trg_feature_flags_updated_at
BEFORE UPDATE ON feature_flags
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO feature_flags (key, description, status, default_enabled, version)
VALUES
  ('identity.sso', 'Enterprise OIDC and SAML identity connections.', 'active', true, 1),
  ('provider.byok', 'Tenant or user supplied Provider credentials.', 'active', true, 1),
  ('audit.export', 'Tenant audit log export.', 'active', true, 1)
ON CONFLICT (key) DO NOTHING;

CREATE TABLE tenant_feature_flag_overrides (
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  flag_key TEXT NOT NULL REFERENCES feature_flags(key) ON DELETE CASCADE,
  enabled BOOLEAN NOT NULL,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  reason TEXT NOT NULL,
  expires_at TIMESTAMPTZ,
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, flag_key),
  CHECK (length(btrim(reason)) BETWEEN 3 AND 500)
);

CREATE INDEX idx_tenant_feature_flag_overrides_expiry
  ON tenant_feature_flag_overrides (expires_at, tenant_id, flag_key)
  WHERE expires_at IS NOT NULL;

CREATE TRIGGER trg_tenant_feature_flag_overrides_updated_at
BEFORE UPDATE ON tenant_feature_flag_overrides
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
