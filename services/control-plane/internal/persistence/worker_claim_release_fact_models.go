package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerClaimReleaseFact struct {
	ClaimFactID   uuid.UUID       `gorm:"column:claim_fact_id;type:uuid;primaryKey;index:idx_worker_claim_release_facts_released,priority:2"`
	ReleasedAt    time.Time       `gorm:"column:released_at;not null;index:idx_worker_claim_release_facts_released,priority:1"`
	RecordedAt    time.Time       `gorm:"column:recorded_at;not null"`
	ReleaseReason string          `gorm:"column:release_reason;not null"`
	AuthorityKind string          `gorm:"column:authority_kind;not null"`
	AuthorityID   *string         `gorm:"column:authority_id"`
	RequestID     *string         `gorm:"column:request_id"`
	Metadata      map[string]any  `gorm:"column:metadata;serializer:json;not null"`
	ClaimFact     WorkerClaimFact `gorm:"foreignKey:ClaimFactID;references:ID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
}

func (WorkerClaimReleaseFact) TableName() string { return "worker_claim_release_facts" }
