package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestInternalSelfHostedPaymentLockdownNormalizesAndFreezesHistoricalPostgresState(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	ctx := context.Background()
	db, err := Open(ctx, postgresisolation.URL(t, databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(ctx, db, migrationsThrough(t, "000153_stage6_internal_self_hosted_release_gate.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "payment-lockdown-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	legacyRoleUserID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.Create(&persistence.User{
		ID: legacyRoleUserID, Email: "legacy-cost-role-" + uuid.NewString() + "@example.com",
		DisplayName: "Legacy cost role", Status: "active", EmailVerifiedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: legacyRoleUserID, Role: "billing_admin",
		Status: "active", InvitedBy: &domain.UserID, JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider, customerID, subscriptionID, priceID := "stripe", "cus_historical", "sub_historical", "price_historical"
	if err := db.Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", domain.TenantID).Updates(map[string]any{
		"status": "past_due", "assignment_source": "billing_provider",
		"external_customer_id": customerID, "external_subscription_id": subscriptionID,
		"billing_provider": provider, "external_price_id": priceID, "provider_observed_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(time.Hour)
	checkout := persistence.CommercialBillingCheckoutSession{
		ID: uuid.New(), TenantID: domain.TenantID, RequestedByUserID: domain.UserID,
		PlanCode: "personal", ExpectedSubscriptionVersion: 1, Provider: provider,
		ProviderPriceID: priceID, Status: "creating", IdempotencyKeySHA256: []byte(strings.Repeat("a", 32)),
		ProviderRequestSHA256: []byte(strings.Repeat("b", 32)), ExpiresAt: &expiresAt,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&checkout).Error; err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var subscription persistence.TenantSubscription
	if err := db.Where("tenant_id = ?", domain.TenantID).Take(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	if subscription.Status != "suspended" || subscription.AssignmentSource != "migration" ||
		subscription.ExternalCustomerID != nil || subscription.ExternalSubscriptionID != nil ||
		subscription.BillingProvider != nil || subscription.ExternalPriceID != nil || subscription.ProviderObservedAt != nil {
		t.Fatalf("normalized internal self-hosted Subscription = %#v", subscription)
	}
	var membership persistence.TenantMembership
	if err := db.Where("tenant_id = ? AND user_id = ?", domain.TenantID, legacyRoleUserID).
		Take(&membership).Error; err != nil {
		t.Fatal(err)
	}
	if membership.Role != "cost_admin" {
		t.Fatalf("normalized internal self-hosted Tenant role = %q, want cost_admin", membership.Role)
	}
	if err := db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, legacyRoleUserID).
		Update("role", "billing_admin").Error; err == nil {
		t.Fatal("PostgreSQL accepted the retired payment-oriented Tenant role")
	}
	if err := db.Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", domain.TenantID).
		Update("status", "past_due").Error; err == nil {
		t.Fatal("PostgreSQL accepted payment-derived Subscription status after lockdown")
	}
	if err := db.Create(&persistence.CommercialBillingCheckoutSession{
		ID: uuid.New(), TenantID: domain.TenantID, RequestedByUserID: domain.UserID,
		PlanCode: "personal", ExpectedSubscriptionVersion: 1, Provider: provider,
		ProviderPriceID: priceID, Status: "creating", IdempotencyKeySHA256: []byte(strings.Repeat("c", 32)),
		ProviderRequestSHA256: []byte(strings.Repeat("d", 32)), ExpiresAt: &expiresAt,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL accepted a new historical Checkout row after lockdown")
	}
	if err := db.Model(&persistence.CommercialBillingCheckoutSession{}).Where("id = ?", checkout.ID).
		Update("updated_at", now.Add(time.Minute)).Error; err == nil {
		t.Fatal("PostgreSQL allowed historical Checkout mutation after lockdown")
	}
	if err := db.Delete(&persistence.CommercialBillingCheckoutSession{}, "id = ?", checkout.ID).Error; err == nil {
		t.Fatal("PostgreSQL allowed historical Checkout deletion after lockdown")
	}
	if err := db.Exec("INSERT INTO stage6_billing_exercises (id) VALUES (?)", uuid.New()).Error; err == nil {
		t.Fatal("PostgreSQL accepted a historical Billing exercise after lockdown")
	}
	if err := db.Exec("INSERT INTO stage6_billing_exercise_approvals (id) VALUES (?)", uuid.New()).Error; err == nil {
		t.Fatal("PostgreSQL accepted a historical Billing exercise approval after lockdown")
	}
}
