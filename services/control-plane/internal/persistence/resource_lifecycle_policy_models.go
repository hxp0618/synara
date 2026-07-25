package persistence

import (
	"time"

	"github.com/google/uuid"
)

type TenantResourceLifecyclePolicy struct {
	TenantID                       uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	WaitingKeepAliveSeconds        *int      `gorm:"column:waiting_keep_alive_seconds"`
	SuspendAfterIdleSeconds        *int      `gorm:"column:suspend_after_idle_seconds"`
	AbsoluteSessionLifetimeSeconds *int      `gorm:"column:absolute_session_lifetime_seconds"`
	WorkspaceRetentionDays         *int      `gorm:"column:workspace_retention_days"`
	WarmPoolMode                   *string   `gorm:"column:warm_pool_mode"`
	Version                        int64     `gorm:"column:version"`
	UpdatedBy                      uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt                      time.Time `gorm:"column:created_at"`
	UpdatedAt                      time.Time `gorm:"column:updated_at"`
}

func (TenantResourceLifecyclePolicy) TableName() string {
	return "tenant_resource_lifecycle_policies"
}

type ProjectResourceLifecyclePolicy struct {
	ProjectID                      uuid.UUID `gorm:"column:project_id;type:uuid;primaryKey"`
	TenantID                       uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	WaitingKeepAliveSeconds        *int      `gorm:"column:waiting_keep_alive_seconds"`
	SuspendAfterIdleSeconds        *int      `gorm:"column:suspend_after_idle_seconds"`
	AbsoluteSessionLifetimeSeconds *int      `gorm:"column:absolute_session_lifetime_seconds"`
	WorkspaceRetentionDays         *int      `gorm:"column:workspace_retention_days"`
	WarmPoolMode                   *string   `gorm:"column:warm_pool_mode"`
	Version                        int64     `gorm:"column:version"`
	UpdatedBy                      uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt                      time.Time `gorm:"column:created_at"`
	UpdatedAt                      time.Time `gorm:"column:updated_at"`
}

func (ProjectResourceLifecyclePolicy) TableName() string {
	return "project_resource_lifecycle_policies"
}
