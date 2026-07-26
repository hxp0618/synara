package sessions

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingdecision"
	"github.com/synara-ai/synara/services/control-plane/internal/workerreleases"
)

// ScheduledExecution freezes the execution row and the exact selected
// scheduling evidence in the caller's transaction. The first evidence format
// is deliberately selected-only: rejected candidates are not available from
// the current routing and capability-gate APIs and must not be invented.
type ScheduledExecution struct {
	Execution persistence.AgentExecution
	Decision  persistence.ExecutionSchedulingDecision
}

// SchedulingEvidencePayload returns the frozen scheduling evidence keys shared
// by every `execution.queued` outbox message and `turn.created` session event.
// Callers merge operation-specific keys on top; keeping the shared set in one
// place prevents the payload copies from silently diverging.
func (s ScheduledExecution) SchedulingEvidencePayload() map[string]any {
	execution := s.Execution
	decision := s.Decision
	return map[string]any{
		"workerReleaseRevisionId":             execution.WorkerReleaseRevisionID,
		"workerReleaseChannel":                execution.WorkerReleaseChannel,
		"workerPoolId":                        execution.WorkerPoolID,
		"workerPoolVersion":                   execution.WorkerPoolVersion,
		"capacityClass":                       execution.CapacityClass,
		"placementPolicyVersion":              execution.PlacementPolicyVersion,
		"placementRegion":                     execution.PlacementRegion,
		"placementClusterId":                  execution.PlacementClusterID,
		"tenantSchedulingPolicyVersion":       execution.TenantSchedulingPolicyVersion,
		"tenantSchedulingPolicyDigest":        execution.TenantSchedulingPolicyDigest,
		"organizationSchedulingPolicyVersion": execution.OrganizationSchedulingPolicyVersion,
		"organizationSchedulingPolicyDigest":  execution.OrganizationSchedulingPolicyDigest,
		"targetGroupId":                       execution.TargetGroupID,
		"targetGroupVersion":                  execution.TargetGroupVersion,
		"targetGroupMemberVersion":            execution.TargetGroupMemberVersion,
		"selectedRegion":                      execution.SelectedRegion,
		"selectedClusterId":                   execution.SelectedClusterID,
		"routingReason":                       execution.RoutingReason,
		"schedulingDecisionId":                decision.ID,
		"schedulingAlgorithmVersion":          decision.AlgorithmVersion,
		"schedulingEvidenceCompleteness":      decision.EvidenceCompleteness,
		"schedulingCandidateSetSha256":        decision.CandidateSetSHA256,
	}
}

// MergeSchedulingEvidencePayload copies the shared scheduling evidence keys
// into payload and returns it.
func (s ScheduledExecution) MergeSchedulingEvidencePayload(payload map[string]any) map[string]any {
	for key, value := range s.SchedulingEvidencePayload() {
		payload[key] = value
	}
	return payload
}

func CreateScheduledExecution(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	launchTarget ExecutionLaunchTarget,
	decidedAt time.Time,
) (ScheduledExecution, error) {
	if tx == nil || execution.ID == uuid.Nil || execution.TenantID == uuid.Nil || decidedAt.IsZero() {
		return ScheduledExecution{}, problem.New(
			500,
			"execution_scheduling_decision_scope_invalid",
			"Execution scheduling evidence requires a transaction, execution identity, tenant, and decision time.",
		)
	}
	if execution.ExecutionTargetID != launchTarget.Target.ID || execution.TargetKind != launchTarget.Target.Kind {
		return ScheduledExecution{}, problem.New(
			500,
			"execution_scheduling_decision_target_mismatch",
			"Execution scheduling evidence does not match the selected Execution Target.",
		)
	}

	ApplyExecutionLaunchTarget(&execution, launchTarget)
	releaseSelection, err := workerreleases.SelectExecution(ctx, tx, execution.ExecutionTargetID, execution.ID)
	if err != nil {
		return ScheduledExecution{}, err
	}
	if releaseSelection != nil {
		execution.WorkerReleaseRevisionID = &releaseSelection.RevisionID
		execution.WorkerReleaseChannel = &releaseSelection.Channel
	}

	algorithm := schedulingdecision.AlgorithmFixedTargetV1
	candidate := schedulingdecision.CandidateFromExecution(execution)
	if selection := launchTarget.RoutingSelection; selection != nil {
		algorithm = schedulingdecision.AlgorithmQueuePressureV1
		candidate.Region = selection.Member.Region
		candidate.ClusterID = selection.Member.ClusterID
		candidate.TargetGroupMemberID = &selection.Member.ID
		candidate.HealthVersion = &selection.Health.Version
		candidate.HealthStatus = &selection.Health.Status
		candidate.CapacityStatus = &selection.Health.CapacityStatus
		candidate.HealthObservedAt = &selection.Health.ObservedAt
		candidate.HealthExpiresAt = &selection.Health.ExpiresAt
		candidate.AvailableCapacityUnits = selection.Health.AvailableCapacityUnits
		candidate.AllocatedCapacityUnits = &selection.Health.AllocatedCapacityUnits
		candidate.QueuedExecutionUnits = &selection.QueuePressure.QueuedExecutionUnits
		candidate.EffectiveLoadRank = &selection.QueuePressure.EffectiveLoadRank
		candidate.Priority = &selection.Member.Priority
		candidate.Weight = &selection.Member.Weight
		if readiness := selection.DRReadiness; readiness != nil {
			candidate.DRReadinessVersion = &readiness.Version
			candidate.SourceDRDomain = &readiness.SourceDRDomain
			candidate.DRDomain = &readiness.DRDomain
			candidate.DRReplicatedThroughAt = &readiness.ReplicatedThroughAt
			candidate.DRArtifactsReady = &readiness.ArtifactsReady
			candidate.DRCheckpointsReady = &readiness.CheckpointsReady
			candidate.DRMemoryReady = &readiness.MemoryReady
			candidate.DRObservedAt = &readiness.ObservedAt
			candidate.DRExpiresAt = &readiness.ExpiresAt
		}
	}

	input := schedulingdecision.NewSelectedOnlyInput(uuid.New(), algorithm, decidedAt, candidate)
	decision, err := schedulingdecision.CreateExecution(ctx, tx, &execution, input)
	if err != nil {
		return ScheduledExecution{}, problem.Wrap(
			500,
			"execution_scheduling_decision_create_failed",
			"Execution and its immutable scheduling evidence could not be created atomically.",
			err,
		)
	}
	return ScheduledExecution{Execution: execution, Decision: decision}, nil
}
