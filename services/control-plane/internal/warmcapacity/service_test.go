package warmcapacity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestObserveAndGetFreshRoundTrip(t *testing.T) {
	fixture := newWarmCapacityFixture(t)
	observedAt := fixture.now
	channel := releaseChannelPromoted
	observed, err := fixture.service.Observe(context.Background(), Observation{
		TenantID:                fixture.tenantID,
		ExecutionTargetID:       fixture.target.ID,
		WorkerPoolID:            fixture.pool.ID,
		WorkerPoolVersion:       fixture.pool.Version,
		WarmSupported:           true,
		WorkerReleaseRevisionID: &fixture.release.ID,
		WorkerReleaseChannel:    &channel,
		DesiredTotalUnits:       3,
		ClaimedUnits:            1,
		ReadyIdleUnits:          2,
		Source:                  "reconciler/orbstack",
		Reason:                  stringPointer("healthy"),
		ObservedAt:              observedAt,
		TTL:                     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.CapacityClass != fixture.pool.CapacityClass ||
		observed.DesiredIdleUnits != fixture.pool.DesiredIdleUnits ||
		observed.MaxActiveUnits != fixture.pool.MaxActiveUnits ||
		observed.Version != 1 ||
		observed.WorkerReleaseRevisionID == nil ||
		*observed.WorkerReleaseRevisionID != fixture.release.ID ||
		observed.WorkerReleaseChannel == nil ||
		*observed.WorkerReleaseChannel != releaseChannelPromoted {
		t.Fatalf("observed warm capacity = %#v", observed)
	}

	fresh, err := fixture.service.GetFresh(context.Background(), FreshLookup{
		TenantID:                fixture.tenantID,
		ExecutionTargetID:       fixture.target.ID,
		WorkerPoolID:            fixture.pool.ID,
		WorkerPoolVersion:       fixture.pool.Version,
		WorkerReleaseRevisionID: &fixture.release.ID,
		WorkerReleaseChannel:    &channel,
		Now:                     observedAt.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh == nil || fresh.ReadyIdleUnits != 2 || fresh.ClaimedUnits != 1 {
		t.Fatalf("fresh warm capacity = %#v", fresh)
	}

	secondObservedAt := observedAt.Add(time.Second)
	advanced, err := fixture.service.Observe(context.Background(), Observation{
		TenantID:                fixture.tenantID,
		ExecutionTargetID:       fixture.target.ID,
		WorkerPoolID:            fixture.pool.ID,
		WorkerPoolVersion:       fixture.pool.Version,
		WarmSupported:           true,
		WorkerReleaseRevisionID: &fixture.release.ID,
		WorkerReleaseChannel:    &channel,
		DesiredTotalUnits:       4,
		ClaimedUnits:            2,
		ReadyIdleUnits:          1,
		Source:                  "reconciler/orbstack",
		ObservedAt:              secondObservedAt,
		TTL:                     2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Version != 2 || advanced.ObservedAt != secondObservedAt || advanced.DesiredTotalUnits != 4 {
		t.Fatalf("advanced warm capacity = %#v", advanced)
	}
}

func TestObserveRejectsInvalidScopeAndCounters(t *testing.T) {
	fixture := newWarmCapacityFixture(t)
	sharedTarget := fixture.target
	sharedTarget.ID = uuid.New()
	sharedTarget.TenantID = nil
	sharedTarget.Name = "shared-kubernetes"
	if err := fixture.db.Create(&sharedTarget).Error; err != nil {
		t.Fatal(err)
	}
	sharedPool := fixture.pool
	sharedPool.ID = uuid.New()
	sharedPool.TenantID = nil
	sharedPool.ExecutionTargetID = sharedTarget.ID
	sharedPool.Name = "shared-warm"
	if err := fixture.db.Create(&sharedPool).Error; err != nil {
		t.Fatal(err)
	}

	base := Observation{
		TenantID:          fixture.tenantID,
		ExecutionTargetID: fixture.target.ID,
		WorkerPoolID:      fixture.pool.ID,
		WorkerPoolVersion: fixture.pool.Version,
		WarmSupported:     true,
		DesiredTotalUnits: 2,
		ClaimedUnits:      1,
		ReadyIdleUnits:    1,
		Source:            "reconciler/test",
		ObservedAt:        fixture.now,
		TTL:               time.Minute,
	}
	nonWarmPool := fixture.pool
	nonWarmPool.ID = uuid.New()
	nonWarmPool.Name = "resident-pool"
	nonWarmPool.Mode = "resident"
	if err := fixture.db.Create(&nonWarmPool).Error; err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		mut  func(*Observation)
		code string
	}{
		{
			name: "shared target rejected",
			mut: func(input *Observation) {
				input.ExecutionTargetID = sharedTarget.ID
				input.WorkerPoolID = sharedPool.ID
			},
			code: "warm_capacity_target_not_found",
		},
		{
			name: "non warm mode rejected",
			mut: func(input *Observation) {
				input.WorkerPoolID = nonWarmPool.ID
				input.WorkerPoolVersion = nonWarmPool.Version
			},
			code: "warm_capacity_pool_mode_invalid",
		},
		{
			name: "unsupported counters rejected",
			mut: func(input *Observation) {
				input.WarmSupported = false
				input.DesiredTotalUnits = 1
				input.ReadyIdleUnits = 1
				input.ClaimedUnits = 3
			},
			code: "invalid_warm_capacity_counters",
		},
		{
			name: "desired total over pool max rejected",
			mut: func(input *Observation) {
				input.DesiredTotalUnits = fixture.pool.MaxActiveUnits + 1
				input.ClaimedUnits = fixture.pool.MaxActiveUnits + 3
				input.ReadyIdleUnits = 0
			},
			code: "invalid_warm_capacity_counters",
		},
		{
			name: "release pair shape rejected",
			mut: func(input *Observation) {
				channel := releaseChannelCanary
				input.WorkerReleaseChannel = &channel
			},
			code: "invalid_warm_capacity_release_pair",
		},
		{
			name: "stale pool version rejected",
			mut: func(input *Observation) {
				input.WorkerPoolVersion = fixture.pool.Version + 1
			},
			code: "warm_capacity_pool_version_stale",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			tc.mut(&input)
			_, err := fixture.service.Observe(context.Background(), input)
			assertProblemCode(t, err, tc.code)
		})
	}
}

func TestGetFreshReturnsNilForExpiredMissingOrReleaseMismatch(t *testing.T) {
	fixture := newWarmCapacityFixture(t)
	channel := releaseChannelPromoted
	if _, err := fixture.service.Observe(context.Background(), Observation{
		TenantID:                fixture.tenantID,
		ExecutionTargetID:       fixture.target.ID,
		WorkerPoolID:            fixture.pool.ID,
		WorkerPoolVersion:       fixture.pool.Version,
		WarmSupported:           true,
		WorkerReleaseRevisionID: &fixture.release.ID,
		WorkerReleaseChannel:    &channel,
		DesiredTotalUnits:       2,
		ClaimedUnits:            1,
		ReadyIdleUnits:          1,
		Source:                  "reconciler/test",
		ObservedAt:              fixture.now,
		TTL:                     20 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	missing, err := fixture.service.GetFresh(context.Background(), FreshLookup{
		TenantID:          fixture.tenantID,
		ExecutionTargetID: fixture.target.ID,
		WorkerPoolID:      uuid.New(),
		WorkerPoolVersion: 1,
		Now:               fixture.now.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("missing warm capacity = %#v", missing)
	}

	mismatchChannel := releaseChannelCanary
	mismatched, err := fixture.service.GetFresh(context.Background(), FreshLookup{
		TenantID:                fixture.tenantID,
		ExecutionTargetID:       fixture.target.ID,
		WorkerPoolID:            fixture.pool.ID,
		WorkerPoolVersion:       fixture.pool.Version,
		WorkerReleaseRevisionID: &fixture.release.ID,
		WorkerReleaseChannel:    &mismatchChannel,
		Now:                     fixture.now.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if mismatched != nil {
		t.Fatalf("mismatched warm capacity = %#v", mismatched)
	}

	expired, err := fixture.service.GetFresh(context.Background(), FreshLookup{
		TenantID:          fixture.tenantID,
		ExecutionTargetID: fixture.target.ID,
		WorkerPoolID:      fixture.pool.ID,
		WorkerPoolVersion: fixture.pool.Version,
		Now:               fixture.now.Add(25 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired != nil {
		t.Fatalf("expired warm capacity = %#v", expired)
	}
}

type warmCapacityFixture struct {
	db       *gorm.DB
	service  *Service
	now      time.Time
	tenantID uuid.UUID
	target   persistence.ExecutionTarget
	pool     persistence.WorkerPool
	release  persistence.WorkerReleaseRevision
}

func newWarmCapacityFixture(t *testing.T) warmCapacityFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{
		TranslateError: true,
		Logger:         logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New()
	tenantID := uuid.New()
	targetID := uuid.New()
	manifestID := uuid.New()
	releaseID := uuid.New()
	if err := db.Create(&persistence.User{
		ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Warm Capacity",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.Tenant{
		ID: tenantID, Slug: "warm-" + uuid.NewString()[:8], Name: "Warm Capacity Tenant",
		Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{},
		CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	target := persistence.ExecutionTarget{
		ID: targetID, TenantID: &tenantID, Kind: targetKindKubernetes, Name: "tenant-k8s", Status: targetStatusActive,
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &tenantID, ExecutionTargetID: targetID,
		Name: "warm-default", Mode: poolModeWarm, CapacityClass: capacityClassInteractive,
		DesiredIdleUnits: 2, MaxActiveUnits: 5, SchedulingTemplate: map[string]any{},
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	manifest := persistence.WorkerManifest{
		ID:                    manifestID,
		ManifestHash:          uuid.NewString(),
		WorkerBuildVersion:    "1.0.0",
		WorkerProtocolMinimum: 2,
		WorkerProtocolMaximum: 2,
		RuntimeEventMinimum:   2,
		RuntimeEventMaximum:   2,
		OperatingSystem:       "linux",
		Architecture:          "amd64",
		FeatureFlags:          map[string]any{},
		CreatedAt:             now,
	}
	if err := db.Create(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	release := persistence.WorkerReleaseRevision{
		ID: releaseID, TenantID: tenantID, ExecutionTargetID: targetID,
		Revision: 1, WorkerManifestID: manifestID, Description: "warm release",
		CreatedBy: userID, CreatedAt: now,
	}
	if err := db.Create(&release).Error; err != nil {
		t.Fatal(err)
	}

	service := NewService(db)
	service.now = func() time.Time { return now }
	return warmCapacityFixture{
		db:       db,
		service:  service,
		now:      now,
		tenantID: tenantID,
		target:   target,
		pool:     pool,
		release:  release,
	}
}

func assertProblemCode(t *testing.T, err error, expected string) {
	t.Helper()
	var problemErr *problem.Error
	if err == nil || !errors.As(err, &problemErr) || problemErr.Code != expected {
		t.Fatalf("problem code = %v, want %q (error: %v)", problemErr, expected, err)
	}
}

func stringPointer(value string) *string { return &value }
