package persistence

import (
	"time"

	"github.com/google/uuid"
)

type APIIdempotencyKey struct {
	TenantID       uuid.UUID      `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ActorID        uuid.UUID      `gorm:"column:actor_id;type:uuid;primaryKey"`
	IdempotencyKey string         `gorm:"column:idempotency_key;primaryKey"`
	Operation      string         `gorm:"column:operation"`
	RequestHash    string         `gorm:"column:request_hash"`
	StatusCode     int            `gorm:"column:status_code"`
	Response       map[string]any `gorm:"column:response;serializer:json"`
	CompletedAt    *time.Time     `gorm:"column:completed_at"`
	CreatedAt      time.Time      `gorm:"column:created_at"`
}

func (APIIdempotencyKey) TableName() string { return "api_idempotency_keys" }
