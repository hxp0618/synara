package executions

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/providercapabilities"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestReviewOperationPersistsAtomicallyAndReplays(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	expected := fixture.lastSequence(t, fixture.sessionID)
	input := StartReviewInput{
		ExpectedLastEventSequence: &expected,
		Target:                    ReviewTarget{Type: "baseBranch"},
		RuntimeMode:               "approval-required",
	}

	first, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID, input,
		"review-replay", "review-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.StatusCode != 202 || first.Value.Type != "review" ||
		first.Value.Turn.TurnKind != "review" || first.Value.Turn.RuntimeMode != "approval-required" ||
		first.Value.ControlCommand.CommandType != "StartReview" || first.Value.ControlCommand.Status != "pending" {
		t.Fatalf("unexpected first Review result: %#v", first)
	}
	if target, ok := first.Value.ControlCommand.Payload["target"].(map[string]any); !ok ||
		target["type"] != "baseBranch" || target["branch"] != "main" {
		t.Fatalf("Review did not resolve the Project default branch: %#v", first.Value.ControlCommand.Payload)
	}

	replayed, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID, input,
		"review-replay", "review-replayed", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.StatusCode != 202 ||
		replayed.Value.ExecutionID != first.Value.ExecutionID ||
		replayed.Value.ControlCommand.ID != first.Value.ControlCommand.ID {
		t.Fatalf("unexpected Review replay: first=%#v replay=%#v", first, replayed)
	}
	fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
}

func TestPrimaryOperationsEnforcePrivateSessionAndSequenceCAS(t *testing.T) {
	t.Run("private session", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		other := fixture.createOperator(t)
		expected := fixture.lastSequence(t, fixture.sessionID)
		_, err := fixture.service.RequestReview(
			context.Background(), other, fixture.sessionID,
			StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
			"review-private", "review-private", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 404, "session_not_found")
		fixture.assertPrimaryOperationCounts(t, 0, 0, 0, 0, 0)
	})

	t.Run("stale sequence", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		if err := fixture.db.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
			Update("last_event_sequence", 3).Error; err != nil {
			t.Fatal(err)
		}
		expected := int64(2)
		_, err := fixture.service.RequestReview(
			context.Background(), fixture.principal, fixture.sessionID,
			StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
			"review-stale", "review-stale", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, "stale_session_sequence")
		fixture.assertPrimaryOperationCounts(t, 0, 0, 0, 0, 0)
	})
}

func TestPrimaryOperationsEnforceQuotaAndObservedCapability(t *testing.T) {
	t.Run("quota", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		maximum := 0
		if err := fixture.db.Create(&persistence.TenantQuota{
			TenantID: fixture.tenantID, MaxConcurrentExecutions: &maximum, UpdatedBy: fixture.principal.UserID,
		}).Error; err != nil {
			t.Fatal(err)
		}
		expected := fixture.lastSequence(t, fixture.sessionID)
		_, err := fixture.service.RequestReview(
			context.Background(), fixture.principal, fixture.sessionID,
			StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
			"review-quota", "review-quota", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, "execution_quota_exceeded")
		fixture.assertPrimaryOperationCounts(t, 0, 0, 0, 0, 0)
	})

	t.Run("capability", func(t *testing.T) {
		capabilities := workerManifestTestCapabilities()
		setTestProviderCapability(capabilities, "codex", "review", "unsupported")
		fixture := newAdvancedOperationFixture(t, capabilities)
		expected := fixture.lastSequence(t, fixture.sessionID)
		_, err := fixture.service.RequestReview(
			context.Background(), fixture.principal, fixture.sessionID,
			StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
			"review-unsupported", "review-unsupported", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, providercapabilities.ReasonCapabilityUnsupported)
		fixture.assertPrimaryOperationCounts(t, 0, 0, 0, 0, 0)
	})
}

func TestPrimaryOperationsUseBoundManifestAcrossExecutionPinnedObservationGap(t *testing.T) {
	for _, test := range []struct {
		name            string
		compactSupport  string
		expectedProblem string
	}{
		{name: "supported", compactSupport: "native"},
		{name: "unsupported remains unobserved", compactSupport: "unsupported", expectedProblem: providercapabilities.ReasonWorkerManifestRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			capabilities := workerManifestTestCapabilities()
			setTestProviderCapability(capabilities, "codex", "compact", test.compactSupport)
			fixture := newAdvancedOperationFixture(t, capabilities)
			expected := fixture.lastSequence(t, fixture.sessionID)
			review, err := fixture.service.RequestReview(
				context.Background(), fixture.principal, fixture.sessionID,
				StartReviewInput{
					ExpectedLastEventSequence: &expected,
					Target:                    ReviewTarget{Type: "uncommittedChanges"},
				},
				"review-observation-gap", "review-observation-gap", "127.0.0.1",
			)
			if err != nil {
				t.Fatal(err)
			}
			fixture.completeExecutionWithCurrentManifest(t, review.Value.ExecutionID)
			fixture.setUsableCursor(t)
			fixture.markWorkersOffline(t)

			expected = fixture.lastSequence(t, fixture.sessionID)
			compact, err := fixture.service.RequestCompact(
				context.Background(), fixture.principal, fixture.sessionID,
				CompactSessionInput{ExpectedLastEventSequence: &expected},
				"compact-observation-gap", "compact-observation-gap", "127.0.0.1",
			)
			if test.expectedProblem != "" {
				assertAdvancedOperationProblem(t, err, 409, test.expectedProblem)
				fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if compact.StatusCode != 202 || compact.Value.Type != "compact" {
				t.Fatalf("unexpected Compact result after observation gap: %#v", compact)
			}
			fixture.assertPrimaryOperationCounts(t, 2, 2, 2, 2, 2)
		})
	}
}

func TestPrimaryOperationsRetryPastPoolScopedObservationGapOnPreferredTarget(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	fixture.markWorkersOffline(t)
	badTarget := fixture.loadTarget(t, fixture.targetID)
	badDefault := configureAdvancedWarmDefaultPool(t, fixture.db, badTarget)
	alternatePool := createAdvancedWorkerPool(t, fixture.db, badTarget, "advanced-alt-supported")
	registerPoolWorkerWithCapabilities(t, fixture.service, badTarget.ID, badTarget.Kind, alternatePool, "advanced-alt-supported", workerManifestTestCapabilities())

	goodTarget := fixture.createExecutionTarget(t, "advanced-good-target", nil)
	goodDefault := configureAdvancedWarmDefaultPool(t, fixture.db, goodTarget)
	registerPoolWorkerWithCapabilities(t, fixture.service, goodTarget.ID, goodTarget.Kind, goodDefault, "advanced-good-default", workerManifestTestCapabilities())

	group, _, destinationMember := fixture.configureSessionTargetGroup(
		t, badTarget, goodTarget, "cn-shanghai", "cluster-a", "cn-shanghai", "cluster-b",
	)
	now := time.Now().UTC()
	fixture.observeTargetHealth(t, badTarget.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)
	fixture.observeTargetHealth(t, goodTarget.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)

	expected := fixture.lastSequence(t, fixture.sessionID)
	review, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-pool-scoped-observation-gap", "review-pool-scoped-observation-gap", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	execution := fixture.loadExecution(t, review.Value.ExecutionID)
	if execution.ExecutionTargetID != goodTarget.ID {
		t.Fatalf("execution target = %s, want %s", execution.ExecutionTargetID, goodTarget.ID)
	}
	if execution.WorkerPoolID == nil || *execution.WorkerPoolID != goodDefault.ID {
		t.Fatalf("execution pool = %#v, want %s", execution.WorkerPoolID, goodDefault.ID)
	}
	if execution.TargetGroupID == nil || *execution.TargetGroupID != group.ID ||
		execution.TargetGroupMemberVersion == nil || *execution.TargetGroupMemberVersion != destinationMember.Version {
		t.Fatalf("execution routing snapshot = %#v", execution)
	}
	session := fixture.loadSession(t)
	if session.ExecutionTargetID != goodTarget.ID {
		t.Fatalf("session execution target = %s, want %s", session.ExecutionTargetID, goodTarget.ID)
	}
	if badDefault.ID == uuid.Nil {
		t.Fatal("bad default pool was not configured")
	}
}

func TestReviewUsesBoundManifestAcrossExecutionPinnedObservationGap(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	expected := fixture.lastSequence(t, fixture.sessionID)
	first, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-observation-gap-first", "review-observation-gap-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.completeExecutionWithCurrentManifest(t, first.Value.ExecutionID)
	fixture.markWorkersOffline(t)

	expected = fixture.lastSequence(t, fixture.sessionID)
	second, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-observation-gap-second", "review-observation-gap-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.StatusCode != 202 || second.Value.Type != "review" {
		t.Fatalf("unexpected Review result after observation gap: %#v", second)
	}
	fixture.assertPrimaryOperationCounts(t, 2, 2, 2, 2, 2)
}

func TestCompactRequiresUsableCursorAfterNewForkAndRollbackHistory(t *testing.T) {
	t.Run("new session", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		expected := fixture.lastSequence(t, fixture.sessionID)
		_, err := fixture.service.RequestCompact(
			context.Background(), fixture.principal, fixture.sessionID,
			CompactSessionInput{ExpectedLastEventSequence: &expected},
			"compact-new", "compact-new", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, providercapabilities.ReasonProviderCursorRequired)
	})

	t.Run("fork", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		expected := fixture.lastSequence(t, fixture.sessionID)
		forked, _, err := fixture.sessions.Fork(
			context.Background(), fixture.principal, fixture.sessionID,
			sessions.ForkSessionInput{ExpectedLastEventSequence: &expected, Title: "Forked history"},
			"fork-before-compact", "fork-before-compact", "127.0.0.1",
		)
		if err != nil {
			t.Fatal(err)
		}
		forkSequence := forked.Session.LastEventSequence
		_, err = fixture.service.RequestCompact(
			context.Background(), fixture.principal, forked.Session.ID,
			CompactSessionInput{ExpectedLastEventSequence: &forkSequence},
			"compact-fork", "compact-fork", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, providercapabilities.ReasonProviderCursorRequired)
	})

	t.Run("rollback", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		turnID := fixture.appendCompletedTurn(t)
		fixture.setUsableCursor(t)
		expected := fixture.lastSequence(t, fixture.sessionID)
		rolledBack, _, err := fixture.sessions.Rollback(
			context.Background(), fixture.principal, fixture.sessionID,
			sessions.RollbackSessionInput{ExpectedLastEventSequence: &expected, FromTurnID: turnID},
			"rollback-before-compact", "rollback-before-compact", "127.0.0.1",
		)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fixture.service.RequestCompact(
			context.Background(), fixture.principal, fixture.sessionID,
			CompactSessionInput{ExpectedLastEventSequence: &rolledBack.EventSequence},
			"compact-rollback", "compact-rollback", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, providercapabilities.ReasonProviderCursorRequired)
	})
}

func TestCompactWithUsableCursorQueuesOnePrimaryOperation(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	fixture.setUsableCursor(t)
	expected := fixture.lastSequence(t, fixture.sessionID)
	result, err := fixture.service.RequestCompact(
		context.Background(), fixture.principal, fixture.sessionID,
		CompactSessionInput{ExpectedLastEventSequence: &expected},
		"compact-success", "compact-success", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != 202 || result.Value.Type != "compact" || result.Value.Turn.TurnKind != "compact" ||
		result.Value.Turn.RuntimeMode != "full-access" || result.Value.ControlCommand.CommandType != "CompactSession" {
		t.Fatalf("unexpected Compact result: %#v", result)
	}
	fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
}

func TestPrimaryOperationsFreezeGroupRoutingSnapshotIntoExecutionEventAndOutbox(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	source := fixture.loadTarget(t, fixture.targetID)
	destination := fixture.createExecutionTarget(t, "advanced-operation-snapshot-destination", nil)
	group, sourceMember, _ := fixture.configureSessionTargetGroup(
		t,
		source,
		destination,
		"cn-shanghai",
		"cluster-a",
		"cn-shanghai",
		"cluster-b",
	)
	now := time.Now().UTC()
	fixture.observeTargetHealth(t, source.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)
	fixture.observeTargetHealth(t, destination.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)

	expected := fixture.lastSequence(t, fixture.sessionID)
	result, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-routing-snapshot", "review-routing-snapshot", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}

	execution := fixture.loadExecution(t, result.Value.ExecutionID)
	if execution.ExecutionTargetID != source.ID ||
		execution.TargetGroupID == nil || *execution.TargetGroupID != group.ID ||
		execution.TargetGroupVersion == nil || *execution.TargetGroupVersion != group.Version ||
		execution.TargetGroupMemberVersion == nil || *execution.TargetGroupMemberVersion != sourceMember.Version ||
		execution.SelectedRegion == nil || *execution.SelectedRegion != "cn-shanghai" ||
		execution.SelectedClusterID == nil || *execution.SelectedClusterID != "cluster-a" ||
		execution.RoutingReason == nil || *execution.RoutingReason != "preferred-target" {
		t.Fatalf("execution routing snapshot = %#v", execution)
	}
	if execution.SchedulingDecisionID == nil {
		t.Fatal("routed operation omitted its scheduling decision identity")
	}
	var decision persistence.ExecutionSchedulingDecision
	if err := fixture.db.Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, execution.ID).
		Take(&decision).Error; err != nil {
		t.Fatal(err)
	}

	event := fixture.loadTurnCreatedEvent(t, result.Value.ExecutionID)
	fixture.assertRoutingPayload(t, event.Payload, routingPayloadExpectation{
		TargetGroupID: group.ID, GroupVersion: group.Version, MemberVersion: sourceMember.Version,
		Region: "cn-shanghai", ClusterID: "cluster-a", Reason: "preferred-target",
	})
	fixture.assertSchedulingDecisionPayload(t, event.Payload, decision)

	outboxMessage := fixture.loadLatestExecutionQueuedOutbox(t)
	fixture.assertRoutingPayload(t, outboxMessage.Payload, routingPayloadExpectation{
		TargetGroupID: group.ID, GroupVersion: group.Version, MemberVersion: sourceMember.Version,
		Region: "cn-shanghai", ClusterID: "cluster-a", Reason: "preferred-target",
	})
	fixture.assertSchedulingDecisionPayload(t, outboxMessage.Payload, decision)

	session := fixture.loadSession(t)
	if session.ExecutionTargetID != source.ID || session.RoutingPolicyVersion == nil || *session.RoutingPolicyVersion != group.Version {
		t.Fatalf("session routing authority = %#v", session)
	}
	fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
}

func TestPrimaryOperationsCrossDomainRerouteUseFrozenSourceAuthorityAndAdvanceSession(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	source := fixture.loadTarget(t, fixture.targetID)
	destination := fixture.createExecutionTarget(t, "advanced-operation-dr-destination", nil)
	group, sourceMember, destinationMember := fixture.configureSessionTargetGroup(
		t,
		source,
		destination,
		"cn-shanghai",
		"cluster-a",
		"cn-beijing",
		"cluster-b",
	)
	now := time.Now().UTC().Add(-10 * time.Second)
	fixture.observeTargetHealth(t, source.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)
	fixture.observeTargetHealth(t, destination.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)

	expected := fixture.lastSequence(t, fixture.sessionID)
	first, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-routing-dr-first", "review-routing-dr-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	firstExecution := fixture.loadExecution(t, first.Value.ExecutionID)
	if firstExecution.TargetGroupMemberVersion == nil || *firstExecution.TargetGroupMemberVersion != sourceMember.Version {
		t.Fatalf("seed execution did not remain on the preferred source target: %#v", firstExecution)
	}
	fixture.completeExecutionWithCurrentManifest(t, first.Value.ExecutionID)

	readyAt := now.Add(time.Second)
	fixture.createReadyWorkspaceCheckpoint(t, firstExecution, first.Value.Turn.ID, readyAt)
	fixture.observeTargetHealth(t, source.ID, routing.HealthUnreachable, routing.CapacityUnknown, nil, 0, readyAt.Add(time.Second))
	fixture.observeTargetHealth(t, destination.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, readyAt.Add(2*time.Second))
	sourceDRDomain := routing.DRDomainForLocation("cn-shanghai", "cluster-a")
	fixture.observeTargetDRReadiness(
		t,
		destination.ID,
		sourceDRDomain,
		routing.DRDomainForLocation("cn-beijing", "cluster-b"),
		readyAt,
		false,
		true,
		false,
		readyAt.Add(3*time.Second),
	)

	expected = fixture.lastSequence(t, fixture.sessionID)
	second, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-routing-dr-second", "review-routing-dr-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}

	execution := fixture.loadExecution(t, second.Value.ExecutionID)
	if execution.ExecutionTargetID != destination.ID ||
		execution.TargetGroupID == nil || *execution.TargetGroupID != group.ID ||
		execution.TargetGroupVersion == nil || *execution.TargetGroupVersion != group.Version ||
		execution.TargetGroupMemberVersion == nil || *execution.TargetGroupMemberVersion != destinationMember.Version ||
		execution.SelectedRegion == nil || *execution.SelectedRegion != "cn-beijing" ||
		execution.SelectedClusterID == nil || *execution.SelectedClusterID != "cluster-b" ||
		execution.RoutingReason == nil || *execution.RoutingReason != routing.StrategyPriority {
		t.Fatalf("rerouted execution snapshot = %#v", execution)
	}
	if execution.SchedulingDecisionID == nil {
		t.Fatal("cross-domain routed Execution omitted its scheduling decision identity")
	}
	var decision persistence.ExecutionSchedulingDecision
	if err := fixture.db.Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, execution.ID).
		Take(&decision).Error; err != nil {
		t.Fatal(err)
	}
	var candidates []persistence.ExecutionSchedulingCandidate
	if err := fixture.db.Where("tenant_id = ? AND decision_id = ?", fixture.tenantID, decision.ID).
		Order("ordinal ASC").Find(&candidates).Error; err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("cross-domain scheduling candidates = %#v", candidates)
	}
	candidateByTarget := make(map[uuid.UUID]persistence.ExecutionSchedulingCandidate, len(candidates))
	for _, value := range candidates {
		candidateByTarget[value.ExecutionTargetID] = value
	}
	candidate := candidateByTarget[destination.ID]
	rejectedSource := candidateByTarget[source.ID]
	if decision.AlgorithmVersion != "queue-pressure-v1" || decision.EvidenceCompleteness != "complete" ||
		decision.CandidateCount != 2 ||
		candidate.DRReadinessVersion == nil || candidate.SourceDRDomain == nil ||
		*candidate.SourceDRDomain != sourceDRDomain || candidate.DRDomain == nil ||
		*candidate.DRDomain != routing.DRDomainForLocation("cn-beijing", "cluster-b") ||
		candidate.DRReplicatedThroughAt == nil || !candidate.DRReplicatedThroughAt.Equal(readyAt) ||
		candidate.DRArtifactsReady == nil || *candidate.DRArtifactsReady ||
		candidate.DRCheckpointsReady == nil || !*candidate.DRCheckpointsReady ||
		candidate.DRMemoryReady == nil || *candidate.DRMemoryReady ||
		candidate.DRObservedAt == nil || candidate.DRExpiresAt == nil ||
		rejectedSource.Eligibility != "rejected" || rejectedSource.RejectionCode == nil ||
		*rejectedSource.RejectionCode != "health-status-ineligible" {
		t.Fatalf("cross-domain scheduling evidence = decision %#v candidate %#v", decision, candidate)
	}

	session := fixture.loadSession(t)
	if session.ExecutionTargetID != destination.ID || session.RoutingPolicyVersion == nil || *session.RoutingPolicyVersion != group.Version {
		t.Fatalf("session did not advance to the rerouted target: %#v", session)
	}
	fixture.assertPrimaryOperationCounts(t, 2, 2, 2, 2, 2)
}

func TestPrimaryOperationsCrossDomainRerouteUsesProviderAffinityBetweenEligibleDestinations(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	source := fixture.loadTarget(t, fixture.targetID)
	avoidDestination := fixture.createExecutionTarget(t, "advanced-operation-dr-avoid", nil)
	preferDestination := fixture.createExecutionTarget(t, "advanced-operation-dr-prefer", nil)
	setAdvancedOperationTargetRoutingPreferences(t, fixture.db, avoidDestination.ID, map[string]string{"codex": "avoid"})
	setAdvancedOperationTargetRoutingPreferences(t, fixture.db, preferDestination.ID, map[string]string{"codex": "prefer"})

	router := routing.NewService(fixture.db)
	group, err := router.CreateGroup(context.Background(), routing.CreateGroupInput{
		TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "advanced-operation-affinity-group-" + uuid.NewString(), Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, MaxFailoverAttempts: intPointer(3), HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceMember, err := router.AddMember(context.Background(), routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: source.ID,
		Region: "cn-shanghai", ClusterID: "cluster-a", Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(context.Background(), routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: avoidDestination.ID,
		Region: "cn-beijing", ClusterID: "cluster-b", Priority: 20, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(context.Background(), routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: preferDestination.ID,
		Region: "cn-beijing", ClusterID: "cluster-c", Priority: 30, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"requested_execution_target_id": source.ID,
			"execution_target_group_id":     group.ID,
			"routing_policy_version":        group.Version,
			"execution_target_id":           source.ID,
			"preferred_execution_region":    "cn-shanghai",
		}).Error; err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(-10 * time.Second)
	fixture.observeTargetHealth(t, source.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)
	fixture.observeTargetHealth(t, avoidDestination.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)
	fixture.observeTargetHealth(t, preferDestination.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, now)

	expected := fixture.lastSequence(t, fixture.sessionID)
	first, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-routing-affinity-first", "review-routing-affinity-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	firstExecution := fixture.loadExecution(t, first.Value.ExecutionID)
	if firstExecution.TargetGroupMemberVersion == nil || *firstExecution.TargetGroupMemberVersion != sourceMember.Version {
		t.Fatalf("seed execution did not stay on the source target: %#v", firstExecution)
	}
	fixture.completeExecutionWithCurrentManifest(t, first.Value.ExecutionID)

	readyAt := now.Add(time.Second)
	fixture.createReadyWorkspaceCheckpoint(t, firstExecution, first.Value.Turn.ID, readyAt)
	fixture.observeTargetHealth(t, source.ID, routing.HealthUnreachable, routing.CapacityUnknown, nil, 0, readyAt.Add(time.Second))
	fixture.observeTargetHealth(t, avoidDestination.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, readyAt.Add(2*time.Second))
	fixture.observeTargetHealth(t, preferDestination.ID, routing.HealthHealthy, routing.CapacityAvailable, intPointer(10), 0, readyAt.Add(3*time.Second))
	sourceDRDomain := routing.DRDomainForLocation("cn-shanghai", "cluster-a")
	fixture.observeTargetDRReadiness(
		t,
		avoidDestination.ID,
		sourceDRDomain,
		routing.DRDomainForLocation("cn-beijing", "cluster-b"),
		readyAt,
		false,
		true,
		false,
		readyAt.Add(4*time.Second),
	)
	fixture.observeTargetDRReadiness(
		t,
		preferDestination.ID,
		sourceDRDomain,
		routing.DRDomainForLocation("cn-beijing", "cluster-c"),
		readyAt,
		false,
		true,
		false,
		readyAt.Add(5*time.Second),
	)

	expected = fixture.lastSequence(t, fixture.sessionID)
	second, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-routing-affinity-second", "review-routing-affinity-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	execution := fixture.loadExecution(t, second.Value.ExecutionID)
	if execution.ExecutionTargetID != preferDestination.ID {
		t.Fatalf("provider-affinity reroute execution = %#v", execution)
	}
}

func TestPrimaryOperationsFailClosedWhenSessionRoutingCannotBeReconstructed(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	source := fixture.loadTarget(t, fixture.targetID)
	destination := fixture.createExecutionTarget(t, "advanced-operation-routing-failure-destination", nil)
	fixture.configureSessionTargetGroup(
		t,
		source,
		destination,
		"cn-shanghai",
		"cluster-a",
		"cn-beijing",
		"cluster-b",
	)
	fixture.seedLegacyCompletedExecutionWithoutRoutingSnapshot(t, source)

	expected := fixture.lastSequence(t, fixture.sessionID)
	_, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-routing-fail-closed", "review-routing-fail-closed", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 409, "session_routing_source_domain_missing")
	fixture.assertPrimaryOperationCounts(t, 1, 1, 0, 0, 0)
}

func TestConcurrentPrimaryOperationRequestsHaveSingleWinner(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	expected := fixture.lastSequence(t, fixture.sessionID)
	type outcome struct {
		result OperationResult[QueuedSessionOperation]
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			result, err := fixture.service.RequestReview(
				context.Background(), fixture.principal, fixture.sessionID,
				StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
				"review-concurrent-"+uuid.NewString(), "review-concurrent", "127.0.0.1",
			)
			outcomes <- outcome{result: result, err: err}
		}(index)
	}
	close(start)
	wait.Wait()
	close(outcomes)

	succeeded := 0
	rejected := 0
	for item := range outcomes {
		if item.err == nil {
			succeeded++
			continue
		}
		var apiError *problem.Error
		if errors.As(item.err, &apiError) &&
			(apiError.Code == "session_execution_active" || apiError.Code == "stale_session_sequence") {
			rejected++
			continue
		}
		t.Fatalf("unexpected concurrent Review failure: %v", item.err)
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent Review outcomes success=%d rejected=%d", succeeded, rejected)
	}
	fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
}

type advancedOperationFixture struct {
	db             *gorm.DB
	service        *Service
	sessions       *sessions.Service
	principal      identity.Principal
	tenantID       uuid.UUID
	organizationID uuid.UUID
	projectID      uuid.UUID
	sessionID      uuid.UUID
	targetID       uuid.UUID
}

func newAdvancedOperationFixture(t *testing.T, capabilities map[string]any) advancedOperationFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "advanced-operation-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectID := uuid.New()
	targetID := uuid.New()
	sessionID := uuid.New()
	models := []any{
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Advanced operations", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
		},
		&persistence.ExecutionTarget{
			ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
			Kind: "kubernetes", Name: "advanced-operation-target", Status: "active", ConfigurationEncrypted: []byte{},
			Capabilities: workerManifestTestTargetCapabilities(), CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Advanced operations", Status: "active",
			Visibility: "private", Provider: "codex", ExecutionTargetID: targetID,
			ProviderResumeCursorState: "absent", CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), profile, nil)
	projectService := projects.NewService(store.DB())
	sessionService := sessions.NewService(store.DB(), projectService, targetService)
	service := NewService(
		store.DB(), sessionService, time.Minute, 90*time.Second, time.Hour, nil, targetService,
		WithProjectService(projectService),
	)
	if capabilities == nil {
		capabilities = workerManifestTestCapabilities()
	}
	registerTestWorkerWithCapabilities(t, service, targetID, "kubernetes", "advanced-operation", capabilities)
	return advancedOperationFixture{
		db: store.DB(), service: service, sessions: sessionService,
		principal: identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		tenantID:  domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, sessionID: sessionID, targetID: targetID,
	}
}

func (f advancedOperationFixture) createOperator(t *testing.T) identity.Principal {
	t.Helper()
	now := time.Now().UTC()
	userID := uuid.New()
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.User{
				ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Advanced operation operator",
				Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.TenantMembership{
				TenantID: f.tenantID, UserID: userID, Role: "member", Status: "active",
				JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.OrganizationMembership{
				TenantID: f.tenantID, OrganizationID: f.organizationID, UserID: userID,
				Role: "agent_operator", Status: "active", CreatedAt: now, UpdatedAt: now,
			},
		}
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return identity.Principal{UserID: userID, ActiveTenantID: &f.tenantID}
}

type routingPayloadExpectation struct {
	TargetGroupID uuid.UUID
	GroupVersion  int64
	MemberVersion int64
	Region        string
	ClusterID     string
	Reason        string
}

func (f advancedOperationFixture) lastSequence(t *testing.T, sessionID uuid.UUID) int64 {
	t.Helper()
	var session persistence.AgentSession
	if err := f.db.Select("last_event_sequence").Where("tenant_id = ? AND id = ?", f.tenantID, sessionID).
		Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session.LastEventSequence
}

func (f advancedOperationFixture) setUsableCursor(t *testing.T) {
	t.Helper()
	if err := f.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", f.tenantID, f.sessionID).
		Updates(map[string]any{
			"provider_resume_cursor_state":     "usable",
			"provider_resume_cursor_encrypted": []byte("encrypted-test-cursor"),
		}).Error; err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) loadSession(t *testing.T) persistence.AgentSession {
	t.Helper()
	var session persistence.AgentSession
	if err := f.db.Where("tenant_id = ? AND id = ?", f.tenantID, f.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session
}

func (f advancedOperationFixture) loadTarget(t *testing.T, targetID uuid.UUID) persistence.ExecutionTarget {
	t.Helper()
	var target persistence.ExecutionTarget
	if err := f.db.Where("id = ?", targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	return target
}

func setAdvancedOperationTargetRoutingPreferences(
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

func (f advancedOperationFixture) createExecutionTarget(
	t *testing.T,
	name string,
	workerCapabilities map[string]any,
) persistence.ExecutionTarget {
	t.Helper()
	now := time.Now().UTC()
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &f.tenantID, OrganizationID: &f.organizationID,
		Kind: "kubernetes", Name: name, Status: "active", ConfigurationEncrypted: []byte{},
		Capabilities: workerManifestTestTargetCapabilities(), CreatedAt: now, UpdatedAt: now,
	}
	if err := f.db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	if workerCapabilities == nil {
		workerCapabilities = workerManifestTestCapabilities()
	}
	registerTestWorkerWithCapabilities(t, f.service, target.ID, target.Kind, name, workerCapabilities)
	return target
}

func (f advancedOperationFixture) configureSessionTargetGroup(
	t *testing.T,
	source persistence.ExecutionTarget,
	destination persistence.ExecutionTarget,
	sourceRegion, sourceClusterID, destinationRegion, destinationClusterID string,
) (
	persistence.ExecutionTargetGroup,
	persistence.ExecutionTargetGroupMember,
	persistence.ExecutionTargetGroupMember,
) {
	t.Helper()
	router := routing.NewService(f.db)
	group, err := router.CreateGroup(context.Background(), routing.CreateGroupInput{
		TenantID: f.tenantID, OrganizationID: &f.organizationID,
		Name: "advanced-operation-group-" + uuid.NewString(), Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, MaxFailoverAttempts: intPointer(3), HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceMember, err := router.AddMember(context.Background(), routing.AddMemberInput{
		TenantID: f.tenantID, TargetGroupID: group.ID, ExecutionTargetID: source.ID,
		Region: sourceRegion, ClusterID: sourceClusterID, Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	destinationMember, err := router.AddMember(context.Background(), routing.AddMemberInput{
		TenantID: f.tenantID, TargetGroupID: group.ID, ExecutionTargetID: destination.ID,
		Region: destinationRegion, ClusterID: destinationClusterID, Priority: 20, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", f.tenantID, f.sessionID).
		Updates(map[string]any{
			"requested_execution_target_id": source.ID,
			"execution_target_group_id":     group.ID,
			"routing_policy_version":        group.Version,
			"execution_target_id":           source.ID,
			"preferred_execution_region":    sourceRegion,
		}).Error; err != nil {
		t.Fatal(err)
	}
	return group, sourceMember, destinationMember
}

func (f advancedOperationFixture) observeTargetHealth(
	t *testing.T,
	targetID uuid.UUID,
	status, capacityStatus string,
	availableCapacityUnits *int,
	allocatedCapacityUnits int,
	observedAt time.Time,
) {
	t.Helper()
	if _, err := routing.NewService(f.db).ObserveHealth(context.Background(), routing.HealthObservation{
		ExecutionTargetID:      targetID,
		Status:                 status,
		CapacityStatus:         capacityStatus,
		AvailableCapacityUnits: availableCapacityUnits,
		AllocatedCapacityUnits: allocatedCapacityUnits,
		Source:                 "advanced-operations-test",
		ObservedAt:             observedAt,
		TTL:                    time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) observeTargetDRReadiness(
	t *testing.T,
	targetID uuid.UUID,
	sourceDRDomain, drDomain string,
	replicatedThroughAt time.Time,
	artifactsReady, checkpointsReady, memoryReady bool,
	observedAt time.Time,
) {
	t.Helper()
	if _, err := routing.NewService(f.db).ObserveDRReadiness(context.Background(), routing.DRReadinessObservation{
		ExecutionTargetID:   targetID,
		SourceDRDomain:      sourceDRDomain,
		DRDomain:            drDomain,
		ReplicatedThroughAt: replicatedThroughAt,
		ArtifactsReady:      artifactsReady,
		CheckpointsReady:    checkpointsReady,
		MemoryReady:         memoryReady,
		PublisherIdentity:   "advanced-operations-test",
		ObservedAt:          observedAt,
		TTL:                 time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) completeExecutionWithCurrentManifest(
	t *testing.T,
	executionID uuid.UUID,
) {
	t.Helper()
	var execution persistence.AgentExecution
	if err := f.db.Where("tenant_id = ? AND id = ?", f.tenantID, executionID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	var worker persistence.WorkerInstance
	if err := f.db.Where("execution_target_id = ? AND current_manifest_id IS NOT NULL", f.targetID).
		Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if execution.ProviderRuntimeBindingID == nil || worker.CurrentManifestID == nil {
		t.Fatal("test execution or Worker omitted its Provider runtime manifest binding")
	}
	now := time.Now().UTC()
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", f.tenantID, execution.ID).
			Updates(map[string]any{
				"status": "completed", "worker_id": worker.ID,
				"worker_manifest_id": worker.CurrentManifestID,
				"started_at":         now, "finished_at": now,
			}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND id = ?", f.tenantID, execution.TurnID).
			Updates(map[string]any{"status": "completed", "started_at": now, "completed_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.ExecutionControlCommand{}).
			Where("tenant_id = ? AND execution_id = ?", f.tenantID, execution.ID).
			Updates(map[string]any{"status": "acknowledged", "acknowledged_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.ProviderRuntimeBinding{}).
			Where("tenant_id = ? AND id = ? AND session_id = ?", f.tenantID,
				*execution.ProviderRuntimeBindingID, f.sessionID).
			Updates(map[string]any{
				"worker_manifest_id": worker.CurrentManifestID,
				"last_execution_id":  execution.ID,
				"last_generation":    execution.Generation,
				"status":             "active", "updated_at": now,
			}).Error
	}); err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) markWorkersOffline(t *testing.T) {
	t.Helper()
	if err := f.db.Model(&persistence.WorkerInstance{}).
		Where("execution_target_id = ?", f.targetID).
		Update("status", "offline").Error; err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) loadExecution(t *testing.T, executionID uuid.UUID) persistence.AgentExecution {
	t.Helper()
	var execution persistence.AgentExecution
	if err := f.db.Where("tenant_id = ? AND id = ?", f.tenantID, executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	return execution
}

func (f advancedOperationFixture) loadTurnCreatedEvent(
	t *testing.T,
	executionID uuid.UUID,
) persistence.SessionEvent {
	t.Helper()
	var event persistence.SessionEvent
	if err := f.db.Where(
		"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
		f.tenantID, f.sessionID, executionID, "turn.created",
	).Order("sequence DESC").Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	return event
}

func (f advancedOperationFixture) loadLatestExecutionQueuedOutbox(t *testing.T) persistence.OutboxMessage {
	t.Helper()
	var message persistence.OutboxMessage
	if err := f.db.Where("tenant_id = ? AND topic = ?", f.tenantID, "execution.queued").
		Order("created_at DESC, id DESC").Take(&message).Error; err != nil {
		t.Fatal(err)
	}
	return message
}

func (f advancedOperationFixture) assertRoutingPayload(
	t *testing.T,
	payload map[string]any,
	expected routingPayloadExpectation,
) {
	t.Helper()
	if payload == nil {
		t.Fatal("routing payload is nil")
	}
	if got := payload["targetGroupId"]; got != expected.TargetGroupID.String() {
		t.Fatalf("payload targetGroupId = %#v, want %s", got, expected.TargetGroupID)
	}
	if got := payload["targetGroupVersion"]; got != float64(expected.GroupVersion) {
		t.Fatalf("payload targetGroupVersion = %#v, want %d", got, expected.GroupVersion)
	}
	if got := payload["targetGroupMemberVersion"]; got != float64(expected.MemberVersion) {
		t.Fatalf("payload targetGroupMemberVersion = %#v, want %d", got, expected.MemberVersion)
	}
	if got := payload["selectedRegion"]; got != expected.Region {
		t.Fatalf("payload selectedRegion = %#v, want %s", got, expected.Region)
	}
	if got := payload["selectedClusterId"]; got != expected.ClusterID {
		t.Fatalf("payload selectedClusterId = %#v, want %s", got, expected.ClusterID)
	}
	if got := payload["routingReason"]; got != expected.Reason {
		t.Fatalf("payload routingReason = %#v, want %s", got, expected.Reason)
	}
}

func (f advancedOperationFixture) assertSchedulingDecisionPayload(
	t *testing.T,
	payload map[string]any,
	decision persistence.ExecutionSchedulingDecision,
) {
	t.Helper()
	if payload == nil {
		t.Fatal("scheduling decision payload is nil")
	}
	if got := payload["schedulingDecisionId"]; got != decision.ID.String() {
		t.Fatalf("payload schedulingDecisionId = %#v, want %s", got, decision.ID)
	}
	if got := payload["schedulingAlgorithmVersion"]; got != decision.AlgorithmVersion {
		t.Fatalf("payload schedulingAlgorithmVersion = %#v, want %s", got, decision.AlgorithmVersion)
	}
	if got := payload["schedulingEvidenceCompleteness"]; got != decision.EvidenceCompleteness {
		t.Fatalf("payload schedulingEvidenceCompleteness = %#v, want %s", got, decision.EvidenceCompleteness)
	}
	if got := payload["schedulingCandidateSetSha256"]; got != decision.CandidateSetSHA256 {
		t.Fatalf("payload schedulingCandidateSetSha256 = %#v, want %s", got, decision.CandidateSetSHA256)
	}
}

func (f advancedOperationFixture) createReadyWorkspaceCheckpoint(
	t *testing.T,
	execution persistence.AgentExecution,
	turnID uuid.UUID,
	readyAt time.Time,
) {
	t.Helper()
	var workspace persistence.RemoteWorkspace
	if err := f.db.Where("tenant_id = ? AND session_id = ?", f.tenantID, f.sessionID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: f.tenantID, WorkspaceID: workspace.ID, SessionID: f.sessionID,
		TurnID: &turnID, ExecutionID: execution.ID, Generation: execution.Generation,
		IdempotencyKey: "advanced-operation-checkpoint-" + uuid.NewString(),
		Strategy:       "manual",
		Status:         "ready",
		CreatedAt:      readyAt.Add(-time.Second),
		ReadyAt:        &readyAt,
	}
	if err := f.db.Create(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", f.tenantID, workspace.ID).
		Updates(map[string]any{
			"current_checkpoint_id": checkpoint.ID,
			"updated_at":            readyAt,
		}).Error; err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) seedLegacyCompletedExecutionWithoutRoutingSnapshot(
	t *testing.T,
	target persistence.ExecutionTarget,
) persistence.AgentExecution {
	t.Helper()
	now := time.Now().UTC()
	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: f.tenantID, SessionID: f.sessionID, CreatedBy: f.principal.UserID,
		Status: "completed", InputText: "legacy-routing-authority", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now, CompletedAt: &now,
	}
	if err := f.db.Create(&turn).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: f.tenantID, SessionID: f.sessionID, TurnID: turn.ID,
		Attempt: 1, Status: "completed", ExecutionTargetID: target.ID, TargetKind: target.Kind,
		Provider: &provider, WarmPoolModeSnapshot: "disabled", RequestedBy: f.principal.UserID,
		QueuedAt: now, FinishedAt: &now,
	}
	if err := f.db.Create(&execution).Error; err != nil {
		t.Fatal(err)
	}
	return execution
}

func (f advancedOperationFixture) appendCompletedTurn(t *testing.T) uuid.UUID {
	t.Helper()
	turnID := uuid.New()
	now := time.Now().UTC()
	if err := f.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: f.tenantID, SessionID: f.sessionID, CreatedBy: f.principal.UserID,
		Status: "completed", InputText: "history to roll back", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", CompletedAt: &now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		_, err := f.sessions.AppendInternalEvent(context.Background(), tx, f.tenantID, f.sessionID, sessions.InternalEventInput{
			EventType: "turn.created", ActorType: "user", ActorID: &f.principal.UserID,
			Payload: map[string]any{"turnId": turnID, "status": "completed", "inputText": "history to roll back"},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return turnID
}

func (f advancedOperationFixture) assertPrimaryOperationCounts(
	t *testing.T,
	turns, executions, commands, events, outboxMessages int64,
) {
	t.Helper()
	assertCount := func(model any, query string, expected int64, args ...any) {
		t.Helper()
		var count int64
		if err := f.db.Model(model).Where(query, args...).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != expected {
			t.Fatalf("%T count = %d, want %d", model, count, expected)
		}
	}
	assertCount(&persistence.AgentTurn{}, "tenant_id = ? AND session_id = ?", turns, f.tenantID, f.sessionID)
	assertCount(&persistence.AgentExecution{}, "tenant_id = ? AND session_id = ?", executions, f.tenantID, f.sessionID)
	assertCount(&persistence.ExecutionControlCommand{}, "tenant_id = ? AND session_id = ?", commands, f.tenantID, f.sessionID)
	assertCount(&persistence.SessionEvent{}, "tenant_id = ? AND session_id = ?", events, f.tenantID, f.sessionID)
	assertCount(&persistence.OutboxMessage{}, "tenant_id = ? AND topic = ?", outboxMessages, f.tenantID, "execution.queued")
}

func assertAdvancedOperationProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != status || apiError.Code != code {
		t.Fatalf("error = %#v, want status %d code %q", err, status, code)
	}
}

func registerPoolWorkerWithCapabilities(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	targetKind string,
	pool persistence.WorkerPool,
	podName string,
	capabilities map[string]any,
) persistence.WorkerInstance {
	t.Helper()
	parsedTargetKind, err := platform.ParseExecutionTargetKind(targetKind)
	if err != nil {
		t.Fatal(err)
	}
	instanceUID := uuid.NewString()
	fullPodName := podName + "-" + uuid.NewString()
	if runtimeCapability, ok := capabilities["workerRuntime"].(map[string]any); ok && runtimeCapability["processContainment"] != nil {
		signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
			ExecutionTargetID: targetID, TargetKind: parsedTargetKind, InstanceUID: instanceUID,
			ClusterID: "test-cluster", Namespace: "default", PodName: fullPodName,
		})
	}
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: targetID,
		TargetKind:        targetKind,
		WorkerMode:        WorkerModeWarmPool,
		WorkerPoolID:      &pool.ID,
		WorkerPoolVersion: &pool.Version,
		CapacityClass:     &pool.CapacityClass,
		InstanceUID:       instanceUID,
		ClusterID:         "test-cluster",
		Namespace:         "default",
		PodName:           fullPodName,
		Version:           "worker-test",
		ProtocolVersion:   WorkerProtocolVersion,
		Capabilities:      capabilities,
		LeaseSupported:    true,
		FencingSupported:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(context.Background(), registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func ensureAdvancedTargetDefaultPlacement(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
) placement.Selection {
	t.Helper()
	selection, err := placement.NewService(db).SelectExecution(context.Background(), db, target, placement.WarmPoolModeDisabled)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func configureAdvancedWarmDefaultPool(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
) persistence.WorkerPool {
	t.Helper()
	ensureAdvancedTargetDefaultPlacement(t, db, target)
	now := time.Now().UTC()
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: target.TenantID, ExecutionTargetID: target.ID,
		Name: "warm-default-" + uuid.NewString(), Mode: placement.PoolModeWarm, CapacityClass: placement.CapacityClassInteractive,
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{},
		Status: placement.PoolStatusActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionPlacementPolicy{}).
		Where("execution_target_id = ?", target.ID).
		Updates(map[string]any{
			"default_pool_id": pool.ID,
			"version":         gorm.Expr("version + 1"),
			"updated_at":      now,
		}).Error; err != nil {
		t.Fatal(err)
	}
	return pool
}

func createAdvancedWorkerPool(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	name string,
) persistence.WorkerPool {
	t.Helper()
	now := time.Now().UTC()
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: target.TenantID, ExecutionTargetID: target.ID,
		Name: name, Mode: placement.PoolModeWarm, CapacityClass: placement.CapacityClassInteractive,
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{},
		Status: placement.PoolStatusActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	return pool
}

func intPointer(value int) *int { return &value }
