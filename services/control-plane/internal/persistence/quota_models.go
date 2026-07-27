package persistence

import (
	"time"

	"github.com/google/uuid"
)

type TenantQuota struct {
	TenantID                    uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	MaxConcurrentExecutions     *int      `gorm:"column:max_concurrent_executions"`
	MaxQueuedExecutions         *int      `gorm:"column:max_queued_executions"`
	MaxConcurrentExecutionUnits *int64    `gorm:"column:max_concurrent_execution_units"`
	MaxArtifactBytes            *int64    `gorm:"column:max_artifact_bytes"`
	UpdatedBy                   uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt                   time.Time `gorm:"column:created_at"`
	UpdatedAt                   time.Time `gorm:"column:updated_at"`
}

func (TenantQuota) TableName() string { return "tenant_quotas" }

// ExecutionQuotaPolicy is the server-authoritative concurrency and normalized
// resource-unit limit for a Project, Session, or Automation. Tenant-wide
// limits remain on TenantQuota for API compatibility; admission evaluates all
// applicable scopes under the same Tenant-row lock.
type ExecutionQuotaPolicy struct {
	TenantID                    uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ScopeKind                   string    `gorm:"column:scope_kind;primaryKey"`
	ScopeID                     uuid.UUID `gorm:"column:scope_id;type:uuid;primaryKey"`
	MaxConcurrentExecutions     *int      `gorm:"column:max_concurrent_executions"`
	MaxQueuedExecutions         *int      `gorm:"column:max_queued_executions"`
	MaxConcurrentExecutionUnits *int64    `gorm:"column:max_concurrent_execution_units"`
	Version                     int64     `gorm:"column:version;not null;default:1"`
	UpdatedBy                   uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt                   time.Time `gorm:"column:created_at"`
	UpdatedAt                   time.Time `gorm:"column:updated_at"`
}

func (ExecutionQuotaPolicy) TableName() string { return "execution_quota_policies" }
