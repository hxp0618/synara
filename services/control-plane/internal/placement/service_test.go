package placement

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestSelectExecutionBackfillsDefaultPoolByTargetKind(t *testing.T) {
	fixture := newPlacementFixture(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		kind     string
		wantMode string
	}{
		{name: "docker", kind: "docker", wantMode: PoolModeResident},
		{name: "kubernetes", kind: "kubernetes", wantMode: PoolModePerExecution},
		{name: "ssh", kind: "ssh", wantMode: PoolModeResident},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := fixture.createTarget(t, tc.kind, false)
			selected, err := fixture.service.SelectExecution(ctx, fixture.db, target, WarmPoolModeDisabled)
			if err != nil {
				t.Fatal(err)
			}
			if selected.PolicyVersion != 1 || selected.Pool.Name != "default" || selected.Pool.Mode != tc.wantMode ||
				selected.Pool.CapacityClass != CapacityClassStandard || selected.Pool.DesiredIdleUnits != 0 ||
				selected.Pool.MaxActiveUnits != 1 || selected.Pool.Status != PoolStatusActive {
				t.Fatalf("default selection = %#v", selected)
			}
			state, err := fixture.service.List(ctx, fixture.owner, fixture.tenantID, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Pools) != 1 || state.Policy.Version != 1 || state.Policy.DefaultPoolID != selected.Pool.ID {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestSelectExecutionFallsBackAcrossActivePools(t *testing.T) {
	fixture := newPlacementFixture(t)
	ctx := context.Background()
	target := fixture.createTarget(t, "ssh", false)

	state, err := fixture.service.List(ctx, fixture.owner, fixture.tenantID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	defaultPoolID := state.Policy.DefaultPoolID
	balanced := fixture.createPool(t, target, "balanced", PoolModeWarm, PoolStatusActive)
	lowLatency := fixture.createPool(t, target, "low-latency", PoolModeWarm, PoolStatusActive)

	state, err = fixture.service.UpdatePolicy(ctx, fixture.owner, fixture.tenantID, target.ID, UpdatePolicyInput{
		ExpectedVersion:  state.Policy.Version,
		DefaultPoolID:    defaultPoolID,
		BalancedPoolID:   &balanced.ID,
		LowLatencyPoolID: &lowLatency.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Policy.Version != 2 || state.Policy.UpdatedBy == nil || *state.Policy.UpdatedBy != fixture.owner.UserID {
		t.Fatalf("updated policy = %#v", state.Policy)
	}

	selected, err := fixture.service.SelectExecution(ctx, fixture.db, target, WarmPoolModeBalanced)
	if err != nil || selected.Pool.ID != balanced.ID {
		t.Fatalf("balanced selection = %#v err=%v", selected, err)
	}
	selected, err = fixture.service.SelectExecution(ctx, fixture.db, target, WarmPoolModeLowLatency)
	if err != nil || selected.Pool.ID != lowLatency.ID {
		t.Fatalf("low-latency selection = %#v err=%v", selected, err)
	}

	if err := fixture.db.Model(&persistence.WorkerPool{}).Where("id = ?", lowLatency.ID).
		Update("status", PoolStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	selected, err = fixture.service.SelectExecution(ctx, fixture.db, target, WarmPoolModeLowLatency)
	if err != nil || selected.Pool.ID != balanced.ID {
		t.Fatalf("low-latency fallback = %#v err=%v", selected, err)
	}

	if err := fixture.db.Model(&persistence.WorkerPool{}).Where("id = ?", balanced.ID).
		Update("status", PoolStatusDraining).Error; err != nil {
		t.Fatal(err)
	}
	selected, err = fixture.service.SelectExecution(ctx, fixture.db, target, WarmPoolModeBalanced)
	if err != nil || selected.Pool.ID != defaultPoolID {
		t.Fatalf("balanced fallback = %#v err=%v", selected, err)
	}

	if err := fixture.db.Model(&persistence.WorkerPool{}).Where("id = ?", defaultPoolID).
		Update("status", PoolStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.SelectExecution(ctx, fixture.db, target, WarmPoolModeDisabled); problemCode(err) != "execution_placement_policy_invalid" {
		t.Fatalf("default failure err = %v", err)
	}
}

func TestSelectExecutionPrefersFirstFreshReadyWarmCandidate(t *testing.T) {
	fixture := newPlacementFixture(t)
	fixture.service.now = func() time.Time { return fixture.now }
	ctx := context.Background()
	target := fixture.createTarget(t, "kubernetes", false)
	state, err := fixture.service.List(ctx, fixture.owner, fixture.tenantID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	balanced := fixture.createPool(t, target, "balanced", PoolModeWarm, PoolStatusActive)
	lowLatency := fixture.createPool(t, target, "low-latency", PoolModeWarm, PoolStatusActive)
	state, err = fixture.service.UpdatePolicy(ctx, fixture.owner, fixture.tenantID, target.ID, UpdatePolicyInput{
		ExpectedVersion:  state.Policy.Version,
		DefaultPoolID:    state.Policy.DefaultPoolID,
		BalancedPoolID:   &balanced.ID,
		LowLatencyPoolID: &lowLatency.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.createWarmCapacity(t, target, balanced, true, 1, fixture.now.Add(time.Minute))

	for name, selectExecution := range map[string]func() (Selection, error){
		"preview": func() (Selection, error) {
			return fixture.service.PreviewExecution(ctx, fixture.db, target, WarmPoolModeLowLatency)
		},
		"final": func() (Selection, error) {
			return fixture.service.SelectExecution(ctx, fixture.db, target, WarmPoolModeLowLatency)
		},
	} {
		t.Run(name, func(t *testing.T) {
			selected, err := selectExecution()
			if err != nil {
				t.Fatal(err)
			}
			if selected.Pool.ID != balanced.ID || selected.PolicyVersion != state.Policy.Version {
				t.Fatalf("selection = %#v, want fresh balanced pool %s", selected, balanced.ID)
			}
		})
	}
}

func TestSelectExecutionFallsBackToFirstActiveCandidateWithoutFreshReadyWarmCapacity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		warmSupported bool
		readyIdle     int
		expiresAt     func(time.Time) time.Time
	}{
		{name: "fresh zero", warmSupported: true, readyIdle: 0, expiresAt: func(now time.Time) time.Time { return now.Add(time.Minute) }},
		{name: "expired ready", warmSupported: true, readyIdle: 1, expiresAt: func(now time.Time) time.Time { return now }},
		{name: "unsupported", warmSupported: false, readyIdle: 0, expiresAt: func(now time.Time) time.Time { return now.Add(time.Minute) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlacementFixture(t)
			fixture.service.now = func() time.Time { return fixture.now }
			ctx := context.Background()
			target := fixture.createTarget(t, "kubernetes", false)
			state, err := fixture.service.List(ctx, fixture.owner, fixture.tenantID, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			balanced := fixture.createPool(t, target, "balanced", PoolModeWarm, PoolStatusActive)
			lowLatency := fixture.createPool(t, target, "low-latency", PoolModeWarm, PoolStatusActive)
			state, err = fixture.service.UpdatePolicy(ctx, fixture.owner, fixture.tenantID, target.ID, UpdatePolicyInput{
				ExpectedVersion:  state.Policy.Version,
				DefaultPoolID:    state.Policy.DefaultPoolID,
				BalancedPoolID:   &balanced.ID,
				LowLatencyPoolID: &lowLatency.ID,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.createWarmCapacity(t, target, balanced, tc.warmSupported, tc.readyIdle, tc.expiresAt(fixture.now))

			for name, selectExecution := range map[string]func() (Selection, error){
				"preview": func() (Selection, error) {
					return fixture.service.PreviewExecution(ctx, fixture.db, target, WarmPoolModeLowLatency)
				},
				"final": func() (Selection, error) {
					return fixture.service.SelectExecution(ctx, fixture.db, target, WarmPoolModeLowLatency)
				},
			} {
				t.Run(name, func(t *testing.T) {
					selected, err := selectExecution()
					if err != nil {
						t.Fatal(err)
					}
					if selected.Pool.ID != lowLatency.ID || selected.PolicyVersion != state.Policy.Version {
						t.Fatalf("selection = %#v, want low-latency cold fallback %s", selected, lowLatency.ID)
					}
				})
			}
		})
	}
}

func TestUpdatePolicyEnforcesCASAndTargetScope(t *testing.T) {
	fixture := newPlacementFixture(t)
	ctx := context.Background()
	target := fixture.createTarget(t, "kubernetes", false)

	state, err := fixture.service.List(ctx, fixture.owner, fixture.tenantID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := fixture.createMember(t, "member")
	if _, err := fixture.service.List(ctx, member, fixture.tenantID, target.ID); problemCode(err) != "tenant_forbidden" {
		t.Fatalf("member list err = %v", err)
	}

	balanced := fixture.createPool(t, target, "balanced", PoolModeWarm, PoolStatusActive)
	otherTarget := fixture.createTarget(t, "ssh", false)
	foreignPool := fixture.createPool(t, otherTarget, "foreign", PoolModeWarm, PoolStatusActive)

	if _, err := fixture.service.UpdatePolicy(ctx, member, fixture.tenantID, target.ID, UpdatePolicyInput{
		ExpectedVersion: state.Policy.Version,
		DefaultPoolID:   state.Policy.DefaultPoolID,
		BalancedPoolID:  &balanced.ID,
	}); problemCode(err) != "tenant_forbidden" {
		t.Fatalf("member update err = %v", err)
	}

	if _, err := fixture.service.UpdatePolicy(ctx, fixture.owner, fixture.tenantID, target.ID, UpdatePolicyInput{
		ExpectedVersion: state.Policy.Version,
		DefaultPoolID:   state.Policy.DefaultPoolID,
		BalancedPoolID:  &foreignPool.ID,
	}); problemCode(err) != "execution_placement_pool_not_found" {
		t.Fatalf("foreign pool err = %v", err)
	}

	if _, err := fixture.service.UpdatePolicy(ctx, fixture.owner, fixture.tenantID, target.ID, UpdatePolicyInput{
		ExpectedVersion: state.Policy.Version,
		DefaultPoolID:   state.Policy.DefaultPoolID,
		BalancedPoolID:  &balanced.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.UpdatePolicy(ctx, fixture.owner, fixture.tenantID, target.ID, UpdatePolicyInput{
		ExpectedVersion: state.Policy.Version,
		DefaultPoolID:   state.Policy.DefaultPoolID,
		BalancedPoolID:  &balanced.ID,
	}); problemCode(err) != "execution_placement_policy_version_conflict" {
		t.Fatalf("stale update err = %v", err)
	}

	sharedTarget := fixture.createTarget(t, "docker", true)
	sharedState, err := fixture.service.List(ctx, fixture.owner, fixture.tenantID, sharedTarget.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.UpdatePolicy(ctx, fixture.owner, fixture.tenantID, sharedTarget.ID, UpdatePolicyInput{
		ExpectedVersion: sharedState.Policy.Version,
		DefaultPoolID:   sharedState.Policy.DefaultPoolID,
	}); problemCode(err) != "shared_execution_target_placement_policy_immutable" {
		t.Fatalf("shared update err = %v", err)
	}
}

func TestCreateAndUpdatePoolValidateAndEnforceCAS(t *testing.T) {
	fixture := newPlacementFixture(t)
	ctx := context.Background()
	target := fixture.createTarget(t, "kubernetes", false)

	minIdleUnits := 1
	created, err := fixture.service.CreatePool(ctx, fixture.owner, fixture.tenantID, target.ID, CreatePoolInput{
		Name:               "Warm Interactive",
		Mode:               PoolModeWarm,
		CapacityClass:      CapacityClassInteractive,
		ClusterID:          "cluster-a",
		Region:             "cn-sha",
		Namespace:          "workers",
		DesiredIdleUnits:   1,
		MinIdleUnits:       &minIdleUnits,
		MaxActiveUnits:     3,
		SchedulingTemplate: map[string]any{"priorityClassName": "interactive"},
		Status:             PoolStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || created.Mode != PoolModeWarm || created.CapacityClass != CapacityClassInteractive || created.MinIdleUnits != 1 {
		t.Fatalf("created pool = %#v", created)
	}

	updated, err := fixture.service.UpdatePool(ctx, fixture.owner, fixture.tenantID, target.ID, created.ID, UpdatePoolInput{
		ExpectedVersion: 1,
		CreatePoolInput: CreatePoolInput{
			Name:               "Warm Interactive 2",
			Mode:               PoolModeWarm,
			CapacityClass:      CapacityClassInteractive,
			ClusterID:          "cluster-b",
			Region:             "us-west",
			Namespace:          "workers-2",
			DesiredIdleUnits:   0,
			MaxActiveUnits:     2,
			SchedulingTemplate: map[string]any{"nodeSelector": map[string]any{"pool": "warm"}},
			Status:             PoolStatusDraining,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.Name != "Warm Interactive 2" || updated.Status != PoolStatusDraining ||
		updated.CapacityClass != CapacityClassInteractive || updated.ClusterID != "cluster-b" {
		t.Fatalf("updated pool = %#v", updated)
	}

	if _, err := fixture.service.UpdatePool(ctx, fixture.owner, fixture.tenantID, target.ID, created.ID, UpdatePoolInput{
		ExpectedVersion: 1,
		CreatePoolInput: CreatePoolInput{
			Name: "stale", Mode: PoolModeWarm, CapacityClass: CapacityClassInteractive,
			DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{}, Status: PoolStatusActive,
		},
	}); problemCode(err) != "worker_pool_version_conflict" {
		t.Fatalf("stale update err = %v", err)
	}

	if _, err := fixture.service.UpdatePool(ctx, fixture.owner, fixture.tenantID, target.ID, created.ID, UpdatePoolInput{
		ExpectedVersion: 2,
		CreatePoolInput: CreatePoolInput{
			Name: "identity change", Mode: PoolModeWarm, CapacityClass: CapacityClassStandard,
			DesiredIdleUnits: 0, MaxActiveUnits: 2, SchedulingTemplate: map[string]any{}, Status: PoolStatusActive,
		},
	}); problemCode(err) != "worker_pool_identity_immutable" {
		t.Fatalf("identity update err = %v", err)
	}

	member := fixture.createMember(t, "member")
	if _, err := fixture.service.CreatePool(ctx, member, fixture.tenantID, target.ID, CreatePoolInput{
		Name: "forbidden", Mode: PoolModeWarm, CapacityClass: CapacityClassStandard,
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{}, Status: PoolStatusActive,
	}); problemCode(err) != "tenant_forbidden" {
		t.Fatalf("member create err = %v", err)
	}

	if _, err := fixture.service.CreatePool(ctx, fixture.owner, fixture.tenantID, target.ID, CreatePoolInput{
		Name: "bad", Mode: "invalid", CapacityClass: CapacityClassStandard,
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{}, Status: PoolStatusActive,
	}); problemCode(err) != "invalid_worker_pool_mode" {
		t.Fatalf("invalid create err = %v", err)
	}

	sharedTarget := fixture.createTarget(t, "docker", true)
	if _, err := fixture.service.CreatePool(ctx, fixture.owner, fixture.tenantID, sharedTarget.ID, CreatePoolInput{
		Name: "shared", Mode: PoolModeWarm, CapacityClass: CapacityClassStandard,
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{}, Status: PoolStatusActive,
	}); problemCode(err) != "shared_execution_target_worker_pool_immutable" {
		t.Fatalf("shared create err = %v", err)
	}

	sshTarget := fixture.createTarget(t, "ssh", false)
	if _, err := fixture.service.CreatePool(ctx, fixture.owner, fixture.tenantID, sshTarget.ID, CreatePoolInput{
		Name: "ssh-warm", Mode: PoolModeWarm, CapacityClass: CapacityClassStandard,
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{}, Status: PoolStatusActive,
	}); problemCode(err) != "worker_pool_mode_target_unsupported" {
		t.Fatalf("ssh warm create err = %v", err)
	}
}

func TestNormalizePoolInputValidatesMinIdleUnits(t *testing.T) {
	tests := []struct {
		name        string
		minIdle     *int
		desiredIdle int
		wantMin     int
		wantCode    string
	}{
		{name: "omitted defaults to zero", desiredIdle: 2, wantMin: 0},
		{name: "within desired idle accepted", minIdle: poolInputIntPointer(1), desiredIdle: 2, wantMin: 1},
		{name: "negative rejected", minIdle: poolInputIntPointer(-1), desiredIdle: 2, wantCode: "invalid_worker_pool_capacity_bounds"},
		{name: "above desired idle rejected", minIdle: poolInputIntPointer(3), desiredIdle: 2, wantCode: "invalid_worker_pool_capacity_bounds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := normalizePoolInput(CreatePoolInput{
				Name: "warm", Mode: PoolModeWarm, CapacityClass: CapacityClassInteractive,
				DesiredIdleUnits: test.desiredIdle, MinIdleUnits: test.minIdle, MaxActiveUnits: 4,
				SchedulingTemplate: map[string]any{}, Status: PoolStatusActive,
			})
			if test.wantCode != "" {
				if code := problemCode(err); code != test.wantCode {
					t.Fatalf("problem code = %q, want %q (error: %v)", code, test.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if normalized.MinIdleUnits == nil || *normalized.MinIdleUnits != test.wantMin {
				t.Fatalf("normalized minIdleUnits = %#v, want %d", normalized.MinIdleUnits, test.wantMin)
			}
		})
	}
}

func TestUpdatePoolBlocksVersionAdvanceWhileNonterminalExecutionsReferenceSnapshot(t *testing.T) {
	fixture := newPlacementFixture(t)
	ctx := context.Background()
	target := fixture.createTarget(t, "kubernetes", false)

	pool, err := fixture.service.CreatePool(ctx, fixture.owner, fixture.tenantID, target.ID, CreatePoolInput{
		Name:               "Warm Interactive",
		Mode:               PoolModeWarm,
		CapacityClass:      CapacityClassInteractive,
		DesiredIdleUnits:   1,
		MaxActiveUnits:     2,
		SchedulingTemplate: map[string]any{},
		Status:             PoolStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionID := uuid.New()
	turnID := uuid.New()
	now := fixture.now
	projectID := uuid.New()
	if err := fixture.db.Create(&persistence.Project{
		ID: projectID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		Name: "Warm update project", DefaultBranch: "main", Visibility: "organization",
		CreatedBy: fixture.userID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: projectID,
		CreatedBy: fixture.userID, Title: "Queued warm execution", Status: "active", Visibility: "organization",
		Provider: "codex", ExecutionTargetID: target.ID, WarmPoolMode: WarmPoolModeBalanced,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: sessionID, CreatedBy: fixture.userID,
		Status: "queued", InputText: "queued warm execution", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	executionID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentExecution{
		ID: executionID, TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: "kubernetes",
		WorkerPoolID: &pool.ID, WorkerPoolVersion: &pool.Version, CapacityClass: &pool.CapacityClass,
		PlacementPolicyVersion: int64Pointer(1), WarmPoolModeSnapshot: WarmPoolModeBalanced,
		RequestedBy: fixture.userID, QueuedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	for _, status := range []string{"queued", "recovering", "leased", "running", "waiting-for-approval", "suspended"} {
		if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
			Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.UpdatePool(ctx, fixture.owner, fixture.tenantID, target.ID, pool.ID, UpdatePoolInput{
			ExpectedVersion: pool.Version,
			CreatePoolInput: CreatePoolInput{
				Name:               "Warm Interactive Updated",
				Mode:               PoolModeWarm,
				CapacityClass:      CapacityClassInteractive,
				DesiredIdleUnits:   0,
				MaxActiveUnits:     2,
				SchedulingTemplate: map[string]any{},
				Status:             PoolStatusActive,
			},
		}); problemCode(err) != "worker_pool_version_update_blocked" {
			t.Fatalf("status %q blocked pool update err = %v", status, err)
		}
	}
}

type placementFixture struct {
	db             *gorm.DB
	service        *Service
	tenantID       uuid.UUID
	organizationID uuid.UUID
	userID         uuid.UUID
	owner          identity.Principal
	now            time.Time
}

func newPlacementFixture(t *testing.T) placementFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID := uuid.New(), uuid.New()
	if err := db.Create(&persistence.User{
		ID: userID, Email: "owner-" + uuid.NewString() + "@example.com", DisplayName: "Owner",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.Tenant{
		ID: tenantID, Slug: "placement-" + strings.ToLower(uuid.NewString())[:12], Name: "Placement Tenant",
		Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{},
		CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{
		TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	organizationID := uuid.New()
	return placementFixture{
		db:             db,
		service:        NewService(db),
		tenantID:       tenantID,
		organizationID: organizationID,
		userID:         userID,
		owner: identity.Principal{
			UserID: userID, SessionID: uuid.New(), ActiveTenantID: &tenantID,
			Email: "owner@example.com", DisplayName: "Owner",
		},
		now: now,
	}
}

func (f placementFixture) createTarget(t *testing.T, kind string, shared bool) persistence.ExecutionTarget {
	t.Helper()
	targetID := uuid.New()
	target := persistence.ExecutionTarget{
		ID: targetID, Kind: kind, Name: kind + "-" + targetID.String()[:8], Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: f.now, UpdatedAt: f.now,
	}
	if !shared {
		target.TenantID = &f.tenantID
	}
	if err := f.db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	return target
}

func (f placementFixture) createPool(
	t *testing.T,
	target persistence.ExecutionTarget,
	name, mode, status string,
) persistence.WorkerPool {
	t.Helper()
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: target.TenantID, ExecutionTargetID: target.ID, Name: name,
		Mode: mode, CapacityClass: CapacityClassStandard, ClusterID: "", Region: "", Namespace: "",
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{},
		Status: status, Version: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}
	if err := f.db.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	return pool
}

func poolInputIntPointer(value int) *int { return &value }

func (f placementFixture) createWarmCapacity(
	t *testing.T,
	target persistence.ExecutionTarget,
	pool persistence.WorkerPool,
	warmSupported bool,
	readyIdle int,
	expiresAt time.Time,
) {
	t.Helper()
	desiredTotal := 1
	if !warmSupported {
		desiredTotal = 0
		readyIdle = 0
	}
	if err := f.db.Create(&persistence.WorkerPoolWarmCapacity{
		WorkerPoolID:      pool.ID,
		WorkerPoolVersion: pool.Version,
		TenantID:          f.tenantID,
		ExecutionTargetID: target.ID,
		CapacityClass:     pool.CapacityClass,
		WarmSupported:     warmSupported,
		DesiredIdleUnits:  pool.DesiredIdleUnits,
		MaxActiveUnits:    pool.MaxActiveUnits,
		DesiredTotalUnits: desiredTotal,
		ReadyIdleUnits:    readyIdle,
		Source:            "placement-test",
		ObservedAt:        f.now.Add(-time.Second),
		ExpiresAt:         expiresAt,
		Version:           1,
		UpdatedAt:         f.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func (f placementFixture) createMember(t *testing.T, role string) identity.Principal {
	t.Helper()
	userID := uuid.New()
	if err := f.db.Create(&persistence.User{
		ID: userID, Email: "member-" + uuid.NewString() + "@example.com", DisplayName: "Member",
		Status: "active", EmailVerifiedAt: &f.now, CreatedAt: f.now, UpdatedAt: f.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&persistence.TenantMembership{
		TenantID: f.tenantID, UserID: userID, Role: role, Status: "active",
		JoinedAt: &f.now, CreatedAt: f.now, UpdatedAt: f.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return identity.Principal{
		UserID: userID, SessionID: uuid.New(), ActiveTenantID: &f.tenantID,
		Email: "member@example.com", DisplayName: "Member",
	}
}

func problemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

func int64Pointer(value int64) *int64 {
	return &value
}
