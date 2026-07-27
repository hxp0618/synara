package sessions

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	sharedrecoverybundle "github.com/synara-ai/synara/services/control-plane/internal/recoverybundle"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

func TestFailoverExecutionCreatesSuccessorWithoutRewritingSourcePlacement(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, destinationTarget, group, sourceMember := configureFailoverTargets(t, fixture)
	policyDocument := schedulingpolicy.UnrestrictedDocument()
	policyDocument.Provider = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"codex"}}
	policySnapshot, err := schedulingpolicy.NewService(fixture.db).UpdateTenant(
		ctx,
		fixture.tenantID,
		schedulingpolicy.UpdateInput{
			ExpectedVersion: 0, Document: policyDocument, ActorID: fixture.principal.UserID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "route me", CreatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", RequestedBy: fixture.principal.UserID, QueuedAt: time.Now().UTC(),
		TenantSchedulingPolicyVersion:       policySnapshot.Tenant.Version,
		TenantSchedulingPolicyDigest:        policySnapshot.Tenant.Digest,
		OrganizationSchedulingPolicyVersion: 0,
		OrganizationSchedulingPolicyDigest:  schedulingpolicy.UnrestrictedDigest,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.FailoverExecution(
		ctx, fixture.tenantID, source.ID, createTargetFailoverFence(t, fixture.db, 7), FailoverReasonOperatorRequest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceExecutionID != source.ID || result.DestinationExecutionTargetID != destinationTarget.ID || result.Attempt != 2 {
		t.Fatalf("failover result = %#v", result)
	}
	var storedSource, destination persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).Take(&storedSource).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.DestinationExecutionID).Take(&destination).Error; err != nil {
		t.Fatal(err)
	}
	if storedSource.Status != "interrupted" || storedSource.ExecutionTargetID != sourceTarget.ID ||
		storedSource.FailureCode == nil || *storedSource.FailureCode != "target_failover" {
		t.Fatalf("source = %#v", storedSource)
	}
	if destination.Status != "queued" || destination.Generation != 0 || destination.PredecessorExecutionID == nil ||
		*destination.PredecessorExecutionID != source.ID || destination.ExecutionTargetID != destinationTarget.ID ||
		destination.NextRecoveryReason != nil || destination.RoutingReason == nil || *destination.RoutingReason != "disaster-recovery" {
		t.Fatalf("destination = %#v", destination)
	}
	if destination.TenantSchedulingPolicyVersion != policySnapshot.Tenant.Version ||
		destination.TenantSchedulingPolicyDigest != policySnapshot.Tenant.Digest ||
		destination.OrganizationSchedulingPolicyVersion != 0 ||
		destination.OrganizationSchedulingPolicyDigest != schedulingpolicy.UnrestrictedDigest {
		t.Fatalf("failover successor Scheduling Policy snapshot = %#v, policy = %#v", destination, policySnapshot)
	}
	if destination.SchedulingDecisionID == nil {
		t.Fatal("failover successor omitted its immutable scheduling decision identity")
	}
	var decision persistence.ExecutionSchedulingDecision
	if err := fixture.db.Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, destination.ID).
		Take(&decision).Error; err != nil {
		t.Fatal(err)
	}
	if decision.ID != *destination.SchedulingDecisionID || decision.AlgorithmVersion != "queue-pressure-v1" ||
		decision.EvidenceCompleteness != "complete" || decision.CandidateCount != 2 ||
		decision.SelectedExecutionTargetID != destinationTarget.ID {
		t.Fatalf("failover successor scheduling decision = %#v", decision)
	}
	var destinationMember persistence.ExecutionTargetGroupMember
	if err := fixture.db.Where("tenant_id = ? AND target_group_id = ? AND execution_target_id = ?",
		fixture.tenantID, group.ID, destinationTarget.ID).Take(&destinationMember).Error; err != nil {
		t.Fatal(err)
	}
	var candidates []persistence.ExecutionSchedulingCandidate
	if err := fixture.db.Where("tenant_id = ? AND decision_id = ?", fixture.tenantID, decision.ID).
		Order("ordinal ASC").Find(&candidates).Error; err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("failover scheduling candidates = %#v", candidates)
	}
	candidateByTarget := make(map[uuid.UUID]persistence.ExecutionSchedulingCandidate, len(candidates))
	for _, value := range candidates {
		candidateByTarget[value.ExecutionTargetID] = value
	}
	candidate := candidateByTarget[destinationTarget.ID]
	rejectedSource := candidateByTarget[sourceTarget.ID]
	if !candidate.Selected || candidate.TargetGroupMemberID == nil ||
		*candidate.TargetGroupMemberID != destinationMember.ID || candidate.HealthVersion == nil ||
		candidate.QueuedExecutionUnits == nil || candidate.EffectiveLoadRank == nil ||
		rejectedSource.Selected || rejectedSource.Eligibility != "rejected" ||
		rejectedSource.RejectionCode == nil || *rejectedSource.RejectionCode != "request-excluded" {
		t.Fatalf("failover successor scheduling candidate = %#v", candidate)
	}
	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.ExecutionTargetID != destinationTarget.ID || session.ResourceState != "restoring" {
		t.Fatalf("session = %#v", session)
	}
	var audit persistence.ExecutionFailoverAttempt
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.FailoverAttemptID).Take(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if audit.Status != "committed" || audit.LeaderFencingToken != 7 || audit.DestinationExecutionID == nil ||
		*audit.DestinationExecutionID != destination.ID || audit.SourceRecoveryBundleID != nil {
		t.Fatalf("audit = %#v", audit)
	}
}

func TestFailoverExecutionUsesProviderAffinityAcrossEligibleDestinations(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()

	var sourceTarget persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.executionTargetID).Take(&sourceTarget).Error; err != nil {
		t.Fatal(err)
	}
	avoidDestination := sourceTarget
	avoidDestination.ID = uuid.New()
	avoidDestination.Name = "failover-destination-avoid-" + uuid.NewString()
	avoidDestination.Status = "active"
	if err := fixture.db.Create(&avoidDestination).Error; err != nil {
		t.Fatal(err)
	}
	preferDestination := sourceTarget
	preferDestination.ID = uuid.New()
	preferDestination.Name = "failover-destination-prefer-" + uuid.NewString()
	preferDestination.Status = "active"
	if err := fixture.db.Create(&preferDestination).Error; err != nil {
		t.Fatal(err)
	}
	setFailoverTargetRoutingPreferences(t, fixture.db, avoidDestination.ID, map[string]string{"codex": "avoid"})
	setFailoverTargetRoutingPreferences(t, fixture.db, preferDestination.ID, map[string]string{"codex": "prefer"})

	router := routing.NewService(fixture.db)
	group, err := router.CreateGroup(ctx, routing.CreateGroupInput{
		TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "failover-affinity-group-" + uuid.NewString(), Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, MaxFailoverAttempts: targetFailoverIntPointer(3), HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceMember, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: sourceTarget.ID,
		Region: "cn-shanghai", ClusterID: "cluster-a", Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: avoidDestination.ID,
		Region: "cn-beijing", ClusterID: "cluster-b", Priority: 20, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: preferDestination.ID,
		Region: "cn-beijing", ClusterID: "cluster-c", Priority: 30, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: sourceTarget.ID, Status: routing.HealthUnreachable, CapacityStatus: routing.CapacityUnknown,
		Source: "failover-affinity-test", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	capacity := 10
	for _, targetID := range []uuid.UUID{avoidDestination.ID, preferDestination.ID} {
		if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
			ExecutionTargetID:      targetID,
			Status:                 routing.HealthHealthy,
			CapacityStatus:         routing.CapacityAvailable,
			AvailableCapacityUnits: &capacity,
			Source:                 "failover-affinity-test",
			ObservedAt:             now,
			TTL:                    time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"requested_execution_target_id": sourceTarget.ID,
			"execution_target_group_id":     group.ID,
			"routing_policy_version":        group.Version,
			"execution_target_id":           sourceTarget.ID,
		}).Error; err != nil {
		t.Fatal(err)
	}

	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "route me by provider affinity", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.FailoverExecution(
		ctx, fixture.tenantID, source.ID, createTargetFailoverFence(t, fixture.db, 8), FailoverReasonOperatorRequest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.DestinationExecutionTargetID != preferDestination.ID {
		t.Fatalf("provider-affinity failover result = %#v", result)
	}
}

func TestFailoverExecutionUsesManagedKubernetesRoutingPublisherHealth(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(fixture.db, platformConfig, cipher)
	publisher := executiontargets.NewManagedKubernetesRoutingPublisher(
		targetService,
		executiontargets.ManagedKubernetesRoutingPublisherConfig{
			PublisherIdentity: "managed-kubernetes-routing-failover-test",
			ObservationTTL:    time.Minute,
		},
	)
	targetCapabilities := managedKubernetesFailoverTargetCapabilities()

	sourceView, err := targetService.Create(ctx, fixture.principal, fixture.tenantID, executiontargets.CreateInput{
		OrganizationID: &fixture.organizationID,
		Kind:           "kubernetes",
		Name:           "failover-source-kubernetes-" + uuid.NewString(),
		Configuration:  managedKubernetesFailoverTestConfiguration(1),
		Capabilities:   targetCapabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	destinationView, err := targetService.Create(ctx, fixture.principal, fixture.tenantID, executiontargets.CreateInput{
		OrganizationID: &fixture.organizationID,
		Kind:           "kubernetes",
		Name:           "failover-destination-kubernetes-" + uuid.NewString(),
		Configuration:  managedKubernetesFailoverTestConfiguration(1),
		Capabilities:   targetCapabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceTarget := loadTargetFailoverExecutionTarget(t, fixture.db, sourceView.ID)
	destinationTarget := loadTargetFailoverExecutionTarget(t, fixture.db, destinationView.ID)

	router := routing.NewService(fixture.db)
	group, err := router.CreateGroup(ctx, routing.CreateGroupInput{
		TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "managed-kubernetes-failover-group-" + uuid.NewString(), Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, MaxFailoverAttempts: targetFailoverIntPointer(3), HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceMember, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: sourceTarget.ID,
		Region: "cn-shanghai", ClusterID: "cluster-a", Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: destinationTarget.ID,
		Region: "cn-beijing", ClusterID: "cluster-b", Priority: 20, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"requested_execution_target_id": sourceTarget.ID,
			"execution_target_group_id":     group.ID,
			"routing_policy_version":        group.Version,
			"execution_target_id":           sourceTarget.ID,
		}).Error; err != nil {
		t.Fatal(err)
	}
	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "publisher-routed-failover", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	if err := publisher.PublishReconcile(ctx, executiontargets.ManagedKubernetesRoutingHealthObservation{
		ExecutionTargetID:      sourceTarget.ID,
		TenantOwned:            true,
		Status:                 routing.HealthHealthy,
		CapacityStatus:         routing.CapacitySaturated,
		AvailableCapacityUnits: targetFailoverIntPointer(1),
		AllocatedCapacityUnits: 1,
		ObservedAt:             now,
		Reason:                 stringPointer("Managed Kubernetes scheduled pod capacity is fully allocated."),
	}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishReconcile(ctx, executiontargets.ManagedKubernetesRoutingHealthObservation{
		ExecutionTargetID:      destinationTarget.ID,
		TenantOwned:            true,
		Status:                 routing.HealthHealthy,
		CapacityStatus:         routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(1),
		AllocatedCapacityUnits: 0,
		ObservedAt:             now,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.FailoverExecution(
		ctx, fixture.tenantID, source.ID, createTargetFailoverFence(t, fixture.db, 27), FailoverReasonOperatorRequest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceExecutionID != source.ID || result.DestinationExecutionTargetID != destinationTarget.ID || result.Attempt != 2 {
		t.Fatalf("failover result = %#v", result)
	}
	var destination persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.DestinationExecutionID).Take(&destination).Error; err != nil {
		t.Fatal(err)
	}
	if destination.ExecutionTargetID != destinationTarget.ID || destination.Status != "queued" ||
		destination.RoutingReason == nil || *destination.RoutingReason != "disaster-recovery" {
		t.Fatalf("destination = %#v", destination)
	}
}

func TestFailoverExecutionCapabilityExhaustionLeavesSourceUnchanged(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, destinationTarget, group, sourceMember := configureFailoverTargets(t, fixture)
	destinationPool := configureWarmDefaultPool(t, fixture.db, destinationTarget)
	seedTargetCapabilityWorker(t, fixture.db, destinationTarget.ID, &destinationPool.ID, &destinationPool.Version, sessionCapabilityManifestOptions{
		CodexSendTurn: "unsupported",
	})

	turnID := uuid.New()
	now := time.Now().UTC()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "fail closed before fencing", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.FailoverExecution(
		ctx, fixture.tenantID, source.ID, createTargetFailoverFence(t, fixture.db, 17), FailoverReasonOperatorRequest,
	)
	assertSessionProblemCode(t, err, "capability_unsupported")

	var storedSource persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).Take(&storedSource).Error; err != nil {
		t.Fatal(err)
	}
	if storedSource.Status != "queued" || storedSource.FailureCode != nil {
		t.Fatalf("source mutated despite failed candidate = %#v", storedSource)
	}
	var failoverAttempts int64
	if err := fixture.db.Model(&persistence.ExecutionFailoverAttempt{}).
		Where("tenant_id = ? AND source_execution_id = ?", fixture.tenantID, source.ID).
		Count(&failoverAttempts).Error; err != nil {
		t.Fatal(err)
	}
	if failoverAttempts != 0 {
		t.Fatalf("unexpected failover attempts persisted: %d", failoverAttempts)
	}
	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.ExecutionTargetID != sourceTarget.ID {
		t.Fatalf("session execution target advanced unexpectedly: %#v", session)
	}
}

func TestReconcileTargetFailoversCommitsRegionFailoverForActiveLocationOutage(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, destinationTarget, group, sourceMember := configureFailoverTargets(t, fixture)
	now := time.Now().UTC()
	if _, err := routing.NewService(fixture.db).ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: sourceTarget.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(10), Source: "region-failover-source",
		ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.NewService(fixture.db).ObserveLocationOutage(ctx, routing.LocationOutageObservation{
		TenantID: fixture.tenantID, Region: "cn-shanghai", ClusterID: "cluster-a",
		Status: routing.LocationStatusUnreachable, PublisherIdentity: "tenant-router",
		ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "auto fail over region", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, WarmPoolModeSnapshot: "disabled", RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	sweep, err := fixture.service.ReconcileTargetFailovers(ctx, createTargetFailoverFence(t, fixture.db, 41), 10)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.Candidates != 1 || sweep.Committed != 1 || sweep.Rejected != 0 {
		t.Fatalf("failover sweep = %#v", sweep)
	}

	var audit persistence.ExecutionFailoverAttempt
	if err := fixture.db.Where("tenant_id = ? AND source_execution_id = ?", fixture.tenantID, source.ID).Take(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if audit.Reason != FailoverReasonRegionFailover || audit.DestinationExecutionID == nil {
		t.Fatalf("region failover audit = %#v", audit)
	}
	var destination persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, *audit.DestinationExecutionID).Take(&destination).Error; err != nil {
		t.Fatal(err)
	}
	if destination.ExecutionTargetID != destinationTarget.ID || destination.PredecessorExecutionID == nil ||
		*destination.PredecessorExecutionID != source.ID {
		t.Fatalf("region failover destination = %#v", destination)
	}
	var storedSource persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).Take(&storedSource).Error; err != nil {
		t.Fatal(err)
	}
	if storedSource.ExecutionTargetID != sourceTarget.ID || storedSource.Status != "interrupted" {
		t.Fatalf("region failover source = %#v", storedSource)
	}
}

func TestReconcileTargetFailoversIgnoresFutureLocationOutageRows(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, _, group, sourceMember := configureFailoverTargets(t, fixture)
	now := time.Now().UTC()
	router := routing.NewService(fixture.db)
	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID:      sourceTarget.ID,
		Status:                 routing.HealthHealthy,
		CapacityStatus:         routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(10),
		AllocatedCapacityUnits: 0,
		Source:                 "future-location-outage-test",
		ObservedAt:             now,
		TTL:                    time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	futureObservedAt := now.Add(time.Minute)
	if err := fixture.db.Create(&persistence.ExecutionLocationOutage{
		TenantID:          fixture.tenantID,
		Region:            "cn-shanghai",
		ClusterID:         "cluster-a",
		Status:            routing.LocationStatusUnreachable,
		PublisherIdentity: "bypassed-service",
		ObservedAt:        futureObservedAt,
		ExpiresAt:         futureObservedAt.Add(time.Minute),
		Version:           1,
		UpdatedAt:         now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "future outage ignored", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, WarmPoolModeSnapshot: "disabled", RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	sweep, err := fixture.service.ReconcileTargetFailovers(ctx, createTargetFailoverFence(t, fixture.db, 42), 10)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.Candidates != 0 || sweep.Committed != 0 || sweep.Rejected != 0 {
		t.Fatalf("future outage should be ignored, sweep = %#v", sweep)
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.ExecutionFailoverAttempt{}).
		Where("tenant_id = ? AND source_execution_id = ?", fixture.tenantID, source.ID).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 0 {
		t.Fatalf("future outage created failover audit rows: %d", auditCount)
	}
}

func TestFailoverExecutionCarriesClaimedRecoveryLineageAcrossAttempts(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, destinationTarget, group, sourceMember := configureFailoverTargets(t, fixture)
	turnID := uuid.New()
	now := time.Now().UTC()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "resume me", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "leased", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", Generation: 1, RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	bundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		ExecutionID: source.ID, Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim",
		AuthoritativeHistorySequence: 0, Payload: map[string]any{}, CreatedAt: now,
	}
	sealTargetFailoverRecoveryBundle(t, &bundle)
	if err := fixture.db.Create(&bundle).Error; err != nil {
		t.Fatal(err)
	}
	recoveryReason := "suspend-resume"
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).
		Updates(map[string]any{"status": "suspended", "next_recovery_reason": recoveryReason}).Error; err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.FailoverExecution(
		ctx, fixture.tenantID, source.ID, createTargetFailoverFence(t, fixture.db, 8), FailoverReasonTargetUnreachable,
	)
	if err != nil {
		t.Fatal(err)
	}
	var destination persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.DestinationExecutionID).Take(&destination).Error; err != nil {
		t.Fatal(err)
	}
	if destination.ExecutionTargetID != destinationTarget.ID || destination.Status != "recovering" ||
		destination.NextRecoveryReason == nil || *destination.NextRecoveryReason != "disaster-recovery" ||
		destination.PredecessorExecutionID == nil || *destination.PredecessorExecutionID != source.ID {
		t.Fatalf("destination = %#v", destination)
	}
	var audit persistence.ExecutionFailoverAttempt
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.FailoverAttemptID).Take(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if audit.SourceRecoveryBundleID == nil || *audit.SourceRecoveryBundleID != bundle.ID {
		t.Fatalf("audit source bundle = %#v", audit.SourceRecoveryBundleID)
	}
}

func TestFailoverExecutionSuspendedSourceReacquiresTenantQuotaWithoutMutation(t *testing.T) {
	fixture, source, sourceTarget := seedSuspendedTargetFailoverSource(t)
	blockerExecutionID := createTargetFailoverQuotaBlocker(t, fixture, sourceTarget)

	assertFailoverRejectionIsMutationFree(
		t,
		fixture,
		source.ID,
		82,
		"execution_quota_exceeded",
	)

	now := time.Now().UTC()
	update := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ? AND status = ?", fixture.tenantID, blockerExecutionID, "queued").
		Updates(map[string]any{"status": "completed", "finished_at": now})
	if update.Error != nil || update.RowsAffected != 1 {
		t.Fatalf("release failover quota blocker: rows=%d err=%v", update.RowsAffected, update.Error)
	}

	result, err := fixture.service.FailoverExecution(
		context.Background(),
		fixture.tenantID,
		source.ID,
		createTargetFailoverFence(t, fixture.db, 83),
		FailoverReasonTargetUnreachable,
	)
	if err != nil {
		t.Fatal(err)
	}
	var destination persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.DestinationExecutionID).
		Take(&destination).Error; err != nil {
		t.Fatal(err)
	}
	if destination.Status != "recovering" || destination.PredecessorExecutionID == nil ||
		*destination.PredecessorExecutionID != source.ID {
		t.Fatalf("destination after quota release = %#v", destination)
	}
}

func seedSuspendedTargetFailoverSource(
	t *testing.T,
) (tenantExecutionPolicyFixture, persistence.AgentExecution, persistence.ExecutionTarget) {
	t.Helper()
	fixture := newTenantExecutionPolicyFixture(t)
	sourceTarget, _, group, sourceMember := configureFailoverTargets(t, fixture)
	now := time.Now().UTC()
	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "resume under quota", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "leased", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", Generation: 1, RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	bundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		ExecutionID: source.ID, Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim",
		AuthoritativeHistorySequence: 0, Payload: map[string]any{}, CreatedAt: now,
	}
	sealTargetFailoverRecoveryBundle(t, &bundle)
	if err := fixture.db.Create(&bundle).Error; err != nil {
		t.Fatal(err)
	}
	recoveryReason := "suspend-resume"
	update := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ? AND status = ?", fixture.tenantID, source.ID, "leased").
		Updates(map[string]any{"status": "suspended", "next_recovery_reason": recoveryReason})
	if update.Error != nil || update.RowsAffected != 1 {
		t.Fatalf("suspend failover source: rows=%d err=%v", update.RowsAffected, update.Error)
	}
	source.Status = "suspended"
	source.NextRecoveryReason = &recoveryReason
	return fixture, source, sourceTarget
}

func createTargetFailoverQuotaBlocker(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	target persistence.ExecutionTarget,
) uuid.UUID {
	t.Helper()
	limit := 1
	now := time.Now().UTC()
	sessionID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	provider := "codex"
	models := []any{
		&persistence.TenantQuota{
			TenantID: fixture.tenantID, MaxConcurrentExecutions: &limit,
			UpdatedBy: fixture.principal.UserID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
			ProjectID: fixture.projectID, CreatedBy: fixture.principal.UserID, Title: "failover quota blocker",
			Status: "active", Visibility: "private", Provider: provider, ExecutionTargetID: target.ID,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: fixture.tenantID, SessionID: sessionID,
			CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "occupy failover quota", CreatedAt: now,
		},
		&persistence.AgentExecution{
			ID: executionID, TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
			Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: target.Kind,
			Provider: &provider, Generation: 0, RequestedBy: fixture.principal.UserID, QueuedAt: now,
		},
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return executionID
}

func TestFailoverExecutionRejectsTamperedSourceRecoveryBundleWithoutMutation(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		wantCode string
		tamper   func(*testing.T, *persistence.ExecutionRecoveryBundle)
	}{
		{
			name:     "canonical hash",
			wantCode: "target_failover_bundle_integrity_failed",
			tamper: func(_ *testing.T, bundle *persistence.ExecutionRecoveryBundle) {
				bundle.PayloadSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			},
		},
		{
			name:     "immutable envelope",
			wantCode: "target_failover_bundle_envelope_mismatch",
			tamper: func(t *testing.T, bundle *persistence.ExecutionRecoveryBundle) {
				bundle.Payload["executionId"] = uuid.New()
				normalized, sha256, err := sharedrecoverybundle.Encode(bundle.Payload)
				if err != nil {
					t.Fatal(err)
				}
				bundle.Payload = normalized
				bundle.PayloadSHA256 = sha256
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTenantExecutionPolicyFixture(t)
			sourceTarget, _, group, sourceMember := configureFailoverTargets(t, fixture)
			now := time.Now().UTC()
			turnID := uuid.New()
			if err := fixture.db.Create(&persistence.AgentTurn{
				ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
				CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "reject corrupt bundle", CreatedAt: now,
			}).Error; err != nil {
				t.Fatal(err)
			}
			provider := "codex"
			source := persistence.AgentExecution{
				ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
				Attempt: 1, Status: "leased", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
				Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
				WarmPoolModeSnapshot: "disabled", Generation: 1, RequestedBy: fixture.principal.UserID, QueuedAt: now,
			}
			routing.ApplyExecutionSelection(&source, routing.Selection{
				Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
			})
			if err := fixture.db.Create(&source).Error; err != nil {
				t.Fatal(err)
			}
			bundle := persistence.ExecutionRecoveryBundle{
				ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
				ExecutionID: source.ID, Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim",
				CreatedAt: now,
			}
			sealTargetFailoverRecoveryBundle(t, &bundle)
			testCase.tamper(t, &bundle)
			if err := fixture.db.Create(&bundle).Error; err != nil {
				t.Fatal(err)
			}
			recoveryReason := "suspend-resume"
			if err := fixture.db.Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).
				Updates(map[string]any{"status": "suspended", "next_recovery_reason": recoveryReason}).Error; err != nil {
				t.Fatal(err)
			}
			source.Status = "suspended"
			source.NextRecoveryReason = &recoveryReason
			var beforeEvents, beforeOutbox int64
			if err := fixture.db.Model(&persistence.SessionEvent{}).
				Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
				Count(&beforeEvents).Error; err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.Model(&persistence.OutboxMessage{}).
				Where("tenant_id = ?", fixture.tenantID).Count(&beforeOutbox).Error; err != nil {
				t.Fatal(err)
			}

			_, err := fixture.service.FailoverExecution(
				context.Background(), fixture.tenantID, source.ID,
				createTargetFailoverFence(t, fixture.db, 81), FailoverReasonTargetUnreachable,
			)
			assertSessionProblemCode(t, err, testCase.wantCode)

			var persisted persistence.AgentExecution
			if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).
				Take(&persisted).Error; err != nil {
				t.Fatal(err)
			}
			if persisted.Status != "suspended" || persisted.FinishedAt != nil {
				t.Fatalf("tampered bundle mutated source: %#v", persisted)
			}
			var session persistence.AgentSession
			if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
				Take(&session).Error; err != nil {
				t.Fatal(err)
			}
			if session.ExecutionTargetID != sourceTarget.ID {
				t.Fatalf("tampered bundle moved Session target to %s", session.ExecutionTargetID)
			}
			var attempts, executions, afterEvents, afterOutbox int64
			for model, destination := range map[any]*int64{
				&persistence.ExecutionFailoverAttempt{}: &attempts,
				&persistence.AgentExecution{}:           &executions,
				&persistence.SessionEvent{}:             &afterEvents,
				&persistence.OutboxMessage{}:            &afterOutbox,
			} {
				query := fixture.db.Model(model).Where("tenant_id = ?", fixture.tenantID)
				if _, ok := model.(*persistence.AgentExecution); ok {
					query = query.Where("turn_id = ?", turnID)
				}
				if _, ok := model.(*persistence.SessionEvent); ok {
					query = query.Where("session_id = ?", fixture.sessionID)
				}
				if err := query.Count(destination).Error; err != nil {
					t.Fatal(err)
				}
			}
			if attempts != 0 || executions != 1 || afterEvents != beforeEvents || afterOutbox != beforeOutbox {
				t.Fatalf(
					"tampered bundle left mutations: attempts=%d executions=%d events=%d/%d outbox=%d/%d",
					attempts, executions, beforeEvents, afterEvents, beforeOutbox, afterOutbox,
				)
			}
		})
	}
}

func TestFailoverExecutionRequiresFreshArtifactAuthorityFromCurrentSessionHistory(t *testing.T) {
	t.Run("artifacts unready blocks failover", func(t *testing.T) {
		fixture, source, destination, bundleID, sourceDRDomain, watermark := seedFailoverArtifactAuthorityFixture(t)
		observeTargetDRReadiness(
			t,
			fixture,
			destination.ID,
			sourceDRDomain,
			routing.DRDomainForLocation("cn-beijing", "cluster-b"),
			watermark,
			false,
			false,
			false,
			watermark.Add(time.Second),
		)

		_, err := fixture.service.FailoverExecution(
			context.Background(),
			fixture.tenantID,
			source.ID,
			createTargetFailoverFence(t, fixture.db, 17),
			FailoverReasonTargetUnreachable,
		)
		assertSessionProblemCode(t, err, "target_group_dr_readiness_unready")

		var audit persistence.ExecutionFailoverAttempt
		if err := fixture.db.Where("tenant_id = ? AND source_recovery_bundle_id = ?", fixture.tenantID, bundleID).Take(&audit).Error; err == nil {
			t.Fatalf("unexpected failover audit persisted for blocked attempt: %#v", audit)
		}
	})

	t.Run("stale watermark blocks runtime-added artifact authority", func(t *testing.T) {
		fixture, source, destination, _, sourceDRDomain, watermark := seedFailoverArtifactAuthorityFixture(t)
		observeTargetDRReadiness(
			t,
			fixture,
			destination.ID,
			sourceDRDomain,
			routing.DRDomainForLocation("cn-beijing", "cluster-b"),
			watermark.Add(-time.Second),
			true,
			false,
			false,
			watermark.Add(time.Second),
		)

		_, err := fixture.service.FailoverExecution(
			context.Background(),
			fixture.tenantID,
			source.ID,
			createTargetFailoverFence(t, fixture.db, 18),
			FailoverReasonTargetUnreachable,
		)
		assertSessionProblemCode(t, err, "target_group_dr_recovery_watermark_stale")
	})

	t.Run("fresh watermark allows failover", func(t *testing.T) {
		fixture, source, destination, _, sourceDRDomain, watermark := seedFailoverArtifactAuthorityFixture(t)
		observeTargetDRReadiness(
			t,
			fixture,
			destination.ID,
			sourceDRDomain,
			routing.DRDomainForLocation("cn-beijing", "cluster-b"),
			watermark,
			true,
			false,
			false,
			watermark.Add(time.Second),
		)
		var authority persistence.ExecutionTargetDRReadiness
		if err := fixture.db.Where(
			"execution_target_id = ? AND source_dr_domain = ?", destination.ID, sourceDRDomain,
		).Take(&authority).Error; err != nil {
			t.Fatal(err)
		}

		result, err := fixture.service.FailoverExecution(
			context.Background(),
			fixture.tenantID,
			source.ID,
			createTargetFailoverFence(t, fixture.db, 19),
			FailoverReasonTargetUnreachable,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.DestinationExecutionTargetID != destination.ID {
			t.Fatalf("destination target = %s, want %s", result.DestinationExecutionTargetID, destination.ID)
		}
		for name, payload := range map[string]map[string]any{
			"event":  loadFailoverCommittedEventPayload(t, fixture, source.SessionID),
			"outbox": loadFailoverCommittedOutboxPayload(t, fixture),
		} {
			destinationAuthority, ok := payload["destinationDrAuthority"].(map[string]any)
			if !ok || fmt.Sprint(destinationAuthority["version"]) != fmt.Sprint(authority.Version) ||
				destinationAuthority["artifactsReady"] != true || destinationAuthority["checkpointsReady"] != false ||
				destinationAuthority["memoryReady"] != false {
				t.Fatalf("%s destination authority version = %#v, want %d", name, destinationAuthority, authority.Version)
			}
		}
	})
}

func TestFailoverExecutionRevalidatesSelectedAuthorityBeforeMutation(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*testing.T, *gorm.DB, routing.Selection)
	}{
		{
			name: "group version",
			mutate: func(t *testing.T, tx *gorm.DB, selection routing.Selection) {
				t.Helper()
				if err := tx.Model(&persistence.ExecutionTargetGroup{}).Where("id = ?", selection.Group.ID).
					Update("version", selection.Group.Version+1).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member location and version",
			mutate: func(t *testing.T, tx *gorm.DB, selection routing.Selection) {
				t.Helper()
				if err := tx.Model(&persistence.ExecutionTargetGroupMember{}).Where("id = ?", selection.Member.ID).
					Updates(map[string]any{"region": "cn-guangdong", "cluster_id": "cluster-c", "version": selection.Member.Version + 1}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "target disabled",
			mutate: func(t *testing.T, tx *gorm.DB, selection routing.Selection) {
				t.Helper()
				if err := tx.Model(&persistence.ExecutionTarget{}).Where("id = ?", selection.Target.ID).
					Update("status", "disabled").Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "destination location outage inserted",
			mutate: func(t *testing.T, tx *gorm.DB, selection routing.Selection) {
				t.Helper()
				now := time.Now().UTC()
				if err := tx.Create(&persistence.ExecutionLocationOutage{
					TenantID: selection.Group.TenantID, Region: selection.Member.Region,
					ClusterID: selection.Member.ClusterID, Status: routing.LocationStatusUnreachable,
					PublisherIdentity: "selection-race-test", ObservedAt: now,
					ExpiresAt: now.Add(time.Minute), Version: 1, UpdatedAt: now,
				}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "health version",
			mutate: func(t *testing.T, tx *gorm.DB, selection routing.Selection) {
				t.Helper()
				observedAt := time.Now().UTC()
				if err := tx.Model(&persistence.ExecutionTargetHealth{}).
					Where("execution_target_id = ?", selection.Target.ID).
					Updates(map[string]any{
						"status": routing.HealthDegraded, "observed_at": observedAt,
						"expires_at": observedAt.Add(time.Minute), "version": selection.Health.Version + 1,
					}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "readiness version and store bits",
			mutate: func(t *testing.T, tx *gorm.DB, selection routing.Selection) {
				t.Helper()
				if selection.DRReadiness == nil {
					t.Fatal("expected DR readiness selection")
				}
				observedAt := time.Now().UTC()
				if err := tx.Model(&persistence.ExecutionTargetDRReadiness{}).
					Where("execution_target_id = ? AND source_dr_domain = ?", selection.Target.ID, selection.DRReadiness.SourceDRDomain).
					Updates(map[string]any{
						"artifacts_ready": false, "observed_at": observedAt,
						"expires_at": observedAt.Add(time.Minute), "version": selection.DRReadiness.Version + 1,
					}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, source, destination, _, sourceDRDomain, watermark := seedFailoverArtifactAuthorityFixture(t)
			observeTargetDRReadiness(
				t, fixture, destination.ID, sourceDRDomain,
				routing.DRDomainForLocation("cn-beijing", "cluster-b"), watermark,
				true, false, false, time.Now().UTC(),
			)
			fixture.service.targetFailoverBeforeCommitValidation = func(
				_ context.Context,
				tx *gorm.DB,
				_ routing.SelectRequest,
				selection routing.Selection,
			) error {
				testCase.mutate(t, tx, selection)
				return nil
			}

			assertFailoverRejectionIsMutationFree(
				t, fixture, source.ID, int64(101+index), "target_routing_selection_stale",
			)
		})
	}
}

func TestFailoverExecutionRejectsInvalidDestinationAuthorityWithoutMutation(t *testing.T) {
	t.Run("wrong destination domain", func(t *testing.T) {
		fixture, source, destination, _, sourceDRDomain, watermark := seedFailoverArtifactAuthorityFixture(t)
		observeTargetDRReadiness(
			t, fixture, destination.ID, sourceDRDomain, "cn-beijing/wrong-cluster", watermark,
			true, false, false, watermark.Add(time.Second),
		)

		assertFailoverRejectionIsMutationFree(
			t, fixture, source.ID, 91, "target_group_dr_destination_domain_mismatch",
		)
	})

	t.Run("future readiness", func(t *testing.T) {
		fixture, source, destination, _, sourceDRDomain, watermark := seedFailoverArtifactAuthorityFixture(t)
		futureObservedAt := time.Now().UTC().Add(time.Minute)
		if err := fixture.db.Create(&persistence.ExecutionTargetDRReadiness{
			ExecutionTargetID: destination.ID, SourceDRDomain: sourceDRDomain,
			DRDomain: routing.DRDomainForLocation("cn-beijing", "cluster-b"), ReplicatedThroughAt: watermark,
			ArtifactsReady: true, PublisherIdentity: "bypassed-service",
			ObservedAt: futureObservedAt, ExpiresAt: futureObservedAt.Add(time.Minute),
			Version: 1, UpdatedAt: time.Now().UTC(),
		}).Error; err != nil {
			t.Fatal(err)
		}

		assertFailoverRejectionIsMutationFree(
			t, fixture, source.ID, 92, "target_group_dr_readiness_observed_in_future",
		)
	})
}

func assertFailoverRejectionIsMutationFree(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	sourceExecutionID uuid.UUID,
	fencingToken int64,
	wantCode string,
) {
	t.Helper()
	var beforeSource persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, sourceExecutionID).Take(&beforeSource).Error; err != nil {
		t.Fatal(err)
	}
	var beforeSession persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, beforeSource.SessionID).Take(&beforeSession).Error; err != nil {
		t.Fatal(err)
	}
	beforeCounts := loadFailoverMutationCounts(t, fixture, beforeSource)

	_, err := fixture.service.FailoverExecution(
		context.Background(), fixture.tenantID, sourceExecutionID,
		createTargetFailoverFence(t, fixture.db, fencingToken), FailoverReasonTargetUnreachable,
	)
	assertSessionProblemCode(t, err, wantCode)

	var afterSource persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, sourceExecutionID).Take(&afterSource).Error; err != nil {
		t.Fatal(err)
	}
	var afterSession persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, beforeSource.SessionID).Take(&afterSession).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeSource, afterSource) {
		t.Fatalf("rejected failover mutated source execution: before=%#v after=%#v", beforeSource, afterSource)
	}
	if !reflect.DeepEqual(beforeSession, afterSession) {
		t.Fatalf("rejected failover mutated session: before=%#v after=%#v", beforeSession, afterSession)
	}
	afterCounts := loadFailoverMutationCounts(t, fixture, beforeSource)
	if beforeCounts != afterCounts {
		t.Fatalf("rejected failover left mutations: before=%#v after=%#v", beforeCounts, afterCounts)
	}
}

type targetFailoverMutationCounts struct {
	executions       int64
	decisions        int64
	candidates       int64
	failoverAttempts int64
	auditLogs        int64
	events           int64
	outbox           int64
}

func loadFailoverMutationCounts(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	source persistence.AgentExecution,
) targetFailoverMutationCounts {
	t.Helper()
	var counts targetFailoverMutationCounts
	queries := []struct {
		model       any
		where       string
		args        []any
		destination *int64
	}{
		{&persistence.AgentExecution{}, "tenant_id = ? AND turn_id = ?", []any{fixture.tenantID, source.TurnID}, &counts.executions},
		{&persistence.ExecutionSchedulingDecision{}, "tenant_id = ? AND execution_id IN (?)", []any{
			fixture.tenantID,
			fixture.db.Model(&persistence.AgentExecution{}).Select("id").
				Where("tenant_id = ? AND turn_id = ?", fixture.tenantID, source.TurnID),
		}, &counts.decisions},
		{&persistence.ExecutionSchedulingCandidate{}, "tenant_id = ? AND decision_id IN (?)", []any{
			fixture.tenantID,
			fixture.db.Model(&persistence.ExecutionSchedulingDecision{}).Select("id").
				Where("tenant_id = ? AND execution_id IN (?)", fixture.tenantID,
					fixture.db.Model(&persistence.AgentExecution{}).Select("id").
						Where("tenant_id = ? AND turn_id = ?", fixture.tenantID, source.TurnID)),
		}, &counts.candidates},
		{&persistence.ExecutionFailoverAttempt{}, "tenant_id = ? AND source_execution_id = ?", []any{fixture.tenantID, source.ID}, &counts.failoverAttempts},
		{&persistence.AuditLog{}, "tenant_id = ?", []any{fixture.tenantID}, &counts.auditLogs},
		{&persistence.SessionEvent{}, "tenant_id = ? AND session_id = ?", []any{fixture.tenantID, source.SessionID}, &counts.events},
		{&persistence.OutboxMessage{}, "tenant_id = ?", []any{fixture.tenantID}, &counts.outbox},
	}
	for _, query := range queries {
		if err := fixture.db.Model(query.model).Where(query.where, query.args...).Count(query.destination).Error; err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func loadFailoverCommittedEventPayload(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	sessionID uuid.UUID,
) map[string]any {
	t.Helper()
	var event persistence.SessionEvent
	if err := fixture.db.Where(
		"tenant_id = ? AND session_id = ? AND event_type = ?",
		fixture.tenantID, sessionID, "execution.failover-committed",
	).Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	return event.Payload
}

func loadFailoverCommittedOutboxPayload(t *testing.T, fixture tenantExecutionPolicyFixture) map[string]any {
	t.Helper()
	var message persistence.OutboxMessage
	if err := fixture.db.Where(
		"tenant_id = ? AND topic = ?", fixture.tenantID, "execution.failover-committed",
	).Take(&message).Error; err != nil {
		t.Fatal(err)
	}
	return message.Payload
}

func TestFailoverExecutionAcceptsForkAncestorArtifactAuthorityOutsideCurrentTail(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, destinationTarget, group, sourceMember := configureFailoverTargets(t, fixture)

	ancestorSessionID := uuid.New()
	prefixSequence := int64(3)
	strategy := "emulated"
	createAuthoritySession(t, fixture, ancestorSessionID, sourceTarget.ID, &sourceTarget.ID, nil, prefixSequence)
	childSession := createForkAuthoritySession(
		t,
		fixture,
		sourceTarget.ID,
		&sourceTarget.ID,
		&group.ID,
		&group.Version,
		ancestorSessionID,
		prefixSequence,
		strategy,
		504,
	)

	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: childSession.ID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "fail over fork ancestor", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: childSession.ID, TurnID: turnID,
		Attempt: 1, Status: "leased", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", Generation: 1, RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	ancestorExecutionID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentExecution{
		ID: ancestorExecutionID, TenantID: fixture.tenantID, SessionID: ancestorSessionID, TurnID: uuid.New(),
		Attempt: 1, Status: "completed", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, RequestedBy: fixture.principal.UserID, QueuedAt: now, FinishedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	ancestorArtifactAt := now.Add(10 * time.Second)
	ancestorArtifact := createReadyExecutionArtifact(t, fixture, ancestorSessionID, &ancestorExecutionID, ancestorArtifactAt)
	ancestorEvents := []persistence.SessionEvent{
		{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: ancestorSessionID, Sequence: 1, EventID: uuid.New(), EventVersion: 1,
			EventType: "content.delta", ActorType: "worker",
			Payload: map[string]any{"streamKind": "assistant_text", "delta": "a"}, OccurredAt: now,
		},
		{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: ancestorSessionID, Sequence: 2, EventID: uuid.New(), EventVersion: 1,
			EventType: "artifact.ready", ActorType: "system", ExecutionID: &ancestorExecutionID,
			Payload: map[string]any{"artifactId": ancestorArtifact.ID, "kind": "generated_file"}, OccurredAt: ancestorArtifactAt,
		},
		{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: ancestorSessionID, Sequence: 3, EventID: uuid.New(), EventVersion: 1,
			EventType: "content.delta", ActorType: "worker",
			Payload: map[string]any{"streamKind": "assistant_text", "delta": "b"}, OccurredAt: ancestorArtifactAt.Add(time.Second),
		},
	}
	if err := fixture.db.Create(&ancestorEvents).Error; err != nil {
		t.Fatal(err)
	}

	createLogicalHistoryEvents(t, fixture.db, fixture.tenantID, childSession.ID, 4, 504)

	bundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: childSession.ID, TurnID: turnID,
		ExecutionID: source.ID, Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim",
		AuthoritativeHistorySequence: 504,
		Payload: map[string]any{
			"execution": map[string]any{
				"selectedRegion":    "cn-shanghai",
				"selectedClusterId": "cluster-a",
			},
			"workload": map[string]any{
				"resumeSnapshot": map[string]any{
					"artifactReferences": []map[string]any{{
						"sequence":    2,
						"artifactId":  ancestorArtifact.ID,
						"executionId": ancestorExecutionID,
					}},
				},
				"memoryReferences": []map[string]any{},
			},
		},
		CreatedAt: now,
	}
	sealTargetFailoverRecoveryBundle(t, &bundle)
	if err := fixture.db.Create(&bundle).Error; err != nil {
		t.Fatal(err)
	}
	recoveryReason := "suspend-resume"
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).
		Updates(map[string]any{"status": "suspended", "next_recovery_reason": recoveryReason}).Error; err != nil {
		t.Fatal(err)
	}

	observeTargetDRReadiness(
		t,
		fixture,
		destinationTarget.ID,
		routing.DRDomainForLocation("cn-shanghai", "cluster-a"),
		routing.DRDomainForLocation("cn-beijing", "cluster-b"),
		ancestorArtifactAt,
		true,
		false,
		false,
		ancestorArtifactAt.Add(time.Second),
	)

	result, err := fixture.service.FailoverExecution(
		ctx,
		fixture.tenantID,
		source.ID,
		createTargetFailoverFence(t, fixture.db, 20),
		FailoverReasonTargetUnreachable,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.DestinationExecutionTargetID != destinationTarget.ID {
		t.Fatalf("destination target = %s, want %s", result.DestinationExecutionTargetID, destinationTarget.ID)
	}
}

func seedFailoverArtifactAuthorityFixture(
	t *testing.T,
) (
	tenantExecutionPolicyFixture,
	persistence.AgentExecution,
	persistence.ExecutionTarget,
	uuid.UUID,
	string,
	time.Time,
) {
	t.Helper()
	fixture := newTenantExecutionPolicyFixture(t)
	sourceTarget, destinationTarget, group, sourceMember := configureFailoverTargets(t, fixture)
	turnID := uuid.New()
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "fail me over", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "leased", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", Generation: 1, RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	initialArtifactAt := now.Add(10 * time.Second)
	initialArtifact := createReadyExecutionArtifact(t, fixture, fixture.sessionID, &source.ID, initialArtifactAt)
	initialArtifactEvent := appendArtifactReadyEvent(t, fixture, fixture.sessionID, &source.ID, initialArtifact.ID, initialArtifactAt)

	payload := map[string]any{
		"execution": map[string]any{
			"selectedRegion":    "cn-shanghai",
			"selectedClusterId": "cluster-a",
		},
		"workload": map[string]any{
			"resumeSnapshot": map[string]any{
				"artifactReferences": []map[string]any{{
					"sequence":    initialArtifactEvent.Sequence,
					"artifactId":  initialArtifact.ID,
					"executionId": source.ID,
				}},
			},
			"memoryReferences": []map[string]any{},
		},
	}
	bundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		ExecutionID: source.ID, Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim",
		AuthoritativeHistorySequence: loadSessionLastEventSequence(t, fixture), Payload: payload,
		CreatedAt: now,
	}
	sealTargetFailoverRecoveryBundle(t, &bundle)
	if err := fixture.db.Create(&bundle).Error; err != nil {
		t.Fatal(err)
	}

	runtimeArtifactAt := initialArtifactAt.Add(20 * time.Second)
	runtimeArtifact := createReadyExecutionArtifact(t, fixture, fixture.sessionID, &source.ID, runtimeArtifactAt)
	appendArtifactReadyEvent(t, fixture, fixture.sessionID, &source.ID, runtimeArtifact.ID, runtimeArtifactAt)

	recoveryReason := "suspend-resume"
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).
		Updates(map[string]any{"status": "suspended", "next_recovery_reason": recoveryReason}).Error; err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.LastEventSequence <= bundle.AuthoritativeHistorySequence {
		t.Fatalf("session authoritative history did not advance past bundle snapshot: session=%d bundle=%d", session.LastEventSequence, bundle.AuthoritativeHistorySequence)
	}
	return fixture, source, destinationTarget, bundle.ID, routing.DRDomainForLocation("cn-shanghai", "cluster-a"), runtimeArtifactAt
}

func loadSessionLastEventSequence(t *testing.T, fixture tenantExecutionPolicyFixture) int64 {
	t.Helper()
	var session persistence.AgentSession
	if err := fixture.db.Select("last_event_sequence").
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session.LastEventSequence
}

func configureFailoverTargets(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
) (
	persistence.ExecutionTarget,
	persistence.ExecutionTarget,
	persistence.ExecutionTargetGroup,
	persistence.ExecutionTargetGroupMember,
) {
	t.Helper()
	ctx := context.Background()
	var source persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.executionTargetID).Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	tenantID := fixture.tenantID
	organizationID := fixture.organizationID
	destination := source
	destination.ID = uuid.New()
	destination.TenantID = &tenantID
	destination.OrganizationID = &organizationID
	destination.Name = "failover-destination-" + uuid.NewString()
	destination.Status = "active"
	if err := fixture.db.Create(&destination).Error; err != nil {
		t.Fatal(err)
	}
	router := routing.NewService(fixture.db)
	group, err := router.CreateGroup(ctx, routing.CreateGroupInput{
		TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "failover-group-" + uuid.NewString(), Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, MaxFailoverAttempts: targetFailoverIntPointer(3), HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceMember, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: source.ID,
		Region: "cn-shanghai", ClusterID: "cluster-a", Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: destination.ID,
		Region: "cn-beijing", ClusterID: "cluster-b", Priority: 20, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: source.ID, Status: routing.HealthUnreachable, CapacityStatus: routing.CapacityUnknown,
		Source: "failover-test", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	capacity := 10
	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: destination.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: &capacity, Source: "failover-test", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"requested_execution_target_id": source.ID, "execution_target_group_id": group.ID,
			"routing_policy_version": group.Version, "execution_target_id": source.ID,
		}).Error; err != nil {
		t.Fatal(err)
	}
	return source, destination, group, sourceMember
}

func targetFailoverIntPointer(value int) *int { return &value }

func createTargetFailoverFence(t *testing.T, db *gorm.DB, token int64) TargetFailoverFence {
	t.Helper()
	now := time.Now().UTC()
	fence := TargetFailoverFence{
		LeaseName: "target-failover-test-" + uuid.NewString(),
		HolderID:  "target-failover-holder-" + uuid.NewString(), FencingToken: token,
	}
	if err := db.Create(&persistence.ReconcilerLease{
		LeaseName: fence.LeaseName, HolderID: fence.HolderID, FencingToken: token,
		AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	return fence
}

func loadTargetFailoverExecutionTarget(t *testing.T, db *gorm.DB, targetID uuid.UUID) persistence.ExecutionTarget {
	t.Helper()
	var target persistence.ExecutionTarget
	if err := db.Where("id = ?", targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	return target
}

func sealTargetFailoverRecoveryBundle(t *testing.T, bundle *persistence.ExecutionRecoveryBundle) {
	t.Helper()
	if bundle.Payload == nil {
		bundle.Payload = make(map[string]any)
	}
	bundle.Payload["schemaVersion"] = bundle.SchemaVersion
	bundle.Payload["executionId"] = bundle.ExecutionID
	bundle.Payload["sessionId"] = bundle.SessionID
	bundle.Payload["turnId"] = bundle.TurnID
	bundle.Payload["generation"] = bundle.Generation
	bundle.Payload["recoveryReason"] = bundle.RecoveryReason
	if bundle.PreviousBundleID != nil {
		bundle.Payload["previousBundleId"] = *bundle.PreviousBundleID
	}
	bundle.Payload["authoritativeHistorySequence"] = bundle.AuthoritativeHistorySequence
	if _, ok := bundle.Payload["execution"].(map[string]any); !ok {
		bundle.Payload["execution"] = map[string]any{}
	}
	workload, ok := bundle.Payload["workload"].(map[string]any)
	if !ok {
		workload = make(map[string]any)
		bundle.Payload["workload"] = workload
	}
	workload["sessionId"] = bundle.SessionID
	workload["turnId"] = bundle.TurnID
	resumeSnapshot, ok := workload["resumeSnapshot"].(map[string]any)
	if !ok {
		resumeSnapshot = make(map[string]any)
		workload["resumeSnapshot"] = resumeSnapshot
	}
	resumeSnapshot["authoritativeHistorySequence"] = bundle.AuthoritativeHistorySequence
	normalized, sha256, err := sharedrecoverybundle.Encode(bundle.Payload)
	if err != nil {
		t.Fatalf("seal target failover Recovery Bundle: %v", err)
	}
	bundle.Payload = normalized
	bundle.PayloadSHA256 = sha256
}

func managedKubernetesFailoverTestConfiguration(maxActivePods int) map[string]any {
	return map[string]any{
		"apiServer": "https://kubernetes.example.com", "bearerToken": "kubernetes-api-token",
		"caCertificate": "fake-ca-for-client-factory", "namespace": "synara-failover",
		"manageNamespace": true, "image": "synara-agentd:test", "imagePullPolicy": "IfNotPresent",
		"controlPlaneUrl": "http://control-plane.test:3780", "allowInsecureControlPlane": true,
		"runnerCommand": []string{"provider-host", "run", "--jsonl"}, "maxActivePods": maxActivePods,
		"egressCidrs": []string{"0.0.0.0/0"}, "cpuRequest": "250m", "cpuLimit": "1",
		"memoryRequest": "256Mi", "memoryLimit": "1Gi", "workspaceSizeLimit": "2Gi",
		"quotaCpuRequests": "1", "quotaCpuLimits": "2", "quotaMemoryRequests": "2Gi", "quotaMemoryLimits": "4Gi",
	}
}

func managedKubernetesFailoverTargetCapabilities() map[string]any {
	capabilities := enabledProviderPolicyTestCapabilities()
	capabilities["workspaceModes"] = []string{"local", "worktree"}
	return capabilities
}

func setFailoverTargetRoutingPreferences(
	t *testing.T,
	db *gorm.DB,
	targetID uuid.UUID,
	preferences map[string]string,
) {
	t.Helper()
	var target persistence.ExecutionTarget
	if err := db.Where("id = ?", targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	capabilities := make(map[string]any, len(target.Capabilities))
	for key, value := range target.Capabilities {
		capabilities[key] = value
	}
	rawPolicy, _ := capabilities["providerPolicy"].(map[string]any)
	providerPolicy := make(map[string]any, len(rawPolicy)+1)
	for key, value := range rawPolicy {
		providerPolicy[key] = value
	}
	routingPreferences := make(map[string]any, len(preferences))
	for provider, preference := range preferences {
		routingPreferences[provider] = preference
	}
	providerPolicy["routingPreferences"] = routingPreferences
	capabilities["providerPolicy"] = providerPolicy
	if err := db.Model(&persistence.ExecutionTarget{}).
		Where("id = ?", targetID).
		Select("capabilities").
		Updates(&persistence.ExecutionTarget{Capabilities: capabilities}).Error; err != nil {
		t.Fatal(err)
	}
}

func stringPointer(value string) *string { return &value }
