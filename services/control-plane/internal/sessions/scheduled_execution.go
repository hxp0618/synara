package sessions

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executionqueue"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingdecision"
	controltracing "github.com/synara-ai/synara/services/control-plane/internal/tracing"
	"github.com/synara-ai/synara/services/control-plane/internal/workerreleases"
)

// ScheduledExecution freezes the execution row and its scheduling evidence in
// the caller's transaction. Routed launches retain the complete bounded
// candidate trace; an explicit fixed Target honestly remains selected-only.
type ScheduledExecution struct {
	Execution         persistence.AgentExecution
	Decision          persistence.ExecutionSchedulingDecision
	CapacityAdmission persistence.ExecutionCapacityAdmission
}

// SchedulingEvidencePayload returns the frozen scheduling evidence keys shared
// by every `execution.queued` outbox message and `turn.created` session event.
// Callers merge operation-specific keys on top; keeping the shared set in one
// place prevents the payload copies from silently diverging.
func (s ScheduledExecution) SchedulingEvidencePayload() map[string]any {
	execution := s.Execution
	decision := s.Decision
	return map[string]any{
		"automationId":                        execution.AutomationID,
		"queueClass":                          execution.QueueClass,
		"queuePriority":                       execution.QueuePriority,
		"quotaUnits":                          execution.QuotaUnits,
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
		"capacityAdmissionMode":               s.CapacityAdmission.AdmissionMode,
		"capacityAdmissionSnapshotSha256":     s.CapacityAdmission.SnapshotSHA256,
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
	queueSnapshot, err := executionqueue.Normalize(executionqueue.Snapshot{
		Class: execution.QueueClass, Priority: execution.QueuePriority,
		QuotaUnits: execution.QuotaUnits, AutomationID: execution.AutomationID,
	})
	if err != nil {
		return ScheduledExecution{}, problem.New(
			500,
			"execution_queue_snapshot_invalid",
			"Execution scheduling requires a valid immutable queue class, priority, Automation identity, and quota-unit snapshot.",
		)
	}
	execution.QueueClass = queueSnapshot.Class
	execution.QueuePriority = queueSnapshot.Priority
	execution.QuotaUnits = queueSnapshot.QuotaUnits
	execution.AutomationID = queueSnapshot.AutomationID
	if execution.Traceparent == nil {
		if traceparent := controltracing.TraceparentFromContext(ctx); traceparent != "" {
			execution.Traceparent = &traceparent
		}
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
	capacityAdmission, err := routing.AdmitExecutionCapacity(
		ctx,
		tx,
		execution.TenantID,
		execution.ID,
		execution.ExecutionTargetID,
		launchTarget.RoutingSelection != nil,
		decidedAt,
	)
	if err != nil {
		return ScheduledExecution{}, err
	}

	algorithm := schedulingdecision.AlgorithmFixedTargetV1
	input := schedulingdecision.NewSelectedOnlyInput(
		uuid.New(),
		algorithm,
		decidedAt,
		schedulingdecision.CandidateFromExecution(execution),
	)
	if selection := launchTarget.RoutingSelection; selection != nil {
		algorithm = schedulingdecision.AlgorithmQueuePressureV1
		if selection.QueuePressure.ReservationAuthorityMode == routing.ReservationAuthorityExactActiveV1 {
			algorithm = schedulingdecision.AlgorithmReservationAwareV1
		}
		if len(launchTarget.CandidateEvidence) == 0 {
			return ScheduledExecution{}, problem.New(
				500,
				"execution_scheduling_candidate_trace_missing",
				"A routed launch completed without its bounded scheduling candidate trace.",
			)
		}
		candidates := make([]schedulingdecision.CandidateSnapshot, 0, len(launchTarget.CandidateEvidence))
		for _, evidence := range launchTarget.CandidateEvidence {
			candidates = append(candidates, schedulingCandidateFromLaunchEvidence(execution, evidence))
		}
		input = schedulingdecision.NewCompleteInput(uuid.New(), algorithm, decidedAt, candidates)
	}

	decision, err := schedulingdecision.CreateExecution(ctx, tx, &execution, input)
	if err != nil {
		return ScheduledExecution{}, problem.Wrap(
			500,
			"execution_scheduling_decision_create_failed",
			"Execution and its immutable scheduling evidence could not be created atomically.",
			err,
		)
	}
	if err := createInitialExecutionGenerationFact(ctx, tx, execution, decidedAt); err != nil {
		return ScheduledExecution{}, err
	}
	if err := routing.CreateCapacityAdmission(ctx, tx, capacityAdmission); err != nil {
		return ScheduledExecution{}, err
	}
	return ScheduledExecution{
		Execution: execution, Decision: decision, CapacityAdmission: capacityAdmission.Evidence,
	}, nil
}

func createInitialExecutionGenerationFact(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	dispatchRequestedAt time.Time,
) error {
	if tx.Dialector.Name() == "sqlite" &&
		!tx.Migrator().HasTable(&persistence.ExecutionGenerationFact{}) {
		return nil
	}
	provider := ""
	if execution.Provider != nil {
		provider = strings.TrimSpace(*execution.Provider)
	}
	warmPoolMode := strings.TrimSpace(execution.WarmPoolModeSnapshot)
	if provider == "" || execution.Generation != 0 ||
		(warmPoolMode != "disabled" && warmPoolMode != "balanced" && warmPoolMode != "low-latency") {
		return problem.New(
			500,
			"execution_generation_fact_initial_scope_invalid",
			"Initial Execution generation observability requires a Provider, Generation zero, and valid warm-pool snapshot.",
		)
	}
	recoveryReason := "initial-claim"
	if execution.NextRecoveryReason != nil {
		switch strings.TrimSpace(*execution.NextRecoveryReason) {
		case "disaster-recovery":
			if execution.PredecessorExecutionID == nil {
				return problem.New(
					500,
					"execution_generation_fact_initial_recovery_invalid",
					"Initial disaster-recovery generation observability requires a predecessor Execution.",
				)
			}
			recoveryReason = "disaster-recovery"
		case "":
		default:
			return problem.New(
				500,
				"execution_generation_fact_initial_recovery_invalid",
				"Initial Execution generation observability received an unsupported recovery reason.",
			)
		}
	}
	warmPoolResult := "pending"
	if warmPoolMode == "disabled" {
		warmPoolResult = "not-requested"
	}
	dispatchCopy := dispatchRequestedAt
	fact := persistence.ExecutionGenerationFact{
		TenantID: execution.TenantID, ExecutionID: execution.ID, Generation: 1,
		SessionID: execution.SessionID, TurnID: execution.TurnID,
		ExecutionTargetID: execution.ExecutionTargetID, TargetKind: execution.TargetKind,
		Provider: provider, RecoveryReason: recoveryReason,
		WarmPoolMode: warmPoolMode, WarmPoolResult: warmPoolResult,
		DispatchRequestedAt: &dispatchCopy,
		CreatedAt:           dispatchRequestedAt, UpdatedAt: dispatchRequestedAt,
	}
	if err := tx.WithContext(ctx).Create(&fact).Error; err != nil {
		return problem.Wrap(
			500,
			"execution_generation_fact_initial_create_failed",
			"Initial Execution generation observability could not be created atomically with scheduling evidence.",
			err,
		)
	}
	return nil
}

func schedulingCandidateFromLaunchEvidence(
	execution persistence.AgentExecution,
	evidence ExecutionLaunchCandidateEvidence,
) schedulingdecision.CandidateSnapshot {
	route := evidence.Routing
	memberID := route.Member.ID
	memberVersion := route.Member.Version
	priority := route.Member.Priority
	weight := route.Member.Weight
	candidate := schedulingdecision.CandidateSnapshot{
		ExecutionTargetID:        route.Target.ID,
		TargetKind:               route.Target.Kind,
		TargetGroupID:            execution.TargetGroupID,
		TargetGroupVersion:       execution.TargetGroupVersion,
		TargetGroupMemberID:      &memberID,
		TargetGroupMemberVersion: &memberVersion,
		Region:                   route.Member.Region,
		ClusterID:                route.Member.ClusterID,
		Priority:                 &priority,
		Weight:                   &weight,
		PriorityRank:             cloneInt(route.PriorityRank),
		RegionRank:               cloneInt(route.RegionRank),
		CapacityRank:             cloneInt(route.CapacityRank),
		Eligibility:              route.Eligibility,
		Selected:                 route.Selected,
	}
	if route.RejectionCode != "" {
		rejectionCode := route.RejectionCode
		candidate.RejectionCode = &rejectionCode
	}
	if health := route.Health; health != nil {
		healthVersion := health.Version
		healthStatus := health.Status
		capacityStatus := health.CapacityStatus
		healthObservedAt := health.ObservedAt
		healthExpiresAt := health.ExpiresAt
		allocatedCapacityUnits := health.AllocatedCapacityUnits
		candidate.HealthVersion = &healthVersion
		candidate.HealthStatus = &healthStatus
		candidate.CapacityStatus = &capacityStatus
		candidate.HealthObservedAt = &healthObservedAt
		candidate.HealthExpiresAt = &healthExpiresAt
		candidate.AvailableCapacityUnits = cloneInt(health.AvailableCapacityUnits)
		candidate.AllocatedCapacityUnits = &allocatedCapacityUnits
	}
	if queuePressure := route.QueuePressure; queuePressure != nil {
		queuedExecutionUnits := queuePressure.QueuedExecutionUnits
		effectiveLoadRank := queuePressure.EffectiveLoadRank
		candidate.QueuedExecutionUnits = &queuedExecutionUnits
		candidate.EffectiveLoadRank = &effectiveLoadRank
	}
	if readiness := route.DRReadiness; readiness != nil {
		readinessVersion := readiness.Version
		sourceDRDomain := readiness.SourceDRDomain
		drDomain := readiness.DRDomain
		replicatedThroughAt := readiness.ReplicatedThroughAt
		artifactsReady := readiness.ArtifactsReady
		checkpointsReady := readiness.CheckpointsReady
		memoryReady := readiness.MemoryReady
		observedAt := readiness.ObservedAt
		expiresAt := readiness.ExpiresAt
		candidate.DRReadinessVersion = &readinessVersion
		candidate.SourceDRDomain = &sourceDRDomain
		candidate.DRDomain = &drDomain
		candidate.DRReplicatedThroughAt = &replicatedThroughAt
		candidate.DRArtifactsReady = &artifactsReady
		candidate.DRCheckpointsReady = &checkpointsReady
		candidate.DRMemoryReady = &memoryReady
		candidate.DRObservedAt = &observedAt
		candidate.DRExpiresAt = &expiresAt
	}
	if selection := evidence.PlacementSelection; selection != nil {
		poolID := selection.Pool.ID
		poolVersion := selection.Pool.Version
		capacityClass := selection.CapacityClass
		policyVersion := selection.PolicyVersion
		candidate.WorkerPoolID = &poolID
		candidate.WorkerPoolVersion = &poolVersion
		candidate.CapacityClass = &capacityClass
		candidate.PlacementPolicyVersion = &policyVersion
	}
	return candidate
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
