package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerInstance struct {
	ID                     uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	ExecutionTargetID      uuid.UUID      `gorm:"column:execution_target_id;type:uuid"`
	TargetKind             string         `gorm:"column:target_kind"`
	ClusterID              string         `gorm:"column:cluster_id"`
	Namespace              string         `gorm:"column:namespace"`
	PodName                string         `gorm:"column:pod_name"`
	Version                string         `gorm:"column:version"`
	ProtocolVersion        int            `gorm:"column:protocol_version"`
	Capabilities           map[string]any `gorm:"column:capabilities;serializer:json"`
	CurrentManifestID      *uuid.UUID     `gorm:"column:current_manifest_id;type:uuid"`
	CompatibilityStatus    string         `gorm:"column:compatibility_status;default:unknown"`
	CompatibilityReason    *string        `gorm:"column:compatibility_reason"`
	CompatibilityCheckedAt *time.Time     `gorm:"column:compatibility_checked_at"`
	LeaseSupported         bool           `gorm:"column:lease_supported"`
	FencingSupported       bool           `gorm:"column:fencing_supported"`
	AuthTokenHash          []byte         `gorm:"column:auth_token_hash"`
	Status                 string         `gorm:"column:status"`
	RegisteredAt           time.Time      `gorm:"column:registered_at"`
	LastHeartbeatAt        time.Time      `gorm:"column:last_heartbeat_at"`
	DrainingAt             *time.Time     `gorm:"column:draining_at"`
	TerminatedAt           *time.Time     `gorm:"column:terminated_at"`
}

func (WorkerInstance) TableName() string { return "worker_instances" }

type AgentExecution struct {
	ID                       uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                 uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	SessionID                uuid.UUID  `gorm:"column:session_id;type:uuid"`
	TurnID                   uuid.UUID  `gorm:"column:turn_id;type:uuid"`
	Attempt                  int        `gorm:"column:attempt"`
	Status                   string     `gorm:"column:status"`
	ExecutionTargetID        uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	TargetKind               string     `gorm:"column:target_kind"`
	Provider                 *string    `gorm:"column:provider"`
	WorkerID                 *uuid.UUID `gorm:"column:worker_id;type:uuid"`
	WorkerManifestID         *uuid.UUID `gorm:"column:worker_manifest_id;type:uuid"`
	ProviderRuntimeBindingID *uuid.UUID `gorm:"column:provider_runtime_binding_id;type:uuid"`
	RemoteWorkspaceID        *uuid.UUID `gorm:"column:remote_workspace_id;type:uuid"`
	RestoreCheckpointID      *uuid.UUID `gorm:"column:restore_checkpoint_id;type:uuid"`
	Generation               int64      `gorm:"column:generation"`
	RequestedBy              uuid.UUID  `gorm:"column:requested_by;type:uuid"`
	QueuedAt                 time.Time  `gorm:"column:queued_at"`
	StartedAt                *time.Time `gorm:"column:started_at"`
	FinishedAt               *time.Time `gorm:"column:finished_at"`
	FailureCode              *string    `gorm:"column:failure_code"`
	FailureMessage           *string    `gorm:"column:failure_message"`
}

func (AgentExecution) TableName() string { return "agent_executions" }

type WorkerLease struct {
	ExecutionID    uuid.UUID `gorm:"column:execution_id;type:uuid;primaryKey"`
	TenantID       uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	WorkerID       uuid.UUID `gorm:"column:worker_id;type:uuid"`
	Generation     int64     `gorm:"column:generation"`
	LeaseTokenHash []byte    `gorm:"column:lease_token_hash"`
	AcquiredAt     time.Time `gorm:"column:acquired_at"`
	HeartbeatAt    time.Time `gorm:"column:heartbeat_at"`
	ExpiresAt      time.Time `gorm:"column:expires_at"`
}

func (WorkerLease) TableName() string { return "worker_leases" }

type WorkerRequestReceipt struct {
	WorkerID    uuid.UUID      `gorm:"column:worker_id;type:uuid;primaryKey"`
	RequestID   string         `gorm:"column:request_id;primaryKey"`
	Operation   string         `gorm:"column:operation"`
	RequestHash string         `gorm:"column:request_hash"`
	StatusCode  int            `gorm:"column:status_code"`
	Response    map[string]any `gorm:"column:response;serializer:json"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
	ExpiresAt   time.Time      `gorm:"column:expires_at"`
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
