package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionInteraction struct {
	ID          uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID    uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	ExecutionID uuid.UUID      `gorm:"column:execution_id;type:uuid"`
	SessionID   uuid.UUID      `gorm:"column:session_id;type:uuid"`
	WorkerID    uuid.UUID      `gorm:"column:worker_id;type:uuid"`
	Generation  int64          `gorm:"column:generation"`
	RequestID   string         `gorm:"column:request_id"`
	Kind        string         `gorm:"column:kind"`
	Status      string         `gorm:"column:status"`
	Payload     map[string]any `gorm:"column:payload;serializer:json"`
	Resolution  map[string]any `gorm:"column:resolution;serializer:json"`
	RequestedAt time.Time      `gorm:"column:requested_at"`
	ResolvedAt  *time.Time     `gorm:"column:resolved_at"`
	ResolvedBy  *uuid.UUID     `gorm:"column:resolved_by;type:uuid"`
}

func (ExecutionInteraction) TableName() string { return "execution_interactions" }
