package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionInteraction struct {
	ID                  uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID            uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	ExecutionID         uuid.UUID      `gorm:"column:execution_id;type:uuid"`
	SessionID           uuid.UUID      `gorm:"column:session_id;type:uuid"`
	TurnID              uuid.UUID      `gorm:"column:turn_id;type:uuid"`
	WorkerID            uuid.UUID      `gorm:"column:worker_id;type:uuid"`
	Generation          int64          `gorm:"column:generation"`
	Provider            string         `gorm:"column:provider"`
	RequestID           string         `gorm:"column:request_id"`
	EventVersion        int            `gorm:"column:event_version;default:1"`
	Kind                string         `gorm:"column:kind"`
	Status              string         `gorm:"column:status"`
	Payload             map[string]any `gorm:"column:payload;serializer:json"`
	Resolution          map[string]any `gorm:"column:resolution;serializer:json"`
	RequestedAt         time.Time      `gorm:"column:requested_at"`
	ExpiresAt           time.Time      `gorm:"column:expires_at"`
	ResolvedAt          *time.Time     `gorm:"column:resolved_at"`
	ResolvedBy          *uuid.UUID     `gorm:"column:resolved_by;type:uuid"`
	ResolutionKind      *string        `gorm:"column:resolution_kind"`
	ResolutionCommandID *string        `gorm:"column:resolution_command_id"`
	DeliveryStatus      string         `gorm:"column:delivery_status;default:not-ready"`
	DeliveryWorkerID    *uuid.UUID     `gorm:"column:delivery_worker_id;type:uuid"`
	DeliveryGeneration  *int64         `gorm:"column:delivery_generation"`
	DeliveryAttempts    int            `gorm:"column:delivery_attempts"`
	DeliveryAvailableAt *time.Time     `gorm:"column:delivery_available_at"`
	DeliveredAt         *time.Time     `gorm:"column:delivered_at"`
	AcknowledgedAt      *time.Time     `gorm:"column:acknowledged_at"`
	DeliveryError       *string        `gorm:"column:delivery_error"`
}

func (ExecutionInteraction) TableName() string { return "execution_interactions" }
