// Package schedulingdecision validates, hashes, and atomically persists the
// immutable evidence graph for an AgentExecution.
package schedulingdecision

import (
	"context"
	"crypto/md5" // #nosec G501 -- deterministic UUID material, not a security digest.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const (
	AlgorithmQueuePressureV1    = "queue-pressure-v1"
	AlgorithmReservationAwareV1 = "reservation-aware-v1"
	AlgorithmFixedTargetV1      = "fixed-target-v1"
	AlgorithmLegacySelectedOnly = "legacy-selected-only"

	EvidenceComplete           = "complete"
	EvidenceSelectedOnly       = "selected-only"
	EvidenceLegacySelectedOnly = "legacy-selected-only"

	DecisionKindTargetGroup = "target-group"
	DecisionKindFixedTarget = "fixed-target"

	EligibilityEligible = "eligible"
	EligibilityRejected = "rejected"
)

var (
	ErrInvalidEvidence            = errors.New("invalid scheduling decision evidence")
	ErrSelectedCandidateMismatch  = errors.New("selected scheduling candidate does not match execution")
	ErrSchedulingDecisionConflict = errors.New("scheduling decision already exists")
	ErrPersistEvidence            = errors.New("persist scheduling decision evidence")
)

// Input describes one immutable decision. Candidate slice order is the durable
// ordinal order; callers cannot supply conflicting ordinals or hashes.
type Input struct {
	ID                   uuid.UUID
	AlgorithmVersion     string
	EvidenceCompleteness string
	DecidedAt            time.Time
	Candidates           []CandidateSnapshot
}

// CandidateSnapshot freezes the values observed by a scheduling algorithm.
// Optional pointer fields mean the authority was not observed; zero values in
// non-pointer fields are observed values, not missing evidence.
type CandidateSnapshot struct {
	ExecutionTargetID        uuid.UUID
	TargetKind               string
	TargetGroupID            *uuid.UUID
	TargetGroupVersion       *int64
	TargetGroupMemberID      *uuid.UUID
	TargetGroupMemberVersion *int64
	Region                   string
	ClusterID                string
	HealthVersion            *int64
	HealthStatus             *string
	CapacityStatus           *string
	HealthObservedAt         *time.Time
	HealthExpiresAt          *time.Time
	DRReadinessVersion       *int64
	SourceDRDomain           *string
	DRDomain                 *string
	DRReplicatedThroughAt    *time.Time
	DRArtifactsReady         *bool
	DRCheckpointsReady       *bool
	DRMemoryReady            *bool
	DRObservedAt             *time.Time
	DRExpiresAt              *time.Time
	AvailableCapacityUnits   *int
	AllocatedCapacityUnits   *int
	QueuedExecutionUnits     *int64
	EffectiveLoadRank        *int64
	Priority                 *int
	Weight                   *int
	PriorityRank             *int
	RegionRank               *int
	CapacityRank             *int
	WorkerPoolID             *uuid.UUID
	WorkerPoolVersion        *int64
	CapacityClass            *string
	PlacementPolicyVersion   *int64
	Eligibility              string
	RejectionCode            *string
	Selected                 bool
}

// CandidateFromExecution provides the exact selected snapshot already frozen
// on an AgentExecution. Routed callers should enrich it with the Member,
// Health, and queue-pressure values used by their selection.
func CandidateFromExecution(execution persistence.AgentExecution) CandidateSnapshot {
	region, clusterID := selectedLocation(execution)
	return CandidateSnapshot{
		ExecutionTargetID:        execution.ExecutionTargetID,
		TargetKind:               execution.TargetKind,
		TargetGroupID:            execution.TargetGroupID,
		TargetGroupVersion:       execution.TargetGroupVersion,
		TargetGroupMemberVersion: execution.TargetGroupMemberVersion,
		Region:                   region,
		ClusterID:                clusterID,
		WorkerPoolID:             execution.WorkerPoolID,
		WorkerPoolVersion:        execution.WorkerPoolVersion,
		CapacityClass:            execution.CapacityClass,
		PlacementPolicyVersion:   execution.PlacementPolicyVersion,
		Eligibility:              EligibilityEligible,
		Selected:                 true,
	}
}

// NewSelectedOnlyInput is the honest first-slice runtime builder. It never
// labels newly observed evidence as legacy.
func NewSelectedOnlyInput(
	id uuid.UUID,
	algorithmVersion string,
	decidedAt time.Time,
	selected CandidateSnapshot,
) Input {
	selected.Selected = true
	selected.Eligibility = EligibilityEligible
	selected.RejectionCode = nil
	return Input{
		ID:                   id,
		AlgorithmVersion:     algorithmVersion,
		EvidenceCompleteness: EvidenceSelectedOnly,
		DecidedAt:            decidedAt,
		Candidates:           []CandidateSnapshot{selected},
	}
}

// CreateExecution atomically inserts an already-constructed execution and its
// complete immutable Decision graph. It mutates SchedulingDecisionID only after
// validation and restores the prior value if the transaction fails.
func CreateExecution(
	ctx context.Context,
	db *gorm.DB,
	execution *persistence.AgentExecution,
	input Input,
) (persistence.ExecutionSchedulingDecision, error) {
	if db == nil || execution == nil {
		return persistence.ExecutionSchedulingDecision{}, invalid("database and execution are required")
	}
	decision, candidates, err := prepare(*execution, input, false)
	if err != nil {
		return persistence.ExecutionSchedulingDecision{}, err
	}
	previousDecisionID := execution.SchedulingDecisionID
	execution.SchedulingDecisionID = &decision.ID
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(execution).Error; err != nil {
			return fmt.Errorf("%w: create execution: %v", ErrPersistEvidence, err)
		}
		if err := tx.Create(&decision).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return fmt.Errorf("%w: %v", ErrSchedulingDecisionConflict, err)
			}
			return fmt.Errorf("%w: create decision: %v", ErrPersistEvidence, err)
		}
		if err := tx.Create(&candidates).Error; err != nil {
			return fmt.Errorf("%w: create candidates: %v", ErrPersistEvidence, err)
		}
		return verifyStoredAggregate(tx, decision)
	})
	if err != nil {
		execution.SchedulingDecisionID = previousDecisionID
		return persistence.ExecutionSchedulingDecision{}, err
	}
	return decision, nil
}

// BuildLegacySelectedOnly creates deterministic records for migration-era
// executions. It is intentionally separate from NewSelectedOnlyInput so a new
// runtime decision cannot accidentally claim to be historical evidence.
func BuildLegacySelectedOnly(
	execution persistence.AgentExecution,
) (persistence.ExecutionSchedulingDecision, []persistence.ExecutionSchedulingCandidate, error) {
	seed := execution.TenantID.String() + ":" + execution.ID.String() + ":scheduling-decision:legacy-v1"
	sum := md5.Sum([]byte(seed)) // #nosec G401 -- deterministic UUID material only.
	decisionID := uuid.UUID(sum)
	input := Input{
		ID:                   decisionID,
		AlgorithmVersion:     AlgorithmLegacySelectedOnly,
		EvidenceCompleteness: EvidenceLegacySelectedOnly,
		DecidedAt:            execution.QueuedAt,
		Candidates:           []CandidateSnapshot{CandidateFromExecution(execution)},
	}
	return prepare(execution, input, true)
}

func verifyStoredAggregate(tx *gorm.DB, decision persistence.ExecutionSchedulingDecision) error {
	var stored []persistence.ExecutionSchedulingCandidate
	if err := tx.Where("tenant_id = ? AND decision_id = ?", decision.TenantID, decision.ID).
		Order("ordinal ASC").Find(&stored).Error; err != nil {
		return fmt.Errorf("%w: reload candidates: %v", ErrPersistEvidence, err)
	}
	hashes := make([]string, 0, len(stored))
	selectedCount := 0
	selectedOrdinal := -1
	for ordinal, candidate := range stored {
		if candidate.Ordinal != ordinal || CandidateSHA256(candidate) != candidate.CandidateSHA256 {
			return fmt.Errorf("%w: stored candidate canonical hash changed", ErrPersistEvidence)
		}
		hashes = append(hashes, candidate.CandidateSHA256)
		if candidate.Selected {
			selectedCount++
			selectedOrdinal = ordinal
		}
	}
	if len(stored) != decision.CandidateCount || selectedCount != 1 || selectedOrdinal != decision.SelectedOrdinal ||
		candidateSetSHA256(hashes) != decision.CandidateSetSHA256 {
		return fmt.Errorf("%w: stored candidate aggregate changed", ErrPersistEvidence)
	}
	selected := stored[selectedOrdinal]
	if selected.ExecutionTargetID != decision.SelectedExecutionTargetID ||
		!uuidPtrEqual(selected.WorkerPoolID, decision.SelectedWorkerPoolID) ||
		selected.Region != decision.SelectedRegion || selected.ClusterID != decision.SelectedClusterID {
		return fmt.Errorf("%w: stored selected candidate changed", ErrPersistEvidence)
	}
	return nil
}

func prepare(
	execution persistence.AgentExecution,
	input Input,
	legacy bool,
) (persistence.ExecutionSchedulingDecision, []persistence.ExecutionSchedulingCandidate, error) {
	if execution.ID == uuid.Nil || execution.TenantID == uuid.Nil || input.ID == uuid.Nil {
		return persistence.ExecutionSchedulingDecision{}, nil, invalid("execution, tenant, and decision IDs are required")
	}
	if execution.SchedulingDecisionID != nil && *execution.SchedulingDecisionID != input.ID {
		return persistence.ExecutionSchedulingDecision{}, nil, invalid("execution carries a different Scheduling Decision identity")
	}
	if input.DecidedAt.IsZero() {
		return persistence.ExecutionSchedulingDecision{}, nil, invalid("decidedAt is required")
	}
	if len(input.Candidates) == 0 || len(input.Candidates) > 4096 {
		return persistence.ExecutionSchedulingDecision{}, nil, invalid("candidate count must be between 1 and 4096")
	}
	if err := validateCompleteness(input, legacy); err != nil {
		return persistence.ExecutionSchedulingDecision{}, nil, err
	}
	decisionKind := DecisionKindFixedTarget
	if execution.TargetGroupID != nil {
		decisionKind = DecisionKindTargetGroup
		if !legacy && (execution.SelectedRegion == nil || execution.SelectedClusterID == nil) {
			return persistence.ExecutionSchedulingDecision{}, nil, invalid("routed execution selected location is required")
		}
	}
	if err := validateAlgorithm(input.AlgorithmVersion, decisionKind, legacy); err != nil {
		return persistence.ExecutionSchedulingDecision{}, nil, err
	}
	provider := "unknown"
	if execution.Provider != nil {
		provider = *execution.Provider
	}
	if !legacy && (execution.Provider == nil || !boundedToken(provider, 80)) {
		return persistence.ExecutionSchedulingDecision{}, nil, invalid("execution provider is required")
	}
	if legacy && !boundedToken(provider, 80) {
		provider = "unknown"
	}
	candidates := make([]persistence.ExecutionSchedulingCandidate, 0, len(input.Candidates))
	hashes := make([]string, 0, len(input.Candidates))
	selectedOrdinal := -1
	for ordinal, snapshot := range input.Candidates {
		if err := validateCandidate(snapshot, input.AlgorithmVersion, legacy); err != nil {
			return persistence.ExecutionSchedulingDecision{}, nil, fmt.Errorf("%w: candidate %d: %v", ErrInvalidEvidence, ordinal, err)
		}
		candidate := persistenceCandidate(execution.TenantID, input.ID, ordinal, snapshot)
		candidate.CandidateSHA256 = CandidateSHA256(candidate)
		if snapshot.Selected {
			if selectedOrdinal >= 0 {
				return persistence.ExecutionSchedulingDecision{}, nil, invalid("exactly one candidate must be selected")
			}
			selectedOrdinal = ordinal
		}
		candidates = append(candidates, candidate)
		hashes = append(hashes, candidate.CandidateSHA256)
	}
	if selectedOrdinal < 0 {
		return persistence.ExecutionSchedulingDecision{}, nil, invalid("exactly one candidate must be selected")
	}
	selected := candidates[selectedOrdinal]
	if err := validateSelectedMatchesExecution(execution, selected); err != nil {
		return persistence.ExecutionSchedulingDecision{}, nil, err
	}
	return persistence.ExecutionSchedulingDecision{
		ID:                        input.ID,
		TenantID:                  execution.TenantID,
		ExecutionID:               execution.ID,
		AlgorithmVersion:          input.AlgorithmVersion,
		EvidenceCompleteness:      input.EvidenceCompleteness,
		Provider:                  provider,
		DecisionKind:              decisionKind,
		CandidateCount:            len(candidates),
		SelectedOrdinal:           selectedOrdinal,
		SelectedExecutionTargetID: selected.ExecutionTargetID,
		SelectedWorkerPoolID:      selected.WorkerPoolID,
		SelectedRegion:            selected.Region,
		SelectedClusterID:         selected.ClusterID,
		CandidateSetSHA256:        candidateSetSHA256(hashes),
		DecidedAt:                 input.DecidedAt.UTC().Truncate(time.Microsecond),
	}, candidates, nil
}

func validateCompleteness(input Input, legacy bool) error {
	switch input.EvidenceCompleteness {
	case EvidenceComplete:
		if legacy {
			return invalid("legacy evidence cannot be complete")
		}
	case EvidenceSelectedOnly:
		if legacy || len(input.Candidates) != 1 {
			return invalid("selected-only evidence must contain exactly one new candidate")
		}
	case EvidenceLegacySelectedOnly:
		if !legacy || len(input.Candidates) != 1 {
			return invalid("legacy-selected-only is reserved for one-candidate migration backfill")
		}
	default:
		return invalid("unsupported evidence completeness")
	}
	return nil
}

func validateAlgorithm(algorithm, decisionKind string, legacy bool) error {
	if legacy {
		if algorithm != AlgorithmLegacySelectedOnly {
			return invalid("legacy backfill requires legacy-selected-only algorithm")
		}
		return nil
	}
	if decisionKind == DecisionKindTargetGroup &&
		algorithm != AlgorithmQueuePressureV1 && algorithm != AlgorithmReservationAwareV1 {
		return invalid("target-group decisions require queue-pressure-v1 or reservation-aware-v1")
	}
	if decisionKind == DecisionKindFixedTarget && algorithm != AlgorithmFixedTargetV1 {
		return invalid("fixed-target decisions require fixed-target-v1")
	}
	return nil
}

func validateCandidate(candidate CandidateSnapshot, algorithm string, legacy bool) error {
	if candidate.ExecutionTargetID == uuid.Nil || !oneOf(candidate.TargetKind, "local", "ssh", "docker", "kubernetes") {
		return errors.New("target identity is invalid")
	}
	if !boundedTrimmed(candidate.Region, 120, true) || !boundedTrimmed(candidate.ClusterID, 200, true) {
		return errors.New("location is invalid")
	}
	if candidate.Eligibility != EligibilityEligible && candidate.Eligibility != EligibilityRejected {
		return errors.New("eligibility is invalid")
	}
	if candidate.Selected && (candidate.Eligibility != EligibilityEligible || candidate.RejectionCode != nil) {
		return errors.New("selected candidate must be eligible without rejection")
	}
	if candidate.Eligibility == EligibilityRejected {
		if candidate.RejectionCode == nil || !boundedToken(*candidate.RejectionCode, 120) {
			return errors.New("rejected candidate requires a bounded rejection code")
		}
	} else if candidate.RejectionCode != nil {
		return errors.New("eligible candidate cannot have a rejection code")
	}
	healthPresence := []bool{candidate.HealthVersion != nil, candidate.HealthStatus != nil, candidate.CapacityStatus != nil, candidate.HealthObservedAt != nil, candidate.HealthExpiresAt != nil, candidate.AllocatedCapacityUnits != nil}
	if !allSame(healthPresence) {
		return errors.New("health snapshot fields must be present together")
	}
	if healthPresence[0] {
		if *candidate.HealthVersion <= 0 || !oneOf(*candidate.HealthStatus, "healthy", "degraded", "unreachable", "unknown") ||
			!oneOf(*candidate.CapacityStatus, "available", "saturated", "unknown") ||
			!candidate.HealthExpiresAt.After(*candidate.HealthObservedAt) || *candidate.AllocatedCapacityUnits < 0 {
			return errors.New("health snapshot is invalid")
		}
	}
	if candidate.AvailableCapacityUnits != nil && (*candidate.AvailableCapacityUnits < 0 || !healthPresence[0]) {
		return errors.New("available capacity is invalid")
	}
	drPresence := []bool{
		candidate.DRReadinessVersion != nil, candidate.SourceDRDomain != nil, candidate.DRDomain != nil,
		candidate.DRReplicatedThroughAt != nil, candidate.DRArtifactsReady != nil,
		candidate.DRCheckpointsReady != nil, candidate.DRMemoryReady != nil,
		candidate.DRObservedAt != nil, candidate.DRExpiresAt != nil,
	}
	if !allSame(drPresence) {
		return errors.New("DR readiness fields must be present together")
	}
	if drPresence[0] && (*candidate.DRReadinessVersion <= 0 ||
		!boundedToken(*candidate.SourceDRDomain, 160) || !boundedToken(*candidate.DRDomain, 160) ||
		candidate.DRReplicatedThroughAt.After(*candidate.DRObservedAt) ||
		!candidate.DRExpiresAt.After(*candidate.DRObservedAt)) {
		return errors.New("DR readiness snapshot is invalid")
	}
	if (candidate.QueuedExecutionUnits == nil) != (candidate.EffectiveLoadRank == nil) {
		return errors.New("queue pressure fields must be present together")
	}
	if candidate.QueuedExecutionUnits != nil && (*candidate.QueuedExecutionUnits < 0 || *candidate.EffectiveLoadRank < 0) {
		return errors.New("queue pressure must be nonnegative")
	}
	if !validNonnegative(candidate.Priority) || !validPositive(candidate.Weight) ||
		!validNonnegative(candidate.PriorityRank) || !validNonnegative(candidate.RegionRank) || !validNonnegative(candidate.CapacityRank) {
		return errors.New("priority, weight, or rank is invalid")
	}
	if (algorithm == AlgorithmQueuePressureV1 || algorithm == AlgorithmReservationAwareV1) && !legacy {
		if candidate.TargetGroupID == nil || candidate.TargetGroupVersion == nil || *candidate.TargetGroupVersion <= 0 ||
			candidate.TargetGroupMemberID == nil || candidate.TargetGroupMemberVersion == nil || *candidate.TargetGroupMemberVersion <= 0 ||
			candidate.Priority == nil || candidate.Weight == nil || candidate.QueuedExecutionUnits == nil || !healthPresence[0] {
			return errors.New("routed scheduling candidate lacks routing authority")
		}
	}
	if candidate.WorkerPoolID == nil {
		if candidate.WorkerPoolVersion != nil || candidate.CapacityClass != nil || candidate.PlacementPolicyVersion != nil {
			return errors.New("worker pool snapshot is incomplete")
		}
	} else if candidate.WorkerPoolVersion == nil || *candidate.WorkerPoolVersion <= 0 ||
		candidate.CapacityClass == nil || !boundedToken(*candidate.CapacityClass, 80) ||
		candidate.PlacementPolicyVersion == nil || *candidate.PlacementPolicyVersion <= 0 {
		return errors.New("worker pool snapshot is incomplete")
	}
	return nil
}

func validateSelectedMatchesExecution(execution persistence.AgentExecution, candidate persistence.ExecutionSchedulingCandidate) error {
	selectedRegion, selectedClusterID := selectedLocation(execution)
	if candidate.ExecutionTargetID != execution.ExecutionTargetID || candidate.TargetKind != execution.TargetKind ||
		candidate.Region != selectedRegion || candidate.ClusterID != selectedClusterID ||
		!uuidPtrEqual(candidate.WorkerPoolID, execution.WorkerPoolID) ||
		!int64PtrEqual(candidate.WorkerPoolVersion, execution.WorkerPoolVersion) ||
		!stringPtrEqual(candidate.CapacityClass, execution.CapacityClass) ||
		!int64PtrEqual(candidate.PlacementPolicyVersion, execution.PlacementPolicyVersion) ||
		!uuidPtrEqual(candidate.TargetGroupID, execution.TargetGroupID) ||
		!int64PtrEqual(candidate.TargetGroupVersion, execution.TargetGroupVersion) ||
		!int64PtrEqual(candidate.TargetGroupMemberVersion, execution.TargetGroupMemberVersion) {
		return ErrSelectedCandidateMismatch
	}
	return nil
}

func selectedLocation(execution persistence.AgentExecution) (string, string) {
	if execution.TargetGroupID != nil && execution.SelectedRegion != nil && execution.SelectedClusterID != nil {
		return *execution.SelectedRegion, *execution.SelectedClusterID
	}
	return execution.PlacementRegion, execution.PlacementClusterID
}

func persistenceCandidate(tenantID, decisionID uuid.UUID, ordinal int, value CandidateSnapshot) persistence.ExecutionSchedulingCandidate {
	return persistence.ExecutionSchedulingCandidate{
		TenantID: tenantID, DecisionID: decisionID, Ordinal: ordinal,
		ExecutionTargetID: value.ExecutionTargetID, TargetKind: value.TargetKind,
		TargetGroupID: value.TargetGroupID, TargetGroupVersion: value.TargetGroupVersion,
		TargetGroupMemberID: value.TargetGroupMemberID, TargetGroupMemberVersion: value.TargetGroupMemberVersion,
		Region: value.Region, ClusterID: value.ClusterID,
		HealthVersion: value.HealthVersion, HealthStatus: value.HealthStatus, CapacityStatus: value.CapacityStatus,
		HealthObservedAt: normalizeTimePtr(value.HealthObservedAt), HealthExpiresAt: normalizeTimePtr(value.HealthExpiresAt),
		DRReadinessVersion: value.DRReadinessVersion, SourceDRDomain: value.SourceDRDomain,
		DRDomain: value.DRDomain, DRReplicatedThroughAt: normalizeTimePtr(value.DRReplicatedThroughAt),
		DRArtifactsReady: value.DRArtifactsReady, DRCheckpointsReady: value.DRCheckpointsReady,
		DRMemoryReady: value.DRMemoryReady, DRObservedAt: normalizeTimePtr(value.DRObservedAt), DRExpiresAt: normalizeTimePtr(value.DRExpiresAt),
		AvailableCapacityUnits: value.AvailableCapacityUnits, AllocatedCapacityUnits: value.AllocatedCapacityUnits,
		QueuedExecutionUnits: value.QueuedExecutionUnits, EffectiveLoadRank: value.EffectiveLoadRank,
		Priority: value.Priority, Weight: value.Weight, PriorityRank: value.PriorityRank,
		RegionRank: value.RegionRank, CapacityRank: value.CapacityRank,
		WorkerPoolID: value.WorkerPoolID, WorkerPoolVersion: value.WorkerPoolVersion,
		CapacityClass: value.CapacityClass, PlacementPolicyVersion: value.PlacementPolicyVersion,
		Eligibility: value.Eligibility, RejectionCode: value.RejectionCode, Selected: value.Selected,
	}
}

// CandidateSHA256 is the cross-dialect canonical candidate digest.
func CandidateSHA256(candidate persistence.ExecutionSchedulingCandidate) string {
	var canonical strings.Builder
	canonical.WriteString("synara/scheduling-candidate/v1\n")
	field := func(name string, value *string) {
		canonical.WriteString(name)
		canonical.WriteByte(':')
		if value == nil {
			canonical.WriteString("-\n")
			return
		}
		canonical.WriteString(strconv.Itoa(len([]byte(*value))))
		canonical.WriteByte(':')
		canonical.WriteString(*value)
		canonical.WriteByte('\n')
	}
	stringValue := func(value string) *string { return &value }
	uuidValue := func(value *uuid.UUID) *string {
		if value == nil {
			return nil
		}
		formatted := value.String()
		return &formatted
	}
	timeValue := func(value *time.Time) *string {
		if value == nil {
			return nil
		}
		formatted := value.UTC().Format(time.RFC3339Nano)
		return &formatted
	}
	field("ordinal", stringValue(strconv.Itoa(candidate.Ordinal)))
	field("execution_target_id", stringValue(candidate.ExecutionTargetID.String()))
	field("target_kind", stringValue(candidate.TargetKind))
	field("target_group_id", uuidValue(candidate.TargetGroupID))
	field("target_group_version", canonicalInt64(candidate.TargetGroupVersion))
	field("target_group_member_id", uuidValue(candidate.TargetGroupMemberID))
	field("target_group_member_version", canonicalInt64(candidate.TargetGroupMemberVersion))
	field("region", stringValue(candidate.Region))
	field("cluster_id", stringValue(candidate.ClusterID))
	field("health_version", canonicalInt64(candidate.HealthVersion))
	field("health_status", candidate.HealthStatus)
	field("capacity_status", candidate.CapacityStatus)
	field("health_observed_at", timeValue(candidate.HealthObservedAt))
	field("health_expires_at", timeValue(candidate.HealthExpiresAt))
	field("dr_readiness_version", canonicalInt64(candidate.DRReadinessVersion))
	field("source_dr_domain", candidate.SourceDRDomain)
	field("dr_domain", candidate.DRDomain)
	field("dr_replicated_through_at", timeValue(candidate.DRReplicatedThroughAt))
	field("dr_artifacts_ready", canonicalBool(candidate.DRArtifactsReady))
	field("dr_checkpoints_ready", canonicalBool(candidate.DRCheckpointsReady))
	field("dr_memory_ready", canonicalBool(candidate.DRMemoryReady))
	field("dr_observed_at", timeValue(candidate.DRObservedAt))
	field("dr_expires_at", timeValue(candidate.DRExpiresAt))
	field("available_capacity_units", canonicalInt(candidate.AvailableCapacityUnits))
	field("allocated_capacity_units", canonicalInt(candidate.AllocatedCapacityUnits))
	field("queued_execution_units", canonicalInt64(candidate.QueuedExecutionUnits))
	field("effective_load_rank", canonicalInt64(candidate.EffectiveLoadRank))
	field("priority", canonicalInt(candidate.Priority))
	field("weight", canonicalInt(candidate.Weight))
	field("priority_rank", canonicalInt(candidate.PriorityRank))
	field("region_rank", canonicalInt(candidate.RegionRank))
	field("capacity_rank", canonicalInt(candidate.CapacityRank))
	field("worker_pool_id", uuidValue(candidate.WorkerPoolID))
	field("worker_pool_version", canonicalInt64(candidate.WorkerPoolVersion))
	field("capacity_class", candidate.CapacityClass)
	field("placement_policy_version", canonicalInt64(candidate.PlacementPolicyVersion))
	field("eligibility", stringValue(candidate.Eligibility))
	field("rejection_code", candidate.RejectionCode)
	field("selected", stringValue(strconv.FormatBool(candidate.Selected)))
	sum := sha256.Sum256([]byte(canonical.String()))
	return hex.EncodeToString(sum[:])
}

func candidateSetSHA256(hashes []string) string {
	canonical := strings.Join(hashes, "\n") + "\n"
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func normalizeTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC().Truncate(time.Microsecond)
	return &normalized
}

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalidEvidence, message) }
func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}
func boundedTrimmed(value string, max int, allowEmpty bool) bool {
	return (allowEmpty || value != "") && len(value) <= max && strings.TrimSpace(value) == value
}
func boundedToken(value string, max int) bool {
	return value != "" && len(value) <= max && strings.IndexFunc(value, unicode.IsSpace) < 0
}
func allSame(values []bool) bool {
	for _, value := range values[1:] {
		if value != values[0] {
			return false
		}
	}
	return true
}
func validNonnegative(value *int) bool { return value == nil || *value >= 0 }
func validPositive(value *int) bool    { return value == nil || *value > 0 }
func canonicalInt(value *int) *string {
	if value == nil {
		return nil
	}
	formatted := strconv.Itoa(*value)
	return &formatted
}
func canonicalInt64(value *int64) *string {
	if value == nil {
		return nil
	}
	formatted := strconv.FormatInt(*value, 10)
	return &formatted
}
func canonicalBool(value *bool) *string {
	if value == nil {
		return nil
	}
	formatted := strconv.FormatBool(*value)
	return &formatted
}
func uuidPtrEqual(left, right *uuid.UUID) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}
func int64PtrEqual(left, right *int64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}
func stringPtrEqual(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}
