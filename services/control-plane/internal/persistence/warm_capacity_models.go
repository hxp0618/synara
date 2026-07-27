package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerPoolWarmCapacity struct {
	WorkerPoolID              uuid.UUID  `gorm:"column:worker_pool_id;type:uuid;primaryKey"`
	WorkerPoolVersion         int64      `gorm:"column:worker_pool_version;primaryKey"`
	TenantID                  uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null;index:idx_worker_pool_warm_capacity_target,priority:1"`
	ExecutionTargetID         uuid.UUID  `gorm:"column:execution_target_id;type:uuid;not null;index:idx_worker_pool_warm_capacity_target,priority:2;index:idx_worker_pool_warm_capacity_expiry,priority:2"`
	CapacityClass             string     `gorm:"column:capacity_class;not null"`
	WarmSupported             bool       `gorm:"column:warm_supported;not null;default:false"`
	WorkerReleaseRevisionID   *uuid.UUID `gorm:"column:worker_release_revision_id;type:uuid"`
	WorkerReleaseChannel      *string    `gorm:"column:worker_release_channel"`
	DesiredIdleUnits          int        `gorm:"column:desired_idle_units;not null;default:0"`
	EffectiveDesiredIdleUnits int        `gorm:"column:effective_desired_idle_units;not null;default:0"`
	MinIdleUnits              int        `gorm:"column:min_idle_units;not null;default:0"`
	MaxActiveUnits            int        `gorm:"column:max_active_units;not null;default:0"`
	DesiredTotalUnits         int        `gorm:"column:desired_total_units;not null;default:0"`
	ClaimedUnits              int        `gorm:"column:claimed_units;not null;default:0"`
	ReadyIdleUnits            int        `gorm:"column:ready_idle_units;not null;default:0"`
	Source                    string     `gorm:"column:source;not null"`
	Reason                    *string    `gorm:"column:reason"`
	ObservedAt                time.Time  `gorm:"column:observed_at;not null;index:idx_worker_pool_warm_capacity_expiry,priority:1"`
	ExpiresAt                 time.Time  `gorm:"column:expires_at;not null;index:idx_worker_pool_warm_capacity_expiry,priority:3"`
	Version                   int64      `gorm:"column:version;not null;default:1"`
	UpdatedAt                 time.Time  `gorm:"column:updated_at;not null"`
}

func (WorkerPoolWarmCapacity) TableName() string { return "worker_pool_warm_capacity" }
