package executiontargets

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestDisableManagedKubernetesTargetSharesReconcilerLockAndIsReplaySafe(t *testing.T) {
	fixture := newKubernetesTargetLifecycleFixture(t)
	ctx := context.Background()

	release, acquired, err := fixture.service.tryKubernetesReconcilerLock(ctx)
	if err != nil || !acquired {
		t.Fatalf("acquire Reconciler lock = acquired=%t err=%v", acquired, err)
	}
	reconciler := NewKubernetesReconciler(fixture.service, KubernetesReconcilerConfig{}, nil)
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("contending Reconciler should skip the held cycle lock: %v", err)
	}
	_, _, err = fixture.disable(ctx)
	assertExecutionTargetProblem(t, err, 409, "kubernetes_reconciler_busy")
	release()

	disabled, replayed, err := fixture.disable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || disabled.Status != "disabled" || disabled.ID != fixture.targetID {
		t.Fatalf("disabled Target = %#v replayed=%t", disabled, replayed)
	}

	var audits []persistence.AuditLog
	if err := fixture.db.Where(
		"tenant_id = ? AND resource_id = ? AND action = ?",
		fixture.tenantID,
		fixture.targetID,
		"execution_target.kubernetes_disabled",
	).Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].ActorID == nil || *audits[0].ActorID != fixture.principal.UserID ||
		audits[0].RequestID != "target-disable-test" {
		t.Fatalf("disable audit = %#v", audits)
	}
	if audits[0].Metadata["healthVersion"] != float64(1) && audits[0].Metadata["healthVersion"] != int64(1) {
		t.Fatalf("disable audit metadata = %#v", audits[0].Metadata)
	}

	replayedTarget, replayed, err := fixture.disable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || replayedTarget.Status != "disabled" || replayedTarget.UpdatedAt != disabled.UpdatedAt {
		t.Fatalf("disable replay = %#v replayed=%t", replayedTarget, replayed)
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND resource_id = ? AND action = ?", fixture.tenantID, fixture.targetID, "execution_target.kubernetes_disabled").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("disable audit count = %d, want 1", auditCount)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("disabled Target should be skipped by Reconciler: %v", err)
	}
}

func TestDisableManagedKubernetesTargetFailsClosedOnEveryDurableBlocker(t *testing.T) {
	tests := []struct {
		name string
		code string
		seed func(*testing.T, *kubernetesTargetLifecycleFixture)
	}{
		{
			name: "active routing member",
			code: "kubernetes_target_routing_active",
			seed: func(t *testing.T, fixture *kubernetesTargetLifecycleFixture) {
				groupID := fixture.createGroup(t)
				fixture.createMember(t, groupID, routing.MemberStatusActive)
			},
		},
		{
			name: "fixed Target Session",
			code: "kubernetes_target_fixed_session_active",
			seed: func(t *testing.T, fixture *kubernetesTargetLifecycleFixture) {
				fixture.createSession(t, nil)
			},
		},
		{
			name: "group-routed nonterminal Execution",
			code: "kubernetes_target_execution_active",
			seed: func(t *testing.T, fixture *kubernetesTargetLifecycleFixture) {
				groupID := fixture.createGroup(t)
				memberID := fixture.createMember(t, groupID, routing.MemberStatusActive)
				sessionID := fixture.createSession(t, &groupID)
				fixture.createQueuedExecution(t, sessionID, groupID)
				fixture.disableMember(t, memberID)
			},
		},
		{
			name: "active Worker Pool",
			code: "kubernetes_target_worker_pool_active",
			seed: func(t *testing.T, fixture *kubernetesTargetLifecycleFixture) {
				if err := fixture.db.Create(&persistence.WorkerPool{
					ID: uuid.New(), TenantID: &fixture.tenantID, ExecutionTargetID: fixture.targetID,
					Name: "target-disable-pool", Mode: "per-execution", CapacityClass: "standard",
					SchedulingTemplate: map[string]any{}, Status: "active", Version: 1,
					CreatedAt: fixture.now, UpdatedAt: fixture.now,
				}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "physical Workspace materialization",
			code: "kubernetes_target_workspace_active",
			seed: func(t *testing.T, fixture *kubernetesTargetLifecycleFixture) {
				groupID := fixture.createGroup(t)
				memberID := fixture.createMember(t, groupID, routing.MemberStatusActive)
				sessionID := fixture.createSession(t, &groupID)
				fixture.disableMember(t, memberID)
				workspaceID := uuid.New()
				if err := fixture.db.Create(&persistence.RemoteWorkspace{
					ID: workspaceID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
					ProjectID: fixture.projectID, SessionID: sessionID, ExecutionTargetID: fixture.targetID,
					WorkspaceMode: "empty", State: "ready", DefaultBranch: "main",
					CreatedAt: fixture.now, UpdatedAt: fixture.now,
				}).Error; err != nil {
					t.Fatal(err)
				}
				if err := fixture.db.Create(&persistence.WorkspaceMaterialization{
					ID: uuid.New(), TenantID: fixture.tenantID, WorkspaceID: workspaceID,
					OrganizationID: fixture.organizationID, ProjectID: fixture.projectID, SessionID: sessionID,
					ExecutionTargetID: fixture.targetID, TargetKind: "kubernetes", StorageScope: "pod",
					LayoutVersion: 3, IncarnationID: uuid.New(), State: "active",
					CreatedAt: fixture.now, UpdatedAt: fixture.now,
				}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "live Worker incarnation",
			code: "kubernetes_target_worker_active",
			seed: func(t *testing.T, fixture *kubernetesTargetLifecycleFixture) {
				if err := fixture.db.Create(&persistence.WorkerInstance{
					ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(),
					ExecutionTargetID: fixture.targetID, TargetKind: "kubernetes", WorkerMode: "general-pool",
					ClusterID: "target-disable", Namespace: "target-disable", PodName: "worker",
					Version: "test", ProtocolVersion: 2, Capabilities: map[string]any{},
					CompatibilityStatus: "unknown", LeaseSupported: true, FencingSupported: true,
					AuthTokenHash: []byte(uuid.NewString()), Status: "online", AdministrativeStatus: "active",
					RegisteredAt: fixture.now, LastHeartbeatAt: fixture.now,
				}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "nonzero reconciled capacity",
			code: "kubernetes_target_health_not_idle",
			seed: func(t *testing.T, fixture *kubernetesTargetLifecycleFixture) {
				available := 10
				if _, err := routing.NewService(fixture.db).ObserveHealth(context.Background(), routing.HealthObservation{
					ExecutionTargetID: fixture.targetID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacitySaturated,
					AvailableCapacityUnits: &available, AllocatedCapacityUnits: 1,
					ReservationAuthority: &routing.ReservationAuthorityObservation{Mode: routing.ReservationAuthorityExactActiveV1},
					Source:               managedKubernetesRoutingPublisherPrefix + "target-disable-test",
					ObservedAt:           time.Now().UTC(), TTL: time.Hour,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stale reconciler health",
			code: "kubernetes_target_health_not_idle",
			seed: func(t *testing.T, fixture *kubernetesTargetLifecycleFixture) {
				fixture.useStaleTargetHealth(t)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newKubernetesTargetLifecycleFixture(t)
			test.seed(t, fixture)
			_, _, err := fixture.disable(context.Background())
			assertExecutionTargetProblem(t, err, 409, test.code)
			var target persistence.ExecutionTarget
			if loadErr := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; loadErr != nil {
				t.Fatal(loadErr)
			}
			if target.Status != "active" {
				t.Fatalf("blocked disable changed Target status to %q", target.Status)
			}
		})
	}
}

func TestDisableManagedKubernetesTargetRequiresTenantOwnedKubernetesAndFreshHealth(t *testing.T) {
	fixture := newKubernetesTargetLifecycleFixture(t)
	ctx := context.Background()

	withoutHealthID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: withoutHealthID, TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "kubernetes", Name: "target-disable-without-health", Status: "active", Capabilities: map[string]any{},
		CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, _, err := fixture.service.DisableManagedKubernetesTarget(
		ctx, fixture.principal, fixture.tenantID, withoutHealthID, "target-disable-test", "127.0.0.1",
	)
	assertExecutionTargetProblem(t, err, 409, "kubernetes_target_health_unavailable")

	localID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: localID, TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "local", Name: "target-disable-local", Status: "active", Capabilities: map[string]any{},
		CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, _, err = fixture.service.DisableManagedKubernetesTarget(
		ctx, fixture.principal, fixture.tenantID, localID, "target-disable-test", "127.0.0.1",
	)
	assertExecutionTargetProblem(t, err, 409, "managed_kubernetes_target_required")

	sharedID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: sharedID, Kind: "kubernetes", Name: "target-disable-shared", Status: "active",
		Capabilities: map[string]any{}, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, _, err = fixture.service.DisableManagedKubernetesTarget(
		ctx, fixture.principal, fixture.tenantID, sharedID, "target-disable-test", "127.0.0.1",
	)
	assertExecutionTargetProblem(t, err, 404, "execution_target_not_found")
}

type kubernetesTargetLifecycleFixture struct {
	db             *gorm.DB
	service        *Service
	principal      identity.Principal
	tenantID       uuid.UUID
	organizationID uuid.UUID
	projectID      uuid.UUID
	targetID       uuid.UUID
	now            time.Time
}

func newKubernetesTargetLifecycleFixture(t *testing.T) *kubernetesTargetLifecycleFixture {
	t.Helper()
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "target-disable-test-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID := uuid.New()
	targetID := uuid.New()
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Target disable", DefaultBranch: "main", Visibility: "organization",
			CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.ExecutionTarget{
			ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
			Kind: "kubernetes", Name: "target-disable", Status: "active",
			ConfigurationEncrypted: []byte("encrypted-target-configuration"), Capabilities: map[string]any{},
			CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	available := 10
	if _, err := routing.NewService(store.DB()).ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: targetID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: &available,
		ReservationAuthority:   &routing.ReservationAuthorityObservation{Mode: routing.ReservationAuthorityExactActiveV1},
		Source:                 managedKubernetesRoutingPublisherPrefix + "target-disable-test",
		ObservedAt:             now, TTL: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	return &kubernetesTargetLifecycleFixture{
		db: store.DB(), service: NewService(store.DB(), profile, nil),
		principal: identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		tenantID:  domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, targetID: targetID, now: now,
	}
}

func (f *kubernetesTargetLifecycleFixture) useStaleTargetHealth(t *testing.T) {
	t.Helper()
	targetID := uuid.New()
	observedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	if err := f.db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &f.tenantID, OrganizationID: &f.organizationID,
		Kind: "kubernetes", Name: "target-disable-stale-" + targetID.String(), Status: "active",
		Capabilities: map[string]any{}, CreatedAt: observedAt, UpdatedAt: observedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	available := 10
	if _, err := routing.NewService(f.db).ObserveHealth(context.Background(), routing.HealthObservation{
		ExecutionTargetID: targetID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: &available,
		ReservationAuthority:   &routing.ReservationAuthorityObservation{Mode: routing.ReservationAuthorityExactActiveV1},
		Source:                 managedKubernetesRoutingPublisherPrefix + "target-disable-test",
		ObservedAt:             observedAt, TTL: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	f.targetID = targetID
}

func (f *kubernetesTargetLifecycleFixture) disable(ctx context.Context) (Target, bool, error) {
	return f.service.DisableManagedKubernetesTarget(
		ctx, f.principal, f.tenantID, f.targetID, "target-disable-test", "127.0.0.1",
	)
}

func (f *kubernetesTargetLifecycleFixture) createGroup(t *testing.T) uuid.UUID {
	t.Helper()
	groupID := uuid.New()
	if err := f.db.Create(&persistence.ExecutionTargetGroup{
		ID: groupID, TenantID: f.tenantID, OrganizationID: &f.organizationID,
		Name: "target-disable-" + groupID.String(), Strategy: routing.StrategyPriority,
		PreferredRegions: []string{}, HealthMaxStalenessSeconds: 90,
		Status: routing.GroupStatusActive, Version: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return groupID
}

func (f *kubernetesTargetLifecycleFixture) createMember(t *testing.T, groupID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	memberID := uuid.New()
	if err := f.db.Create(&persistence.ExecutionTargetGroupMember{
		ID: memberID, TenantID: f.tenantID, TargetGroupID: groupID, ExecutionTargetID: f.targetID,
		Region: "local", ClusterID: "orbstack", Priority: 100, Weight: 100,
		Status: status, Version: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return memberID
}

func (f *kubernetesTargetLifecycleFixture) disableMember(t *testing.T, memberID uuid.UUID) {
	t.Helper()
	result := f.db.Model(&persistence.ExecutionTargetGroupMember{}).
		Where("id = ? AND version = ?", memberID, 1).
		Updates(map[string]any{
			"status": routing.MemberStatusDisabled, "version": 2, "updated_at": f.now.Add(time.Second),
		})
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if result.RowsAffected != 1 {
		t.Fatalf("disabled member rows = %d, want 1", result.RowsAffected)
	}
}

func (f *kubernetesTargetLifecycleFixture) createSession(t *testing.T, groupID *uuid.UUID) uuid.UUID {
	t.Helper()
	sessionID := uuid.New()
	var routingVersion *int64
	if groupID != nil {
		version := int64(1)
		routingVersion = &version
	}
	if err := f.db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: f.tenantID, OrganizationID: f.organizationID, ProjectID: f.projectID,
		CreatedBy: f.principal.UserID, Title: "Target disable Session", Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: f.targetID, RequestedExecutionTargetID: f.targetID,
		ExecutionTargetGroupID: groupID, RoutingPolicyVersion: routingVersion,
		ResourceState: "idle", CreatedAt: f.now, UpdatedAt: f.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return sessionID
}

func (f *kubernetesTargetLifecycleFixture) createQueuedExecution(
	t *testing.T,
	sessionID, groupID uuid.UUID,
) {
	t.Helper()
	turnID := uuid.New()
	if err := f.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: f.tenantID, SessionID: sessionID, CreatedBy: f.principal.UserID,
		Status: "queued", InputText: "Target disable blocker", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: f.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	version := int64(1)
	region := "local"
	clusterID := "orbstack"
	reason := routing.StrategyPriority
	provider := "codex"
	if err := f.db.Create(&persistence.AgentExecution{
		ID: uuid.New(), TenantID: f.tenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: f.targetID, TargetKind: "kubernetes",
		PlacementRegion: region, PlacementClusterID: clusterID,
		TargetGroupID: &groupID, TargetGroupVersion: &version, TargetGroupMemberVersion: &version,
		SelectedRegion: &region, SelectedClusterID: &clusterID, RoutingReason: &reason,
		Provider: &provider, RequestedBy: f.principal.UserID, QueuedAt: f.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}
