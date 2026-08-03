package persistence

import (
	"time"

	"github.com/google/uuid"
)

type CommercialBillingCheckoutSession struct {
	ID                          uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                    uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	RequestedByUserID           uuid.UUID  `gorm:"column:requested_by_user_id;type:uuid"`
	PlanCode                    string     `gorm:"column:plan_code"`
	ExpectedSubscriptionVersion int64      `gorm:"column:expected_subscription_version"`
	Provider                    string     `gorm:"column:provider"`
	ProviderPriceID             string     `gorm:"column:provider_price_id"`
	ProviderSessionID           *string    `gorm:"column:provider_session_id"`
	ProviderSubscriptionID      *string    `gorm:"column:provider_subscription_id"`
	Status                      string     `gorm:"column:status"`
	IdempotencyKeySHA256        []byte     `gorm:"column:idempotency_key_sha256"`
	ProviderRequestSHA256       []byte     `gorm:"column:provider_request_sha256"`
	ExpiresAt                   *time.Time `gorm:"column:expires_at"`
	CompletedAt                 *time.Time `gorm:"column:completed_at"`
	CreatedAt                   time.Time  `gorm:"column:created_at"`
	UpdatedAt                   time.Time  `gorm:"column:updated_at"`
}

func (CommercialBillingCheckoutSession) TableName() string {
	return "commercial_billing_checkout_sessions"
}

type CommercialBillingProviderEvent struct {
	Provider           string    `gorm:"column:provider;primaryKey"`
	EventID            string    `gorm:"column:event_id;primaryKey"`
	EventType          string    `gorm:"column:event_type"`
	ProviderResourceID string    `gorm:"column:provider_resource_id"`
	TenantID           uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	PayloadSHA256      []byte    `gorm:"column:payload_sha256"`
	LiveMode           bool      `gorm:"column:live_mode"`
	Outcome            string    `gorm:"column:outcome"`
	EventCreatedAt     time.Time `gorm:"column:event_created_at"`
	ProcessedAt        time.Time `gorm:"column:processed_at"`
}

func (CommercialBillingProviderEvent) TableName() string {
	return "commercial_billing_provider_events"
}
