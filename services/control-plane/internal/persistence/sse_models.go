package persistence

import (
	"time"

	"github.com/google/uuid"
)

type SSEConnectionLease struct {
	ID          uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TenantID    uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	UserID      uuid.UUID `gorm:"column:user_id;type:uuid"`
	SessionID   uuid.UUID `gorm:"column:session_id;type:uuid"`
	InstanceID  string    `gorm:"column:instance_id"`
	ConnectedAt time.Time `gorm:"column:connected_at"`
	RenewedAt   time.Time `gorm:"column:renewed_at"`
	ExpiresAt   time.Time `gorm:"column:expires_at"`
}

func (SSEConnectionLease) TableName() string { return "sse_connection_leases" }
