package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerReleaseRevision struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey;uniqueIndex:uq_worker_release_revision_tenant_id,priority:2;uniqueIndex:uq_worker_release_revision_target_id,priority:2"`
	TenantID          uuid.UUID `gorm:"column:tenant_id;type:uuid;not null;uniqueIndex:uq_worker_release_revision_tenant_id,priority:1"`
	ExecutionTargetID uuid.UUID `gorm:"column:execution_target_id;type:uuid;not null;uniqueIndex:uq_worker_release_revision_target_id,priority:1;uniqueIndex:uq_worker_release_revision_target_number,priority:1;uniqueIndex:uq_worker_release_revision_target_manifest,priority:1"`
	Revision          int64     `gorm:"column:revision;not null;uniqueIndex:uq_worker_release_revision_target_number,priority:2"`
	WorkerManifestID  uuid.UUID `gorm:"column:worker_manifest_id;type:uuid;not null;uniqueIndex:uq_worker_release_revision_target_manifest,priority:2"`
	Description       string    `gorm:"column:description;not null;default:''"`
	CreatedBy         uuid.UUID `gorm:"column:created_by;type:uuid;not null"`
	CreatedAt         time.Time `gorm:"column:created_at;not null"`
}

func (WorkerReleaseRevision) TableName() string { return "worker_release_revisions" }

type WorkerReleasePolicy struct {
	TenantID           uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null;uniqueIndex:uq_worker_release_policy_tenant_target"`
	ExecutionTargetID  uuid.UUID  `gorm:"column:execution_target_id;type:uuid;primaryKey;uniqueIndex:uq_worker_release_policy_tenant_target"`
	PolicyVersion      int64      `gorm:"column:policy_version;not null"`
	PromotedRevisionID uuid.UUID  `gorm:"column:promoted_revision_id;type:uuid;not null"`
	CanaryRevisionID   *uuid.UUID `gorm:"column:canary_revision_id;type:uuid"`
	CanaryPercent      int        `gorm:"column:canary_percent;not null;default:0"`
	UpdatedBy          uuid.UUID  `gorm:"column:updated_by;type:uuid;not null"`
	UpdatedAt          time.Time  `gorm:"column:updated_at;not null"`
}

func (WorkerReleasePolicy) TableName() string { return "worker_release_policies" }

type WorkerReleaseTransition struct {
	ID                     uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID               uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null"`
	ExecutionTargetID      uuid.UUID  `gorm:"column:execution_target_id;type:uuid;not null;uniqueIndex:uq_worker_release_transition_target_version"`
	PolicyVersion          int64      `gorm:"column:policy_version;not null;uniqueIndex:uq_worker_release_transition_target_version"`
	Action                 string     `gorm:"column:action;not null"`
	FromPromotedRevisionID *uuid.UUID `gorm:"column:from_promoted_revision_id;type:uuid"`
	FromCanaryRevisionID   *uuid.UUID `gorm:"column:from_canary_revision_id;type:uuid"`
	ToPromotedRevisionID   uuid.UUID  `gorm:"column:to_promoted_revision_id;type:uuid;not null"`
	ToCanaryRevisionID     *uuid.UUID `gorm:"column:to_canary_revision_id;type:uuid"`
	CanaryPercent          int        `gorm:"column:canary_percent;not null;default:0"`
	Reason                 string     `gorm:"column:reason;not null"`
	ActorID                uuid.UUID  `gorm:"column:actor_id;type:uuid;not null"`
	RequestID              *string    `gorm:"column:request_id"`
	OccurredAt             time.Time  `gorm:"column:occurred_at;not null"`
}

func (WorkerReleaseTransition) TableName() string { return "worker_release_transitions" }
