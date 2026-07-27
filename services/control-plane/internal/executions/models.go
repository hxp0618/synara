package executions

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const (
	WorkerProtocolVersion = 2

	WorkerRegistrationTrustSharedToken          = "shared-token"
	WorkerRegistrationTrustKubernetesPodBoundV1 = "kubernetes-pod-bound-v1"
	WorkerModeExecutionPinned                   = "execution-pinned"
	WorkerModeWarmPool                          = "warm-pool"
	WorkerModeGeneralPool                       = "general-pool"

	ResourceSuspendCompletionWorkerAttestedV1        = "worker-attested-v1"
	ResourceSuspendCompletionKubernetesPodTerminalV1 = "kubernetes-pod-terminal-v1"
)

type Worker struct {
	ID                             uuid.UUID      `json:"id"`
	Incarnation                    int64          `json:"incarnation"`
	InstanceUID                    string         `json:"instanceUid"`
	ExecutionTargetID              uuid.UUID      `json:"executionTargetId"`
	TargetKind                     string         `json:"targetKind"`
	WorkerMode                     string         `json:"workerMode"`
	AssignedExecutionID            *uuid.UUID     `json:"assignedExecutionId,omitempty"`
	WorkerPoolID                   *uuid.UUID     `json:"workerPoolId,omitempty"`
	WorkerPoolVersion              *int64         `json:"workerPoolVersion,omitempty"`
	CapacityClass                  *string        `json:"capacityClass,omitempty"`
	TenantBindingID                *uuid.UUID     `json:"tenantBindingId,omitempty"`
	ClusterID                      string         `json:"clusterId"`
	Namespace                      string         `json:"namespace"`
	PodName                        string         `json:"podName"`
	Version                        string         `json:"version"`
	ProtocolVersion                int            `json:"protocolVersion"`
	Capabilities                   map[string]any `json:"capabilities"`
	CurrentManifestID              *uuid.UUID     `json:"currentManifestId,omitempty"`
	CompatibilityStatus            string         `json:"compatibilityStatus"`
	CompatibilityReason            *string        `json:"compatibilityReason,omitempty"`
	CompatibilityCheckedAt         *time.Time     `json:"compatibilityCheckedAt,omitempty"`
	WorkerReleaseRevisionID        *uuid.UUID     `json:"workerReleaseRevisionId,omitempty"`
	WorkerReleaseChannel           *string        `json:"workerReleaseChannel,omitempty"`
	WorkerReleaseStatus            string         `json:"workerReleaseStatus"`
	WorkerReleaseReason            *string        `json:"workerReleaseReason,omitempty"`
	WorkerReleaseCheckedAt         *time.Time     `json:"workerReleaseCheckedAt,omitempty"`
	LeaseSupported                 bool           `json:"leaseSupported"`
	FencingSupported               bool           `json:"fencingSupported"`
	Status                         string         `json:"status"`
	AdministrativeStatus           string         `json:"administrativeStatus"`
	RegisteredAt                   time.Time      `json:"registeredAt"`
	LastHeartbeatAt                time.Time      `json:"lastHeartbeatAt"`
	DrainingAt                     *time.Time     `json:"drainingAt"`
	ReconciliationDrainIncarnation *int64         `json:"reconciliationDrainIncarnation,omitempty"`
	ReconciliationDrainInstanceUID *string        `json:"reconciliationDrainInstanceUid,omitempty"`
	ReconciliationDrainRequestedAt *time.Time     `json:"reconciliationDrainRequestedAt,omitempty"`
	ReconciliationDrainReason      *string        `json:"reconciliationDrainReason,omitempty"`
	TerminatedAt                   *time.Time     `json:"terminatedAt"`
	RevokedAt                      *time.Time     `json:"revokedAt,omitempty"`
	RevokedBy                      *uuid.UUID     `json:"revokedBy,omitempty"`
	RevocationReason               *string        `json:"revocationReason,omitempty"`
}

type RegisteredWorker struct {
	Worker Worker `json:"worker"`
	Token  string `json:"token"`
}

type ManagedWorker struct {
	ID                             uuid.UUID  `json:"id"`
	Incarnation                    int64      `json:"incarnation"`
	InstanceUID                    string     `json:"instanceUid"`
	ExecutionTargetID              uuid.UUID  `json:"executionTargetId"`
	TargetKind                     string     `json:"targetKind"`
	WorkerMode                     string     `json:"workerMode"`
	AssignedExecutionID            *uuid.UUID `json:"assignedExecutionId,omitempty"`
	WorkerPoolID                   *uuid.UUID `json:"workerPoolId,omitempty"`
	WorkerPoolVersion              *int64     `json:"workerPoolVersion,omitempty"`
	CapacityClass                  *string    `json:"capacityClass,omitempty"`
	TenantBindingID                *uuid.UUID `json:"tenantBindingId,omitempty"`
	ClusterID                      string     `json:"clusterId"`
	Namespace                      string     `json:"namespace"`
	PodName                        string     `json:"podName"`
	Version                        string     `json:"version"`
	ProtocolVersion                int        `json:"protocolVersion"`
	CurrentManifestID              *uuid.UUID `json:"currentManifestId,omitempty"`
	CompatibilityStatus            string     `json:"compatibilityStatus"`
	CompatibilityReason            *string    `json:"compatibilityReason,omitempty"`
	CompatibilityCheckedAt         *time.Time `json:"compatibilityCheckedAt,omitempty"`
	WorkerReleaseRevisionID        *uuid.UUID `json:"workerReleaseRevisionId,omitempty"`
	WorkerReleaseChannel           *string    `json:"workerReleaseChannel,omitempty"`
	WorkerReleaseStatus            string     `json:"workerReleaseStatus"`
	WorkerReleaseReason            *string    `json:"workerReleaseReason,omitempty"`
	WorkerReleaseCheckedAt         *time.Time `json:"workerReleaseCheckedAt,omitempty"`
	LeaseSupported                 bool       `json:"leaseSupported"`
	FencingSupported               bool       `json:"fencingSupported"`
	Status                         string     `json:"status"`
	AdministrativeStatus           string     `json:"administrativeStatus"`
	RegisteredAt                   time.Time  `json:"registeredAt"`
	LastHeartbeatAt                time.Time  `json:"lastHeartbeatAt"`
	DrainingAt                     *time.Time `json:"drainingAt,omitempty"`
	ReconciliationDrainIncarnation *int64     `json:"reconciliationDrainIncarnation,omitempty"`
	ReconciliationDrainInstanceUID *string    `json:"reconciliationDrainInstanceUid,omitempty"`
	ReconciliationDrainRequestedAt *time.Time `json:"reconciliationDrainRequestedAt,omitempty"`
	ReconciliationDrainReason      *string    `json:"reconciliationDrainReason,omitempty"`
	TerminatedAt                   *time.Time `json:"terminatedAt,omitempty"`
	RevokedAt                      *time.Time `json:"revokedAt,omitempty"`
	RevokedBy                      *uuid.UUID `json:"revokedBy,omitempty"`
	RevocationReason               *string    `json:"revocationReason,omitempty"`
}

type RevokeWorkerInput struct {
	ExpectedIncarnation int64  `json:"expectedIncarnation"`
	Reason              string `json:"reason"`
}

type WorkerRevocation struct {
	Worker                          ManagedWorker `json:"worker"`
	ReleasedExecutionLeases         int           `json:"releasedExecutionLeases"`
	RecoveringExecutions            int           `json:"recoveringExecutions"`
	OutcomeUnknownExecutions        int           `json:"outcomeUnknownExecutions"`
	CheckpointUnconfirmedExecutions int           `json:"checkpointUnconfirmedExecutions"`
	RequeuedWorkspaceCleanups       int           `json:"requeuedWorkspaceCleanups"`
}

type Execution struct {
	ID                                  uuid.UUID  `json:"id"`
	TenantID                            uuid.UUID  `json:"tenantId"`
	SessionID                           uuid.UUID  `json:"sessionId"`
	TurnID                              uuid.UUID  `json:"turnId"`
	AutomationID                        *uuid.UUID `json:"automationId,omitempty"`
	QueueClass                          string     `json:"queueClass"`
	QueuePriority                       int        `json:"queuePriority"`
	QuotaUnits                          int        `json:"quotaUnits"`
	Attempt                             int        `json:"attempt"`
	Status                              string     `json:"status"`
	ExecutionTargetID                   uuid.UUID  `json:"executionTargetId"`
	TargetKind                          string     `json:"targetKind"`
	WorkerPoolID                        *uuid.UUID `json:"workerPoolId,omitempty"`
	WorkerPoolVersion                   *int64     `json:"workerPoolVersion,omitempty"`
	CapacityClass                       *string    `json:"capacityClass,omitempty"`
	PlacementPolicyVersion              *int64     `json:"placementPolicyVersion,omitempty"`
	PlacementRegion                     string     `json:"placementRegion"`
	PlacementClusterID                  string     `json:"placementClusterId"`
	TenantSchedulingPolicyVersion       int64      `json:"tenantSchedulingPolicyVersion"`
	TenantSchedulingPolicyDigest        string     `json:"tenantSchedulingPolicyDigest"`
	OrganizationSchedulingPolicyVersion int64      `json:"organizationSchedulingPolicyVersion"`
	OrganizationSchedulingPolicyDigest  string     `json:"organizationSchedulingPolicyDigest"`
	TargetGroupID                       *uuid.UUID `json:"targetGroupId,omitempty"`
	TargetGroupVersion                  *int64     `json:"targetGroupVersion,omitempty"`
	TargetGroupMemberVersion            *int64     `json:"targetGroupMemberVersion,omitempty"`
	SelectedRegion                      *string    `json:"selectedRegion,omitempty"`
	SelectedClusterID                   *string    `json:"selectedClusterId,omitempty"`
	RoutingReason                       *string    `json:"routingReason,omitempty"`
	SchedulingDecisionID                *uuid.UUID `json:"schedulingDecisionId,omitempty"`
	PredecessorExecutionID              *uuid.UUID `json:"predecessorExecutionId,omitempty"`
	Provider                            *string    `json:"provider,omitempty"`
	WorkerID                            *uuid.UUID `json:"workerId"`
	WorkerManifestID                    *uuid.UUID `json:"workerManifestId,omitempty"`
	WorkerReleaseRevisionID             *uuid.UUID `json:"workerReleaseRevisionId,omitempty"`
	WorkerReleaseChannel                *string    `json:"workerReleaseChannel,omitempty"`
	ProviderRuntimeBindingID            *uuid.UUID `json:"providerRuntimeBindingId,omitempty"`
	RemoteWorkspaceID                   *uuid.UUID `json:"remoteWorkspaceId,omitempty"`
	WorkspaceMaterializationID          *uuid.UUID `json:"workspaceMaterializationId,omitempty"`
	RestoreCheckpointID                 *uuid.UUID `json:"restoreCheckpointId,omitempty"`
	Generation                          int64      `json:"generation"`
	RequestedBy                         uuid.UUID  `json:"requestedBy"`
	QueuedAt                            time.Time  `json:"queuedAt"`
	StartedAt                           *time.Time `json:"startedAt"`
	FinishedAt                          *time.Time `json:"finishedAt"`
	FailureCode                         *string    `json:"failureCode"`
	FailureMessage                      *string    `json:"failureMessage"`
}

type Lease struct {
	ExecutionID              uuid.UUID                 `json:"executionId"`
	TenantID                 uuid.UUID                 `json:"tenantId"`
	WorkerID                 uuid.UUID                 `json:"workerId"`
	Generation               int64                     `json:"generation"`
	LeaseToken               string                    `json:"leaseToken,omitempty"`
	AcquiredAt               time.Time                 `json:"acquiredAt"`
	HeartbeatAt              time.Time                 `json:"heartbeatAt"`
	ExpiresAt                time.Time                 `json:"expiresAt"`
	ProviderCredentialAccess *ProviderCredentialAccess `json:"providerCredentialAccess,omitempty"`
}

type ProviderCredentialAccess struct {
	GrantID           uuid.UUID  `json:"grantId"`
	Serial            int64      `json:"serial"`
	Status            string     `json:"status"`
	ActivitySequence  int64      `json:"activitySequence"`
	ActivityAt        time.Time  `json:"activityAt"`
	IssuedAt          time.Time  `json:"issuedAt"`
	RenewedAt         time.Time  `json:"renewedAt"`
	ExpiresAt         time.Time  `json:"expiresAt"`
	RefreshDeadlineAt time.Time  `json:"refreshDeadlineAt"`
	HardExpiresAt     *time.Time `json:"hardExpiresAt,omitempty"`
	RenewAfterAt      *time.Time `json:"renewAfterAt,omitempty"`
}

type ClaimResult struct {
	Execution            *Execution `json:"execution"`
	Lease                *Lease     `json:"lease"`
	Workload             *Workload  `json:"workload"`
	ProviderResumeCursor *string    `json:"providerResumeCursor,omitempty"`
}

type WorkspaceCleanupClaimInput struct {
	ExecutionTargetID uuid.UUID `json:"executionTargetId"`
	TargetKind        string    `json:"targetKind"`
}

type WorkspaceCleanupLease struct {
	CleanupID          uuid.UUID `json:"cleanupId"`
	DispatchGeneration int64     `json:"dispatchGeneration"`
	LeaseToken         string    `json:"leaseToken,omitempty"`
	ExpiresAt          time.Time `json:"expiresAt"`
}

type WorkspaceCleanupClaim struct {
	CleanupID          uuid.UUID             `json:"cleanupId"`
	TenantID           uuid.UUID             `json:"tenantId"`
	OrganizationID     uuid.UUID             `json:"organizationId"`
	ProjectID          uuid.UUID             `json:"projectId"`
	SessionID          uuid.UUID             `json:"sessionId"`
	LogicalWorkspaceID uuid.UUID             `json:"logicalWorkspaceId"`
	MaterializationID  uuid.UUID             `json:"materializationId"`
	IncarnationID      uuid.UUID             `json:"incarnationId"`
	ExecutionTargetID  uuid.UUID             `json:"executionTargetId"`
	TargetKind         string                `json:"targetKind"`
	StorageScope       string                `json:"storageScope"`
	LayoutVersion      int                   `json:"layoutVersion"`
	Reason             string                `json:"reason"`
	DispatchGeneration int64                 `json:"dispatchGeneration"`
	Lease              WorkspaceCleanupLease `json:"lease"`
}

type WorkspaceCleanupLeaseInput struct {
	DispatchGeneration int64  `json:"dispatchGeneration"`
	LeaseToken         string `json:"leaseToken"`
}

type WorkspaceCleanupFailedInput struct {
	WorkspaceCleanupLeaseInput
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
	Retryable    bool   `json:"retryable"`
}

type WorkspaceCleanupState struct {
	CleanupID           uuid.UUID  `json:"cleanupId"`
	MaterializationID   uuid.UUID  `json:"materializationId"`
	Status              string     `json:"status"`
	DispatchGeneration  int64      `json:"dispatchGeneration"`
	DeliveryAttempts    int        `json:"deliveryAttempts"`
	DeliveryAvailableAt time.Time  `json:"deliveryAvailableAt"`
	LeaseExpiresAt      *time.Time `json:"leaseExpiresAt,omitempty"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

type Workload struct {
	TenantID                              uuid.UUID                   `json:"tenantId"`
	OrganizationID                        uuid.UUID                   `json:"organizationId"`
	ProjectID                             uuid.UUID                   `json:"projectId"`
	SessionID                             uuid.UUID                   `json:"sessionId"`
	TurnID                                uuid.UUID                   `json:"turnId"`
	SessionTitle                          string                      `json:"sessionTitle"`
	Provider                              string                      `json:"provider"`
	ProviderRuntimeBindingID              *uuid.UUID                  `json:"providerRuntimeBindingId,omitempty"`
	RemoteWorkspaceID                     *uuid.UUID                  `json:"remoteWorkspaceId,omitempty"`
	WorkspaceMaterializationID            *uuid.UUID                  `json:"workspaceMaterializationId,omitempty"`
	WorkspaceMaterializationIncarnationID *uuid.UUID                  `json:"workspaceMaterializationIncarnationId,omitempty"`
	WorkspaceLayoutVersion                int                         `json:"workspaceLayoutVersion,omitempty"`
	RestoreCheckpointID                   *uuid.UUID                  `json:"restoreCheckpointId,omitempty"`
	RestoreCheckpoint                     *WorkspaceCheckpoint        `json:"restoreCheckpoint,omitempty"`
	WorkspaceRepositoryFingerprint        *string                     `json:"workspaceRepositoryFingerprint,omitempty"`
	WorkerManifestID                      *uuid.UUID                  `json:"workerManifestId,omitempty"`
	Model                                 *string                     `json:"model"`
	ProviderCredentialID                  *uuid.UUID                  `json:"providerCredentialId"`
	ProviderCredentialGrantID             *uuid.UUID                  `json:"providerCredentialGrantId,omitempty"`
	CredentialGrants                      []CredentialGrantDescriptor `json:"credentialGrants,omitempty"`
	InputText                             string                      `json:"inputText"`
	TurnKind                              string                      `json:"turnKind"`
	PrimaryOperation                      *PrimaryOperation           `json:"primaryOperation,omitempty"`
	RuntimeMode                           string                      `json:"runtimeMode"`
	InteractionMode                       string                      `json:"interactionMode"`
	RepositoryURL                         *string                     `json:"repositoryUrl"`
	DefaultBranch                         string                      `json:"defaultBranch"`
	ConversationHistory                   []ConversationMessage       `json:"conversationHistory,omitempty"`
	ResumeSnapshot                        *ResumeSnapshot             `json:"resumeSnapshot,omitempty"`
	MemoryReferences                      []RecoveryMemoryReference   `json:"memoryReferences"`
	RecoveryBundle                        *RecoveryBundle             `json:"recoveryBundle,omitempty"`
}

const RecoveryBundleSchemaVersionV1 = 1

const (
	MaximumRecoveryMemoryReferences    = 64
	MaximumRecoveryMemoryArtifactBytes = 256 << 10
	MaximumRecoveryMemoryTotalBytes    = 1 << 20
)

type RecoveryBundle struct {
	ID                           uuid.UUID                 `json:"id"`
	SchemaVersion                int                       `json:"schemaVersion"`
	ExecutionID                  uuid.UUID                 `json:"executionId"`
	SessionID                    uuid.UUID                 `json:"sessionId"`
	TurnID                       uuid.UUID                 `json:"turnId"`
	Generation                   int64                     `json:"generation"`
	RecoveryReason               string                    `json:"recoveryReason"`
	PreviousBundleID             *uuid.UUID                `json:"previousBundleId,omitempty"`
	AuthoritativeHistorySequence int64                     `json:"authoritativeHistorySequence"`
	Execution                    RecoveryExecutionSnapshot `json:"execution"`
	PayloadSHA256                string                    `json:"payloadSha256"`
	CreatedAt                    time.Time                 `json:"createdAt"`
}

type RecoveryExecutionSnapshot struct {
	ExecutionTargetID                   uuid.UUID  `json:"executionTargetId"`
	TargetKind                          string     `json:"targetKind"`
	PlacementRegion                     string     `json:"placementRegion,omitempty"`
	PlacementClusterID                  string     `json:"placementClusterId,omitempty"`
	TenantSchedulingPolicyVersion       int64      `json:"tenantSchedulingPolicyVersion,omitempty"`
	TenantSchedulingPolicyDigest        string     `json:"tenantSchedulingPolicyDigest,omitempty"`
	OrganizationSchedulingPolicyVersion int64      `json:"organizationSchedulingPolicyVersion,omitempty"`
	OrganizationSchedulingPolicyDigest  string     `json:"organizationSchedulingPolicyDigest,omitempty"`
	TargetGroupID                       *uuid.UUID `json:"targetGroupId,omitempty"`
	TargetGroupVersion                  *int64     `json:"targetGroupVersion,omitempty"`
	TargetGroupMemberVersion            *int64     `json:"targetGroupMemberVersion,omitempty"`
	SelectedRegion                      *string    `json:"selectedRegion,omitempty"`
	SelectedClusterID                   *string    `json:"selectedClusterId,omitempty"`
	RoutingReason                       *string    `json:"routingReason,omitempty"`
	SchedulingDecisionID                *uuid.UUID `json:"schedulingDecisionId,omitempty"`
	PredecessorExecutionID              *uuid.UUID `json:"predecessorExecutionId,omitempty"`
	WorkerManifestID                    *uuid.UUID `json:"workerManifestId,omitempty"`
	WorkerReleaseRevisionID             *uuid.UUID `json:"workerReleaseRevisionId,omitempty"`
	WorkerReleaseChannel                *string    `json:"workerReleaseChannel,omitempty"`
	Provider                            *string    `json:"provider,omitempty"`
	ProviderRuntimeBindingID            *uuid.UUID `json:"providerRuntimeBindingId,omitempty"`
	ProviderCredentialID                *uuid.UUID `json:"providerCredentialId,omitempty"`
	ProviderCredentialVersion           *int       `json:"providerCredentialVersion,omitempty"`
	ProviderResumeStrategy              string     `json:"providerResumeStrategy"`
	RemoteWorkspaceID                   *uuid.UUID `json:"remoteWorkspaceId,omitempty"`
	WorkspaceMaterializationID          *uuid.UUID `json:"workspaceMaterializationId,omitempty"`
	RestoreCheckpointID                 *uuid.UUID `json:"restoreCheckpointId,omitempty"`
}

type RecoveryMemoryReference struct {
	Scope      string    `json:"scope"`
	ScopeID    uuid.UUID `json:"scopeId"`
	HeadID     uuid.UUID `json:"headId"`
	MemoryKey  string    `json:"memoryKey"`
	RevisionID uuid.UUID `json:"revisionId"`
	ArtifactID uuid.UUID `json:"artifactId"`
	SHA256     string    `json:"sha256"`
	MediaType  string    `json:"mediaType"`
	SizeBytes  int64     `json:"sizeBytes"`
}

type PrimaryOperation struct {
	ControlCommandID uuid.UUID      `json:"controlCommandId"`
	Provider         string         `json:"provider"`
	CommandType      string         `json:"commandType"`
	CommandID        string         `json:"commandId"`
	Payload          map[string]any `json:"payload"`
}

type ConversationMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

const ResumeSnapshotVersionV1 = 1

type ResumeSnapshot struct {
	Version                      int                         `json:"version"`
	SessionID                    uuid.UUID                   `json:"sessionId"`
	TurnID                       uuid.UUID                   `json:"turnId"`
	Provider                     string                      `json:"provider"`
	Model                        *string                     `json:"model,omitempty"`
	Messages                     []ResumeMessage             `json:"messages"`
	ToolResults                  []ResumeToolResult          `json:"toolResults"`
	ArtifactReferences           []ResumeArtifactReference   `json:"artifactReferences"`
	Mode                         ResumeMode                  `json:"mode"`
	CompactBoundary              *ResumeCompactBoundary      `json:"compactBoundary,omitempty"`
	PendingInteractions          []ResumePendingInteraction  `json:"pendingInteractions"`
	ResumeRecordedInteractions   []ResumeRecordedInteraction `json:"resumeRecordedInteractions"`
	ActiveTurnCheckpoint         *ResumeActiveTurnCheckpoint `json:"activeTurnCheckpoint,omitempty"`
	Workspace                    *ResumeWorkspaceReference   `json:"workspace,omitempty"`
	SourceSequenceRange          ResumeSequenceRange         `json:"sourceSequenceRange"`
	IncludedSequenceRange        *ResumeSequenceRange        `json:"includedSequenceRange,omitempty"`
	CurrentTurnSequence          int64                       `json:"currentTurnSequence,omitempty"`
	AuthoritativeHistorySequence int64                       `json:"authoritativeHistorySequence"`
	Budget                       ResumeSnapshotBudget        `json:"budget"`
	Truncation                   *ResumeSnapshotTruncation   `json:"truncation,omitempty"`
}

type ResumeActiveTurnCheckpoint struct {
	SuspendAttemptID                   uuid.UUID `json:"suspendAttemptId"`
	SourceGeneration                   int64     `json:"sourceGeneration"`
	BoundaryMeaningfulActivitySequence int64     `json:"boundaryMeaningfulActivitySequence"`
	ActiveCommandID                    string    `json:"activeCommandId"`
	CheckpointHistorySequence          int64     `json:"checkpointHistorySequence"`
	CurrentTurnSequence                int64     `json:"currentTurnSequence"`
	CheckpointProtocol                 string    `json:"checkpointProtocol"`
	ReceiptSHA256                      string    `json:"receiptSha256"`
}

type ResumeSequenceRange struct {
	From    int64 `json:"from"`
	Through int64 `json:"through"`
}

type ResumeMessage struct {
	Role            string `json:"role"`
	Text            string `json:"text"`
	SequenceFrom    int64  `json:"sequenceFrom"`
	SequenceThrough int64  `json:"sequenceThrough"`
}

type ResumeToolResult struct {
	Sequence   int64    `json:"sequence"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title,omitempty"`
	Summary    string   `json:"summary"`
	ToolUseIDs []string `json:"toolUseIds,omitempty"`
}

type ResumeArtifactReference struct {
	Sequence    int64      `json:"sequence"`
	ArtifactID  uuid.UUID  `json:"artifactId"`
	ExecutionID *uuid.UUID `json:"executionId,omitempty"`
	Kind        string     `json:"kind"`
	ContentType *string    `json:"contentType,omitempty"`
	SizeBytes   *int64     `json:"sizeBytes,omitempty"`
	SHA256      *string    `json:"sha256,omitempty"`
}

type ResumeMode struct {
	RuntimeMode     string `json:"runtimeMode"`
	InteractionMode string `json:"interactionMode"`
	Plan            bool   `json:"plan"`
	Review          bool   `json:"review"`
	ReviewSequence  *int64 `json:"reviewSequence,omitempty"`
}

type ResumeCompactBoundary struct {
	Sequence int64  `json:"sequence"`
	Summary  string `json:"summary,omitempty"`
}

type ResumePendingInteraction struct {
	ID           uuid.UUID                   `json:"id"`
	ExecutionID  uuid.UUID                   `json:"executionId"`
	TurnID       uuid.UUID                   `json:"turnId"`
	Provider     string                      `json:"provider"`
	RequestID    string                      `json:"requestId"`
	EventVersion int                         `json:"eventVersion"`
	Kind         string                      `json:"kind"`
	RequestType  string                      `json:"requestType,omitempty"`
	Detail       string                      `json:"detail,omitempty"`
	Questions    []ResumeInteractionQuestion `json:"questions,omitempty"`
	RequestedAt  time.Time                   `json:"requestedAt"`
	ExpiresAt    time.Time                   `json:"expiresAt"`
}

type ResumeRecordedInteraction struct {
	ID             uuid.UUID                   `json:"id"`
	ExecutionID    uuid.UUID                   `json:"executionId"`
	TurnID         uuid.UUID                   `json:"turnId"`
	Provider       string                      `json:"provider"`
	RequestID      string                      `json:"requestId"`
	EventVersion   int                         `json:"eventVersion"`
	Kind           string                      `json:"kind"`
	RequestType    string                      `json:"requestType,omitempty"`
	Detail         string                      `json:"detail,omitempty"`
	Questions      []ResumeInteractionQuestion `json:"questions,omitempty"`
	ResolutionKind string                      `json:"resolutionKind"`
	Resolution     map[string]any              `json:"resolution"`
	RequestedAt    time.Time                   `json:"requestedAt"`
	ResolvedAt     time.Time                   `json:"resolvedAt"`
}

type ResumeInteractionQuestion struct {
	ID          string                    `json:"id"`
	Header      string                    `json:"header"`
	Question    string                    `json:"question"`
	MultiSelect bool                      `json:"multiSelect,omitempty"`
	Options     []ResumeInteractionOption `json:"options"`
}

type ResumeInteractionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type ResumeWorkspaceReference struct {
	WorkspaceID                  uuid.UUID                  `json:"workspaceId"`
	MaterializationID            *uuid.UUID                 `json:"materializationId,omitempty"`
	MaterializationIncarnationID *uuid.UUID                 `json:"materializationIncarnationId,omitempty"`
	LayoutVersion                int                        `json:"layoutVersion,omitempty"`
	RepositoryFingerprint        *string                    `json:"repositoryFingerprint,omitempty"`
	DefaultBranch                string                     `json:"defaultBranch"`
	CurrentBranch                *string                    `json:"currentBranch,omitempty"`
	BaseCommit                   *string                    `json:"baseCommit,omitempty"`
	HeadCommit                   *string                    `json:"headCommit,omitempty"`
	Checkpoint                   *ResumeCheckpointReference `json:"checkpoint,omitempty"`
}

type ResumeCheckpointReference struct {
	CheckpointID  uuid.UUID  `json:"checkpointId"`
	Strategy      string     `json:"strategy"`
	ArtifactID    *uuid.UUID `json:"artifactId,omitempty"`
	BaseCommit    *string    `json:"baseCommit,omitempty"`
	HeadCommit    *string    `json:"headCommit,omitempty"`
	CurrentBranch *string    `json:"currentBranch,omitempty"`
	SHA256        *string    `json:"sha256,omitempty"`
}

type ResumeSnapshotBudget struct {
	ByteLimit       int `json:"byteLimit"`
	TokenLimit      int `json:"tokenLimit"`
	UsedBytes       int `json:"usedBytes"`
	EstimatedTokens int `json:"estimatedTokens"`
}

type ResumeSnapshotTruncation struct {
	Reasons               []string `json:"reasons"`
	DroppedBeforeSequence *int64   `json:"droppedBeforeSequence,omitempty"`
}

type OperationResult[T any] struct {
	Value      T
	Replayed   bool
	StatusCode int
}

type RegisterWorkerInput struct {
	ExecutionTargetID      uuid.UUID      `json:"executionTargetId"`
	TargetKind             string         `json:"targetKind"`
	WorkerMode             string         `json:"workerMode"`
	AssignedExecutionID    *uuid.UUID     `json:"assignedExecutionId,omitempty"`
	WorkerPoolID           *uuid.UUID     `json:"workerPoolId,omitempty"`
	WorkerPoolVersion      *int64         `json:"workerPoolVersion,omitempty"`
	CapacityClass          *string        `json:"capacityClass,omitempty"`
	InstanceUID            string         `json:"instanceUid"`
	SSHBootstrapGeneration *int64         `json:"sshBootstrapGeneration,omitempty"`
	ClusterID              string         `json:"clusterId"`
	Namespace              string         `json:"namespace"`
	PodName                string         `json:"podName"`
	Version                string         `json:"version"`
	ProtocolVersion        int            `json:"protocolVersion"`
	Capabilities           map[string]any `json:"capabilities"`
	LeaseSupported         bool           `json:"leaseSupported"`
	FencingSupported       bool           `json:"fencingSupported"`
	// Requested resources are injected only from the authenticated Kubernetes
	// Pod spec. Worker JSON cannot self-report cost/accounting values.
	RequestedCPUMillicores         *int64 `json:"-"`
	RequestedMemoryBytes           *int64 `json:"-"`
	RequestedEphemeralStorageBytes *int64 `json:"-"`
	// RegistrationTrustMode is assigned by the authenticated HTTP boundary. It
	// is never accepted from Worker JSON.
	RegistrationTrustMode string `json:"-"`
}

type HeartbeatInput struct {
	Version                string         `json:"version"`
	ProtocolVersion        int            `json:"protocolVersion"`
	SSHBootstrapGeneration *int64         `json:"sshBootstrapGeneration,omitempty"`
	Capabilities           map[string]any `json:"capabilities"`
	Draining               *bool          `json:"draining"`
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
	Reason                         string `json:"reason"`
	PreserveInteractionResolutions bool   `json:"preserveInteractionResolutions,omitempty"`
}

type PullResourceDirectiveInput struct {
	LeaseInput
}

type ResourceDirective struct {
	Action               string    `json:"action"`
	Reason               string    `json:"reason,omitempty"`
	CompletionMode       string    `json:"completionMode,omitempty"`
	SuspendAttemptID     uuid.UUID `json:"suspendAttemptId,omitempty"`
	ControlCommandID     uuid.UUID `json:"controlCommandId,omitempty"`
	CommandID            string    `json:"commandId,omitempty"`
	RequestedAt          time.Time `json:"requestedAt"`
	CheckpointDeadlineAt time.Time `json:"checkpointDeadlineAt"`
}

type CompleteResourceSuspendInput struct {
	LeaseInput
	SuspendAttemptID uuid.UUID `json:"suspendAttemptId"`
	CheckpointStatus string    `json:"checkpointStatus"`
}

type MarkResourceSuspendCheckpointReadyInput struct {
	LeaseInput
	SuspendAttemptID uuid.UUID `json:"suspendAttemptId"`
	CheckpointStatus string    `json:"checkpointStatus"`
}

type ResourceSuspendCheckpointReadyReceipt struct {
	SuspendAttemptID  uuid.UUID `json:"suspendAttemptId"`
	CheckpointStatus  string    `json:"checkpointStatus"`
	CheckpointReadyAt time.Time `json:"checkpointReadyAt"`
}

type KubernetesPodTerminalProof struct {
	ExecutionTargetID uuid.UUID `json:"executionTargetId"`
	ExecutionID       uuid.UUID `json:"executionId"`
	Generation        int64     `json:"generation"`
	Namespace         string    `json:"namespace"`
	PodName           string    `json:"podName"`
	PodUID            string    `json:"podUid"`
	Phase             string    `json:"phase"`
	ObservedAt        time.Time `json:"observedAt"`
}

type MarkResourceSuspendQuiescedInput struct {
	LeaseInput
	SuspendAttemptID uuid.UUID `json:"suspendAttemptId"`
}

type ResourceSuspendQuiesceReceipt struct {
	SuspendAttemptID   uuid.UUID `json:"suspendAttemptId"`
	ProviderQuiescedAt time.Time `json:"providerQuiescedAt"`
}

type AbortResourceSuspendInput struct {
	LeaseInput
	SuspendAttemptID uuid.UUID `json:"suspendAttemptId"`
	FailureCode      string    `json:"failureCode"`
	FailureMessage   string    `json:"failureMessage"`
}

const (
	RuntimeEventVersionV1       = 1
	RuntimeEventVersionV2       = 2
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

type PendingInteraction struct {
	ID          uuid.UUID      `json:"id"`
	ExecutionID uuid.UUID      `json:"executionId"`
	TurnID      uuid.UUID      `json:"turnId"`
	Provider    string         `json:"provider"`
	RequestID   string         `json:"requestId"`
	Kind        string         `json:"kind"`
	Payload     map[string]any `json:"payload"`
	RequestedAt time.Time      `json:"requestedAt"`
	ExpiresAt   time.Time      `json:"expiresAt"`
}

type PendingInteractionSnapshot struct {
	Items            []PendingInteraction `json:"items"`
	SnapshotSequence int64                `json:"snapshotSequence"`
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

// PullControlUpdatesInput carries independent limits so the combined pull
// keeps the per-kind bounds the two separate pulls enforced.
type PullControlUpdatesInput struct {
	LeaseInput
	ControlCommandLimit        int `json:"controlCommandLimit,omitempty"`
	InteractionResolutionLimit int `json:"interactionResolutionLimit,omitempty"`
}

type ControlUpdates struct {
	ControlCommands        []ControlCommandDelivery        `json:"controlCommands"`
	InteractionResolutions []InteractionResolutionDelivery `json:"interactionResolutions"`
}

type SteerActiveTurnInput struct {
	InputText string `json:"inputText"`
}

type CompactSessionInput struct {
	ExpectedLastEventSequence *int64 `json:"expectedLastEventSequence"`
}

type ReviewTarget struct {
	Type   string  `json:"type"`
	Branch *string `json:"branch,omitempty"`
}

type StartReviewInput struct {
	ExpectedLastEventSequence *int64       `json:"expectedLastEventSequence"`
	Target                    ReviewTarget `json:"target"`
	RuntimeMode               string       `json:"runtimeMode,omitempty"`
}

type QueuedSessionOperation struct {
	Type           string         `json:"type"`
	Turn           sessions.Turn  `json:"turn"`
	ExecutionID    uuid.UUID      `json:"executionId"`
	ControlCommand ControlCommand `json:"controlCommand"`
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
		ID: model.ID, Incarnation: model.Incarnation, InstanceUID: model.InstanceUID,
		ExecutionTargetID: model.ExecutionTargetID, TargetKind: model.TargetKind,
		WorkerMode:          model.WorkerMode,
		AssignedExecutionID: model.AssignedExecutionID,
		WorkerPoolID:        model.WorkerPoolID,
		WorkerPoolVersion:   model.WorkerPoolVersion,
		CapacityClass:       model.CapacityClass,
		TenantBindingID:     model.TenantBindingID,
		ClusterID:           model.ClusterID,
		Namespace:           model.Namespace, PodName: model.PodName, Version: model.Version,
		ProtocolVersion: model.ProtocolVersion, Capabilities: capabilities, LeaseSupported: model.LeaseSupported,
		CurrentManifestID: model.CurrentManifestID, CompatibilityStatus: model.CompatibilityStatus,
		CompatibilityReason: model.CompatibilityReason, CompatibilityCheckedAt: model.CompatibilityCheckedAt,
		WorkerReleaseRevisionID: model.WorkerReleaseRevisionID, WorkerReleaseChannel: model.WorkerReleaseChannel,
		WorkerReleaseStatus: model.WorkerReleaseStatus, WorkerReleaseReason: model.WorkerReleaseReason,
		WorkerReleaseCheckedAt: model.WorkerReleaseCheckedAt,
		FencingSupported:       model.FencingSupported, Status: model.Status, RegisteredAt: model.RegisteredAt,
		AdministrativeStatus: model.AdministrativeStatus,
		LastHeartbeatAt:      model.LastHeartbeatAt, DrainingAt: model.DrainingAt,
		ReconciliationDrainIncarnation: model.ReconciliationDrainIncarnation,
		ReconciliationDrainInstanceUID: model.ReconciliationDrainInstanceUID,
		ReconciliationDrainRequestedAt: model.ReconciliationDrainRequestedAt,
		ReconciliationDrainReason:      model.ReconciliationDrainReason,
		TerminatedAt:                   model.TerminatedAt, RevokedAt: model.RevokedAt,
		RevokedBy: model.RevokedBy, RevocationReason: model.RevocationReason,
	}
}

func normalizeWorkerMode(value string) (string, error) {
	mode := strings.TrimSpace(value)
	if mode == "" {
		return WorkerModeGeneralPool, nil
	}
	switch mode {
	case WorkerModeExecutionPinned, WorkerModeWarmPool, WorkerModeGeneralPool:
		return mode, nil
	default:
		return "", problem.New(400, "invalid_worker_mode", "workerMode is invalid.")
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

func toPendingInteraction(model persistence.ExecutionInteraction) PendingInteraction {
	payload := model.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	return PendingInteraction{
		ID: model.ID, ExecutionID: model.ExecutionID, TurnID: model.TurnID,
		Provider: model.Provider, RequestID: model.RequestID, Kind: model.Kind,
		Payload: payload, RequestedAt: model.RequestedAt, ExpiresAt: model.ExpiresAt,
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
		AutomationID: model.AutomationID, QueueClass: model.QueueClass,
		QueuePriority: model.QueuePriority, QuotaUnits: model.QuotaUnits,
		Attempt: model.Attempt, Status: model.Status, ExecutionTargetID: model.ExecutionTargetID,
		TargetKind: model.TargetKind, WorkerPoolID: model.WorkerPoolID,
		WorkerPoolVersion: model.WorkerPoolVersion,
		CapacityClass:     model.CapacityClass, PlacementPolicyVersion: model.PlacementPolicyVersion,
		PlacementRegion: model.PlacementRegion, PlacementClusterID: model.PlacementClusterID,
		TenantSchedulingPolicyVersion:       model.TenantSchedulingPolicyVersion,
		TenantSchedulingPolicyDigest:        model.TenantSchedulingPolicyDigest,
		OrganizationSchedulingPolicyVersion: model.OrganizationSchedulingPolicyVersion,
		OrganizationSchedulingPolicyDigest:  model.OrganizationSchedulingPolicyDigest,
		TargetGroupID:                       model.TargetGroupID, TargetGroupVersion: model.TargetGroupVersion,
		TargetGroupMemberVersion: model.TargetGroupMemberVersion, SelectedRegion: model.SelectedRegion,
		SelectedClusterID: model.SelectedClusterID, RoutingReason: model.RoutingReason,
		SchedulingDecisionID:   model.SchedulingDecisionID,
		PredecessorExecutionID: model.PredecessorExecutionID,
		Provider:               model.Provider,
		WorkerID:               model.WorkerID, WorkerManifestID: model.WorkerManifestID,
		WorkerReleaseRevisionID: model.WorkerReleaseRevisionID, WorkerReleaseChannel: model.WorkerReleaseChannel,
		ProviderRuntimeBindingID: model.ProviderRuntimeBindingID, RemoteWorkspaceID: model.RemoteWorkspaceID,
		WorkspaceMaterializationID: model.WorkspaceMaterializationID,
		RestoreCheckpointID:        model.RestoreCheckpointID,
		Generation:                 model.Generation, RequestedBy: model.RequestedBy,
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
