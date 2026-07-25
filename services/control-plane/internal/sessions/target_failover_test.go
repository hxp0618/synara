package sessions

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

func TestFailoverExecutionCreatesSuccessorWithoutRewritingSourcePlacement(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, destinationTarget, group, sourceMember := configureFailoverTargets(t, fixture)
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
		ObservedAt:             now.Add(time.Second),
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
	now := time.Now().UTC().Add(time.Second)
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
		AuthoritativeHistorySequence: 0, Payload: map[string]any{}, PayloadSHA256: strings.Repeat("0", 64), CreatedAt: now,
	}
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
	})
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

	now := time.Now().UTC().Truncate(time.Second)
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
		PayloadSHA256: strings.Repeat("0", 64), CreatedAt: now,
	}
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
	ctx := context.Background()
	sourceTarget, destinationTarget, group, sourceMember := configureFailoverTargets(t, fixture)
	turnID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
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
		PayloadSHA256: strings.Repeat("0", 64), CreatedAt: now,
	}
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
	if _, err := routing.NewService(fixture.db).ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: destinationTarget.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(10), Source: "target-failover-artifact-test",
		ObservedAt: runtimeArtifactAt.Add(time.Second), TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
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
