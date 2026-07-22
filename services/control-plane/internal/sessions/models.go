package sessions

import (
	"time"

	"github.com/google/uuid"
)

type Session struct {
	ID                   uuid.UUID  `json:"id"`
	TenantID             uuid.UUID  `json:"tenantId"`
	OrganizationID       uuid.UUID  `json:"organizationId"`
	ProjectID            uuid.UUID  `json:"projectId"`
	CreatedBy            uuid.UUID  `json:"createdBy"`
	Title                string     `json:"title"`
	Status               string     `json:"status"`
	Visibility           string     `json:"visibility"`
	Provider             string     `json:"provider"`
	Model                *string    `json:"model"`
	ProviderCredentialID *uuid.UUID `json:"providerCredentialId"`
	ExecutionTargetID    uuid.UUID  `json:"executionTargetId"`
	ForkSourceSessionID  *uuid.UUID `json:"forkSourceSessionId,omitempty"`
	ForkSourceTurnID     *uuid.UUID `json:"forkSourceTurnId,omitempty"`
	ForkSourceSequence   *int64     `json:"forkSourceEventSequence,omitempty"`
	ForkStrategy         *string    `json:"forkStrategy,omitempty"`
	LastEventSequence    int64      `json:"lastEventSequence"`
	CreatedAt            time.Time  `json:"createdAt"`
	UpdatedAt            time.Time  `json:"updatedAt"`
	ArchivedAt           *time.Time `json:"archivedAt"`
}

type Turn struct {
	ID              uuid.UUID  `json:"id"`
	TenantID        uuid.UUID  `json:"tenantId"`
	SessionID       uuid.UUID  `json:"sessionId"`
	CreatedBy       uuid.UUID  `json:"createdBy"`
	Status          string     `json:"status"`
	InputText       string     `json:"inputText"`
	TurnKind        string     `json:"turnKind"`
	RuntimeMode     string     `json:"runtimeMode"`
	InteractionMode string     `json:"interactionMode"`
	StartedAt       *time.Time `json:"startedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
	CreatedAt       time.Time  `json:"createdAt"`
}

type Event struct {
	EventID        uuid.UUID      `json:"eventId"`
	EventVersion   int            `json:"eventVersion"`
	TenantID       uuid.UUID      `json:"tenantId"`
	OrganizationID uuid.UUID      `json:"organizationId"`
	ProjectID      uuid.UUID      `json:"projectId"`
	SessionID      uuid.UUID      `json:"sessionId"`
	ExecutionID    *uuid.UUID     `json:"executionId"`
	WorkerID       *uuid.UUID     `json:"workerId"`
	Generation     *int64         `json:"generation"`
	Sequence       int64          `json:"sequence"`
	EventType      string         `json:"eventType"`
	ActorType      string         `json:"actorType"`
	ActorID        *uuid.UUID     `json:"actorId"`
	Payload        map[string]any `json:"payload"`
	OccurredAt     time.Time      `json:"occurredAt"`
}

type CreateSessionInput struct {
	Title                string     `json:"title"`
	Visibility           string     `json:"visibility"`
	Provider             string     `json:"provider"`
	Model                *string    `json:"model"`
	ProviderCredentialID *uuid.UUID `json:"providerCredentialId"`
	ExecutionTargetID    *uuid.UUID `json:"executionTargetId"`
}

type CreateTurnInput struct {
	InputText          string                       `json:"inputText"`
	RuntimeMode        string                       `json:"runtimeMode"`
	InteractionMode    string                       `json:"interactionMode"`
	SourceProposedPlan *SourceProposedPlanReference `json:"sourceProposedPlan,omitempty"`
}

type SourceProposedPlanReference struct {
	ThreadID string `json:"threadId"`
	PlanID   string `json:"planId"`
}

type SwitchModelInput struct {
	Model                 string  `json:"model"`
	ExpectedModel         *string `json:"expectedModel"`
	ExpectedModelProvided bool    `json:"-"`
}

type RollbackSessionInput struct {
	ExpectedLastEventSequence *int64    `json:"expectedLastEventSequence"`
	FromTurnID                uuid.UUID `json:"fromTurnId"`
}

type RollbackSessionResult struct {
	SessionID                   uuid.UUID `json:"sessionId"`
	EventID                     uuid.UUID `json:"eventId"`
	EventSequence               int64     `json:"eventSequence"`
	FromSessionID               uuid.UUID `json:"fromSessionId"`
	FromTurnID                  uuid.UUID `json:"fromTurnId"`
	FromSequence                int64     `json:"fromSequence"`
	RemovedTurnCount            int       `json:"removedTurnCount"`
	SupportMode                 string    `json:"supportMode"`
	WorkspaceDisposition        string    `json:"workspaceDisposition"`
	ExternalSideEffectsReverted bool      `json:"externalSideEffectsReverted"`
}

type ForkSessionInput struct {
	ExpectedLastEventSequence *int64     `json:"expectedLastEventSequence"`
	Title                     string     `json:"title"`
	Visibility                string     `json:"visibility"`
	ProviderCredentialID      *uuid.UUID `json:"providerCredentialId,omitempty"`
	ExecutionTargetID         *uuid.UUID `json:"executionTargetId,omitempty"`
}

type ForkSessionResult struct {
	Session             Session   `json:"session"`
	SourceSessionID     uuid.UUID `json:"sourceSessionId"`
	SourceEventSequence int64     `json:"sourceEventSequence"`
	SupportMode         string    `json:"supportMode"`
}

type EventPage struct {
	Items        []Event `json:"items"`
	LastSequence int64   `json:"lastSequence"`
}

type EventAccess struct {
	CanReadInteractionDetails bool
}
