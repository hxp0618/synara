package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionControlCommand struct {
	ID                  uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID            uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	ExecutionID         uuid.UUID      `gorm:"column:execution_id;type:uuid"`
	SessionID           uuid.UUID      `gorm:"column:session_id;type:uuid"`
	TurnID              uuid.UUID      `gorm:"column:turn_id;type:uuid"`
	Provider            string         `gorm:"column:provider"`
	CommandType         string         `gorm:"column:command_type"`
	CommandID           string         `gorm:"column:command_id"`
	Payload             map[string]any `gorm:"column:payload;serializer:json"`
	Status              string         `gorm:"column:status"`
	RequestedBy         uuid.UUID      `gorm:"column:requested_by;type:uuid"`
	RequestedAt         time.Time      `gorm:"column:requested_at"`
	DeliveryWorkerID    *uuid.UUID     `gorm:"column:delivery_worker_id;type:uuid"`
	DeliveryGeneration  *int64         `gorm:"column:delivery_generation"`
	DeliveryAttempts    int            `gorm:"column:delivery_attempts"`
	DeliveryAvailableAt time.Time      `gorm:"column:delivery_available_at"`
	DeliveredAt         *time.Time     `gorm:"column:delivered_at"`
	AcknowledgedAt      *time.Time     `gorm:"column:acknowledged_at"`
	DeliveryError       *string        `gorm:"column:delivery_error"`
}

func (ExecutionControlCommand) TableName() string { return "execution_control_commands" }
