package persistence

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Project struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID  uuid.UUID  `gorm:"column:organization_id;type:uuid"`
	Name            string     `gorm:"column:name"`
	RepositoryURL   *string    `gorm:"column:repository_url"`
	DefaultBranch   string     `gorm:"column:default_branch"`
	GitCredentialID *uuid.UUID `gorm:"column:git_credential_id;type:uuid"`
	Visibility      string     `gorm:"column:visibility"`
	CreatedBy       uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
	ArchivedAt      *time.Time `gorm:"column:archived_at"`
}

func (Project) TableName() string { return "projects" }

type ProjectCostAllocation struct {
	TenantID       uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ProjectID      uuid.UUID `gorm:"column:project_id;type:uuid;primaryKey"`
	CostCenterCode string    `gorm:"column:cost_center_code"`
	DepartmentCode string    `gorm:"column:department_code"`
	Version        int64     `gorm:"column:version;not null;default:1"`
	UpdatedBy      uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (ProjectCostAllocation) TableName() string { return "project_cost_allocations" }

type AgentSession struct {
	ID                                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                              uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID                        uuid.UUID  `gorm:"column:organization_id;type:uuid"`
	ProjectID                             uuid.UUID  `gorm:"column:project_id;type:uuid"`
	CreatedBy                             uuid.UUID  `gorm:"column:created_by;type:uuid"`
	Title                                 string     `gorm:"column:title"`
	Status                                string     `gorm:"column:status"`
	Visibility                            string     `gorm:"column:visibility"`
	Provider                              string     `gorm:"column:provider"`
	Model                                 *string    `gorm:"column:model"`
	ProviderCredentialID                  *uuid.UUID `gorm:"column:provider_credential_id;type:uuid"`
	ExecutionTargetID                     uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	RequestedExecutionTargetID            uuid.UUID  `gorm:"column:requested_execution_target_id;type:uuid"`
	ExecutionTargetGroupID                *uuid.UUID `gorm:"column:execution_target_group_id;type:uuid"`
	RoutingPolicyVersion                  *int64     `gorm:"column:routing_policy_version"`
	PreferredExecutionRegion              *string    `gorm:"column:preferred_execution_region"`
	ProviderResumeCursorEncrypted         []byte     `gorm:"column:provider_resume_cursor_encrypted"`
	ProviderResumeCursorKeyID             *string    `gorm:"column:provider_resume_cursor_key_id"`
	ProviderResumeCursorState             string     `gorm:"column:provider_resume_cursor_state;default:absent"`
	ProviderResumeCursorSourceExecutionID *uuid.UUID `gorm:"column:provider_resume_cursor_source_execution_id;type:uuid"`
	ProviderResumeCursorSourceGeneration  *int64     `gorm:"column:provider_resume_cursor_source_generation"`
	ProviderResumeCursorHistorySequence   *int64     `gorm:"column:provider_resume_cursor_history_sequence"`
	CurrentRuntimeBindingID               *uuid.UUID `gorm:"column:current_runtime_binding_id;type:uuid"`
	ForkSourceSessionID                   *uuid.UUID `gorm:"column:fork_source_session_id;type:uuid"`
	ForkSourceTurnID                      *uuid.UUID `gorm:"column:fork_source_turn_id;type:uuid"`
	ForkSourceEventSequence               *int64     `gorm:"column:fork_source_event_sequence"`
	ForkStrategy                          *string    `gorm:"column:fork_strategy"`
	LastEventSequence                     int64      `gorm:"column:last_event_sequence"`
	MeaningfulActivitySequence            int64      `gorm:"column:meaningful_activity_sequence;not null;default:0"`
	ResourceState                         string     `gorm:"column:resource_state;default:idle"`
	MeaningfulActivityAt                  time.Time  `gorm:"column:meaningful_activity_at"`
	ResourceIdleSince                     *time.Time `gorm:"column:resource_idle_since"`
	AbsoluteExpiresAt                     *time.Time `gorm:"column:absolute_expires_at"`
	WaitingKeepAliveSeconds               int        `gorm:"column:waiting_keep_alive_seconds;default:900"`
	SuspendAfterIdleSeconds               int        `gorm:"column:suspend_after_idle_seconds;default:1800"`
	AbsoluteSessionLifetimeSeconds        *int       `gorm:"column:absolute_session_lifetime_seconds"`
	WorkspaceRetentionDays                int        `gorm:"column:workspace_retention_days;default:30"`
	WarmPoolMode                          string     `gorm:"column:warm_pool_mode;default:disabled"`
	CreatedAt                             time.Time  `gorm:"column:created_at"`
	UpdatedAt                             time.Time  `gorm:"column:updated_at"`
	ArchivedAt                            *time.Time `gorm:"column:archived_at"`
}

func (AgentSession) TableName() string { return "agent_sessions" }

// BeforeCreate keeps direct persistence fixtures and import paths safe while
// allowing old-schema migration tests to exclude the Stage 4 column through an
// explicit Select list.
func (model *AgentSession) BeforeCreate(_ *gorm.DB) error {
	if model.RequestedExecutionTargetID == uuid.Nil {
		model.RequestedExecutionTargetID = model.ExecutionTargetID
	}
	if model.MeaningfulActivityAt.IsZero() {
		model.MeaningfulActivityAt = time.Now().UTC()
	}
	if model.WaitingKeepAliveSeconds == 0 {
		model.WaitingKeepAliveSeconds = 900
	}
	if model.SuspendAfterIdleSeconds == 0 {
		model.SuspendAfterIdleSeconds = 1800
	}
	if model.WorkspaceRetentionDays == 0 {
		model.WorkspaceRetentionDays = 30
	}
	if model.WarmPoolMode == "" {
		model.WarmPoolMode = "disabled"
	}
	return nil
}

type AgentTurn struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	SessionID       uuid.UUID  `gorm:"column:session_id;type:uuid"`
	CreatedBy       uuid.UUID  `gorm:"column:created_by;type:uuid"`
	Status          string     `gorm:"column:status"`
	InputText       string     `gorm:"column:input_text"`
	TurnKind        string     `gorm:"column:turn_kind;default:message"`
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
