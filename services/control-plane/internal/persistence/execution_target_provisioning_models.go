package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionTargetProvisioningOperation struct {
	ID                uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID          uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	OrganizationID    *uuid.UUID     `gorm:"column:organization_id;type:uuid"`
	ExecutionTargetID uuid.UUID      `gorm:"column:execution_target_id;type:uuid"`
	ActorType         string         `gorm:"column:actor_type"`
	ActorID           uuid.UUID      `gorm:"column:actor_id;type:uuid"`
	Action            string         `gorm:"column:action"`
	IdempotencyKey    string         `gorm:"column:idempotency_key"`
	RequestHash       string         `gorm:"column:request_hash"`
	State             string         `gorm:"column:state"`
	AttemptGeneration int64          `gorm:"column:attempt_generation"`
	ClaimHolder       *string        `gorm:"column:claim_holder"`
	ClaimExpiresAt    *time.Time     `gorm:"column:claim_expires_at"`
	Result            map[string]any `gorm:"column:result;serializer:json"`
	ErrorCode         *string        `gorm:"column:error_code"`
	ErrorMessage      *string        `gorm:"column:error_message"`
	RequestID         string         `gorm:"column:request_id"`
	IPAddress         string         `gorm:"column:ip_address"`
	CreatedAt         time.Time      `gorm:"column:created_at"`
	UpdatedAt         time.Time      `gorm:"column:updated_at"`
	StartedAt         *time.Time     `gorm:"column:started_at"`
	CompletedAt       *time.Time     `gorm:"column:completed_at"`
}

func (ExecutionTargetProvisioningOperation) TableName() string {
	return "execution_target_provisioning_operations"
}
