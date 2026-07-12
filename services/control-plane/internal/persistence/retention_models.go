package persistence

import (
	"time"

	"github.com/google/uuid"
)

type TenantRetentionPolicy struct {
	TenantID                uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	SessionArchiveAfterDays *int      `gorm:"column:session_archive_after_days"`
	ArtifactDeleteAfterDays *int      `gorm:"column:artifact_delete_after_days"`
	UpdatedBy               uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt               time.Time `gorm:"column:created_at"`
	UpdatedAt               time.Time `gorm:"column:updated_at"`
}

func (TenantRetentionPolicy) TableName() string { return "tenant_retention_policies" }
