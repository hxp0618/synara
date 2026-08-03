ALTER TABLE tenant_subscriptions
  ADD COLUMN billing_provider TEXT,
  ADD COLUMN external_price_id TEXT,
  ADD COLUMN provider_observed_at TIMESTAMPTZ,
  ADD CONSTRAINT ck_tenant_subscriptions_billing_provider CHECK (
    billing_provider IS NULL OR billing_provider = 'stripe'
  ),
  ADD CONSTRAINT ck_tenant_subscriptions_external_price CHECK (
    external_price_id IS NULL OR length(external_price_id) BETWEEN 1 AND 500
  ),
  ADD CONSTRAINT ck_tenant_subscriptions_provider_shape CHECK (
    (
      assignment_source = 'billing_provider'
      AND billing_provider IS NOT NULL
      AND external_customer_id IS NOT NULL
      AND external_subscription_id IS NOT NULL
      AND external_price_id IS NOT NULL
      AND provider_observed_at IS NOT NULL
    )
    OR
    (
      assignment_source <> 'billing_provider'
      AND billing_provider IS NULL
      AND external_price_id IS NULL
      AND provider_observed_at IS NULL
    )
  );

COMMENT ON COLUMN tenant_subscriptions.status IS
  'effective-subscription-v1: trialing/active grant Entitlements, past_due is explicit grace, suspended/cancelled grant none';

CREATE UNIQUE INDEX uq_tenant_subscriptions_provider_customer
  ON tenant_subscriptions (billing_provider, external_customer_id)
  WHERE billing_provider IS NOT NULL;

CREATE UNIQUE INDEX uq_tenant_subscriptions_provider_subscription
  ON tenant_subscriptions (billing_provider, external_subscription_id)
  WHERE billing_provider IS NOT NULL;

CREATE TABLE commercial_billing_checkout_sessions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  requested_by_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  plan_code TEXT NOT NULL REFERENCES saas_plans(code) ON DELETE RESTRICT,
  expected_subscription_version BIGINT NOT NULL CHECK (expected_subscription_version > 0),
  provider TEXT NOT NULL CHECK (provider = 'stripe'),
  provider_price_id TEXT NOT NULL CHECK (length(provider_price_id) BETWEEN 1 AND 500),
  provider_session_id TEXT,
  provider_subscription_id TEXT,
  status TEXT NOT NULL CHECK (status IN ('creating', 'open', 'completed', 'expired', 'abandoned')),
  idempotency_key_sha256 BYTEA NOT NULL UNIQUE,
  provider_request_sha256 BYTEA NOT NULL,
  expires_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (octet_length(idempotency_key_sha256) = 32),
  CHECK (octet_length(provider_request_sha256) = 32),
  CHECK (provider_session_id IS NULL OR length(provider_session_id) BETWEEN 1 AND 500),
  CHECK (provider_subscription_id IS NULL OR length(provider_subscription_id) BETWEEN 1 AND 500),
  CHECK (
    (status = 'creating' AND provider_session_id IS NULL AND provider_subscription_id IS NULL AND expires_at IS NOT NULL AND completed_at IS NULL)
    OR (status = 'open' AND provider_session_id IS NOT NULL AND provider_subscription_id IS NULL AND expires_at IS NOT NULL AND completed_at IS NULL)
    OR (status = 'completed' AND provider_session_id IS NOT NULL AND provider_subscription_id IS NOT NULL AND expires_at IS NOT NULL AND completed_at IS NOT NULL)
    OR (status = 'expired' AND provider_session_id IS NOT NULL AND provider_subscription_id IS NULL AND expires_at IS NOT NULL AND completed_at IS NULL)
    OR (status = 'abandoned' AND provider_session_id IS NULL AND provider_subscription_id IS NULL AND expires_at IS NOT NULL AND completed_at IS NULL)
  )
);

CREATE UNIQUE INDEX uq_commercial_billing_checkout_provider_session
  ON commercial_billing_checkout_sessions (provider, provider_session_id)
  WHERE provider_session_id IS NOT NULL;

CREATE UNIQUE INDEX uq_commercial_billing_checkout_provider_subscription
  ON commercial_billing_checkout_sessions (provider, provider_subscription_id)
  WHERE provider_subscription_id IS NOT NULL;

CREATE UNIQUE INDEX uq_commercial_billing_checkout_active_intent
  ON commercial_billing_checkout_sessions (tenant_id, expected_subscription_version)
  WHERE status IN ('creating', 'open');

CREATE INDEX idx_commercial_billing_checkout_tenant
  ON commercial_billing_checkout_sessions (tenant_id, created_at DESC, id);

CREATE TRIGGER trg_commercial_billing_checkout_updated_at
BEFORE UPDATE ON commercial_billing_checkout_sessions
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE OR REPLACE FUNCTION enforce_commercial_billing_checkout_transition()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
     OR NEW.requested_by_user_id IS DISTINCT FROM OLD.requested_by_user_id
     OR NEW.plan_code IS DISTINCT FROM OLD.plan_code
     OR NEW.expected_subscription_version IS DISTINCT FROM OLD.expected_subscription_version
     OR NEW.provider IS DISTINCT FROM OLD.provider
     OR NEW.provider_price_id IS DISTINCT FROM OLD.provider_price_id
     OR NEW.idempotency_key_sha256 IS DISTINCT FROM OLD.idempotency_key_sha256
     OR NEW.provider_request_sha256 IS DISTINCT FROM OLD.provider_request_sha256
     OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'Commercial billing Checkout identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF (OLD.status = 'creating' AND NEW.status NOT IN ('creating', 'open', 'completed', 'abandoned'))
     OR (OLD.status = 'open' AND NEW.status NOT IN ('open', 'completed', 'expired'))
     OR (OLD.status = 'abandoned' AND NEW.status NOT IN ('abandoned', 'completed'))
     OR (OLD.status = 'expired' AND NEW.status NOT IN ('expired', 'completed'))
     OR (OLD.status = 'completed' AND NEW.status <> OLD.status) THEN
    RAISE EXCEPTION 'Invalid commercial billing Checkout transition' USING ERRCODE = '23514';
  END IF;
  IF OLD.provider_session_id IS NOT NULL
     AND NEW.provider_session_id IS DISTINCT FROM OLD.provider_session_id THEN
    RAISE EXCEPTION 'Commercial billing provider Session identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.provider_subscription_id IS NOT NULL
     AND NEW.provider_subscription_id IS DISTINCT FROM OLD.provider_subscription_id THEN
    RAISE EXCEPTION 'Commercial billing provider Subscription identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at THEN
    RAISE EXCEPTION 'Commercial billing Checkout completion is immutable' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_commercial_billing_checkout_transition
BEFORE UPDATE ON commercial_billing_checkout_sessions
FOR EACH ROW EXECUTE FUNCTION enforce_commercial_billing_checkout_transition();

CREATE TABLE commercial_billing_provider_events (
  provider TEXT NOT NULL CHECK (provider = 'stripe'),
  event_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  provider_resource_id TEXT NOT NULL,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  payload_sha256 BYTEA NOT NULL,
  live_mode BOOLEAN NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('applied', 'no_change')),
  event_created_at TIMESTAMPTZ NOT NULL,
  processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, event_id),
  CHECK (length(event_id) BETWEEN 1 AND 500),
  CHECK (length(event_type) BETWEEN 1 AND 200),
  CHECK (length(provider_resource_id) BETWEEN 1 AND 500),
  CHECK (octet_length(payload_sha256) = 32)
);

CREATE INDEX idx_commercial_billing_provider_events_tenant
  ON commercial_billing_provider_events (tenant_id, event_created_at DESC, event_id);

CREATE OR REPLACE FUNCTION reject_commercial_billing_provider_event_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Commercial billing provider events are immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_commercial_billing_provider_events_no_update
BEFORE UPDATE ON commercial_billing_provider_events
FOR EACH ROW EXECUTE FUNCTION reject_commercial_billing_provider_event_mutation();

CREATE TRIGGER trg_commercial_billing_provider_events_no_delete
BEFORE DELETE ON commercial_billing_provider_events
FOR EACH ROW EXECUTE FUNCTION reject_commercial_billing_provider_event_mutation();

COMMENT ON TABLE commercial_billing_provider_events IS
  'Signed external subscription events after successful idempotent projection. Raw payloads are not retained.';
