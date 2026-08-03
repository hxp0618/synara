UPDATE tenant_subscriptions
SET status = CASE WHEN status = 'past_due' THEN 'suspended' ELSE status END,
    assignment_source = CASE WHEN assignment_source = 'billing_provider' THEN 'migration' ELSE assignment_source END,
    external_customer_id = NULL,
    external_subscription_id = NULL,
    billing_provider = NULL,
    external_price_id = NULL,
    provider_observed_at = NULL,
    updated_at = now()
WHERE status = 'past_due'
   OR assignment_source = 'billing_provider'
   OR external_customer_id IS NOT NULL
   OR external_subscription_id IS NOT NULL
   OR billing_provider IS NOT NULL
   OR external_price_id IS NOT NULL
   OR provider_observed_at IS NOT NULL;

ALTER TABLE tenant_subscriptions
  DROP CONSTRAINT IF EXISTS tenant_subscriptions_status_check,
  DROP CONSTRAINT IF EXISTS tenant_subscriptions_assignment_source_check,
  DROP CONSTRAINT IF EXISTS ck_tenant_subscriptions_billing_provider,
  DROP CONSTRAINT IF EXISTS ck_tenant_subscriptions_external_price,
  DROP CONSTRAINT IF EXISTS ck_tenant_subscriptions_provider_shape;

ALTER TABLE tenant_subscriptions
  ADD CONSTRAINT ck_tenant_subscriptions_internal_self_hosted_status CHECK (
    status IN ('trialing', 'active', 'suspended', 'cancelled')
  ),
  ADD CONSTRAINT ck_tenant_subscriptions_internal_self_hosted_source CHECK (
    assignment_source IN ('migration', 'self_service', 'platform_admin')
  ),
  ADD CONSTRAINT ck_tenant_subscriptions_no_payment_state CHECK (
    external_customer_id IS NULL
    AND external_subscription_id IS NULL
    AND billing_provider IS NULL
    AND external_price_id IS NULL
    AND provider_observed_at IS NULL
  );

DROP INDEX IF EXISTS uq_tenant_subscriptions_provider_customer;
DROP INDEX IF EXISTS uq_tenant_subscriptions_provider_subscription;

CREATE OR REPLACE FUNCTION reject_internal_self_hosted_payment_state()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    RAISE EXCEPTION 'payment state is unsupported by the internal-self-hosted product' USING ERRCODE = '23514';
  END IF;
  RAISE EXCEPTION 'historical payment state is read-only' USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_commercial_billing_checkout_transition ON commercial_billing_checkout_sessions;
DROP TRIGGER IF EXISTS trg_commercial_billing_checkout_updated_at ON commercial_billing_checkout_sessions;
DROP TRIGGER IF EXISTS trg_commercial_billing_provider_events_no_update ON commercial_billing_provider_events;
DROP TRIGGER IF EXISTS trg_commercial_billing_provider_events_no_delete ON commercial_billing_provider_events;

DROP TRIGGER IF EXISTS trg_internal_self_hosted_checkout_no_insert ON commercial_billing_checkout_sessions;
DROP TRIGGER IF EXISTS trg_internal_self_hosted_checkout_no_update ON commercial_billing_checkout_sessions;
DROP TRIGGER IF EXISTS trg_internal_self_hosted_checkout_no_delete ON commercial_billing_checkout_sessions;
CREATE TRIGGER trg_internal_self_hosted_checkout_no_insert
BEFORE INSERT ON commercial_billing_checkout_sessions
FOR EACH ROW EXECUTE FUNCTION reject_internal_self_hosted_payment_state();
CREATE TRIGGER trg_internal_self_hosted_checkout_no_update
BEFORE UPDATE ON commercial_billing_checkout_sessions
FOR EACH ROW EXECUTE FUNCTION reject_internal_self_hosted_payment_state();
CREATE TRIGGER trg_internal_self_hosted_checkout_no_delete
BEFORE DELETE ON commercial_billing_checkout_sessions
FOR EACH ROW EXECUTE FUNCTION reject_internal_self_hosted_payment_state();

DROP TRIGGER IF EXISTS trg_internal_self_hosted_provider_event_no_insert ON commercial_billing_provider_events;
DROP TRIGGER IF EXISTS trg_internal_self_hosted_provider_event_no_update ON commercial_billing_provider_events;
DROP TRIGGER IF EXISTS trg_internal_self_hosted_provider_event_no_delete ON commercial_billing_provider_events;
CREATE TRIGGER trg_internal_self_hosted_provider_event_no_insert
BEFORE INSERT ON commercial_billing_provider_events
FOR EACH ROW EXECUTE FUNCTION reject_internal_self_hosted_payment_state();
CREATE TRIGGER trg_internal_self_hosted_provider_event_no_update
BEFORE UPDATE ON commercial_billing_provider_events
FOR EACH ROW EXECUTE FUNCTION reject_internal_self_hosted_payment_state();
CREATE TRIGGER trg_internal_self_hosted_provider_event_no_delete
BEFORE DELETE ON commercial_billing_provider_events
FOR EACH ROW EXECUTE FUNCTION reject_internal_self_hosted_payment_state();

COMMENT ON TABLE commercial_billing_checkout_sessions IS
  'Historical Migration 000109 compatibility only; current internal-self-hosted runtime cannot create, mutate or delete payment state.';
COMMENT ON TABLE commercial_billing_provider_events IS
  'Historical Migration 000109 compatibility only; current internal-self-hosted runtime cannot create, mutate or delete payment state.';
