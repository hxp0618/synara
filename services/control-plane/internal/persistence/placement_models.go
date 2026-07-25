package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerPool struct {
	ID                 uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID           *uuid.UUID     `gorm:"column:tenant_id;type:uuid"`
	ExecutionTargetID  uuid.UUID      `gorm:"column:execution_target_id;type:uuid;not null;uniqueIndex:uq_worker_pool_target_name,priority:1;index:idx_worker_pool_target_status,priority:1"`
	Name               string         `gorm:"column:name;not null;uniqueIndex:uq_worker_pool_target_name,priority:2"`
	Mode               string         `gorm:"column:mode;not null"`
	CapacityClass      string         `gorm:"column:capacity_class;not null;index:idx_worker_pool_target_status,priority:3"`
	ClusterID          string         `gorm:"column:cluster_id;not null;default:''"`
	Region             string         `gorm:"column:region;not null;default:''"`
	Namespace          string         `gorm:"column:namespace;not null;default:''"`
	DesiredIdleUnits   int            `gorm:"column:desired_idle_units;not null;default:0"`
	MaxActiveUnits     int            `gorm:"column:max_active_units;not null;default:1"`
	SchedulingTemplate map[string]any `gorm:"column:scheduling_template;serializer:json"`
	Status             string         `gorm:"column:status;not null;default:active;index:idx_worker_pool_target_status,priority:2"`
	Version            int64          `gorm:"column:version;not null;default:1"`
	CreatedAt          time.Time      `gorm:"column:created_at;not null"`
	UpdatedAt          time.Time      `gorm:"column:updated_at;not null"`
}

func (WorkerPool) TableName() string { return "worker_pools" }

type ExecutionPlacementPolicy struct {
	TenantID          *uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	ExecutionTargetID uuid.UUID  `gorm:"column:execution_target_id;type:uuid;primaryKey"`
	Version           int64      `gorm:"column:version;not null;default:1"`
	DefaultPoolID     uuid.UUID  `gorm:"column:default_pool_id;type:uuid;not null"`
	BalancedPoolID    *uuid.UUID `gorm:"column:balanced_pool_id;type:uuid"`
	LowLatencyPoolID  *uuid.UUID `gorm:"column:low_latency_pool_id;type:uuid"`
	UpdatedBy         *uuid.UUID `gorm:"column:updated_by;type:uuid"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;not null"`
}

func (ExecutionPlacementPolicy) TableName() string { return "execution_placement_policies" }
