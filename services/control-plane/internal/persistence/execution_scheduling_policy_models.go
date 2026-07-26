package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionSchedulingPolicyHead struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null;uniqueIndex:uq_execution_scheduling_policy_head_scope,priority:1"`
	ScopeKind         string     `gorm:"column:scope_kind;not null;uniqueIndex:uq_execution_scheduling_policy_head_scope,priority:2"`
	ScopeID           uuid.UUID  `gorm:"column:scope_id;type:uuid;not null;uniqueIndex:uq_execution_scheduling_policy_head_scope,priority:3"`
	OrganizationID    *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	CurrentRevisionID *uuid.UUID `gorm:"column:current_revision_id;type:uuid"`
	Version           int64      `gorm:"column:version;not null;default:0"`
	UpdatedBy         uuid.UUID  `gorm:"column:updated_by;type:uuid;not null"`
	CreatedAt         time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;not null"`
}

func (ExecutionSchedulingPolicyHead) TableName() string {
	return "execution_scheduling_policy_heads"
}

type ExecutionSchedulingPolicyRevision struct {
	ID             uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TenantID       uuid.UUID `gorm:"column:tenant_id;type:uuid;not null;uniqueIndex:uq_execution_scheduling_policy_revision_number,priority:1"`
	PolicyHeadID   uuid.UUID `gorm:"column:policy_head_id;type:uuid;not null;uniqueIndex:uq_execution_scheduling_policy_revision_number,priority:2"`
	RevisionNumber int64     `gorm:"column:revision_number;not null;uniqueIndex:uq_execution_scheduling_policy_revision_number,priority:3"`
	SHA256         string    `gorm:"column:sha256;not null"`
	DenyAll        bool      `gorm:"column:deny_all;not null;default:false"`
	CreatedBy      uuid.UUID `gorm:"column:created_by;type:uuid;not null"`
	CreatedAt      time.Time `gorm:"column:created_at;not null"`
}

func (ExecutionSchedulingPolicyRevision) TableName() string {
	return "execution_scheduling_policy_revisions"
}

type ExecutionSchedulingPolicyRule struct {
	TenantID   uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	RevisionID uuid.UUID `gorm:"column:revision_id;type:uuid;primaryKey"`
	Dimension  string    `gorm:"column:dimension;primaryKey"`
	Mode       string    `gorm:"column:mode;not null"`
}

func (ExecutionSchedulingPolicyRule) TableName() string {
	return "execution_scheduling_policy_rules"
}

type ExecutionSchedulingPolicyRuleValue struct {
	TenantID   uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	RevisionID uuid.UUID `gorm:"column:revision_id;type:uuid;primaryKey"`
	Dimension  string    `gorm:"column:dimension;primaryKey"`
	Value      string    `gorm:"column:value;primaryKey"`
}

func (ExecutionSchedulingPolicyRuleValue) TableName() string {
	return "execution_scheduling_policy_rule_values"
}
