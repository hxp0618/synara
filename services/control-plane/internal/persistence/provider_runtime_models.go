package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerManifest struct {
	ID                    uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	ManifestHash          string         `gorm:"column:manifest_hash;uniqueIndex"`
	WorkerBuildVersion    string         `gorm:"column:worker_build_version"`
	WorkerBuildGitSHA     *string        `gorm:"column:worker_build_git_sha"`
	WorkerProtocolMinimum int            `gorm:"column:worker_protocol_minimum"`
	WorkerProtocolMaximum int            `gorm:"column:worker_protocol_maximum"`
	RuntimeEventMinimum   int            `gorm:"column:runtime_event_minimum"`
	RuntimeEventMaximum   int            `gorm:"column:runtime_event_maximum"`
	OperatingSystem       string         `gorm:"column:operating_system"`
	Architecture          string         `gorm:"column:architecture"`
	ImageDigest           *string        `gorm:"column:image_digest"`
	FeatureFlags          map[string]any `gorm:"column:feature_flags;serializer:json"`
	CreatedAt             time.Time      `gorm:"column:created_at"`
}

func (WorkerManifest) TableName() string { return "worker_manifests" }

type WorkerProviderManifest struct {
	WorkerManifestID         uuid.UUID      `gorm:"column:worker_manifest_id;type:uuid;primaryKey"`
	Provider                 string         `gorm:"column:provider;primaryKey"`
	SupportTier              string         `gorm:"column:support_tier"`
	CompatibilityStatus      string         `gorm:"column:compatibility_status"`
	ProviderHostMajor        int            `gorm:"column:provider_host_protocol_major"`
	ProviderHostMinor        int            `gorm:"column:provider_host_protocol_minor"`
	HostBuildVersion         string         `gorm:"column:host_build_version"`
	AdapterVersion           string         `gorm:"column:adapter_version"`
	ProviderCLIVersion       *string        `gorm:"column:provider_cli_version"`
	MaximumCommandBytes      int            `gorm:"column:maximum_command_bytes"`
	MaximumMessageBytes      int            `gorm:"column:maximum_message_bytes"`
	RuntimeEventMinimum      int            `gorm:"column:runtime_event_minimum"`
	RuntimeEventMaximum      int            `gorm:"column:runtime_event_maximum"`
	CredentialDeliveryModes  []string       `gorm:"column:credential_delivery_modes;serializer:json"`
	ResumeStrategies         []string       `gorm:"column:resume_strategies;serializer:json"`
	CapabilityDescriptorHash string         `gorm:"column:capability_descriptor_hash"`
	Capabilities             map[string]any `gorm:"column:capabilities;serializer:json"`
	IncompatibilityCode      *string        `gorm:"column:incompatibility_code"`
	IncompatibilityMessage   *string        `gorm:"column:incompatibility_message"`
	CheckedAt                time.Time      `gorm:"column:checked_at"`
}

func (WorkerProviderManifest) TableName() string { return "worker_provider_manifests" }

type ProviderRuntimeBinding struct {
	ID                           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                     uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	SessionID                    uuid.UUID  `gorm:"column:session_id;type:uuid"`
	Provider                     string     `gorm:"column:provider"`
	Revision                     int        `gorm:"column:revision"`
	Status                       string     `gorm:"column:status"`
	WorkerManifestID             *uuid.UUID `gorm:"column:worker_manifest_id;type:uuid"`
	CapabilityDescriptorHash     *string    `gorm:"column:capability_descriptor_hash"`
	ProviderHostProtocolMajor    *int       `gorm:"column:provider_host_protocol_major"`
	ProviderHostProtocolMinor    *int       `gorm:"column:provider_host_protocol_minor"`
	AdapterVersion               *string    `gorm:"column:adapter_version"`
	ProviderCLIVersion           *string    `gorm:"column:provider_cli_version"`
	ResumeStrategy               string     `gorm:"column:resume_strategy"`
	CursorCompatibilityKey       *string    `gorm:"column:cursor_compatibility_key"`
	CursorUpdatedAt              *time.Time `gorm:"column:cursor_updated_at"`
	AuthoritativeHistorySequence int64      `gorm:"column:authoritative_history_sequence"`
	LastExecutionID              *uuid.UUID `gorm:"column:last_execution_id;type:uuid"`
	LastGeneration               *int64     `gorm:"column:last_generation"`
	CreatedAt                    time.Time  `gorm:"column:created_at"`
	UpdatedAt                    time.Time  `gorm:"column:updated_at"`
	ReleasedAt                   *time.Time `gorm:"column:released_at"`
}

func (ProviderRuntimeBinding) TableName() string { return "provider_runtime_bindings" }

type RemoteWorkspace struct {
	ID                       uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                 uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID           uuid.UUID  `gorm:"column:organization_id;type:uuid"`
	ProjectID                uuid.UUID  `gorm:"column:project_id;type:uuid"`
	SessionID                uuid.UUID  `gorm:"column:session_id;type:uuid"`
	ExecutionTargetID        uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	WorkspaceMode            string     `gorm:"column:workspace_mode"`
	State                    string     `gorm:"column:state"`
	RepositoryFingerprint    *string    `gorm:"column:repository_fingerprint"`
	DefaultBranch            string     `gorm:"column:default_branch"`
	CurrentBranch            *string    `gorm:"column:current_branch"`
	BaseCommit               *string    `gorm:"column:base_commit"`
	HeadCommit               *string    `gorm:"column:head_commit"`
	LastWorkerID             *uuid.UUID `gorm:"column:last_worker_id;type:uuid"`
	LastExecutionID          *uuid.UUID `gorm:"column:last_execution_id;type:uuid"`
	LastGeneration           *int64     `gorm:"column:last_generation"`
	CurrentCheckpointID      *uuid.UUID `gorm:"column:current_checkpoint_id;type:uuid"`
	CurrentMaterializationID *uuid.UUID `gorm:"column:current_materialization_id;type:uuid"`
	RetentionUntil           *time.Time `gorm:"column:retention_until"`
	LastUsedAt               *time.Time `gorm:"column:last_used_at"`
	CreatedAt                time.Time  `gorm:"column:created_at"`
	UpdatedAt                time.Time  `gorm:"column:updated_at"`
	CleanedAt                *time.Time `gorm:"column:cleaned_at"`
}

func (RemoteWorkspace) TableName() string { return "remote_workspaces" }

type WorkspaceMaterialization struct {
	ID                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	WorkspaceID        uuid.UUID  `gorm:"column:workspace_id;type:uuid"`
	OrganizationID     uuid.UUID  `gorm:"column:organization_id;type:uuid"`
	ProjectID          uuid.UUID  `gorm:"column:project_id;type:uuid"`
	SessionID          uuid.UUID  `gorm:"column:session_id;type:uuid"`
	ExecutionTargetID  uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	TargetKind         string     `gorm:"column:target_kind"`
	StorageScope       string     `gorm:"column:storage_scope"`
	LayoutVersion      int        `gorm:"column:layout_version"`
	IncarnationID      uuid.UUID  `gorm:"column:incarnation_id;type:uuid"`
	WorkerID           *uuid.UUID `gorm:"column:worker_id;type:uuid"`
	WorkerIncarnation  *int64     `gorm:"column:worker_incarnation"`
	WorkerInstanceUID  *string    `gorm:"column:worker_instance_uid"`
	LastExecutionID    *uuid.UUID `gorm:"column:last_execution_id;type:uuid"`
	LastGeneration     *int64     `gorm:"column:last_generation"`
	State              string     `gorm:"column:state"`
	CleanupReason      *string    `gorm:"column:cleanup_reason"`
	CleanupRequestedAt *time.Time `gorm:"column:cleanup_requested_at"`
	FailureCode        *string    `gorm:"column:failure_code"`
	FailureMessage     *string    `gorm:"column:failure_message"`
	FailedAt           *time.Time `gorm:"column:failed_at"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
	CleanedAt          *time.Time `gorm:"column:cleaned_at"`
}

func (WorkspaceMaterialization) TableName() string { return "workspace_materializations" }

type WorkspaceCleanupCommand struct {
	ID                           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                     uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	MaterializationID            uuid.UUID  `gorm:"column:materialization_id;type:uuid"`
	MaterializationIncarnationID uuid.UUID  `gorm:"column:materialization_incarnation_id;type:uuid"`
	WorkspaceID                  uuid.UUID  `gorm:"column:workspace_id;type:uuid"`
	ExecutionTargetID            uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	TargetKind                   string     `gorm:"column:target_kind"`
	StorageScope                 string     `gorm:"column:storage_scope"`
	LayoutVersion                int        `gorm:"column:layout_version"`
	Reason                       string     `gorm:"column:reason"`
	Status                       string     `gorm:"column:status"`
	LeaseTokenHash               []byte     `gorm:"column:lease_token_hash"`
	DispatchGeneration           int64      `gorm:"column:dispatch_generation"`
	DeliveryWorkerID             *uuid.UUID `gorm:"column:delivery_worker_id;type:uuid"`
	DeliveryWorkerIncarnation    *int64     `gorm:"column:delivery_worker_incarnation"`
	DeliveryAttempts             int        `gorm:"column:delivery_attempts"`
	DeliveryAvailableAt          time.Time  `gorm:"column:delivery_available_at"`
	LeaseExpiresAt               *time.Time `gorm:"column:lease_expires_at"`
	RequestedAt                  time.Time  `gorm:"column:requested_at"`
	LeasedAt                     *time.Time `gorm:"column:leased_at"`
	StartedAt                    *time.Time `gorm:"column:started_at"`
	AcknowledgedAt               *time.Time `gorm:"column:acknowledged_at"`
	FailedAt                     *time.Time `gorm:"column:failed_at"`
	SupersededAt                 *time.Time `gorm:"column:superseded_at"`
	LastErrorCode                *string    `gorm:"column:last_error_code"`
	LastErrorMessage             *string    `gorm:"column:last_error_message"`
	CreatedAt                    time.Time  `gorm:"column:created_at"`
	UpdatedAt                    time.Time  `gorm:"column:updated_at"`
}

func (WorkspaceCleanupCommand) TableName() string { return "workspace_cleanup_commands" }

type WorkspaceCheckpoint struct {
	ID             uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID       uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	WorkspaceID    uuid.UUID      `gorm:"column:workspace_id;type:uuid"`
	SessionID      uuid.UUID      `gorm:"column:session_id;type:uuid"`
	TurnID         *uuid.UUID     `gorm:"column:turn_id;type:uuid"`
	ExecutionID    uuid.UUID      `gorm:"column:execution_id;type:uuid"`
	Generation     int64          `gorm:"column:generation"`
	IdempotencyKey string         `gorm:"column:idempotency_key"`
	Strategy       string         `gorm:"column:strategy"`
	Status         string         `gorm:"column:status"`
	BaseCommit     *string        `gorm:"column:base_commit"`
	HeadCommit     *string        `gorm:"column:head_commit"`
	CurrentBranch  *string        `gorm:"column:current_branch"`
	ArtifactID     *uuid.UUID     `gorm:"column:artifact_id;type:uuid"`
	Manifest       map[string]any `gorm:"column:manifest;serializer:json"`
	FileCount      *int           `gorm:"column:file_count"`
	TotalBytes     *int64         `gorm:"column:total_bytes"`
	SHA256         *string        `gorm:"column:sha256"`
	FailureCode    *string        `gorm:"column:failure_code"`
	FailureMessage *string        `gorm:"column:failure_message"`
	CreatedAt      time.Time      `gorm:"column:created_at"`
	ReadyAt        *time.Time     `gorm:"column:ready_at"`
	FailedAt       *time.Time     `gorm:"column:failed_at"`
	ExpiresAt      *time.Time     `gorm:"column:expires_at"`
}

func (WorkspaceCheckpoint) TableName() string { return "workspace_checkpoints" }
