package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionKubernetesAllocation struct {
	TenantID                 uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ExecutionID              uuid.UUID  `gorm:"column:execution_id;type:uuid;primaryKey"`
	Generation               int64      `gorm:"column:generation;primaryKey"`
	ExecutionTargetID        uuid.UUID  `gorm:"column:execution_target_id;type:uuid;index:idx_execution_kubernetes_allocations_target_status,priority:1"`
	Backend                  string     `gorm:"column:backend"`
	Namespace                string     `gorm:"column:namespace"`
	SandboxTemplateName      string     `gorm:"column:sandbox_template_name"`
	SandboxWarmPoolName      string     `gorm:"column:sandbox_warm_pool_name"`
	ConfigurationDigest      string     `gorm:"column:configuration_digest"`
	ClaimName                string     `gorm:"column:claim_name"`
	ClaimUID                 *string    `gorm:"column:claim_uid"`
	SandboxName              *string    `gorm:"column:sandbox_name"`
	SandboxUID               *string    `gorm:"column:sandbox_uid"`
	PodName                  *string    `gorm:"column:pod_name"`
	PodUID                   *string    `gorm:"column:pod_uid"`
	Status                   string     `gorm:"column:status;index:idx_execution_kubernetes_allocations_target_status,priority:2"`
	FailureReasonCode        *string    `gorm:"column:failure_reason_code"`
	MaterializationStartedAt time.Time  `gorm:"column:materialization_started_at"`
	ClaimReadyAt             *time.Time `gorm:"column:claim_ready_at"`
	BoundAt                  *time.Time `gorm:"column:bound_at"`
	DeleteRequestedAt        *time.Time `gorm:"column:delete_requested_at"`
	DeletedAt                *time.Time `gorm:"column:deleted_at"`
	LastObservedAt           time.Time  `gorm:"column:last_observed_at"`
	CreatedAt                time.Time  `gorm:"column:created_at"`
	UpdatedAt                time.Time  `gorm:"column:updated_at"`
}

func (ExecutionKubernetesAllocation) TableName() string {
	return "execution_kubernetes_allocations"
}
