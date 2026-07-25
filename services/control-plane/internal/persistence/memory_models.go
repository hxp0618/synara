package persistence

import (
	"time"

	"github.com/google/uuid"
)

type AgentMemoryHead struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	ScopeType         string     `gorm:"column:scope_type"`
	ScopeUserID       *uuid.UUID `gorm:"column:scope_user_id;type:uuid"`
	ScopeProjectID    *uuid.UUID `gorm:"column:scope_project_id;type:uuid"`
	ScopeSessionID    *uuid.UUID `gorm:"column:scope_session_id;type:uuid"`
	MemoryKey         string     `gorm:"column:memory_key"`
	CurrentRevisionID *uuid.UUID `gorm:"column:current_revision_id;type:uuid"`
	Version           int64      `gorm:"column:version"`
	Enabled           bool       `gorm:"column:enabled"`
	UpdatedBy         uuid.UUID  `gorm:"column:updated_by;type:uuid"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

func (AgentMemoryHead) TableName() string { return "agent_memory_heads" }

type AgentMemoryRevision struct {
	ID             uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TenantID       uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	MemoryHeadID   uuid.UUID `gorm:"column:memory_head_id;type:uuid"`
	RevisionNumber int64     `gorm:"column:revision_number"`
	ArtifactID     uuid.UUID `gorm:"column:artifact_id;type:uuid"`
	SHA256         string    `gorm:"column:sha256"`
	MediaType      string    `gorm:"column:media_type"`
	SizeBytes      int64     `gorm:"column:size_bytes"`
	CreatedBy      uuid.UUID `gorm:"column:created_by;type:uuid"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (AgentMemoryRevision) TableName() string { return "agent_memory_revisions" }
