package persistence

import (
	"time"

	"github.com/google/uuid"
)

type TenantQuota struct {
	TenantID                uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	MaxConcurrentExecutions *int      `gorm:"column:max_concurrent_executions"`
	MaxArtifactBytes        *int64    `gorm:"column:max_artifact_bytes"`
	UpdatedBy               uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt               time.Time `gorm:"column:created_at"`
	UpdatedAt               time.Time `gorm:"column:updated_at"`
}

func (TenantQuota) TableName() string { return "tenant_quotas" }
