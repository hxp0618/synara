package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionTargetGroup struct {
	ID                        uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                  uuid.UUID      `gorm:"column:tenant_id;type:uuid;not null;uniqueIndex:uq_execution_target_group_tenant_name,priority:1"`
	OrganizationID            *uuid.UUID     `gorm:"column:organization_id;type:uuid"`
	Name                      string         `gorm:"column:name;not null;uniqueIndex:uq_execution_target_group_tenant_name,priority:2"`
	Strategy                  string         `gorm:"column:strategy;not null;default:priority"`
	PreferredRegions          []string       `gorm:"column:preferred_regions;serializer:json"`
	AllowCrossRegion          bool           `gorm:"column:allow_cross_region;not null;default:false"`
	MaxFailoverAttempts       int            `gorm:"column:max_failover_attempts;not null;default:2"`
	HealthMaxStalenessSeconds int            `gorm:"column:health_max_staleness_seconds;not null;default:90"`
	Status                    string         `gorm:"column:status;not null;default:active"`
	Version                   int64          `gorm:"column:version;not null;default:1"`
	CreatedAt                 time.Time      `gorm:"column:created_at;not null"`
	UpdatedAt                 time.Time      `gorm:"column:updated_at;not null"`
	Members                   []TargetMember `gorm:"-"`
}

func (ExecutionTargetGroup) TableName() string { return "execution_target_groups" }

type ExecutionTargetGroupMember struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TenantID          uuid.UUID `gorm:"column:tenant_id;type:uuid;not null"`
	TargetGroupID     uuid.UUID `gorm:"column:target_group_id;type:uuid;not null;uniqueIndex:uq_execution_target_group_member,priority:1"`
	ExecutionTargetID uuid.UUID `gorm:"column:execution_target_id;type:uuid;not null;uniqueIndex:uq_execution_target_group_member,priority:2"`
	Region            string    `gorm:"column:region;not null"`
	ClusterID         string    `gorm:"column:cluster_id;not null"`
	Priority          int       `gorm:"column:priority;not null;default:100"`
	Weight            int       `gorm:"column:weight;not null;default:100"`
	Status            string    `gorm:"column:status;not null;default:active"`
	Version           int64     `gorm:"column:version;not null;default:1"`
	CreatedAt         time.Time `gorm:"column:created_at;not null"`
	UpdatedAt         time.Time `gorm:"column:updated_at;not null"`
}

func (ExecutionTargetGroupMember) TableName() string { return "execution_target_group_members" }

// TargetMember is only a convenience projection used by persistence callers.
// It is excluded from migrations because group membership is stored separately.
type TargetMember struct {
	Member      ExecutionTargetGroupMember
	Target      ExecutionTarget
	Health      *ExecutionTargetHealth
	DRReadiness *ExecutionTargetDRReadiness
}

type ExecutionTargetHealth struct {
	ExecutionTargetID uuid.UUID `gorm:"column:execution_target_id;type:uuid;primaryKey"`
	Status            string    `gorm:"column:status;not null"`
	CapacityStatus    string    `gorm:"column:capacity_status;not null;default:unknown"`
	// AvailableCapacityUnits stores the total schedulable capacity ceiling.
	AvailableCapacityUnits *int      `gorm:"column:available_capacity_units"`
	AllocatedCapacityUnits int       `gorm:"column:allocated_capacity_units;not null;default:0"`
	Source                 string    `gorm:"column:source;not null"`
	Reason                 *string   `gorm:"column:reason"`
	ObservedAt             time.Time `gorm:"column:observed_at;not null"`
	ExpiresAt              time.Time `gorm:"column:expires_at;not null"`
	Version                int64     `gorm:"column:version;not null;default:1"`
	UpdatedAt              time.Time `gorm:"column:updated_at;not null"`
}

func (ExecutionTargetHealth) TableName() string { return "execution_target_health" }

type ExecutionTargetDRReadiness struct {
	ExecutionTargetID   uuid.UUID `gorm:"column:execution_target_id;type:uuid;primaryKey"`
	SourceDRDomain      string    `gorm:"column:source_dr_domain;primaryKey"`
	DRDomain            string    `gorm:"column:dr_domain;not null"`
	ReplicatedThroughAt time.Time `gorm:"column:replicated_through_at;not null"`
	ArtifactsReady      bool      `gorm:"column:artifacts_ready;not null;default:false"`
	CheckpointsReady    bool      `gorm:"column:checkpoints_ready;not null;default:false"`
	MemoryReady         bool      `gorm:"column:memory_ready;not null;default:false"`
	PublisherIdentity   string    `gorm:"column:publisher_identity;not null"`
	Reason              *string   `gorm:"column:reason"`
	ObservedAt          time.Time `gorm:"column:observed_at;not null"`
	ExpiresAt           time.Time `gorm:"column:expires_at;not null"`
	Version             int64     `gorm:"column:version;not null;default:1"`
	UpdatedAt           time.Time `gorm:"column:updated_at;not null"`
}

func (ExecutionTargetDRReadiness) TableName() string { return "execution_target_dr_readiness" }

type ExecutionLocationOutage struct {
	TenantID          uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	Region            string    `gorm:"column:region;primaryKey"`
	ClusterID         string    `gorm:"column:cluster_id;not null;default:'';primaryKey"`
	Status            string    `gorm:"column:status;not null"`
	PublisherIdentity string    `gorm:"column:publisher_identity;not null"`
	Reason            *string   `gorm:"column:reason"`
	ObservedAt        time.Time `gorm:"column:observed_at;not null"`
	ExpiresAt         time.Time `gorm:"column:expires_at;not null"`
	Version           int64     `gorm:"column:version;not null;default:1"`
	UpdatedAt         time.Time `gorm:"column:updated_at;not null"`
}

func (ExecutionLocationOutage) TableName() string { return "execution_location_outages" }

type ExecutionFailoverAttempt struct {
	ID                           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                     uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null"`
	SessionID                    uuid.UUID  `gorm:"column:session_id;type:uuid;not null"`
	TurnID                       uuid.UUID  `gorm:"column:turn_id;type:uuid;not null"`
	TargetGroupID                uuid.UUID  `gorm:"column:target_group_id;type:uuid;not null"`
	SourceExecutionID            uuid.UUID  `gorm:"column:source_execution_id;type:uuid;not null"`
	DestinationExecutionID       *uuid.UUID `gorm:"column:destination_execution_id;type:uuid"`
	SourceExecutionTargetID      uuid.UUID  `gorm:"column:source_execution_target_id;type:uuid;not null"`
	DestinationExecutionTargetID uuid.UUID  `gorm:"column:destination_execution_target_id;type:uuid;not null"`
	SourceGeneration             int64      `gorm:"column:source_generation;not null"`
	SourceRecoveryBundleID       *uuid.UUID `gorm:"column:source_recovery_bundle_id;type:uuid"`
	LeaderFencingToken           int64      `gorm:"column:leader_fencing_token;not null"`
	Reason                       string     `gorm:"column:reason;not null"`
	Status                       string     `gorm:"column:status;not null"`
	RequestedAt                  time.Time  `gorm:"column:requested_at;not null"`
	CompletedAt                  *time.Time `gorm:"column:completed_at"`
	FailureCode                  *string    `gorm:"column:failure_code"`
	FailureMessage               *string    `gorm:"column:failure_message"`
}

func (ExecutionFailoverAttempt) TableName() string { return "execution_failover_attempts" }
