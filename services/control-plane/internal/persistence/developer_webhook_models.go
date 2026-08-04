package persistence

import (
	"time"

	"github.com/google/uuid"
)

type DeveloperWebhookEndpoint struct {
	ID                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	Name               string     `gorm:"column:name"`
	URL                string     `gorm:"column:url"`
	Status             string     `gorm:"column:status"`
	EventTypes         []string   `gorm:"column:event_types;serializer:json"`
	SecretVersion      int64      `gorm:"column:secret_version"`
	EncryptedSecret    []byte     `gorm:"column:encrypted_secret"`
	EncryptedDataKey   []byte     `gorm:"column:encrypted_data_key"`
	KMSProvider        string     `gorm:"column:kms_provider"`
	KMSKeyID           string     `gorm:"column:kms_key_id"`
	CreatedBy          uuid.UUID  `gorm:"column:created_by;type:uuid"`
	LastDeliveredAt    *time.Time `gorm:"column:last_delivered_at"`
	LastFailedAt       *time.Time `gorm:"column:last_failed_at"`
	LastFailureSummary *string    `gorm:"column:last_failure_summary"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
	RevokedAt          *time.Time `gorm:"column:revoked_at"`
}

func (DeveloperWebhookEndpoint) TableName() string { return "developer_webhook_endpoints" }

type DeveloperWebhookDelivery struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	EndpointID      uuid.UUID  `gorm:"column:endpoint_id;type:uuid"`
	SessionEventID  uuid.UUID  `gorm:"column:session_event_id;type:uuid"`
	EventType       string     `gorm:"column:event_type"`
	SessionID       uuid.UUID  `gorm:"column:session_id;type:uuid"`
	ExecutionID     *uuid.UUID `gorm:"column:execution_id;type:uuid"`
	Sequence        int64      `gorm:"column:sequence"`
	OutboxMessageID uuid.UUID  `gorm:"column:outbox_message_id;type:uuid"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
}

func (DeveloperWebhookDelivery) TableName() string { return "developer_webhook_deliveries" }
