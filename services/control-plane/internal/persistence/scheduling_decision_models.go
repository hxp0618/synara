package persistence

import (
	"time"

	"github.com/google/uuid"
)

// ExecutionSchedulingDecision is the immutable header for the scheduling
// evidence used to create one AgentExecution. SchedulingDecisionID on the
// execution binds the graph in the other direction so durable consumers can
// freeze the evidence identity without rediscovering it by query.
type ExecutionSchedulingDecision struct {
	ID                        uuid.UUID  `gorm:"column:id;type:uuid;primaryKey;uniqueIndex:uq_execution_scheduling_decisions_tenant_id,priority:2"`
	TenantID                  uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null;uniqueIndex:uq_execution_scheduling_decisions_tenant_id,priority:1;uniqueIndex:uq_execution_scheduling_decisions_execution,priority:1"`
	ExecutionID               uuid.UUID  `gorm:"column:execution_id;type:uuid;not null;uniqueIndex:uq_execution_scheduling_decisions_execution,priority:2"`
	AlgorithmVersion          string     `gorm:"column:algorithm_version;not null"`
	EvidenceCompleteness      string     `gorm:"column:evidence_completeness;not null"`
	Provider                  string     `gorm:"column:provider;not null"`
	DecisionKind              string     `gorm:"column:decision_kind;not null"`
	CandidateCount            int        `gorm:"column:candidate_count;not null"`
	SelectedOrdinal           int        `gorm:"column:selected_ordinal;not null"`
	SelectedExecutionTargetID uuid.UUID  `gorm:"column:selected_execution_target_id;type:uuid;not null"`
	SelectedWorkerPoolID      *uuid.UUID `gorm:"column:selected_worker_pool_id;type:uuid"`
	SelectedRegion            string     `gorm:"column:selected_region;not null;default:''"`
	SelectedClusterID         string     `gorm:"column:selected_cluster_id;not null;default:''"`
	CandidateSetSHA256        string     `gorm:"column:candidate_set_sha256;not null"`
	DecidedAt                 time.Time  `gorm:"column:decided_at;not null"`
}

func (ExecutionSchedulingDecision) TableName() string {
	return "execution_scheduling_decisions"
}

// ExecutionSchedulingCandidate freezes one ordered candidate snapshot. The
// candidate aggregate hash is validated by schedulingdecision before and after
// write. PostgreSQL also enforces count, selection, and the ordered set hash in
// a deferred trigger. SQLite enforces row shape and selected identity in SQL;
// its missing commit-time aggregate trigger is the explicit core boundary.
type ExecutionSchedulingCandidate struct {
	TenantID                 uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	DecisionID               uuid.UUID  `gorm:"column:decision_id;type:uuid;primaryKey"`
	Ordinal                  int        `gorm:"column:ordinal;primaryKey"`
	ExecutionTargetID        uuid.UUID  `gorm:"column:execution_target_id;type:uuid;not null"`
	TargetKind               string     `gorm:"column:target_kind;not null"`
	TargetGroupID            *uuid.UUID `gorm:"column:target_group_id;type:uuid"`
	TargetGroupVersion       *int64     `gorm:"column:target_group_version"`
	TargetGroupMemberID      *uuid.UUID `gorm:"column:target_group_member_id;type:uuid"`
	TargetGroupMemberVersion *int64     `gorm:"column:target_group_member_version"`
	Region                   string     `gorm:"column:region;not null;default:''"`
	ClusterID                string     `gorm:"column:cluster_id;not null;default:''"`
	HealthVersion            *int64     `gorm:"column:health_version"`
	HealthStatus             *string    `gorm:"column:health_status"`
	CapacityStatus           *string    `gorm:"column:capacity_status"`
	HealthObservedAt         *time.Time `gorm:"column:health_observed_at"`
	HealthExpiresAt          *time.Time `gorm:"column:health_expires_at"`
	DRReadinessVersion       *int64     `gorm:"column:dr_readiness_version"`
	SourceDRDomain           *string    `gorm:"column:source_dr_domain"`
	DRDomain                 *string    `gorm:"column:dr_domain"`
	DRReplicatedThroughAt    *time.Time `gorm:"column:dr_replicated_through_at"`
	DRArtifactsReady         *bool      `gorm:"column:dr_artifacts_ready"`
	DRCheckpointsReady       *bool      `gorm:"column:dr_checkpoints_ready"`
	DRMemoryReady            *bool      `gorm:"column:dr_memory_ready"`
	DRObservedAt             *time.Time `gorm:"column:dr_observed_at"`
	DRExpiresAt              *time.Time `gorm:"column:dr_expires_at"`
	AvailableCapacityUnits   *int       `gorm:"column:available_capacity_units"`
	AllocatedCapacityUnits   *int       `gorm:"column:allocated_capacity_units"`
	QueuedExecutionUnits     *int64     `gorm:"column:queued_execution_units"`
	EffectiveLoadRank        *int64     `gorm:"column:effective_load_rank"`
	Priority                 *int       `gorm:"column:priority"`
	Weight                   *int       `gorm:"column:weight"`
	PriorityRank             *int       `gorm:"column:priority_rank"`
	RegionRank               *int       `gorm:"column:region_rank"`
	CapacityRank             *int       `gorm:"column:capacity_rank"`
	WorkerPoolID             *uuid.UUID `gorm:"column:worker_pool_id;type:uuid"`
	WorkerPoolVersion        *int64     `gorm:"column:worker_pool_version"`
	CapacityClass            *string    `gorm:"column:capacity_class"`
	PlacementPolicyVersion   *int64     `gorm:"column:placement_policy_version"`
	Eligibility              string     `gorm:"column:eligibility;not null"`
	RejectionCode            *string    `gorm:"column:rejection_code"`
	Selected                 bool       `gorm:"column:selected;not null"`
	CandidateSHA256          string     `gorm:"column:candidate_sha256;not null"`
}

func (ExecutionSchedulingCandidate) TableName() string {
	return "execution_scheduling_candidates"
}
