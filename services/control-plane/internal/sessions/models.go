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
	InputText       string `json:"inputText"`
	RuntimeMode     string `json:"runtimeMode"`
	InteractionMode string `json:"interactionMode"`
}

type EventPage struct {
	Items        []Event `json:"items"`
	LastSequence int64   `json:"lastSequence"`
}

type EventAccess struct {
	CanReadInteractionDetails bool
}
