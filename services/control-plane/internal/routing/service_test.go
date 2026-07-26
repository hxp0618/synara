package routing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
)

func TestSelectUsesFreshHealthCapacityAndImmutableSnapshot(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyBalanced, true, []string{"cn-shanghai"})
	first := fixture.createTarget(t, "cluster-a")
	second := fixture.createTarget(t, "cluster-b")
	fixture.addMember(t, group.ID, first.ID, "cn-shanghai", "cluster-a", 100, 100)
	fixture.addMember(t, group.ID, second.ID, "cn-shanghai", "cluster-b", 100, 100)
	fixture.observe(t, first.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 8)
	fixture.observe(t, second.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 1)

	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		RequiredTargetKind: "kubernetes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != second.ID || selection.RoutingReason != StrategyBalanced {
		t.Fatalf("selection = %#v", selection)
	}

	preferred := first.ID
	selection, err = fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		PreferredTargetID: &preferred,
	})
	if err != nil || selection.Target.ID != first.ID || selection.RoutingReason != "preferred-target" {
		t.Fatalf("preferred selection = %#v err=%v", selection, err)
	}

	session := persistence.AgentSession{ExecutionTargetID: second.ID}
	ApplySessionSelection(&session, selection, &preferred, nil)
	execution := persistence.AgentExecution{}
	ApplyExecutionSelection(&execution, selection)
	if session.ExecutionTargetID != first.ID || session.RequestedExecutionTargetID != first.ID ||
		session.ExecutionTargetGroupID == nil || *session.ExecutionTargetGroupID != group.ID ||
		execution.TargetGroupID == nil || *execution.TargetGroupID != group.ID ||
		execution.SelectedRegion == nil || *execution.SelectedRegion != "cn-shanghai" ||
		execution.SelectedClusterID == nil || *execution.SelectedClusterID != "cluster-a" {
		t.Fatalf("routing snapshots session=%#v execution=%#v", session, execution)
	}
}

func TestSelectUsesQueuedAndRecoveringExecutionsAsBalancedSoftPressure(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyBalanced, true, []string{"cn-shanghai"})
	pressured := fixture.createTarget(t, "queue-pressured")
	idle := fixture.createTarget(t, "queue-idle")
	fixture.addMember(t, group.ID, pressured.ID, "cn-shanghai", "cluster-a", 10, 100)
	fixture.addMember(t, group.ID, idle.ID, "cn-shanghai", "cluster-b", 10, 100)
	fixture.observe(t, pressured.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 1)
	fixture.observe(t, idle.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 1)
	fixture.createPressureExecution(t, pressured.ID, "queued")
	fixture.createPressureExecution(t, pressured.ID, "recovering")
	fixture.createPressureExecution(t, idle.ID, "completed")

	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != idle.ID || selection.QueuePressure.QueuedExecutionUnits != 0 ||
		selection.QueuePressure.EffectiveLoadRank != 1_000 {
		t.Fatalf("queue-pressure selection = %#v", selection)
	}
}

func TestSelectQueuePressureRemainsSoftAndPreservesPriorityStrategy(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	priority := fixture.createTarget(t, "priority-with-queue")
	idle := fixture.createTarget(t, "idle-lower-priority")
	fixture.addMember(t, group.ID, priority.ID, "cn-shanghai", "cluster-a", 1, 100)
	fixture.addMember(t, group.ID, idle.ID, "cn-shanghai", "cluster-b", 100, 100)
	fixture.observe(t, priority.ID, fixture.now, HealthHealthy, CapacityAvailable, 1, 0)
	fixture.observe(t, idle.ID, fixture.now, HealthHealthy, CapacityAvailable, 1, 0)
	fixture.createPressureExecution(t, priority.ID, "queued")

	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != priority.ID || selection.QueuePressure.QueuedExecutionUnits != 1 {
		t.Fatalf("priority queue-pressure selection = %#v", selection)
	}
}

func TestLockSelectionForCommitRejectsChangedQueuePressure(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyBalanced, true, nil)
	target := fixture.createTarget(t, "queue-pressure-commit")
	fixture.addMember(t, group.ID, target.ID, "cn-shanghai", "cluster-a", 10, 100)
	fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)
	request := SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	}
	selection, err := fixture.service.Select(context.Background(), fixture.db, request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.createPressureExecution(t, target.ID, "recovering")

	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		_, lockErr := fixture.service.LockSelectionForCommit(context.Background(), tx, request, selection)
		return lockErr
	})
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "target_routing_selection_stale" {
		t.Fatalf("queue-pressure commit err = %v", err)
	}
	if apiError.Details["reason"] != "queue-pressure-changed" ||
		apiError.Details["expectedQueuedExecutionUnits"] != int64(0) ||
		apiError.Details["actualQueuedExecutionUnits"] != int64(1) {
		t.Fatalf("queue-pressure stale details = %#v", apiError.Details)
	}
}

func TestAddMemberRejectsAmbiguousDRDomainComponents(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	target := fixture.createTarget(t, "cluster-a")

	for _, testCase := range []struct {
		name      string
		region    string
		clusterID string
		wantCode  string
	}{
		{name: "region", region: "cn/shanghai", clusterID: "cluster-a", wantCode: "invalid_target_group_member_region"},
		{name: "cluster", region: "cn-shanghai", clusterID: "cluster/a", wantCode: "invalid_target_group_member_cluster"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := fixture.service.AddMember(context.Background(), AddMemberInput{
				TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: target.ID,
				Region: testCase.region, ClusterID: testCase.clusterID, Priority: 10, Weight: 100,
			})
			if codeOf(err) != testCase.wantCode {
				t.Fatalf("ambiguous component err = %v", err)
			}
		})
	}
}

func TestSelectAppliesSoftProviderAffinityAfterRegionAndHealthBeforePriorityLoadAndWeight(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, []string{"cn-shanghai"})
	preferred := fixture.createTargetWithCapabilities(t, "cluster-a", map[string]any{
		"providerPolicy": map[string]any{
			"routingPreferences": map[string]any{"codex": "prefer"},
		},
	})
	avoided := fixture.createTargetWithCapabilities(t, "cluster-b", map[string]any{
		"providerPolicy": map[string]any{
			"routingPreferences": map[string]any{"codex": "avoid"},
		},
	})
	fixture.addMember(t, group.ID, preferred.ID, "cn-shanghai", "cluster-a", 20, 50)
	fixture.addMember(t, group.ID, avoided.ID, "cn-shanghai", "cluster-b", 10, 100)
	fixture.observe(t, preferred.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 6)
	fixture.observe(t, avoided.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != preferred.ID {
		t.Fatalf("provider-affinity selection = %#v", selection)
	}
}

func TestSelectProviderAffinityNeverOverridesPreferredRegion(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, []string{"cn-beijing"})
	preferredRegionTarget := fixture.createTargetWithCapabilities(t, "cluster-beijing", map[string]any{})
	affinityTarget := fixture.createTargetWithCapabilities(t, "cluster-shanghai", map[string]any{
		"providerPolicy": map[string]any{
			"routingPreferences": map[string]any{"codex": "prefer"},
		},
	})
	fixture.addMember(t, group.ID, preferredRegionTarget.ID, "cn-beijing", "cluster-beijing", 20, 100)
	fixture.addMember(t, group.ID, affinityTarget.ID, "cn-shanghai", "cluster-shanghai", 10, 100)
	fixture.observe(t, preferredRegionTarget.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)
	fixture.observe(t, affinityTarget.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != preferredRegionTarget.ID {
		t.Fatalf("preferred-region selection = %#v", selection)
	}
}

func TestSelectHardPolicyOverridesPreferredTargetAndRejectsAnEmptyAllowList(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, []string{"cn-shanghai"})
	preferred := fixture.createTarget(t, "policy-preferred-denied")
	allowed := fixture.createTarget(t, "policy-allowed")
	fixture.addMember(t, group.ID, preferred.ID, "cn-shanghai", "cluster-preferred", 1, 100)
	fixture.addMember(t, group.ID, allowed.ID, "cn-shanghai", "cluster-allowed", 100, 100)
	fixture.observe(t, preferred.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)
	fixture.observe(t, allowed.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

	document := schedulingpolicy.UnrestrictedDocument()
	document.Target = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{allowed.ID.String()}}
	policy, err := schedulingpolicy.NewService(fixture.db).UpdateTenant(
		context.Background(),
		fixture.tenantID,
		schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: document, ActorID: uuid.New()},
	)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		PreferredTargetID: &preferred.ID, Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != allowed.ID || selection.SchedulingPolicySnapshot.Tenant.Version != policy.Tenant.Version {
		t.Fatalf("hard-policy selection = %#v, policy = %#v", selection, policy)
	}

	document.Target = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{}}
	if _, err := schedulingpolicy.NewService(fixture.db).UpdateTenant(
		context.Background(),
		fixture.tenantID,
		schedulingpolicy.UpdateInput{ExpectedVersion: policy.Tenant.Version, Document: document, ActorID: uuid.New()},
	); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		Provider: "codex",
	})
	if codeOf(err) != "target_group_no_policy_eligible_destination" {
		t.Fatalf("empty Target allow-list err = %v", err)
	}
}

func TestLockSelectionForCommitRejectsSchedulingPolicyVersionChange(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	target := fixture.createTarget(t, "policy-commit-stale")
	fixture.addMember(t, group.ID, target.ID, "cn-shanghai", "cluster-a", 10, 100)
	fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

	document := schedulingpolicy.UnrestrictedDocument()
	policy, err := schedulingpolicy.NewService(fixture.db).UpdateTenant(
		context.Background(),
		fixture.tenantID,
		schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: document, ActorID: uuid.New()},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		Provider: "codex",
	}
	selection, err := fixture.service.Select(context.Background(), fixture.db, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schedulingpolicy.NewService(fixture.db).UpdateTenant(
		context.Background(),
		fixture.tenantID,
		schedulingpolicy.UpdateInput{
			ExpectedVersion: policy.Tenant.Version,
			Document:        document,
			ActorID:         uuid.New(),
		},
	); err != nil {
		t.Fatal(err)
	}

	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		_, lockErr := fixture.service.LockSelectionForCommit(context.Background(), tx, request, selection)
		return lockErr
	})
	if codeOf(err) != "target_routing_selection_stale" {
		t.Fatalf("policy version change commit err = %v", err)
	}
}

func TestDisasterRecoveryExcludesSourceAndHonorsCrossRegionPolicy(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, false, []string{"cn-shanghai"})
	shanghai := fixture.createTarget(t, "cluster-shanghai")
	beijing := fixture.createTarget(t, "cluster-beijing")
	fixture.addMember(t, group.ID, shanghai.ID, "cn-shanghai", "cluster-shanghai", 10, 100)
	fixture.addMember(t, group.ID, beijing.ID, "cn-beijing", "cluster-beijing", 20, 100)
	fixture.observe(t, shanghai.ID, fixture.now, HealthUnreachable, CapacityUnknown, 0, 0)
	fixture.observe(t, beijing.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

	sourceDRDomain := DRDomainForLocation("cn-shanghai", "cluster-shanghai")
	replicatedThroughAt := fixture.now.Add(-time.Minute)
	_, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		ExcludedTargetIDs: []uuid.UUID{shanghai.ID},
		SourceRegion:      "cn-shanghai", SourceClusterID: "cluster-shanghai", SourceDRDomain: sourceDRDomain,
		ReplicatedThroughAt: replicatedThroughAt, RequiredDRStores: DRStoreRequirements{Checkpoints: true},
		DisasterRecovery: true,
	})
	if codeOf(err) != "target_group_no_eligible_destination" {
		t.Fatalf("cross-region disabled err = %v", err)
	}

	if err := fixture.db.Model(&persistence.ExecutionTargetGroup{}).Where("id = ?", group.ID).
		Updates(map[string]any{"allow_cross_region": true, "version": 2}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.observeDRReadiness(t, beijing.ID, sourceDRDomain, DRDomainForLocation("cn-beijing", "cluster-beijing"), replicatedThroughAt, false, true, false)
	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		ExcludedTargetIDs: []uuid.UUID{shanghai.ID},
		SourceRegion:      "cn-shanghai", SourceClusterID: "cluster-shanghai", SourceDRDomain: sourceDRDomain,
		ReplicatedThroughAt: replicatedThroughAt, RequiredDRStores: DRStoreRequirements{Checkpoints: true},
		DisasterRecovery: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != beijing.ID || selection.RoutingReason != "disaster-recovery" || selection.Group.Version != 2 {
		t.Fatalf("failover selection = %#v", selection)
	}
	if selection.DRReadiness == nil || selection.DRReadiness.SourceDRDomain != sourceDRDomain || !selection.DRReadiness.CheckpointsReady {
		t.Fatalf("selection readiness = %#v", selection.DRReadiness)
	}
}

func TestSelectRequiresExplicitDRContextForCrossDomainReroute(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, []string{"cn-shanghai"})
	shanghai := fixture.createTarget(t, "cluster-shanghai")
	beijing := fixture.createTarget(t, "cluster-beijing")
	fixture.addMember(t, group.ID, shanghai.ID, "cn-shanghai", "cluster-shanghai", 10, 100)
	fixture.addMember(t, group.ID, beijing.ID, "cn-beijing", "cluster-beijing", 20, 100)
	fixture.observe(t, shanghai.ID, fixture.now, HealthUnreachable, CapacityUnknown, 0, 0)
	fixture.observe(t, beijing.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

	preferred := shanghai.ID
	_, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		PreferredTargetID: &preferred, RequiredDRStores: DRStoreRequirements{Checkpoints: true},
	})
	if codeOf(err) != "target_group_dr_readiness_context_required" {
		t.Fatalf("missing explicit source authority err = %v", err)
	}
}

func TestSelectRequiresFreshArtifactReadinessForCrossDomainReroute(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, []string{"cn-shanghai"})
	shanghai := fixture.createTarget(t, "cluster-shanghai")
	beijing := fixture.createTarget(t, "cluster-beijing")
	fixture.addMember(t, group.ID, shanghai.ID, "cn-shanghai", "cluster-shanghai", 10, 100)
	fixture.addMember(t, group.ID, beijing.ID, "cn-beijing", "cluster-beijing", 20, 100)
	fixture.observe(t, shanghai.ID, fixture.now, HealthUnreachable, CapacityUnknown, 0, 0)
	fixture.observe(t, beijing.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

	sourceDRDomain := DRDomainForLocation("cn-shanghai", "cluster-shanghai")
	replicatedThroughAt := fixture.now.Add(-time.Minute)

	fixture.observeDRReadiness(
		t,
		beijing.ID,
		sourceDRDomain,
		DRDomainForLocation("cn-beijing", "cluster-beijing"),
		replicatedThroughAt,
		false,
		false,
		false,
	)
	_, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		ExcludedTargetIDs:   []uuid.UUID{shanghai.ID},
		SourceRegion:        "cn-shanghai",
		SourceClusterID:     "cluster-shanghai",
		SourceDRDomain:      sourceDRDomain,
		ReplicatedThroughAt: replicatedThroughAt,
		RequiredDRStores:    DRStoreRequirements{Artifacts: true},
		DisasterRecovery:    true,
	})
	if codeOf(err) != "target_group_dr_readiness_unready" {
		t.Fatalf("artifacts-unready err = %v", err)
	}

	fixture.now = fixture.now.Add(time.Second)
	fixture.observeDRReadiness(
		t,
		beijing.ID,
		sourceDRDomain,
		DRDomainForLocation("cn-beijing", "cluster-beijing"),
		replicatedThroughAt.Add(-time.Second),
		true,
		false,
		false,
	)
	_, err = fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		ExcludedTargetIDs:   []uuid.UUID{shanghai.ID},
		SourceRegion:        "cn-shanghai",
		SourceClusterID:     "cluster-shanghai",
		SourceDRDomain:      sourceDRDomain,
		ReplicatedThroughAt: replicatedThroughAt,
		RequiredDRStores:    DRStoreRequirements{Artifacts: true},
		DisasterRecovery:    true,
	})
	if codeOf(err) != "target_group_dr_recovery_watermark_stale" {
		t.Fatalf("artifacts-stale err = %v", err)
	}

	fixture.now = fixture.now.Add(time.Second)
	fixture.observeDRReadiness(
		t,
		beijing.ID,
		sourceDRDomain,
		DRDomainForLocation("cn-beijing", "cluster-beijing"),
		replicatedThroughAt,
		true,
		false,
		false,
	)
	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		ExcludedTargetIDs:   []uuid.UUID{shanghai.ID},
		SourceRegion:        "cn-shanghai",
		SourceClusterID:     "cluster-shanghai",
		SourceDRDomain:      sourceDRDomain,
		ReplicatedThroughAt: replicatedThroughAt,
		RequiredDRStores:    DRStoreRequirements{Artifacts: true},
		DisasterRecovery:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != beijing.ID || selection.DRReadiness == nil || !selection.DRReadiness.ArtifactsReady {
		t.Fatalf("artifact-ready selection = %#v", selection)
	}
}

func TestSelectExcludesCandidatesInsideActiveLocationOutage(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, []string{"cn-shanghai"})
	preferred := fixture.createTarget(t, "cluster-a")
	fallback := fixture.createTarget(t, "cluster-b")
	fixture.addMember(t, group.ID, preferred.ID, "cn-shanghai", "cluster-a", 10, 100)
	fixture.addMember(t, group.ID, fallback.ID, "cn-beijing", "cluster-b", 20, 100)
	fixture.observe(t, preferred.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)
	fixture.observe(t, fallback.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)
	fixture.observeLocationOutage(
		t, "cn-shanghai", "cluster-a", LocationStatusDraining, fixture.now, time.Minute,
	)

	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != fallback.ID {
		t.Fatalf("selection with active location outage = %#v", selection)
	}
}

func TestSelectFailsClosedWhenEveryHealthyCandidateIsInsideLocationOutage(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	target := fixture.createTarget(t, "cluster-a")
	fixture.addMember(t, group.ID, target.ID, "cn-shanghai", "cluster-a", 10, 100)
	fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)
	fixture.observeLocationOutage(
		t, "cn-shanghai", "", LocationStatusUnreachable, fixture.now, time.Minute,
	)

	_, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if codeOf(err) != "target_group_location_outage_active" {
		t.Fatalf("location outage err = %v", err)
	}
}

func TestObserveLocationOutageRejectsFutureObservedAt(t *testing.T) {
	fixture := newRoutingFixture(t)

	_, err := fixture.service.ObserveLocationOutage(context.Background(), LocationOutageObservation{
		TenantID:          fixture.tenantID,
		Region:            "cn-shanghai",
		ClusterID:         "cluster-a",
		Status:            LocationStatusDraining,
		PublisherIdentity: "routing-test",
		ObservedAt:        fixture.now.Add(time.Second),
		TTL:               time.Minute,
	})
	if codeOf(err) != "invalid_location_outage_observed_at" {
		t.Fatalf("future observedAt err = %v", err)
	}
}

func TestObserveHealthRejectsFutureObservedAtWithoutAdvancingAuthority(t *testing.T) {
	fixture := newRoutingFixture(t)
	target := fixture.createTarget(t, "cluster-a")
	first := fixture.observe(t, target.ID, fixture.now.Add(-2*time.Second), HealthHealthy, CapacityAvailable, 10, 0)

	_, err := fixture.service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: target.ID, Status: HealthUnreachable, CapacityStatus: CapacityUnknown,
		Source: "future-publisher", ObservedAt: fixture.now.Add(time.Second), TTL: time.Minute,
	})
	if codeOf(err) != "invalid_target_health_observed_at" {
		t.Fatalf("future health observedAt err = %v", err)
	}

	advanced := fixture.observe(t, target.ID, fixture.now.Add(-time.Second), HealthDegraded, CapacityAvailable, 10, 0)
	if advanced.Version != first.Version+1 || advanced.Status != HealthDegraded {
		t.Fatalf("health authority was displaced by future observation: first=%#v advanced=%#v", first, advanced)
	}
}

func TestObserveDRReadinessRejectsFutureObservedAtWithoutAdvancingAuthority(t *testing.T) {
	fixture := newRoutingFixture(t)
	target := fixture.createTarget(t, "cluster-a")
	sourceDRDomain := DRDomainForLocation("cn-shanghai", "source")
	destinationDRDomain := DRDomainForLocation("cn-beijing", "cluster-a")
	firstObservedAt := fixture.now.Add(-2 * time.Second)
	first, err := fixture.service.ObserveDRReadiness(context.Background(), DRReadinessObservation{
		ExecutionTargetID: target.ID, SourceDRDomain: sourceDRDomain, DRDomain: destinationDRDomain,
		ReplicatedThroughAt: fixture.now.Add(-time.Minute), CheckpointsReady: true,
		PublisherIdentity: "routing-test", ObservedAt: firstObservedAt, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = fixture.service.ObserveDRReadiness(context.Background(), DRReadinessObservation{
		ExecutionTargetID: target.ID, SourceDRDomain: sourceDRDomain, DRDomain: "future/domain",
		ReplicatedThroughAt: fixture.now.Add(-time.Minute), CheckpointsReady: true,
		PublisherIdentity: "future-publisher", ObservedAt: fixture.now.Add(time.Second), TTL: time.Minute,
	})
	if codeOf(err) != "invalid_target_dr_readiness_observed_at" {
		t.Fatalf("future readiness observedAt err = %v", err)
	}

	advanced, err := fixture.service.ObserveDRReadiness(context.Background(), DRReadinessObservation{
		ExecutionTargetID: target.ID, SourceDRDomain: sourceDRDomain, DRDomain: destinationDRDomain,
		ReplicatedThroughAt: fixture.now.Add(-30 * time.Second), CheckpointsReady: true,
		PublisherIdentity: "routing-test", ObservedAt: fixture.now.Add(-time.Second), TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Version != first.Version+1 || advanced.DRDomain != destinationDRDomain {
		t.Fatalf("readiness authority was displaced by future observation: first=%#v advanced=%#v", first, advanced)
	}
}

func TestSelectRejectsFutureHealthAuthority(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	target := fixture.createTarget(t, "cluster-a")
	fixture.addMember(t, group.ID, target.ID, "cn-shanghai", "cluster-a", 10, 100)
	futureObservedAt := fixture.now.Add(time.Minute)
	if err := fixture.db.Create(&persistence.ExecutionTargetHealth{
		ExecutionTargetID: target.ID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(10), Source: "bypassed-service",
		ObservedAt: futureObservedAt, ExpiresAt: futureObservedAt.Add(time.Minute), Version: 1, UpdatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if codeOf(err) != "target_group_no_eligible_destination" {
		t.Fatalf("future health selection err = %v", err)
	}
}

func TestSelectBindsDRReadinessToCurrentDestinationDomain(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	target := fixture.createTarget(t, "destination")
	fixture.addMember(t, group.ID, target.ID, "cn-beijing", "cluster-b", 10, 100)
	fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)
	sourceDRDomain := DRDomainForLocation("cn-shanghai", "cluster-a")
	watermark := fixture.now.Add(-time.Minute)

	fixture.observeDRReadiness(t, target.ID, sourceDRDomain, "cn-beijing/wrong-cluster", watermark, false, true, false)
	request := SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		SourceRegion: "cn-shanghai", SourceClusterID: "cluster-a", SourceDRDomain: sourceDRDomain,
		ReplicatedThroughAt: watermark, RequiredDRStores: DRStoreRequirements{Checkpoints: true}, DisasterRecovery: true,
	}
	_, err := fixture.service.Select(context.Background(), fixture.db, request)
	if codeOf(err) != "target_group_dr_destination_domain_mismatch" {
		t.Fatalf("wrong destination domain err = %v", err)
	}

	fixture.now = fixture.now.Add(time.Second)
	correct := fixture.observeDRReadiness(
		t, target.ID, sourceDRDomain, DRDomainForLocation("cn-beijing", "cluster-b"), watermark, false, true, false,
	)
	selection, err := fixture.service.Select(context.Background(), fixture.db, request)
	if err != nil || selection.DRReadiness == nil || selection.DRReadiness.Version != correct.Version {
		t.Fatalf("correct destination authority selection = %#v err=%v", selection, err)
	}

	if err := fixture.db.Model(&persistence.ExecutionTargetGroupMember{}).
		Where("tenant_id = ? AND target_group_id = ? AND execution_target_id = ?", fixture.tenantID, group.ID, target.ID).
		Updates(map[string]any{"region": "cn-guangdong", "cluster_id": "cluster-c", "version": gorm.Expr("version + 1")}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Select(context.Background(), fixture.db, request)
	if codeOf(err) != "target_group_dr_destination_domain_mismatch" {
		t.Fatalf("moved membership stale destination err = %v", err)
	}

	fixture.now = fixture.now.Add(time.Second)
	fixture.observeDRReadiness(
		t, target.ID, sourceDRDomain, DRDomainForLocation("cn-guangdong", "cluster-c"), watermark, false, true, false,
	)
	selection, err = fixture.service.Select(context.Background(), fixture.db, request)
	if err != nil || selection.Member.Region != "cn-guangdong" {
		t.Fatalf("moved membership refreshed authority selection = %#v err=%v", selection, err)
	}
}

func TestSelectRejectsFutureDRReadinessAuthority(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	target := fixture.createTarget(t, "destination")
	fixture.addMember(t, group.ID, target.ID, "cn-beijing", "cluster-b", 10, 100)
	fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)
	sourceDRDomain := DRDomainForLocation("cn-shanghai", "cluster-a")
	watermark := fixture.now.Add(-time.Minute)
	futureObservedAt := fixture.now.Add(time.Minute)
	if err := fixture.db.Create(&persistence.ExecutionTargetDRReadiness{
		ExecutionTargetID: target.ID, SourceDRDomain: sourceDRDomain,
		DRDomain: DRDomainForLocation("cn-beijing", "cluster-b"), ReplicatedThroughAt: watermark,
		CheckpointsReady: true, PublisherIdentity: "bypassed-service",
		ObservedAt: futureObservedAt, ExpiresAt: futureObservedAt.Add(time.Minute), Version: 1, UpdatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
		SourceRegion: "cn-shanghai", SourceClusterID: "cluster-a", SourceDRDomain: sourceDRDomain,
		ReplicatedThroughAt: watermark, RequiredDRStores: DRStoreRequirements{Checkpoints: true}, DisasterRecovery: true,
	})
	if codeOf(err) != "target_group_dr_readiness_observed_in_future" {
		t.Fatalf("future readiness selection err = %v", err)
	}
}

func TestSelectIgnoresFutureLocationOutageRows(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	target := fixture.createTarget(t, "cluster-a")
	fixture.addMember(t, group.ID, target.ID, "cn-shanghai", "cluster-a", 10, 100)
	fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

	futureObservedAt := fixture.now.Add(time.Minute)
	if err := fixture.db.Create(&persistence.ExecutionLocationOutage{
		TenantID:          fixture.tenantID,
		Region:            "cn-shanghai",
		ClusterID:         "cluster-a",
		Status:            LocationStatusUnreachable,
		PublisherIdentity: "bypassed-service",
		ObservedAt:        futureObservedAt,
		ExpiresAt:         futureObservedAt.Add(time.Minute),
		Version:           1,
		UpdatedAt:         fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Target.ID != target.ID {
		t.Fatalf("future location outage should be ignored, selection = %#v", selection)
	}
}

func TestPreferredTargetBypassesDRReadinessOnlyWhenLocationStillMatchesFrozenSource(t *testing.T) {
	t.Run("same location still bypasses", func(t *testing.T) {
		fixture := newRoutingFixture(t)
		group := fixture.createGroup(t, StrategyPriority, true, []string{"cn-shanghai"})
		target := fixture.createTarget(t, "cluster-shanghai")
		fixture.addMember(t, group.ID, target.ID, "cn-shanghai", "cluster-shanghai", 10, 100)
		fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

		preferred := target.ID
		selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
			PreferredTargetID: &preferred,
			SourceRegion:      "cn-shanghai", SourceClusterID: "cluster-shanghai",
			SourceDRDomain:      DRDomainForLocation("cn-shanghai", "cluster-shanghai"),
			ReplicatedThroughAt: fixture.now.Add(-time.Minute),
			RequiredDRStores:    DRStoreRequirements{Checkpoints: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if selection.Target.ID != target.ID || selection.RoutingReason != "preferred-target" || selection.DRReadiness != nil {
			t.Fatalf("preferred same-location selection = %#v", selection)
		}
	})

	t.Run("same target moved location now requires readiness", func(t *testing.T) {
		fixture := newRoutingFixture(t)
		group := fixture.createGroup(t, StrategyPriority, true, []string{"cn-shanghai"})
		target := fixture.createTarget(t, "cluster-shanghai")
		fixture.addMember(t, group.ID, target.ID, "cn-beijing", "cluster-beijing", 10, 100)
		fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 10, 0)

		preferred := target.ID
		sourceDRDomain := DRDomainForLocation("cn-shanghai", "cluster-shanghai")
		replicatedThroughAt := fixture.now.Add(-time.Minute)
		_, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
			PreferredTargetID:   &preferred,
			SourceRegion:        "cn-shanghai",
			SourceClusterID:     "cluster-shanghai",
			SourceDRDomain:      sourceDRDomain,
			ReplicatedThroughAt: replicatedThroughAt,
			RequiredDRStores:    DRStoreRequirements{Checkpoints: true},
		})
		if codeOf(err) != "target_group_dr_readiness_required" {
			t.Fatalf("moved preferred target err = %v", err)
		}

		fixture.observeDRReadiness(
			t,
			target.ID,
			sourceDRDomain,
			DRDomainForLocation("cn-beijing", "cluster-beijing"),
			replicatedThroughAt,
			false,
			true,
			false,
		)
		selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
			PreferredTargetID:   &preferred,
			SourceRegion:        "cn-shanghai",
			SourceClusterID:     "cluster-shanghai",
			SourceDRDomain:      sourceDRDomain,
			ReplicatedThroughAt: replicatedThroughAt,
			RequiredDRStores:    DRStoreRequirements{Checkpoints: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if selection.Target.ID != target.ID || selection.RoutingReason != "preferred-target" ||
			selection.DRReadiness == nil || selection.DRReadiness.SourceDRDomain != sourceDRDomain {
			t.Fatalf("preferred moved-location selection = %#v", selection)
		}
	})
}

func TestHealthObservationIsMonotonicAndExpiresClosed(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyPriority, true, nil)
	target := fixture.createTarget(t, "cluster-a")
	fixture.addMember(t, group.ID, target.ID, "cn-shanghai", "cluster-a", 10, 100)
	first := fixture.observe(t, target.ID, fixture.now, HealthHealthy, CapacityAvailable, 4, 0)
	if first.Version != 1 {
		t.Fatalf("first version = %d", first.Version)
	}
	_, err := fixture.service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: target.ID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(4), Source: "test", ObservedAt: fixture.now, TTL: time.Minute,
	})
	if codeOf(err) != "target_health_observation_stale" {
		t.Fatalf("stale observation err = %v", err)
	}
	fixture.now = fixture.now.Add(time.Second)
	second := fixture.observe(t, target.ID, fixture.now, HealthDegraded, CapacityAvailable, 4, 1)
	if second.Version != 2 || second.Status != HealthDegraded {
		t.Fatalf("second observation = %#v", second)
	}

	fixture.service.now = func() time.Time { return fixture.now.Add(2 * time.Minute) }
	_, err = fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if codeOf(err) != "target_group_no_eligible_destination" {
		t.Fatalf("expired health err = %v", err)
	}
}

type routingFixture struct {
	t              *testing.T
	db             *gorm.DB
	service        *Service
	tenantID       uuid.UUID
	organizationID uuid.UUID
	now            time.Time
}

func newRoutingFixture(t *testing.T) *routingFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.Tenant{}, &persistence.Organization{}, &persistence.AuditLog{}, &persistence.ExecutionTarget{},
		&persistence.ExecutionTargetGroup{}, &persistence.ExecutionTargetGroupMember{},
		&persistence.ExecutionTargetHealth{}, &persistence.ExecutionTargetDRReadiness{},
		&persistence.ExecutionLocationOutage{},
		&persistence.AgentExecution{},
		&persistence.ExecutionSchedulingPolicyHead{}, &persistence.ExecutionSchedulingPolicyRevision{},
		&persistence.ExecutionSchedulingPolicyRule{}, &persistence.ExecutionSchedulingPolicyRuleValue{},
	); err != nil {
		t.Fatal(err)
	}
	fixture := &routingFixture{
		t: t, db: db, service: NewService(db), tenantID: uuid.New(), organizationID: uuid.New(),
		now: time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC),
	}
	fixture.service.now = func() time.Time { return fixture.now }
	if err := db.Create(&persistence.Tenant{ID: fixture.tenantID, Name: "routing tenant", Slug: "routing-" + uuid.NewString(), Status: "active", CreatedAt: fixture.now, UpdatedAt: fixture.now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.Organization{ID: fixture.organizationID, TenantID: fixture.tenantID, Name: "routing org", Slug: "routing-org-" + uuid.NewString(), CreatedAt: fixture.now, UpdatedAt: fixture.now}).Error; err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *routingFixture) createGroup(t *testing.T, strategy string, crossRegion bool, regions []string) persistence.ExecutionTargetGroup {
	t.Helper()
	group, err := f.service.CreateGroup(context.Background(), CreateGroupInput{
		TenantID: f.tenantID, OrganizationID: &f.organizationID, Name: "group-" + uuid.NewString(),
		Strategy: strategy, PreferredRegions: regions, AllowCrossRegion: crossRegion,
		MaxFailoverAttempts: intPointer(2), HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func (f *routingFixture) createTarget(t *testing.T, name string) persistence.ExecutionTarget {
	t.Helper()
	return f.createTargetWithCapabilities(t, name, map[string]any{})
}

func (f *routingFixture) createTargetWithCapabilities(
	t *testing.T,
	name string,
	capabilities map[string]any,
) persistence.ExecutionTarget {
	t.Helper()
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &f.tenantID, OrganizationID: &f.organizationID, Kind: "kubernetes",
		Name: name, Status: "active", Capabilities: capabilities, CreatedAt: f.now, UpdatedAt: f.now,
	}
	if err := f.db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	return target
}

func (f *routingFixture) addMember(t *testing.T, groupID, targetID uuid.UUID, region, cluster string, priority, weight int) {
	t.Helper()
	if _, err := f.service.AddMember(context.Background(), AddMemberInput{
		TenantID: f.tenantID, TargetGroupID: groupID, ExecutionTargetID: targetID,
		Region: region, ClusterID: cluster, Priority: priority, Weight: weight,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *routingFixture) observe(
	t *testing.T,
	targetID uuid.UUID,
	observedAt time.Time,
	status, capacity string,
	available, allocated int,
) persistence.ExecutionTargetHealth {
	t.Helper()
	observation, err := f.service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: targetID, Status: status, CapacityStatus: capacity,
		AvailableCapacityUnits: intPointer(available), AllocatedCapacityUnits: allocated,
		Source: "routing-test", ObservedAt: observedAt, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func (f *routingFixture) createPressureExecution(t *testing.T, targetID uuid.UUID, status string) {
	t.Helper()
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: f.tenantID, SessionID: uuid.New(), TurnID: uuid.New(),
		Attempt: 1, Status: status, ExecutionTargetID: targetID, TargetKind: "kubernetes",
		Generation: 0, RequestedBy: uuid.New(), QueuedAt: f.now,
	}
	if err := f.db.Create(&execution).Error; err != nil {
		t.Fatal(err)
	}
}

func (f *routingFixture) observeDRReadiness(
	t *testing.T,
	targetID uuid.UUID,
	sourceDRDomain, drDomain string,
	replicatedThroughAt time.Time,
	artifactsReady, checkpointsReady, memoryReady bool,
) persistence.ExecutionTargetDRReadiness {
	t.Helper()
	observation, err := f.service.ObserveDRReadiness(context.Background(), DRReadinessObservation{
		ExecutionTargetID:   targetID,
		SourceDRDomain:      sourceDRDomain,
		DRDomain:            drDomain,
		ReplicatedThroughAt: replicatedThroughAt,
		ArtifactsReady:      artifactsReady,
		CheckpointsReady:    checkpointsReady,
		MemoryReady:         memoryReady,
		PublisherIdentity:   "routing-test",
		ObservedAt:          f.now,
		TTL:                 time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func (f *routingFixture) observeLocationOutage(
	t *testing.T,
	region, clusterID, status string,
	observedAt time.Time,
	ttl time.Duration,
) persistence.ExecutionLocationOutage {
	t.Helper()
	observation, err := f.service.ObserveLocationOutage(context.Background(), LocationOutageObservation{
		TenantID:          f.tenantID,
		Region:            region,
		ClusterID:         clusterID,
		Status:            status,
		PublisherIdentity: "routing-test",
		ObservedAt:        observedAt,
		TTL:               ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func codeOf(err error) string {
	if err == nil {
		return ""
	}
	var item *problem.Error
	if errors.As(err, &item) {
		return item.Code
	}
	return ""
}

func intPointer(value int) *int { return &value }
