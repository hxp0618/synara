package executions

import (
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const WorkerProtocolVersion = 1

type Worker struct {
	ID                uuid.UUID      `json:"id"`
	ExecutionTargetID uuid.UUID      `json:"executionTargetId"`
	TargetKind        string         `json:"targetKind"`
	ClusterID         string         `json:"clusterId"`
	Namespace         string         `json:"namespace"`
	PodName           string         `json:"podName"`
	Version           string         `json:"version"`
	ProtocolVersion   int            `json:"protocolVersion"`
	Capabilities      map[string]any `json:"capabilities"`
	LeaseSupported    bool           `json:"leaseSupported"`
	FencingSupported  bool           `json:"fencingSupported"`
	Status            string         `json:"status"`
	RegisteredAt      time.Time      `json:"registeredAt"`
	LastHeartbeatAt   time.Time      `json:"lastHeartbeatAt"`
	DrainingAt        *time.Time     `json:"drainingAt"`
	TerminatedAt      *time.Time     `json:"terminatedAt"`
}

type RegisteredWorker struct {
	Worker Worker `json:"worker"`
	Token  string `json:"token"`
}

type Execution struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenantId"`
	SessionID         uuid.UUID  `json:"sessionId"`
	TurnID            uuid.UUID  `json:"turnId"`
	Attempt           int        `json:"attempt"`
	Status            string     `json:"status"`
	ExecutionTargetID uuid.UUID  `json:"executionTargetId"`
	TargetKind        string     `json:"targetKind"`
	WorkerID          *uuid.UUID `json:"workerId"`
	Generation        int64      `json:"generation"`
	RequestedBy       uuid.UUID  `json:"requestedBy"`
	QueuedAt          time.Time  `json:"queuedAt"`
	StartedAt         *time.Time `json:"startedAt"`
	FinishedAt        *time.Time `json:"finishedAt"`
	FailureCode       *string    `json:"failureCode"`
	FailureMessage    *string    `json:"failureMessage"`
}

type Lease struct {
	ExecutionID uuid.UUID `json:"executionId"`
	TenantID    uuid.UUID `json:"tenantId"`
	WorkerID    uuid.UUID `json:"workerId"`
	Generation  int64     `json:"generation"`
	LeaseToken  string    `json:"leaseToken,omitempty"`
	AcquiredAt  time.Time `json:"acquiredAt"`
	HeartbeatAt time.Time `json:"heartbeatAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type ClaimResult struct {
	Execution            *Execution `json:"execution"`
	Lease                *Lease     `json:"lease"`
	Workload             *Workload  `json:"workload"`
	ProviderResumeCursor *string    `json:"providerResumeCursor,omitempty"`
}

type Workload struct {
	TenantID             uuid.UUID             `json:"tenantId"`
	OrganizationID       uuid.UUID             `json:"organizationId"`
	ProjectID            uuid.UUID             `json:"projectId"`
	SessionID            uuid.UUID             `json:"sessionId"`
	TurnID               uuid.UUID             `json:"turnId"`
	SessionTitle         string                `json:"sessionTitle"`
	Provider             string                `json:"provider"`
	Model                *string               `json:"model"`
	ProviderCredentialID *uuid.UUID            `json:"providerCredentialId"`
	InputText            string                `json:"inputText"`
	RepositoryURL        *string               `json:"repositoryUrl"`
	DefaultBranch        string                `json:"defaultBranch"`
	ConversationHistory  []ConversationMessage `json:"conversationHistory,omitempty"`
}

type ConversationMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type OperationResult[T any] struct {
	Value      T
	Replayed   bool
	StatusCode int
}

type RegisterWorkerInput struct {
	ExecutionTargetID uuid.UUID      `json:"executionTargetId"`
	TargetKind        string         `json:"targetKind"`
	ClusterID         string         `json:"clusterId"`
	Namespace         string         `json:"namespace"`
	PodName           string         `json:"podName"`
	Version           string         `json:"version"`
	ProtocolVersion   int            `json:"protocolVersion"`
	Capabilities      map[string]any `json:"capabilities"`
	LeaseSupported    bool           `json:"leaseSupported"`
	FencingSupported  bool           `json:"fencingSupported"`
}

type HeartbeatInput struct {
	Version         string         `json:"version"`
	ProtocolVersion int            `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	Draining        *bool          `json:"draining"`
}

type ClaimExecutionInput struct {
	ExecutionTargetID uuid.UUID  `json:"executionTargetId"`
	TargetKind        string     `json:"targetKind"`
	ExecutionID       *uuid.UUID `json:"executionId,omitempty"`
}

type LeaseInput struct {
	TenantID   uuid.UUID `json:"tenantId"`
	Generation int64     `json:"generation"`
	LeaseToken string    `json:"leaseToken"`
}

type RenewLeaseInput struct {
	LeaseInput
	ProviderResumeCursor *string `json:"providerResumeCursor"`
}

type CompleteExecutionInput struct {
	LeaseInput
	ProviderResumeCursor *string        `json:"providerResumeCursor"`
	Output               map[string]any `json:"output"`
}

type FailExecutionInput struct {
	LeaseInput
	FailureCode          string  `json:"failureCode"`
	FailureMessage       string  `json:"failureMessage"`
	ProviderResumeCursor *string `json:"providerResumeCursor"`
}

type ReleaseLeaseInput struct {
	LeaseInput
	Reason string `json:"reason"`
}

type RuntimeEventInput struct {
	LeaseInput
	EventID      uuid.UUID      `json:"eventId"`
	EventVersion int            `json:"eventVersion"`
	EventType    string         `json:"eventType"`
	Payload      map[string]any `json:"payload"`
	OccurredAt   time.Time      `json:"occurredAt"`
}

type RuntimeEventResult struct {
	EventID      uuid.UUID `json:"eventId"`
	SessionID    uuid.UUID `json:"sessionId"`
	Sequence     int64     `json:"sequence"`
	EventVersion int       `json:"eventVersion"`
}

type Interaction struct {
	ID          uuid.UUID      `json:"id"`
	ExecutionID uuid.UUID      `json:"executionId"`
	SessionID   uuid.UUID      `json:"sessionId"`
	WorkerID    uuid.UUID      `json:"workerId"`
	Generation  int64          `json:"generation"`
	RequestID   string         `json:"requestId"`
	Kind        string         `json:"kind"`
	Status      string         `json:"status"`
	Payload     map[string]any `json:"payload"`
	Resolution  map[string]any `json:"resolution,omitempty"`
	RequestedAt time.Time      `json:"requestedAt"`
	ResolvedAt  *time.Time     `json:"resolvedAt"`
	ResolvedBy  *uuid.UUID     `json:"resolvedBy"`
}

type ResolveApprovalInput struct {
	Decision string `json:"decision"`
}

type ResolveUserInputInput struct {
	Answers map[string]any `json:"answers"`
}

func toWorker(model persistence.WorkerInstance) Worker {
	capabilities := model.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	return Worker{
		ID: model.ID, ExecutionTargetID: model.ExecutionTargetID, TargetKind: model.TargetKind,
		ClusterID: model.ClusterID,
		Namespace: model.Namespace, PodName: model.PodName, Version: model.Version,
		ProtocolVersion: model.ProtocolVersion, Capabilities: capabilities, LeaseSupported: model.LeaseSupported,
		FencingSupported: model.FencingSupported, Status: model.Status, RegisteredAt: model.RegisteredAt,
		LastHeartbeatAt: model.LastHeartbeatAt, DrainingAt: model.DrainingAt,
		TerminatedAt: model.TerminatedAt,
	}
}

func toInteraction(model persistence.ExecutionInteraction) Interaction {
	payload := model.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	return Interaction{
		ID: model.ID, ExecutionID: model.ExecutionID, SessionID: model.SessionID,
		WorkerID: model.WorkerID, Generation: model.Generation, RequestID: model.RequestID,
		Kind: model.Kind, Status: model.Status, Payload: payload, Resolution: model.Resolution,
		RequestedAt: model.RequestedAt, ResolvedAt: model.ResolvedAt, ResolvedBy: model.ResolvedBy,
	}
}

func toExecution(model persistence.AgentExecution) Execution {
	return Execution{
		ID: model.ID, TenantID: model.TenantID, SessionID: model.SessionID, TurnID: model.TurnID,
		Attempt: model.Attempt, Status: model.Status, ExecutionTargetID: model.ExecutionTargetID,
		TargetKind: model.TargetKind,
		WorkerID:   model.WorkerID, Generation: model.Generation, RequestedBy: model.RequestedBy,
		QueuedAt: model.QueuedAt, StartedAt: model.StartedAt, FinishedAt: model.FinishedAt,
		FailureCode: model.FailureCode, FailureMessage: model.FailureMessage,
	}
}

func toLease(model persistence.WorkerLease, plainToken string) Lease {
	return Lease{
		ExecutionID: model.ExecutionID, TenantID: model.TenantID, WorkerID: model.WorkerID,
		Generation: model.Generation, LeaseToken: plainToken, AcquiredAt: model.AcquiredAt,
		HeartbeatAt: model.HeartbeatAt, ExpiresAt: model.ExpiresAt,
	}
}
