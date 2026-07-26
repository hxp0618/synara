package persistence

import (
	"time"

	"github.com/google/uuid"
)

// PlatformRoutingPublication is the immutable receipt for one authenticated
// platform/integration routing-authority bundle. The corresponding health and
// DR rows are committed in the same transaction as this receipt.
type PlatformRoutingPublication struct {
	PublisherIdentity string         `gorm:"column:publisher_identity;primaryKey"`
	Nonce             uuid.UUID      `gorm:"column:nonce;type:uuid;primaryKey"`
	KeyID             string         `gorm:"column:key_id;not null"`
	PublicKeySHA256   string         `gorm:"column:public_key_sha256;not null"`
	ExecutionTargetID uuid.UUID      `gorm:"column:execution_target_id;type:uuid;not null"`
	RequestSHA256     string         `gorm:"column:request_sha256;not null"`
	ObservedAt        time.Time      `gorm:"column:observed_at;not null"`
	Response          map[string]any `gorm:"column:response;serializer:json;not null"`
	ResponseSHA256    string         `gorm:"column:response_sha256;not null"`
	ReceivedAt        time.Time      `gorm:"column:received_at;not null"`
}

func (PlatformRoutingPublication) TableName() string { return "platform_routing_publications" }
