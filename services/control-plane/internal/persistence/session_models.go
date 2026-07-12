package persistence

import (
	"time"

	"github.com/google/uuid"
)

type Project struct {
	ID             uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID uuid.UUID  `gorm:"column:organization_id;type:uuid"`
	Name           string     `gorm:"column:name"`
	RepositoryURL  *string    `gorm:"column:repository_url"`
	DefaultBranch  string     `gorm:"column:default_branch"`
	Visibility     string     `gorm:"column:visibility"`
	CreatedBy      uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
	ArchivedAt     *time.Time `gorm:"column:archived_at"`
}

func (Project) TableName() string { return "projects" }

type AgentSession struct {
	ID                            uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                      uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID                uuid.UUID  `gorm:"column:organization_id;type:uuid"`
	ProjectID                     uuid.UUID  `gorm:"column:project_id;type:uuid"`
	CreatedBy                     uuid.UUID  `gorm:"column:created_by;type:uuid"`
	Title                         string     `gorm:"column:title"`
	Status                        string     `gorm:"column:status"`
	Visibility                    string     `gorm:"column:visibility"`
	Provider                      string     `gorm:"column:provider"`
	Model                         *string    `gorm:"column:model"`
	ProviderCredentialID          *uuid.UUID `gorm:"column:provider_credential_id;type:uuid"`
	ExecutionTargetID             uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	ProviderResumeCursorEncrypted []byte     `gorm:"column:provider_resume_cursor_encrypted"`
	CurrentRuntimeBindingID       *uuid.UUID `gorm:"column:current_runtime_binding_id;type:uuid"`
	LastEventSequence             int64      `gorm:"column:last_event_sequence"`
	CreatedAt                     time.Time  `gorm:"column:created_at"`
	UpdatedAt                     time.Time  `gorm:"column:updated_at"`
	ArchivedAt                    *time.Time `gorm:"column:archived_at"`
}

func (AgentSession) TableName() string { return "agent_sessions" }

type AgentTurn struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	SessionID       uuid.UUID  `gorm:"column:session_id;type:uuid"`
	CreatedBy       uuid.UUID  `gorm:"column:created_by;type:uuid"`
	Status          string     `gorm:"column:status"`
	InputText       string     `gorm:"column:input_text"`
	RuntimeMode     string     `gorm:"column:runtime_mode;default:full-access"`
	InteractionMode string     `gorm:"column:interaction_mode;default:default"`
	StartedAt       *time.Time `gorm:"column:started_at"`
	CompletedAt     *time.Time `gorm:"column:completed_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
}

func (AgentTurn) TableName() string { return "agent_turns" }

type SessionEvent struct {
	TenantID       uuid.UUID      `gorm:"column:tenant_id;type:uuid;primaryKey"`
	OrganizationID uuid.UUID      `gorm:"column:organization_id;type:uuid"`
	ProjectID      uuid.UUID      `gorm:"column:project_id;type:uuid"`
	SessionID      uuid.UUID      `gorm:"column:session_id;type:uuid;primaryKey"`
	Sequence       int64          `gorm:"column:sequence;primaryKey"`
	EventID        uuid.UUID      `gorm:"column:event_id;type:uuid"`
	EventVersion   int            `gorm:"column:event_version"`
	EventType      string         `gorm:"column:event_type"`
	ActorType      string         `gorm:"column:actor_type"`
	ActorID        *uuid.UUID     `gorm:"column:actor_id;type:uuid"`
	ExecutionID    *uuid.UUID     `gorm:"column:execution_id;type:uuid"`
	WorkerID       *uuid.UUID     `gorm:"column:worker_id;type:uuid"`
	Generation     *int64         `gorm:"column:generation"`
	Payload        map[string]any `gorm:"column:payload;serializer:json"`
	OccurredAt     time.Time      `gorm:"column:occurred_at"`
}

func (SessionEvent) TableName() string { return "session_events" }

type Automation struct {
	ID             uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID uuid.UUID  `gorm:"column:organization_id;type:uuid"`
	ProjectID      uuid.UUID  `gorm:"column:project_id;type:uuid"`
	CreatedBy      uuid.UUID  `gorm:"column:created_by;type:uuid"`
	Name           string     `gorm:"column:name"`
	Prompt         string     `gorm:"column:prompt"`
	Schedule       string     `gorm:"column:schedule"`
	Timezone       string     `gorm:"column:timezone"`
	Status         string     `gorm:"column:status"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
	ArchivedAt     *time.Time `gorm:"column:archived_at"`
}

func (Automation) TableName() string { return "automations" }
