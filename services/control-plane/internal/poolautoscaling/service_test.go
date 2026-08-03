package poolautoscaling

import (
	"context"
	"errors"
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
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkerPoolAutoscalingRejectsInactiveTenantBeforeStorageAccess(t *testing.T) {
	service := NewService(nil)
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	targetID, poolID := uuid.New(), uuid.New()

	_, err := service.Get(context.Background(), principal, requestedTenantID, targetID, poolID)
	assertProblemCode(t, err, "tenant_not_found")
	_, err = service.Put(
		context.Background(), principal, requestedTenantID, targetID, poolID,
		PutInput{}, "inactive-autoscaling", "127.0.0.1",
	)
	assertProblemCode(t, err, "tenant_not_found")
}

func TestWorkerPoolAutoscalingUsesQueueDelayCooldownAndHardColdStartGate(t *testing.T) {
	fixture := newAutoscalingFixture(t)
	ctx := context.Background()
	coldLimit := 20
	view, err := fixture.service.Put(
		ctx, fixture.principal, fixture.tenantID, fixture.targetID, fixture.poolID,
		PutInput{
			Enabled: true, MinIdleUnits: 1, MaxIdleUnits: 5,
			TargetQueueDelaySeconds: 10, InteractiveColdStartMaxSeconds: &coldLimit,
			ScaleUpStep: 2, ScaleDownStep: 1, CooldownSeconds: 1,
			ScaleDownStabilizationSeconds: 60,
		},
		"autoscaling-create", "127.0.0.1",
	)
	if err != nil || view.Policy.Version != 1 || view.State != nil {
		t.Fatalf("Put() = %#v, %v", view, err)
	}

	base := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	fixture.service.now = func() time.Time { return base }
	for index := 0; index < 3; index++ {
		fixture.createQueuedExecution(t, base.Add(-time.Duration(30-index)*time.Second))
	}

	summary, err := fixture.service.RunOnce(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Evaluated != 1 || summary.ScaledUp != 1 || summary.ColdStartViolated != 1 ||
		summary.ExpiredExecutions != 3 || len(fixture.expiries) != 1 {
		t.Fatalf("first autoscaling summary = %#v, expiries=%#v", summary, fixture.expiries)
	}
	state := fixture.loadState(t)
	if state.DesiredIdleUnits != 3 || state.QueueDepth != 3 || state.DecisionReason != "target-delay" ||
		state.ColdStartGateStatus != GateViolated {
		t.Fatalf("first autoscaling state = %#v", state)
	}

	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("worker_pool_id = ?", fixture.poolID).
		Updates(map[string]any{"status": "completed", "finished_at": base}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.service.now = func() time.Time { return base.Add(30 * time.Second) }
	if _, err := fixture.service.RunOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if state = fixture.loadState(t); state.DesiredIdleUnits != 3 || state.DecisionReason != "scale-down-stabilizing" {
		t.Fatalf("stabilizing autoscaling state = %#v", state)
	}

	fixture.service.now = func() time.Time { return base.Add(2 * time.Minute) }
	if summary, err = fixture.service.RunOnce(ctx, 10); err != nil || summary.ScaledDown != 1 {
		t.Fatalf("scale-down summary = %#v, %v", summary, err)
	}
	if state = fixture.loadState(t); state.DesiredIdleUnits != 2 || state.DecisionReason != "scale-down" ||
		state.ColdStartGateStatus != GateHealthy {
		t.Fatalf("scaled-down autoscaling state = %#v", state)
	}
}

func TestWorkerPoolAutoscalingPolicyUsesExactVersionCAS(t *testing.T) {
	fixture := newAutoscalingFixture(t)
	ctx := context.Background()
	input := PutInput{
		Enabled: true, MinIdleUnits: 1, MaxIdleUnits: 3, TargetQueueDelaySeconds: 10,
		ScaleUpStep: 1, ScaleDownStep: 1, CooldownSeconds: 5, ScaleDownStabilizationSeconds: 60,
	}
	created, err := fixture.service.Put(
		ctx, fixture.principal, fixture.tenantID, fixture.targetID, fixture.poolID,
		input, "autoscaling-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	stale := int64(0)
	input.ExpectedVersion = &stale
	_, err = fixture.service.Put(
		ctx, fixture.principal, fixture.tenantID, fixture.targetID, fixture.poolID,
		input, "autoscaling-stale", "127.0.0.1",
	)
	assertProblemCode(t, err, "worker_pool_autoscaling_policy_version_conflict")
	expected := created.Policy.Version
	input.ExpectedVersion = &expected
	updated, err := fixture.service.Put(
		ctx, fixture.principal, fixture.tenantID, fixture.targetID, fixture.poolID,
		input, "autoscaling-update", "127.0.0.1",
	)
	if err != nil || updated.Policy.Version != 2 {
		t.Fatalf("updated policy = %#v, %v", updated, err)
	}
}

type autoscalingFixture struct {
	db        *gorm.DB
	service   *Service
	principal identity.Principal
	tenantID  uuid.UUID
	targetID  uuid.UUID
	poolID    uuid.UUID
	projectID uuid.UUID
	sessionID uuid.UUID
	userID    uuid.UUID
	expiries  []ColdStartExpiry
}

func newAutoscalingFixture(t *testing.T) *autoscalingFixture {
	t.Helper()
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "autoscaling-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fixture := &autoscalingFixture{
		db: store.DB(), tenantID: domain.TenantID, userID: domain.UserID,
		principal: identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		targetID:  uuid.New(), poolID: uuid.New(), projectID: uuid.New(), sessionID: uuid.New(),
	}
	fixture.service = NewService(store.DB(), WithColdStartExpirer(func(_ context.Context, input ColdStartExpiry) (int, error) {
		fixture.expiries = append(fixture.expiries, input)
		var count int64
		if err := fixture.db.Model(&persistence.AgentExecution{}).
			Where("worker_pool_id = ? AND queued_at <= ? AND status IN ?", input.WorkerPoolID, input.Cutoff, []string{"queued", "recovering"}).
			Count(&count).Error; err != nil {
			return 0, err
		}
		return int(count), nil
	}))
	models := []any{
		&persistence.ExecutionTarget{
			ID: fixture.targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
			Kind: "kubernetes", Name: "Autoscaling", Status: "active",
			ConfigurationEncrypted: []byte("fixture"), Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Project{
			ID: fixture.projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Autoscaling", DefaultBranch: "main", Visibility: "private",
			CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentSession{
			ID: fixture.sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: fixture.projectID, CreatedBy: domain.UserID, Title: "Autoscaling",
			Status: "active", Visibility: "private", Provider: "codex",
			ExecutionTargetID: fixture.targetID, RequestedExecutionTargetID: fixture.targetID,
			ResourceState: "provisioning", CreatedAt: now, UpdatedAt: now,
		},
		&persistence.WorkerPool{
			ID: fixture.poolID, TenantID: &domain.TenantID, ExecutionTargetID: fixture.targetID,
			Name: "interactive", Mode: "warm", CapacityClass: "interactive", TenantIsolation: "pinned",
			DesiredIdleUnits: 1, MinIdleUnits: 1, MaxActiveUnits: 5,
			SchedulingTemplate: map[string]any{}, Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatalf("seed autoscaling fixture %T: %v", model, err)
		}
	}
	return fixture
}

func (f *autoscalingFixture) createQueuedExecution(t *testing.T, queuedAt time.Time) {
	t.Helper()
	sessionID := uuid.New()
	if err := f.db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: f.tenantID, ProjectID: f.projectID,
		CreatedBy: f.userID, Title: "Autoscaling queued execution", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: f.targetID,
		RequestedExecutionTargetID: f.targetID, ResourceState: "provisioning",
		CreatedAt: queuedAt, UpdatedAt: queuedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	turnID := uuid.New()
	if err := f.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: f.tenantID, SessionID: sessionID, CreatedBy: f.userID,
		Status: "queued", InputText: "autoscale", TurnKind: "message", RuntimeMode: "full-access",
		InteractionMode: "default", CreatedAt: queuedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&persistence.AgentExecution{
		ID: uuid.New(), TenantID: f.tenantID, SessionID: sessionID, TurnID: turnID,
		QueueClass: "interactive", QuotaUnits: 1, Attempt: 1, Status: "queued",
		ExecutionTargetID: f.targetID, TargetKind: "kubernetes",
		WorkerPoolID: &f.poolID, WorkerPoolVersion: pointer(int64(1)), CapacityClass: pointer("interactive"),
		Generation: 0, RequestedBy: f.userID, QueuedAt: queuedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func (f *autoscalingFixture) loadState(t *testing.T) persistence.WorkerPoolAutoscalingState {
	t.Helper()
	var state persistence.WorkerPoolAutoscalingState
	if err := f.db.Where("worker_pool_id = ? AND worker_pool_version = ?", f.poolID, 1).Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	return state
}

func pointer[T any](value T) *T { return &value }

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("problem code = %v, want %s", err, code)
	}
}
