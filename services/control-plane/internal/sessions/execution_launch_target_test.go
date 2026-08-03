package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
)

func TestSelectExecutionLaunchTargetAppliesFixedTargetSchedulingPolicySnapshot(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	target := loadCapabilityRouteTarget(t, fixture.db, fixture.executionTargetID)
	document := schedulingpolicy.UnrestrictedDocument()
	document.Provider = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"codex"}}
	document.CapacityClass = schedulingpolicy.Rule{
		Mode: schedulingpolicy.ModeAllow, Values: []string{placement.CapacityClassStandard},
	}
	policy, err := schedulingpolicy.NewService(fixture.db).UpdateTenant(
		context.Background(),
		fixture.tenantID,
		schedulingpolicy.UpdateInput{
			ExpectedVersion: 0, Document: document, ActorID: fixture.principal.UserID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	var launchTarget ExecutionLaunchTarget
	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		var selectErr error
		launchTarget, selectErr = SelectExecutionLaunchTarget(
			context.Background(),
			tx,
			&target,
			nil,
			ExecutionLaunchPolicyScope{
				TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, Provider: "codex",
			},
			placement.WarmPoolModeDisabled,
			nil,
		)
		return selectErr
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := persistence.AgentExecution{}
	ApplyExecutionLaunchTarget(&execution, launchTarget)
	if execution.TenantSchedulingPolicyVersion != policy.Tenant.Version ||
		execution.TenantSchedulingPolicyDigest != policy.Tenant.Digest ||
		execution.OrganizationSchedulingPolicyVersion != 0 ||
		execution.OrganizationSchedulingPolicyDigest != schedulingpolicy.UnrestrictedDigest ||
		execution.PlacementRegion != launchTarget.PlacementSelection.Pool.Region ||
		execution.PlacementClusterID != launchTarget.PlacementSelection.Pool.ClusterID {
		t.Fatalf("fixed-target launch snapshot = %#v, policy = %#v", execution, policy)
	}

	document.CapacityClass = schedulingpolicy.Rule{
		Mode: schedulingpolicy.ModeAllow, Values: []string{placement.CapacityClassInteractive},
	}
	if _, err := schedulingpolicy.NewService(fixture.db).UpdateTenant(
		context.Background(),
		fixture.tenantID,
		schedulingpolicy.UpdateInput{
			ExpectedVersion: policy.Tenant.Version, Document: document, ActorID: fixture.principal.UserID,
		},
	); err != nil {
		t.Fatal(err)
	}
	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		_, selectErr := SelectExecutionLaunchTarget(
			context.Background(),
			tx,
			&target,
			nil,
			ExecutionLaunchPolicyScope{
				TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, Provider: "codex",
			},
			placement.WarmPoolModeDisabled,
			nil,
		)
		return selectErr
	})
	assertSessionProblemCode(t, err, "execution_scheduling_policy_denied")
}

func TestCreateTurnPersistsCurrentSchedulingPolicySnapshot(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	document := schedulingpolicy.UnrestrictedDocument()
	document.Provider = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"codex"}}
	policy, err := schedulingpolicy.NewService(fixture.db).UpdateTenant(
		context.Background(),
		fixture.tenantID,
		schedulingpolicy.UpdateInput{
			ExpectedVersion: 0, Document: document, ActorID: fixture.principal.UserID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	traceID := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := trace.SpanID{17, 18, 19, 20, 21, 22, 23, 24}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	})
	requestContext := trace.ContextWithSpanContext(context.Background(), spanContext)
	turn, err := fixture.service.CreateTurn(
		requestContext,
		fixture.principal,
		fixture.sessionID,
		CreateTurnInput{InputText: "freeze scheduling policy"},
		"scheduling-policy-turn",
		"127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND turn_id = ?", fixture.tenantID, turn.ID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.TenantSchedulingPolicyVersion != policy.Tenant.Version ||
		execution.TenantSchedulingPolicyDigest != policy.Tenant.Digest ||
		execution.OrganizationSchedulingPolicyVersion != 0 ||
		execution.OrganizationSchedulingPolicyDigest != schedulingpolicy.UnrestrictedDigest {
		t.Fatalf("persisted scheduling policy snapshot = %#v, policy = %#v", execution, policy)
	}
	if execution.SchedulingDecisionID == nil {
		t.Fatal("persisted Execution omitted its immutable scheduling decision identity")
	}
	expectedTraceparent := "00-" + traceID.String() + "-" + spanID.String() + "-01"
	if execution.Traceparent == nil || *execution.Traceparent != expectedTraceparent {
		t.Fatalf("persisted Execution traceparent = %#v, want %q", execution.Traceparent, expectedTraceparent)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", execution.ID).
		Update("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01").Error; err == nil {
		t.Fatal("database allowed immutable Execution trace context mutation")
	}
	var decision persistence.ExecutionSchedulingDecision
	if err := fixture.db.Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, execution.ID).
		Take(&decision).Error; err != nil {
		t.Fatal(err)
	}
	if decision.ID != *execution.SchedulingDecisionID || decision.AlgorithmVersion != "fixed-target-v1" ||
		decision.EvidenceCompleteness != "selected-only" || decision.CandidateCount != 1 ||
		decision.SelectedExecutionTargetID != execution.ExecutionTargetID ||
		decision.SelectedWorkerPoolID == nil || execution.WorkerPoolID == nil ||
		*decision.SelectedWorkerPoolID != *execution.WorkerPoolID || decision.CandidateSetSHA256 == "" {
		t.Fatalf("persisted scheduling decision = %#v, execution = %#v", decision, execution)
	}
	var candidate persistence.ExecutionSchedulingCandidate
	if err := fixture.db.Where("tenant_id = ? AND decision_id = ?", fixture.tenantID, decision.ID).
		Take(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	if !candidate.Selected || candidate.Ordinal != 0 || candidate.ExecutionTargetID != execution.ExecutionTargetID ||
		candidate.WorkerPoolID == nil || *candidate.WorkerPoolID != *execution.WorkerPoolID ||
		candidate.CandidateSHA256 == "" {
		t.Fatalf("persisted scheduling candidate = %#v", candidate)
	}
}

func TestEffectiveExecutionPlacementLocationInheritsOrRejectsRoutedPoolAuthority(t *testing.T) {
	routeSelection := &routing.Selection{Member: persistence.ExecutionTargetGroupMember{
		Region: "cn-shanghai", ClusterID: "cluster-a",
	}}
	region, clusterID, err := effectiveExecutionPlacementLocation(routeSelection, placement.Pool{})
	if err != nil || region != "cn-shanghai" || clusterID != "cluster-a" {
		t.Fatalf("inherited routed location = %q/%q err=%v", region, clusterID, err)
	}
	_, _, err = effectiveExecutionPlacementLocation(routeSelection, placement.Pool{
		Region: "cn-beijing", ClusterID: "cluster-a",
	})
	assertSessionProblemCode(t, err, "execution_placement_location_mismatch")
}

func TestSelectExecutionLaunchTargetRejectsHealthChangeBeforeFinalPlacement(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	target := loadCapabilityRouteTarget(t, fixture.db, fixture.executionTargetID)
	destination := createCapabilityRouteTargetCopy(t, fixture.db, target, "commit-validation-excluded")
	initialPlacement := ensureTargetDefaultPlacement(t, fixture.db, target)
	group := configureCapabilityRouteGroup(t, fixture, target, destination)
	request := routing.SelectRequest{
		TenantID:          fixture.tenantID,
		OrganizationID:    fixture.organizationID,
		TargetGroupID:     group.ID,
		Provider:          "codex",
		ExcludedTargetIDs: []uuid.UUID{destination.ID},
	}
	var initialHealth persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", target.ID).Take(&initialHealth).Error; err != nil {
		t.Fatal(err)
	}
	var initialPool persistence.WorkerPool
	if err := fixture.db.Where("id = ?", initialPlacement.Pool.ID).Take(&initialPool).Error; err != nil {
		t.Fatal(err)
	}
	poolQueries := 0
	callbackName := "test:execution-launch-placement-query:" + uuid.NewString()
	queryCallbacks := fixture.db.Callback().Query()
	if err := queryCallbacks.After("gorm:query").Register(callbackName, func(db *gorm.DB) {
		if db.Statement.Table == (persistence.WorkerPool{}).TableName() {
			poolQueries++
		}
	}); err != nil {
		t.Fatal(err)
	}

	gateCalls := 0
	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, selectErr := SelectExecutionLaunchTarget(
			context.Background(),
			tx,
			nil,
			&request,
			ExecutionLaunchPolicyScope{
				TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, Provider: "codex",
			},
			placement.WarmPoolModeDisabled,
			func(
				ctx context.Context,
				tx *gorm.DB,
				selectedTarget persistence.ExecutionTarget,
				preview placement.Selection,
				routeSelection *routing.Selection,
			) error {
				gateCalls++
				if gateCalls != 1 {
					t.Fatalf("capability gate crossed stale routing authority: call %d", gateCalls)
				}
				if selectedTarget.ID != target.ID {
					t.Fatalf("preview target = %s, want %s", selectedTarget.ID, target.ID)
				}
				observedAt := routeSelection.Health.ObservedAt.Add(time.Second)
				if err := tx.WithContext(ctx).Model(&persistence.ExecutionTargetHealth{}).
					Where("execution_target_id = ?", selectedTarget.ID).
					Updates(map[string]any{
						"status":          routing.HealthUnreachable,
						"capacity_status": routing.CapacityUnknown,
						"observed_at":     observedAt,
						"expires_at":      observedAt.Add(time.Minute),
						"version":         gorm.Expr("version + 1"),
						"updated_at":      time.Now().UTC(),
					}).Error; err != nil {
					t.Fatal(err)
				}
				// Advance placement authority too. The query callback below proves
				// SelectExecution did not reload it before routing rejected stale
				// health, and the failed transaction must roll this update back.
				if err := tx.WithContext(ctx).Model(&persistence.WorkerPool{}).
					Where("id = ?", preview.Pool.ID).
					Updates(map[string]any{
						"version":    gorm.Expr("version + 1"),
						"updated_at": time.Now().UTC(),
					}).Error; err != nil {
					t.Fatal(err)
				}
				return nil
			},
		)
		return selectErr
	})
	if err := queryCallbacks.Remove(callbackName); err != nil {
		t.Fatal(err)
	}
	assertSessionProblemCode(t, err, "target_routing_selection_stale")
	if gateCalls != 1 {
		t.Fatalf("capability gate calls = %d, want preview only", gateCalls)
	}
	if poolQueries != 1 {
		t.Fatalf("Worker pool queries = %d, want preview only", poolQueries)
	}
	var rolledBackHealth persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", target.ID).Take(&rolledBackHealth).Error; err != nil {
		t.Fatal(err)
	}
	if rolledBackHealth.Status != initialHealth.Status || rolledBackHealth.Version != initialHealth.Version {
		t.Fatalf(
			"health after stale rollback = status %q version %d, want status %q version %d",
			rolledBackHealth.Status,
			rolledBackHealth.Version,
			initialHealth.Status,
			initialHealth.Version,
		)
	}
	var rolledBackPool persistence.WorkerPool
	if err := fixture.db.Where("id = ?", initialPool.ID).Take(&rolledBackPool).Error; err != nil {
		t.Fatal(err)
	}
	if rolledBackPool.Status != initialPool.Status || rolledBackPool.Version != initialPool.Version {
		t.Fatalf(
			"Worker pool after stale rollback = status %q version %d, want status %q version %d",
			rolledBackPool.Status,
			rolledBackPool.Version,
			initialPool.Status,
			initialPool.Version,
		)
	}
}

func TestSelectExecutionLaunchTargetDoesNotRetryAfterRoutingLocks(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	first := loadCapabilityRouteTarget(t, fixture.db, fixture.executionTargetID)
	second := createCapabilityRouteTargetCopy(t, fixture.db, first, "post-lock-retry-forbidden")
	ensureTargetDefaultPlacement(t, fixture.db, first)
	ensureTargetDefaultPlacement(t, fixture.db, second)
	group := configureCapabilityRouteGroup(t, fixture, first, second)
	request := routing.SelectRequest{
		TenantID:       fixture.tenantID,
		OrganizationID: fixture.organizationID,
		TargetGroupID:  group.ID,
		Provider:       "codex",
	}

	gateCalls := 0
	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, selectErr := SelectExecutionLaunchTarget(
			context.Background(),
			tx,
			nil,
			&request,
			ExecutionLaunchPolicyScope{
				TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, Provider: "codex",
			},
			placement.WarmPoolModeDisabled,
			func(
				_ context.Context,
				_ *gorm.DB,
				target persistence.ExecutionTarget,
				_ placement.Selection,
				_ *routing.Selection,
			) error {
				gateCalls++
				if target.ID != first.ID {
					t.Fatalf("capability gate target = %s, want first candidate %s", target.ID, first.ID)
				}
				if gateCalls == 2 {
					return problem.New(409, "capability_unsupported", "Capability changed after routing locks.")
				}
				return nil
			},
		)
		return selectErr
	})
	assertSessionProblemCode(t, err, "capability_unsupported")
	if gateCalls != 2 {
		t.Fatalf("capability gate calls = %d, want preview and final validation for one target", gateCalls)
	}
}
