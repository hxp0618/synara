package executions

import (
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const WorkerProtocolVersion = 1

type Worker struct {
	ID                     uuid.UUID      `json:"id"`
	ExecutionTargetID      uuid.UUID      `json:"executionTargetId"`
	TargetKind             string         `json:"targetKind"`
	ClusterID              string         `json:"clusterId"`
	Namespace              string         `json:"namespace"`
	PodName                string         `json:"podName"`
	Version                string         `json:"version"`
	ProtocolVersion        int            `json:"protocolVersion"`
	Capabilities           map[string]any `json:"capabilities"`
	CurrentManifestID      *uuid.UUID     `json:"currentManifestId,omitempty"`
	CompatibilityStatus    string         `json:"compatibilityStatus"`
	CompatibilityReason    *string        `json:"compatibilityReason,omitempty"`
	CompatibilityCheckedAt *time.Time     `json:"compatibilityCheckedAt,omitempty"`
	LeaseSupported         bool           `json:"leaseSupported"`
	FencingSupported       bool           `json:"fencingSupported"`
	Status                 string         `json:"status"`
	RegisteredAt           time.Time      `json:"registeredAt"`
	LastHeartbeatAt        time.Time      `json:"lastHeartbeatAt"`
	DrainingAt             *time.Time     `json:"drainingAt"`
	TerminatedAt           *time.Time     `json:"terminatedAt"`
}

type RegisteredWorker struct {
	Worker Worker `json:"worker"`
	Token  string `json:"token"`
}

type Execution struct {
	ID                       uuid.UUID  `json:"id"`
	TenantID                 uuid.UUID  `json:"tenantId"`
	SessionID                uuid.UUID  `json:"sessionId"`
	TurnID                   uuid.UUID  `json:"turnId"`
	Attempt                  int        `json:"attempt"`
	Status                   string     `json:"status"`
	ExecutionTargetID        uuid.UUID  `json:"executionTargetId"`
	TargetKind               string     `json:"targetKind"`
	Provider                 *string    `json:"provider,omitempty"`
	WorkerID                 *uuid.UUID `json:"workerId"`
	WorkerManifestID         *uuid.UUID `json:"workerManifestId,omitempty"`
	ProviderRuntimeBindingID *uuid.UUID `json:"providerRuntimeBindingId,omitempty"`
	RemoteWorkspaceID        *uuid.UUID `json:"remoteWorkspaceId,omitempty"`
	RestoreCheckpointID      *uuid.UUID `json:"restoreCheckpointId,omitempty"`
	Generation               int64      `json:"generation"`
	RequestedBy              uuid.UUID  `json:"requestedBy"`
	QueuedAt                 time.Time  `json:"queuedAt"`
	StartedAt                *time.Time `json:"startedAt"`
	FinishedAt               *time.Time `json:"finishedAt"`
	FailureCode              *string    `json:"failureCode"`
	FailureMessage           *string    `json:"failureMessage"`
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
	TenantID                       uuid.UUID             `json:"tenantId"`
	OrganizationID                 uuid.UUID             `json:"organizationId"`
	ProjectID                      uuid.UUID             `json:"projectId"`
	SessionID                      uuid.UUID             `json:"sessionId"`
	TurnID                         uuid.UUID             `json:"turnId"`
	SessionTitle                   string                `json:"sessionTitle"`
	Provider                       string                `json:"provider"`
	ProviderRuntimeBindingID       *uuid.UUID            `json:"providerRuntimeBindingId,omitempty"`
	RemoteWorkspaceID              *uuid.UUID            `json:"remoteWorkspaceId,omitempty"`
	RestoreCheckpointID            *uuid.UUID            `json:"restoreCheckpointId,omitempty"`
	RestoreCheckpoint              *WorkspaceCheckpoint  `json:"restoreCheckpoint,omitempty"`
	WorkspaceRepositoryFingerprint *string               `json:"workspaceRepositoryFingerprint,omitempty"`
	WorkerManifestID               *uuid.UUID            `json:"workerManifestId,omitempty"`
	Model                          *string               `json:"model"`
	ProviderCredentialID           *uuid.UUID            `json:"providerCredentialId"`
	GitCredentialID                *uuid.UUID            `json:"gitCredentialId"`
	InputText                      string                `json:"inputText"`
	RuntimeMode                    string                `json:"runtimeMode"`
	InteractionMode                string                `json:"interactionMode"`
	RepositoryURL                  *string               `json:"repositoryUrl"`
	DefaultBranch                  string                `json:"defaultBranch"`
	ConversationHistory            []ConversationMessage `json:"conversationHistory,omitempty"`
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

type WorkspaceReadyInput struct {
	LeaseInput
	RepositoryFingerprint *string    `json:"repositoryFingerprint,omitempty"`
	CurrentBranch         *string    `json:"currentBranch,omitempty"`
	BaseCommit            *string    `json:"baseCommit,omitempty"`
	HeadCommit            *string    `json:"headCommit,omitempty"`
	RestoredCheckpointID  *uuid.UUID `json:"restoredCheckpointId,omitempty"`
}

type WorkspaceFailedInput struct {
	LeaseInput
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

type WorkspaceDirtyInput struct {
	LeaseInput
	CurrentBranch *string `json:"currentBranch,omitempty"`
	HeadCommit    *string `json:"headCommit,omitempty"`
}

type WorkspaceState struct {
	ID                    uuid.UUID  `json:"id"`
	State                 string     `json:"state"`
	RepositoryFingerprint *string    `json:"repositoryFingerprint,omitempty"`
	CurrentBranch         *string    `json:"currentBranch,omitempty"`
	BaseCommit            *string    `json:"baseCommit,omitempty"`
	HeadCommit            *string    `json:"headCommit,omitempty"`
	LastWorkerID          *uuid.UUID `json:"lastWorkerId,omitempty"`
	LastExecutionID       *uuid.UUID `json:"lastExecutionId,omitempty"`
	LastGeneration        *int64     `json:"lastGeneration,omitempty"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

type CreateWorkspaceCheckpointInput struct {
	LeaseInput
	IdempotencyKey string         `json:"idempotencyKey"`
	Strategy       string         `json:"strategy"`
	BaseCommit     *string        `json:"baseCommit,omitempty"`
	HeadCommit     *string        `json:"headCommit,omitempty"`
	CurrentBranch  *string        `json:"currentBranch,omitempty"`
	Manifest       map[string]any `json:"manifest,omitempty"`
	FileCount      *int           `json:"fileCount,omitempty"`
	TotalBytes     *int64         `json:"totalBytes,omitempty"`
	ExpiresAt      *time.Time     `json:"expiresAt,omitempty"`
}

type WorkspaceCheckpointReadyInput struct {
	LeaseInput
	ArtifactID *uuid.UUID `json:"artifactId,omitempty"`
	SHA256     *string    `json:"sha256,omitempty"`
}

type WorkspaceCheckpointFailedInput struct {
	LeaseInput
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

type WorkspaceCheckpoint struct {
	ID             uuid.UUID      `json:"id"`
	WorkspaceID    uuid.UUID      `json:"workspaceId"`
	SessionID      uuid.UUID      `json:"sessionId"`
	TurnID         *uuid.UUID     `json:"turnId,omitempty"`
	ExecutionID    uuid.UUID      `json:"executionId"`
	Generation     int64          `json:"generation"`
	IdempotencyKey string         `json:"idempotencyKey"`
	Strategy       string         `json:"strategy"`
	Status         string         `json:"status"`
	BaseCommit     *string        `json:"baseCommit,omitempty"`
	HeadCommit     *string        `json:"headCommit,omitempty"`
	CurrentBranch  *string        `json:"currentBranch,omitempty"`
	ArtifactID     *uuid.UUID     `json:"artifactId,omitempty"`
	Manifest       map[string]any `json:"manifest"`
	FileCount      *int           `json:"fileCount,omitempty"`
	TotalBytes     *int64         `json:"totalBytes,omitempty"`
	SHA256         *string        `json:"sha256,omitempty"`
	FailureCode    *string        `json:"failureCode,omitempty"`
	FailureMessage *string        `json:"failureMessage,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	ReadyAt        *time.Time     `json:"readyAt,omitempty"`
	FailedAt       *time.Time     `json:"failedAt,omitempty"`
	ExpiresAt      *time.Time     `json:"expiresAt,omitempty"`
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

const (
	RuntimeEventVersionV1       = 1
	RuntimeEventMaxPayloadBytes = 64 << 10
)

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
	ID                  uuid.UUID      `json:"id"`
	ExecutionID         uuid.UUID      `json:"executionId"`
	SessionID           uuid.UUID      `json:"sessionId"`
	TurnID              uuid.UUID      `json:"turnId"`
	WorkerID            uuid.UUID      `json:"workerId"`
	Generation          int64          `json:"generation"`
	Provider            string         `json:"provider"`
	RequestID           string         `json:"requestId"`
	Kind                string         `json:"kind"`
	Status              string         `json:"status"`
	Payload             map[string]any `json:"payload"`
	Resolution          map[string]any `json:"resolution,omitempty"`
	RequestedAt         time.Time      `json:"requestedAt"`
	ExpiresAt           time.Time      `json:"expiresAt"`
	ResolvedAt          *time.Time     `json:"resolvedAt"`
	ResolvedBy          *uuid.UUID     `json:"resolvedBy"`
	ResolutionKind      *string        `json:"resolutionKind,omitempty"`
	ResolutionCommandID *string        `json:"resolutionCommandId,omitempty"`
	DeliveryStatus      string         `json:"deliveryStatus"`
	DeliveryWorkerID    *uuid.UUID     `json:"deliveryWorkerId,omitempty"`
	DeliveryGeneration  *int64         `json:"deliveryGeneration,omitempty"`
	DeliveryAttempts    int            `json:"deliveryAttempts"`
	DeliveryAvailableAt *time.Time     `json:"deliveryAvailableAt,omitempty"`
	DeliveredAt         *time.Time     `json:"deliveredAt,omitempty"`
	AcknowledgedAt      *time.Time     `json:"acknowledgedAt,omitempty"`
	DeliveryError       *string        `json:"deliveryError,omitempty"`
}

type ResolveApprovalInput struct {
	Decision string `json:"decision"`
}

type ResolveUserInputInput struct {
	Answers map[string]any `json:"answers"`
}

type PullInteractionResolutionsInput struct {
	LeaseInput
	Limit int `json:"limit,omitempty"`
}

type InteractionResolutionDelivery struct {
	InteractionID       uuid.UUID      `json:"interactionId"`
	RequestID           string         `json:"requestId"`
	Provider            string         `json:"provider"`
	CommandType         string         `json:"commandType"`
	CommandID           string         `json:"commandId"`
	ResolutionKind      string         `json:"resolutionKind"`
	Resolution          map[string]any `json:"resolution"`
	DeliveryStatus      string         `json:"deliveryStatus"`
	DeliveryAttempts    int            `json:"deliveryAttempts"`
	DeliveryAvailableAt time.Time      `json:"deliveryAvailableAt"`
}

type InteractionResolutionDeliveryInput struct {
	LeaseInput
	ResolutionCommandID string `json:"resolutionCommandId"`
}

type ControlCommand struct {
	ID                  uuid.UUID      `json:"id"`
	ExecutionID         uuid.UUID      `json:"executionId"`
	SessionID           uuid.UUID      `json:"sessionId"`
	TurnID              uuid.UUID      `json:"turnId"`
	Provider            string         `json:"provider"`
	CommandType         string         `json:"commandType"`
	CommandID           string         `json:"commandId"`
	Payload             map[string]any `json:"payload"`
	Status              string         `json:"status"`
	RequestedBy         uuid.UUID      `json:"requestedBy"`
	RequestedAt         time.Time      `json:"requestedAt"`
	DeliveryWorkerID    *uuid.UUID     `json:"deliveryWorkerId,omitempty"`
	DeliveryGeneration  *int64         `json:"deliveryGeneration,omitempty"`
	DeliveryAttempts    int            `json:"deliveryAttempts"`
	DeliveryAvailableAt time.Time      `json:"deliveryAvailableAt"`
	DeliveredAt         *time.Time     `json:"deliveredAt,omitempty"`
	AcknowledgedAt      *time.Time     `json:"acknowledgedAt,omitempty"`
	DeliveryError       *string        `json:"deliveryError,omitempty"`
}

type PullControlCommandsInput struct {
	LeaseInput
	Limit int `json:"limit,omitempty"`
}

type SteerActiveTurnInput struct {
	InputText string `json:"inputText"`
}

type ControlCommandDelivery struct {
	ControlCommandID    uuid.UUID      `json:"controlCommandId"`
	Provider            string         `json:"provider"`
	CommandType         string         `json:"commandType"`
	CommandID           string         `json:"commandId"`
	Payload             map[string]any `json:"payload"`
	DeliveryStatus      string         `json:"deliveryStatus"`
	DeliveryAttempts    int            `json:"deliveryAttempts"`
	DeliveryAvailableAt time.Time      `json:"deliveryAvailableAt"`
}

type ControlCommandDeliveryInput struct {
	LeaseInput
	CommandID            string         `json:"commandId"`
	ProviderResumeCursor *string        `json:"providerResumeCursor,omitempty"`
	Result               map[string]any `json:"result,omitempty"`
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
		CurrentManifestID: model.CurrentManifestID, CompatibilityStatus: model.CompatibilityStatus,
		CompatibilityReason: model.CompatibilityReason, CompatibilityCheckedAt: model.CompatibilityCheckedAt,
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
		ID: model.ID, ExecutionID: model.ExecutionID, SessionID: model.SessionID, TurnID: model.TurnID,
		WorkerID: model.WorkerID, Generation: model.Generation, Provider: model.Provider, RequestID: model.RequestID,
		Kind: model.Kind, Status: model.Status, Payload: payload, Resolution: model.Resolution,
		RequestedAt: model.RequestedAt, ExpiresAt: model.ExpiresAt, ResolvedAt: model.ResolvedAt, ResolvedBy: model.ResolvedBy,
		ResolutionKind: model.ResolutionKind, ResolutionCommandID: model.ResolutionCommandID,
		DeliveryStatus: model.DeliveryStatus, DeliveryWorkerID: model.DeliveryWorkerID,
		DeliveryGeneration: model.DeliveryGeneration, DeliveryAttempts: model.DeliveryAttempts,
		DeliveryAvailableAt: model.DeliveryAvailableAt, DeliveredAt: model.DeliveredAt,
		AcknowledgedAt: model.AcknowledgedAt, DeliveryError: model.DeliveryError,
	}
}

func toControlCommand(model persistence.ExecutionControlCommand) ControlCommand {
	payload := model.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	return ControlCommand{
		ID: model.ID, ExecutionID: model.ExecutionID, SessionID: model.SessionID, TurnID: model.TurnID,
		Provider: model.Provider, CommandType: model.CommandType, CommandID: model.CommandID,
		Payload: payload, Status: model.Status, RequestedBy: model.RequestedBy, RequestedAt: model.RequestedAt,
		DeliveryWorkerID: model.DeliveryWorkerID, DeliveryGeneration: model.DeliveryGeneration,
		DeliveryAttempts: model.DeliveryAttempts, DeliveryAvailableAt: model.DeliveryAvailableAt,
		DeliveredAt: model.DeliveredAt, AcknowledgedAt: model.AcknowledgedAt, DeliveryError: model.DeliveryError,
	}
}

func toExecution(model persistence.AgentExecution) Execution {
	return Execution{
		ID: model.ID, TenantID: model.TenantID, SessionID: model.SessionID, TurnID: model.TurnID,
		Attempt: model.Attempt, Status: model.Status, ExecutionTargetID: model.ExecutionTargetID,
		TargetKind: model.TargetKind, Provider: model.Provider,
		WorkerID: model.WorkerID, WorkerManifestID: model.WorkerManifestID,
		ProviderRuntimeBindingID: model.ProviderRuntimeBindingID, RemoteWorkspaceID: model.RemoteWorkspaceID,
		RestoreCheckpointID: model.RestoreCheckpointID,
		Generation:          model.Generation, RequestedBy: model.RequestedBy,
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
