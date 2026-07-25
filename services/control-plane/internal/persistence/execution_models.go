package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerInstance struct {
	ID                      uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	Incarnation             int64          `gorm:"column:incarnation;default:1"`
	InstanceUID             string         `gorm:"column:instance_uid"`
	ExecutionTargetID       uuid.UUID      `gorm:"column:execution_target_id;type:uuid"`
	TargetKind              string         `gorm:"column:target_kind"`
	WorkerMode              string         `gorm:"column:worker_mode;not null;default:general-pool"`
	AssignedExecutionID     *uuid.UUID     `gorm:"column:assigned_execution_id;type:uuid"`
	WorkerPoolID            *uuid.UUID     `gorm:"column:worker_pool_id;type:uuid"`
	WorkerPoolVersion       *int64         `gorm:"column:worker_pool_version"`
	CapacityClass           *string        `gorm:"column:capacity_class"`
	RegistrationTrustMode   string         `gorm:"column:registration_trust_mode;not null;default:shared-token"`
	ClusterID               string         `gorm:"column:cluster_id"`
	Namespace               string         `gorm:"column:namespace"`
	PodName                 string         `gorm:"column:pod_name"`
	Version                 string         `gorm:"column:version"`
	ProtocolVersion         int            `gorm:"column:protocol_version"`
	Capabilities            map[string]any `gorm:"column:capabilities;serializer:json"`
	CurrentManifestID       *uuid.UUID     `gorm:"column:current_manifest_id;type:uuid"`
	CompatibilityStatus     string         `gorm:"column:compatibility_status;default:unknown"`
	CompatibilityReason     *string        `gorm:"column:compatibility_reason"`
	CompatibilityCheckedAt  *time.Time     `gorm:"column:compatibility_checked_at"`
	WorkerReleaseRevisionID *uuid.UUID     `gorm:"column:worker_release_revision_id;type:uuid"`
	WorkerReleaseChannel    *string        `gorm:"column:worker_release_channel"`
	WorkerReleaseStatus     string         `gorm:"column:worker_release_status;default:unmanaged"`
	WorkerReleaseReason     *string        `gorm:"column:worker_release_reason"`
	WorkerReleaseCheckedAt  *time.Time     `gorm:"column:worker_release_checked_at"`
	LeaseSupported          bool           `gorm:"column:lease_supported"`
	FencingSupported        bool           `gorm:"column:fencing_supported"`
	AuthTokenHash           []byte         `gorm:"column:auth_token_hash"`
	Status                  string         `gorm:"column:status"`
	AdministrativeStatus    string         `gorm:"column:administrative_status;default:active"`
	RegisteredAt            time.Time      `gorm:"column:registered_at"`
	LastHeartbeatAt         time.Time      `gorm:"column:last_heartbeat_at"`
	DrainingAt              *time.Time     `gorm:"column:draining_at"`
	TerminatedAt            *time.Time     `gorm:"column:terminated_at"`
	RevokedAt               *time.Time     `gorm:"column:revoked_at"`
	RevokedBy               *uuid.UUID     `gorm:"column:revoked_by;type:uuid"`
	RevocationReason        *string        `gorm:"column:revocation_reason"`
}

func (WorkerInstance) TableName() string { return "worker_instances" }

type WorkerIdentityTombstone struct {
	ExecutionTargetID uuid.UUID  `gorm:"column:execution_target_id;type:uuid;primaryKey"`
	ClusterID         string     `gorm:"column:cluster_id;primaryKey"`
	Namespace         string     `gorm:"column:namespace;primaryKey"`
	PodName           string     `gorm:"column:pod_name;primaryKey"`
	WorkerID          uuid.UUID  `gorm:"column:worker_id;type:uuid"`
	WorkerIncarnation int64      `gorm:"column:worker_incarnation"`
	RevokedAt         time.Time  `gorm:"column:revoked_at"`
	RevokedBy         *uuid.UUID `gorm:"column:revoked_by;type:uuid"`
	RevocationReason  string     `gorm:"column:revocation_reason"`
}

func (WorkerIdentityTombstone) TableName() string { return "worker_identity_tombstones" }

type AgentExecution struct {
	ID                                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                          uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	SessionID                         uuid.UUID  `gorm:"column:session_id;type:uuid"`
	TurnID                            uuid.UUID  `gorm:"column:turn_id;type:uuid"`
	Attempt                           int        `gorm:"column:attempt"`
	Status                            string     `gorm:"column:status"`
	ExecutionTargetID                 uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	TargetKind                        string     `gorm:"column:target_kind"`
	WorkerPoolID                      *uuid.UUID `gorm:"column:worker_pool_id;type:uuid"`
	WorkerPoolVersion                 *int64     `gorm:"column:worker_pool_version"`
	CapacityClass                     *string    `gorm:"column:capacity_class"`
	PlacementPolicyVersion            *int64     `gorm:"column:placement_policy_version"`
	TargetGroupID                     *uuid.UUID `gorm:"column:target_group_id;type:uuid"`
	TargetGroupVersion                *int64     `gorm:"column:target_group_version"`
	TargetGroupMemberVersion          *int64     `gorm:"column:target_group_member_version"`
	SelectedRegion                    *string    `gorm:"column:selected_region"`
	SelectedClusterID                 *string    `gorm:"column:selected_cluster_id"`
	RoutingReason                     *string    `gorm:"column:routing_reason"`
	PredecessorExecutionID            *uuid.UUID `gorm:"column:predecessor_execution_id;type:uuid"`
	WarmPoolModeSnapshot              string     `gorm:"column:warm_pool_mode_snapshot;default:disabled"`
	Provider                          *string    `gorm:"column:provider"`
	WorkerID                          *uuid.UUID `gorm:"column:worker_id;type:uuid"`
	WorkerManifestID                  *uuid.UUID `gorm:"column:worker_manifest_id;type:uuid"`
	WorkerReleaseRevisionID           *uuid.UUID `gorm:"column:worker_release_revision_id;type:uuid"`
	WorkerReleaseChannel              *string    `gorm:"column:worker_release_channel"`
	ProviderCredentialIDSnapshot      *uuid.UUID `gorm:"column:provider_credential_id_snapshot;type:uuid"`
	ProviderCredentialVersionSnapshot *int       `gorm:"column:provider_credential_version_snapshot"`
	ProviderResumeStrategySnapshot    string     `gorm:"column:provider_resume_strategy_snapshot;default:authoritative-history"`
	ProviderCursorBindingVersion      *int       `gorm:"column:provider_cursor_binding_version"`
	ProviderCursorBindingDigest       []byte     `gorm:"column:provider_cursor_binding_digest"`
	ProviderRuntimeBindingID          *uuid.UUID `gorm:"column:provider_runtime_binding_id;type:uuid"`
	RemoteWorkspaceID                 *uuid.UUID `gorm:"column:remote_workspace_id;type:uuid"`
	WorkspaceMaterializationID        *uuid.UUID `gorm:"column:workspace_materialization_id;type:uuid"`
	RestoreCheckpointID               *uuid.UUID `gorm:"column:restore_checkpoint_id;type:uuid"`
	NextRecoveryReason                *string    `gorm:"column:next_recovery_reason"`
	Generation                        int64      `gorm:"column:generation"`
	RequestedBy                       uuid.UUID  `gorm:"column:requested_by;type:uuid"`
	QueuedAt                          time.Time  `gorm:"column:queued_at"`
	StartedAt                         *time.Time `gorm:"column:started_at"`
	FinishedAt                        *time.Time `gorm:"column:finished_at"`
	FailureCode                       *string    `gorm:"column:failure_code"`
	FailureMessage                    *string    `gorm:"column:failure_message"`
}

func (AgentExecution) TableName() string { return "agent_executions" }

type ExecutionRecoveryBundle struct {
	ID                           uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                     uuid.UUID      `gorm:"column:tenant_id;type:uuid;uniqueIndex:uq_execution_recovery_bundle_generation,priority:1"`
	SessionID                    uuid.UUID      `gorm:"column:session_id;type:uuid"`
	TurnID                       uuid.UUID      `gorm:"column:turn_id;type:uuid"`
	ExecutionID                  uuid.UUID      `gorm:"column:execution_id;type:uuid;uniqueIndex:uq_execution_recovery_bundle_generation,priority:2"`
	Generation                   int64          `gorm:"column:generation;uniqueIndex:uq_execution_recovery_bundle_generation,priority:3"`
	SchemaVersion                int            `gorm:"column:schema_version"`
	RecoveryReason               string         `gorm:"column:recovery_reason"`
	PreviousBundleID             *uuid.UUID     `gorm:"column:previous_bundle_id;type:uuid"`
	AuthoritativeHistorySequence int64          `gorm:"column:authoritative_history_sequence"`
	Payload                      map[string]any `gorm:"column:payload;serializer:json"`
	PayloadSHA256                string         `gorm:"column:payload_sha256"`
	CreatedAt                    time.Time      `gorm:"column:created_at"`
}

func (ExecutionRecoveryBundle) TableName() string { return "execution_recovery_bundles" }

type ExecutionSuspendAttempt struct {
	ID                                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                           uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	SessionID                          uuid.UUID  `gorm:"column:session_id;type:uuid"`
	TurnID                             uuid.UUID  `gorm:"column:turn_id;type:uuid"`
	ExecutionID                        uuid.UUID  `gorm:"column:execution_id;type:uuid"`
	WorkerID                           uuid.UUID  `gorm:"column:worker_id;type:uuid"`
	ExecutionTargetID                  uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	Generation                         int64      `gorm:"column:generation"`
	Reason                             string     `gorm:"column:reason"`
	Status                             string     `gorm:"column:status"`
	CompletionMode                     string     `gorm:"column:completion_mode;not null;default:worker-attested-v1"`
	ControlCommandID                   *uuid.UUID `gorm:"column:control_command_id;type:uuid"`
	BoundaryMeaningfulActivitySequence *int64     `gorm:"column:boundary_meaningful_activity_sequence"`
	ActiveCommandID                    *string    `gorm:"column:active_command_id"`
	CheckpointHistorySequence          *int64     `gorm:"column:checkpoint_history_sequence"`
	CurrentTurnSequence                *int64     `gorm:"column:current_turn_sequence"`
	ProviderRuntimeBindingID           *uuid.UUID `gorm:"column:provider_runtime_binding_id;type:uuid"`
	ProviderCheckpointProtocol         *string    `gorm:"column:provider_checkpoint_protocol"`
	ProviderCursorSourceExecutionID    *uuid.UUID `gorm:"column:provider_cursor_source_execution_id;type:uuid"`
	ProviderCursorSourceGeneration     *int64     `gorm:"column:provider_cursor_source_generation"`
	ProviderCursorHistorySequence      *int64     `gorm:"column:provider_cursor_history_sequence"`
	ProviderCursorBindingVersion       *int       `gorm:"column:provider_cursor_binding_version"`
	ProviderCursorBindingDigest        []byte     `gorm:"column:provider_cursor_binding_digest"`
	ProviderCursorSHA256               []byte     `gorm:"column:provider_cursor_sha256"`
	CheckpointReceiptSHA256            []byte     `gorm:"column:checkpoint_receipt_sha256"`
	WorkerIncarnation                  int64      `gorm:"column:worker_incarnation"`
	WorkerInstanceUID                  string     `gorm:"column:worker_instance_uid"`
	WorkerClusterID                    string     `gorm:"column:worker_cluster_id"`
	WorkerNamespace                    string     `gorm:"column:worker_namespace"`
	WorkerPodName                      string     `gorm:"column:worker_pod_name"`
	RequestedAt                        time.Time  `gorm:"column:requested_at"`
	CheckpointDeadlineAt               time.Time  `gorm:"column:checkpoint_deadline_at"`
	ProviderQuiescedAt                 *time.Time `gorm:"column:provider_quiesced_at"`
	CheckpointStatus                   *string    `gorm:"column:checkpoint_status"`
	CheckpointReadyAt                  *time.Time `gorm:"column:checkpoint_ready_at"`
	PodTerminalObservedAt              *time.Time `gorm:"column:pod_terminal_observed_at"`
	PodTerminalPhase                   *string    `gorm:"column:pod_terminal_phase"`
	ResumeBundleID                     *uuid.UUID `gorm:"column:resume_bundle_id;type:uuid"`
	ResumeGeneration                   *int64     `gorm:"column:resume_generation"`
	ResumeBoundAt                      *time.Time `gorm:"column:resume_bound_at"`
	ResumeOutcomeUnknownAt             *time.Time `gorm:"column:resume_outcome_unknown_at"`
	CompletedAt                        *time.Time `gorm:"column:completed_at"`
	AbortedAt                          *time.Time `gorm:"column:aborted_at"`
	FailureCode                        *string    `gorm:"column:failure_code"`
	FailureMessage                     *string    `gorm:"column:failure_message"`
}

func (ExecutionSuspendAttempt) TableName() string { return "execution_suspend_attempts" }

type WorkerLease struct {
	ExecutionID                         uuid.UUID  `gorm:"column:execution_id;type:uuid;primaryKey"`
	TenantID                            uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	WorkerID                            uuid.UUID  `gorm:"column:worker_id;type:uuid"`
	WorkerIncarnation                   int64      `gorm:"column:worker_incarnation;not null;default:1"`
	WorkerInstanceUID                   string     `gorm:"column:worker_instance_uid;not null;default:''"`
	Generation                          int64      `gorm:"column:generation"`
	ProviderCredentialGrantID           *uuid.UUID `gorm:"column:provider_credential_grant_id;type:uuid"`
	ProviderCredentialAccessSerial      *int64     `gorm:"column:provider_credential_access_serial"`
	ProviderCredentialActivitySequence  *int64     `gorm:"column:provider_credential_activity_sequence"`
	ProviderCredentialActivityAt        *time.Time `gorm:"column:provider_credential_activity_at"`
	ProviderCredentialAccessIssuedAt    *time.Time `gorm:"column:provider_credential_access_issued_at"`
	ProviderCredentialAccessRenewedAt   *time.Time `gorm:"column:provider_credential_access_renewed_at"`
	ProviderCredentialAccessExpiresAt   *time.Time `gorm:"column:provider_credential_access_expires_at"`
	ProviderCredentialRefreshDeadlineAt *time.Time `gorm:"column:provider_credential_refresh_deadline_at"`
	ProviderCredentialHardExpiresAt     *time.Time `gorm:"column:provider_credential_hard_expires_at"`
	LeaseTokenHash                      []byte     `gorm:"column:lease_token_hash"`
	AcquiredAt                          time.Time  `gorm:"column:acquired_at"`
	HeartbeatAt                         time.Time  `gorm:"column:heartbeat_at"`
	ExpiresAt                           time.Time  `gorm:"column:expires_at"`
}

func (WorkerLease) TableName() string { return "worker_leases" }

type WorkerRequestReceipt struct {
	WorkerID          uuid.UUID      `gorm:"column:worker_id;type:uuid;primaryKey"`
	WorkerIncarnation int64          `gorm:"column:worker_incarnation;default:1"`
	RequestID         string         `gorm:"column:request_id;primaryKey"`
	Operation         string         `gorm:"column:operation"`
	RequestHash       string         `gorm:"column:request_hash"`
	StatusCode        int            `gorm:"column:status_code"`
	Response          map[string]any `gorm:"column:response;serializer:json"`
	CreatedAt         time.Time      `gorm:"column:created_at"`
	ExpiresAt         time.Time      `gorm:"column:expires_at"`
}

func (WorkerRequestReceipt) TableName() string { return "worker_request_receipts" }

type ExecutionTarget struct {
	ID                     uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID               *uuid.UUID     `gorm:"column:tenant_id;type:uuid"`
	OrganizationID         *uuid.UUID     `gorm:"column:organization_id;type:uuid"`
	Kind                   string         `gorm:"column:kind"`
	Name                   string         `gorm:"column:name"`
	Status                 string         `gorm:"column:status"`
	ConfigurationEncrypted []byte         `gorm:"column:configuration_encrypted"`
	Capabilities           map[string]any `gorm:"column:capabilities;serializer:json"`
	CreatedAt              time.Time      `gorm:"column:created_at"`
	UpdatedAt              time.Time      `gorm:"column:updated_at"`
}

func (ExecutionTarget) TableName() string { return "execution_targets" }
