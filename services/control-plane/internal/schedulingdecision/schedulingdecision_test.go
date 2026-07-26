package schedulingdecision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestCreateExecutionPersistsSelectedOnlyGraphAtomically(t *testing.T) {
	db := schedulingDecisionTestDB(t)
	execution := fixedExecutionFixture()
	input := NewSelectedOnlyInput(uuid.New(), AlgorithmFixedTargetV1, execution.QueuedAt, CandidateFromExecution(execution))

	decision, err := CreateExecution(context.Background(), db, &execution, input)
	if err != nil {
		t.Fatal(err)
	}
	if execution.SchedulingDecisionID == nil || *execution.SchedulingDecisionID != decision.ID {
		t.Fatalf("Execution Scheduling Decision identity = %v, want %s", execution.SchedulingDecisionID, decision.ID)
	}
	if decision.EvidenceCompleteness != EvidenceSelectedOnly || decision.AlgorithmVersion != AlgorithmFixedTargetV1 || decision.CandidateCount != 1 {
		t.Fatalf("Decision = %#v", decision)
	}
	var candidate persistence.ExecutionSchedulingCandidate
	if err := db.Where("tenant_id = ? AND decision_id = ?", execution.TenantID, decision.ID).Take(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	if candidate.Ordinal != 0 || !candidate.Selected || candidate.CandidateSHA256 != CandidateSHA256(candidate) {
		t.Fatalf("Candidate = %#v", candidate)
	}
}

func TestCreateExecutionRejectsSelectedMismatchBeforeWrite(t *testing.T) {
	db := schedulingDecisionTestDB(t)
	execution := fixedExecutionFixture()
	candidate := CandidateFromExecution(execution)
	candidate.ExecutionTargetID = uuid.New()
	input := NewSelectedOnlyInput(uuid.New(), AlgorithmFixedTargetV1, execution.QueuedAt, candidate)

	_, err := CreateExecution(context.Background(), db, &execution, input)
	if !errors.Is(err, ErrSelectedCandidateMismatch) {
		t.Fatalf("CreateExecution error = %v, want selected mismatch", err)
	}
	var count int64
	if err := db.Model(&persistence.AgentExecution{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 || execution.SchedulingDecisionID != nil {
		t.Fatalf("invalid graph wrote Execution count=%d decisionID=%v", count, execution.SchedulingDecisionID)
	}
}

func TestNewRuntimeCannotClaimLegacyCompleteness(t *testing.T) {
	db := schedulingDecisionTestDB(t)
	execution := fixedExecutionFixture()
	input := NewSelectedOnlyInput(uuid.New(), AlgorithmFixedTargetV1, execution.QueuedAt, CandidateFromExecution(execution))
	input.AlgorithmVersion = AlgorithmLegacySelectedOnly
	input.EvidenceCompleteness = EvidenceLegacySelectedOnly

	_, err := CreateExecution(context.Background(), db, &execution, input)
	if !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("CreateExecution error = %v, want invalid evidence", err)
	}
}

func TestCandidateHashCoversDRReadiness(t *testing.T) {
	execution := fixedExecutionFixture()
	candidate := persistenceCandidate(execution.TenantID, uuid.New(), 0, CandidateFromExecution(execution))
	version := int64(3)
	sourceDomain, drDomain := "cn-east-primary", "cn-west-dr"
	replicated := execution.QueuedAt.Add(-time.Minute)
	observed := execution.QueuedAt
	expires := execution.QueuedAt.Add(time.Minute)
	ready := true
	candidate.DRReadinessVersion = &version
	candidate.SourceDRDomain = &sourceDomain
	candidate.DRDomain = &drDomain
	candidate.DRReplicatedThroughAt = &replicated
	candidate.DRArtifactsReady = &ready
	candidate.DRCheckpointsReady = &ready
	candidate.DRMemoryReady = &ready
	candidate.DRObservedAt = &observed
	candidate.DRExpiresAt = &expires
	first := CandidateSHA256(candidate)
	falseValue := false
	candidate.DRMemoryReady = &falseValue
	if second := CandidateSHA256(candidate); second == first {
		t.Fatal("DR readiness mutation did not change Candidate SHA-256")
	}
}

func TestQueuePressureSelectedOnlyUsesMemberLocationAndAuthority(t *testing.T) {
	db := schedulingDecisionTestDB(t)
	execution := fixedExecutionFixture()
	groupID, memberID := uuid.New(), uuid.New()
	groupVersion, memberVersion := int64(4), int64(7)
	selectedRegion, selectedCluster := "member-region", "member-cluster"
	execution.TargetGroupID = &groupID
	execution.TargetGroupVersion = &groupVersion
	execution.TargetGroupMemberVersion = &memberVersion
	execution.SelectedRegion = &selectedRegion
	execution.SelectedClusterID = &selectedCluster
	execution.PlacementRegion = "effective-pool-region"
	execution.PlacementClusterID = "effective-pool-cluster"
	healthVersion := int64(9)
	healthStatus, capacityStatus := "healthy", "available"
	observed := execution.QueuedAt.Add(-time.Second)
	expires := execution.QueuedAt.Add(time.Minute)
	allocated := 2
	queued, effectiveRank := int64(3), int64(5_000)
	priority, weight := 100, 25
	candidate := CandidateFromExecution(execution)
	candidate.TargetGroupMemberID = &memberID
	candidate.HealthVersion = &healthVersion
	candidate.HealthStatus = &healthStatus
	candidate.CapacityStatus = &capacityStatus
	candidate.HealthObservedAt = &observed
	candidate.HealthExpiresAt = &expires
	candidate.AllocatedCapacityUnits = &allocated
	candidate.QueuedExecutionUnits = &queued
	candidate.EffectiveLoadRank = &effectiveRank
	candidate.Priority = &priority
	candidate.Weight = &weight

	input := NewSelectedOnlyInput(uuid.New(), AlgorithmQueuePressureV1, execution.QueuedAt, candidate)
	decision, err := CreateExecution(context.Background(), db, &execution, input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedRegion != selectedRegion || decision.SelectedClusterID != selectedCluster {
		t.Fatalf("Decision selected location = %s/%s, want Member %s/%s", decision.SelectedRegion, decision.SelectedClusterID, selectedRegion, selectedCluster)
	}
}

func schedulingDecisionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.AgentExecution{},
		&persistence.ExecutionSchedulingDecision{},
		&persistence.ExecutionSchedulingCandidate{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func fixedExecutionFixture() persistence.AgentExecution {
	provider := "codex"
	return persistence.AgentExecution{
		ID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		Attempt: 1, Status: "queued", ExecutionTargetID: uuid.New(), TargetKind: "local",
		Provider: &provider, Generation: 0, RequestedBy: uuid.New(), QueuedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}
