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
	second := fixture.observe(t, target.ID, fixture.now.Add(time.Second), HealthDegraded, CapacityAvailable, 4, 1)
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
		&persistence.Tenant{}, &persistence.Organization{}, &persistence.ExecutionTarget{},
		&persistence.ExecutionTargetGroup{}, &persistence.ExecutionTargetGroupMember{},
		&persistence.ExecutionTargetHealth{}, &persistence.ExecutionTargetDRReadiness{},
		&persistence.ExecutionLocationOutage{},
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
