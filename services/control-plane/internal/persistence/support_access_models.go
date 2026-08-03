package persistence

import (
	"time"

	"github.com/google/uuid"
)

type TenantSupportPolicy struct {
	TenantID             uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	SupportAccessEnabled bool      `gorm:"column:support_access_enabled"`
	Version              int64     `gorm:"column:version;not null;default:1"`
	Reason               string    `gorm:"column:reason"`
	UpdatedBy            uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt            time.Time `gorm:"column:created_at"`
	UpdatedAt            time.Time `gorm:"column:updated_at"`
}

func (TenantSupportPolicy) TableName() string { return "tenant_support_policies" }

type SupportAccessGrant struct {
	ID                       uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                 uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OperatorTenantID         *uuid.UUID `gorm:"column:operator_tenant_id;type:uuid"`
	RequesterUserID          uuid.UUID  `gorm:"column:requester_user_id;type:uuid"`
	Status                   string     `gorm:"column:status"`
	Version                  int64      `gorm:"column:version;not null;default:1"`
	Reason                   string     `gorm:"column:reason"`
	RequestedDurationSeconds int        `gorm:"column:requested_duration_seconds"`
	RequestedAt              time.Time  `gorm:"column:requested_at"`
	DecidedBy                *uuid.UUID `gorm:"column:decided_by;type:uuid"`
	DecisionReason           *string    `gorm:"column:decision_reason"`
	DecidedAt                *time.Time `gorm:"column:decided_at"`
	ExpiresAt                *time.Time `gorm:"column:expires_at"`
	RevokedBy                *uuid.UUID `gorm:"column:revoked_by;type:uuid"`
	RevocationReason         *string    `gorm:"column:revocation_reason"`
	RevokedAt                *time.Time `gorm:"column:revoked_at"`
	CreatedAt                time.Time  `gorm:"column:created_at"`
	UpdatedAt                time.Time  `gorm:"column:updated_at"`
}

func (SupportAccessGrant) TableName() string { return "support_access_grants" }
