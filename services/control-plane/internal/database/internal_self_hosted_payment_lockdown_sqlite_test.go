package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestInternalSelfHostedSQLiteRejectsPaymentState(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "payment-lockdown-sqlite-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, domain.UserID).
		Update("role", "cost_admin").Error; err != nil {
		t.Fatalf("SQLite rejected the internal cost administrator role: %v", err)
	}
	if err := store.DB().Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, domain.UserID).
		Update("role", "billing_admin").Error; err == nil {
		t.Fatal("SQLite accepted the retired payment-oriented Tenant role")
	}
	if err := store.DB().Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", domain.TenantID).
		Update("status", "past_due").Error; err == nil {
		t.Fatal("SQLite accepted payment-derived Subscription status")
	}
	now := time.Now().UTC().Truncate(time.Second)
	expiresAt := now.Add(time.Hour)
	if err := store.DB().Create(&persistence.CommercialBillingCheckoutSession{
		ID: uuid.New(), TenantID: domain.TenantID, RequestedByUserID: domain.UserID,
		PlanCode: "personal", ExpectedSubscriptionVersion: 1, Provider: "stripe",
		ProviderPriceID: "price_historical", Status: "creating",
		IdempotencyKeySHA256: []byte(strings.Repeat("a", 32)), ProviderRequestSHA256: []byte(strings.Repeat("b", 32)),
		ExpiresAt: &expiresAt, CreatedAt: now, UpdatedAt: now,
	}).Error; err == nil {
		t.Fatal("SQLite accepted historical Checkout creation")
	}
	if err := store.DB().Create(&persistence.CommercialBillingProviderEvent{
		Provider: "stripe", EventID: "evt_historical", EventType: "checkout.completed",
		ProviderResourceID: "cs_historical", TenantID: domain.TenantID,
		PayloadSHA256: []byte(strings.Repeat("c", 32)), LiveMode: false, Outcome: "applied",
		EventCreatedAt: now, ProcessedAt: now,
	}).Error; err == nil {
		t.Fatal("SQLite accepted historical payment-provider event creation")
	}
	if err := store.DB().Exec("INSERT INTO stage6_billing_exercises (id) VALUES (?)", uuid.New()).Error; err == nil {
		t.Fatal("SQLite accepted a historical Billing exercise after lockdown")
	}
	if err := store.DB().Exec("INSERT INTO stage6_billing_exercise_approvals (id) VALUES (?)", uuid.New()).Error; err == nil {
		t.Fatal("SQLite accepted a historical Billing exercise approval after lockdown")
	}
}
