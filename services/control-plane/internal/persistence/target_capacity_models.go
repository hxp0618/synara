package persistence

import (
	"time"

	"github.com/google/uuid"
)

// ExecutionTargetCapacity is the latest short-lived, target-scoped resource
// authority reported by the target reconciler. Nullable vectors mean that the
// operator did not configure a hard quota for that resource; zero is a known
// exhausted value and must not be treated as unknown.
type ExecutionTargetCapacity struct {
	ExecutionTargetID uuid.UUID `gorm:"column:execution_target_id;type:uuid;primaryKey"`
	TenantID          uuid.UUID `gorm:"column:tenant_id;type:uuid;not null;index:idx_execution_target_capacity_expiry,priority:2"`
	TargetKind        string    `gorm:"column:target_kind;not null;index:idx_execution_target_capacity_expiry,priority:1"`
	Source            string    `gorm:"column:source;not null"`

	TotalPods        int64 `gorm:"column:total_pods;not null"`
	AllocatedPods    int64 `gorm:"column:allocated_pods;not null"`
	AvailablePods    int64 `gorm:"column:available_pods;not null"`
	SchedulableUnits int64 `gorm:"column:schedulable_units;not null"`

	PodRequestCPUMillicores         *int64  `gorm:"column:pod_request_cpu_millicores"`
	TotalCPUMillicores              *int64  `gorm:"column:total_cpu_millicores"`
	AllocatedCPUMillicores          *int64  `gorm:"column:allocated_cpu_millicores"`
	AvailableCPUMillicores          *int64  `gorm:"column:available_cpu_millicores"`
	PodRequestMemoryBytes           *int64  `gorm:"column:pod_request_memory_bytes"`
	TotalMemoryBytes                *int64  `gorm:"column:total_memory_bytes"`
	AllocatedMemoryBytes            *int64  `gorm:"column:allocated_memory_bytes"`
	AvailableMemoryBytes            *int64  `gorm:"column:available_memory_bytes"`
	PodRequestEphemeralStorageBytes *int64  `gorm:"column:pod_request_ephemeral_storage_bytes"`
	TotalEphemeralStorageBytes      *int64  `gorm:"column:total_ephemeral_storage_bytes"`
	AllocatedEphemeralStorageBytes  *int64  `gorm:"column:allocated_ephemeral_storage_bytes"`
	AvailableEphemeralStorageBytes  *int64  `gorm:"column:available_ephemeral_storage_bytes"`
	GPUResourceName                 *string `gorm:"column:gpu_resource_name"`
	PodRequestGPUUnits              *int64  `gorm:"column:pod_request_gpu_units"`
	TotalGPUUnits                   *int64  `gorm:"column:total_gpu_units"`
	AllocatedGPUUnits               *int64  `gorm:"column:allocated_gpu_units"`
	AvailableGPUUnits               *int64  `gorm:"column:available_gpu_units"`

	ObservedAt time.Time `gorm:"column:observed_at;not null;index:idx_execution_target_capacity_expiry,priority:3"`
	ExpiresAt  time.Time `gorm:"column:expires_at;not null;index:idx_execution_target_capacity_expiry,priority:4"`
	Version    int64     `gorm:"column:version;not null;default:1"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null"`
}

func (ExecutionTargetCapacity) TableName() string { return "execution_target_capacities" }
