package persistence

import (
	"time"

	"github.com/google/uuid"
)

type LegalHold struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	ScopeType       string     `gorm:"column:scope_type"`
	ScopeID         uuid.UUID  `gorm:"column:scope_id;type:uuid"`
	Name            string     `gorm:"column:name"`
	MatterReference string     `gorm:"column:matter_reference"`
	Reason          string     `gorm:"column:reason"`
	Status          string     `gorm:"column:status"`
	Version         int64      `gorm:"column:version;not null;default:1"`
	CreatedBy       uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	ReleasedBy      *uuid.UUID `gorm:"column:released_by;type:uuid"`
	ReleaseReason   *string    `gorm:"column:release_reason"`
	ReleasedAt      *time.Time `gorm:"column:released_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (LegalHold) TableName() string { return "legal_holds" }
