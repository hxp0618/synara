package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// migrateCommercialBillingSQLiteSafety retains the historical Migration 000109
// tables as read-only compatibility records. The internal-self-hosted product
// has no payment runtime, so every new Checkout/provider-event write fails.
func migrateCommercialBillingSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_tenant_subscriptions_commercial_billing_insert`,
		`DROP TRIGGER IF EXISTS trg_tenant_subscriptions_commercial_billing_update`,
		`DROP TRIGGER IF EXISTS trg_commercial_billing_checkout_insert`,
		`DROP TRIGGER IF EXISTS trg_commercial_billing_checkout_update`,
		`DROP TRIGGER IF EXISTS trg_commercial_billing_provider_events_insert`,
		`DROP TRIGGER IF EXISTS trg_commercial_billing_provider_events_no_update`,
		`DROP TRIGGER IF EXISTS trg_commercial_billing_provider_events_no_delete`,
		`DROP TRIGGER IF EXISTS trg_internal_self_hosted_checkout_no_insert`,
		`DROP TRIGGER IF EXISTS trg_internal_self_hosted_checkout_no_update`,
		`DROP TRIGGER IF EXISTS trg_internal_self_hosted_checkout_no_delete`,
		`DROP TRIGGER IF EXISTS trg_internal_self_hosted_provider_event_no_insert`,
		`DROP TRIGGER IF EXISTS trg_internal_self_hosted_provider_event_no_update`,
		`DROP TRIGGER IF EXISTS trg_internal_self_hosted_provider_event_no_delete`,
		`UPDATE tenant_subscriptions SET
			status = CASE WHEN status = 'past_due' THEN 'suspended' ELSE status END,
			assignment_source = CASE WHEN assignment_source = 'billing_provider' THEN 'migration' ELSE assignment_source END,
			external_customer_id = NULL, external_subscription_id = NULL,
			billing_provider = NULL, external_price_id = NULL, provider_observed_at = NULL
		 WHERE status = 'past_due' OR assignment_source = 'billing_provider'
			OR external_customer_id IS NOT NULL OR external_subscription_id IS NOT NULL
			OR billing_provider IS NOT NULL OR external_price_id IS NOT NULL OR provider_observed_at IS NOT NULL`,
		`DROP TRIGGER IF EXISTS trg_internal_self_hosted_subscription_insert`,
		`CREATE TRIGGER trg_internal_self_hosted_subscription_insert BEFORE INSERT ON tenant_subscriptions BEGIN
		  SELECT RAISE(ABORT, 'internal self-hosted Subscription cannot contain payment state') WHERE
		    NEW.status NOT IN ('trialing', 'active', 'suspended', 'cancelled')
		    OR NEW.assignment_source NOT IN ('migration', 'self_service', 'platform_admin')
		    OR NEW.external_customer_id IS NOT NULL OR NEW.external_subscription_id IS NOT NULL
		    OR NEW.billing_provider IS NOT NULL OR NEW.external_price_id IS NOT NULL OR NEW.provider_observed_at IS NOT NULL;
		END`,
		`DROP TRIGGER IF EXISTS trg_internal_self_hosted_subscription_update`,
		`CREATE TRIGGER trg_internal_self_hosted_subscription_update BEFORE UPDATE ON tenant_subscriptions BEGIN
		  SELECT RAISE(ABORT, 'internal self-hosted Subscription cannot contain payment state') WHERE
		    NEW.status NOT IN ('trialing', 'active', 'suspended', 'cancelled')
		    OR NEW.assignment_source NOT IN ('migration', 'self_service', 'platform_admin')
		    OR NEW.external_customer_id IS NOT NULL OR NEW.external_subscription_id IS NOT NULL
		    OR NEW.billing_provider IS NOT NULL OR NEW.external_price_id IS NOT NULL OR NEW.provider_observed_at IS NOT NULL;
		END`,
		`CREATE TRIGGER trg_internal_self_hosted_checkout_no_insert BEFORE INSERT ON commercial_billing_checkout_sessions BEGIN SELECT RAISE(ABORT, 'payment state is unsupported by the internal-self-hosted product'); END`,
		`CREATE TRIGGER trg_internal_self_hosted_checkout_no_update BEFORE UPDATE ON commercial_billing_checkout_sessions BEGIN SELECT RAISE(ABORT, 'historical payment state is read-only'); END`,
		`CREATE TRIGGER trg_internal_self_hosted_checkout_no_delete BEFORE DELETE ON commercial_billing_checkout_sessions BEGIN SELECT RAISE(ABORT, 'historical payment state is read-only'); END`,
		`CREATE TRIGGER trg_internal_self_hosted_provider_event_no_insert BEFORE INSERT ON commercial_billing_provider_events BEGIN SELECT RAISE(ABORT, 'payment state is unsupported by the internal-self-hosted product'); END`,
		`CREATE TRIGGER trg_internal_self_hosted_provider_event_no_update BEFORE UPDATE ON commercial_billing_provider_events BEGIN SELECT RAISE(ABORT, 'historical payment state is read-only'); END`,
		`CREATE TRIGGER trg_internal_self_hosted_provider_event_no_delete BEFORE DELETE ON commercial_billing_provider_events BEGIN SELECT RAISE(ABORT, 'historical payment state is read-only'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite internal self-hosted payment-state lockdown: %w", err)
		}
	}
	return nil
}
