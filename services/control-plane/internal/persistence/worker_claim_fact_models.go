package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerClaimFact struct {
	ID                        uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	WorkerID                  uuid.UUID  `gorm:"column:worker_id;type:uuid;not null;index:idx_worker_claim_facts_billing,priority:1;index:idx_worker_claim_facts_request,priority:1"`
	WorkerIncarnation         int64      `gorm:"column:worker_incarnation;not null;index:idx_worker_claim_facts_billing,priority:2;index:idx_worker_claim_facts_request,priority:2"`
	TenantID                  uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null"`
	ExecutionTargetID         uuid.UUID  `gorm:"column:execution_target_id;type:uuid;not null"`
	TargetKind                string     `gorm:"column:target_kind;not null"`
	ClaimKind                 string     `gorm:"column:claim_kind;not null"`
	RequestID                 string     `gorm:"column:request_id;not null;index:idx_worker_claim_facts_request,priority:3"`
	ClaimedAt                 time.Time  `gorm:"column:claimed_at;not null;index:idx_worker_claim_facts_billing,priority:3"`
	ExecutionID               *uuid.UUID `gorm:"column:execution_id;type:uuid;uniqueIndex:uq_worker_claim_facts_execution_generation,priority:1"`
	ExecutionGeneration       *int64     `gorm:"column:execution_generation;uniqueIndex:uq_worker_claim_facts_execution_generation,priority:2"`
	CleanupCommandID          *uuid.UUID `gorm:"column:cleanup_command_id;type:uuid;uniqueIndex:uq_worker_claim_facts_cleanup_dispatch,priority:1"`
	CleanupDispatchGeneration *int64     `gorm:"column:cleanup_dispatch_generation;uniqueIndex:uq_worker_claim_facts_cleanup_dispatch,priority:2"`
	CreatedAt                 time.Time  `gorm:"column:created_at;not null"`
}

func (WorkerClaimFact) TableName() string { return "worker_claim_facts" }
