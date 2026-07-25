package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerIncarnationFact struct {
	WorkerID                       uuid.UUID  `gorm:"column:worker_id;type:uuid;primaryKey"`
	WorkerIncarnation              int64      `gorm:"column:worker_incarnation;primaryKey"`
	TenantID                       *uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	ExecutionTargetID              uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	TargetKind                     string     `gorm:"column:target_kind"`
	WorkerMode                     string     `gorm:"column:worker_mode"`
	WorkerPoolID                   *uuid.UUID `gorm:"column:worker_pool_id;type:uuid"`
	WorkerPoolVersion              *int64     `gorm:"column:worker_pool_version"`
	PoolMode                       *string    `gorm:"column:pool_mode"`
	CapacityClass                  *string    `gorm:"column:capacity_class"`
	ClusterID                      string     `gorm:"column:cluster_id"`
	Region                         string     `gorm:"column:region;default:''"`
	Namespace                      string     `gorm:"column:namespace"`
	PodName                        string     `gorm:"column:pod_name"`
	InstanceUID                    string     `gorm:"column:instance_uid"`
	RegisteredAt                   time.Time  `gorm:"column:registered_at"`
	CurrentState                   string     `gorm:"column:current_state"`
	StateChangedAt                 time.Time  `gorm:"column:state_changed_at"`
	TerminatedAt                   *time.Time `gorm:"column:terminated_at"`
	TerminalReason                 *string    `gorm:"column:terminal_reason"`
	AccumulatedActiveSeconds       int64      `gorm:"column:accumulated_active_seconds;default:0"`
	AccumulatedIdleSeconds         int64      `gorm:"column:accumulated_idle_seconds;default:0"`
	ClaimCount                     int64      `gorm:"column:claim_count;default:0"`
	RequestedCPUMillicores         *int64     `gorm:"column:requested_cpu_millicores"`
	RequestedMemoryBytes           *int64     `gorm:"column:requested_memory_bytes"`
	RequestedEphemeralStorageBytes *int64     `gorm:"column:requested_ephemeral_storage_bytes"`
	CreatedAt                      time.Time  `gorm:"column:created_at"`
	UpdatedAt                      time.Time  `gorm:"column:updated_at"`
}

func (WorkerIncarnationFact) TableName() string { return "worker_incarnation_facts" }
