package executions

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestWaitingInteractionSuspendsAndResolveCreatesRecoveryGeneration(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	firstWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "suspend-first")
	secondWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "suspend-second")
	cleanupWorkers(t, db, firstWorker.ID, secondWorker.ID)

	claim, err := service.Claim(ctx, firstWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "suspend-first-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	firstLease := *claim.Value.Lease
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
	}
	if _, err := service.Start(ctx, firstWorker, fixture.ExecutionID, leaseInput, "suspend-first-start"); err != nil {
		t.Fatal(err)
	}
	requestID := "approval-suspend-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(ctx, firstWorker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "request.opened", OccurredAt: current,
		Payload: map[string]any{
			"requestId": requestID, "requestType": "exec_command_approval", "detail": "Apply the approved change",
		},
	}, "suspend-interaction-opened"); err != nil {
		t.Fatal(err)
	}

	tooEarly, err := service.PullResourceDirective(ctx, firstWorker, fixture.ExecutionID, PullResourceDirectiveInput{LeaseInput: leaseInput})
	if err != nil || tooEarly != nil {
		t.Fatalf("early directive = %#v, %v", tooEarly, err)
	}
	current = current.Add(16 * time.Minute)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"heartbeat_at": current, "expires_at": current.Add(service.leaseTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := service.PullResourceDirective(ctx, firstWorker, fixture.ExecutionID, PullResourceDirectiveInput{LeaseInput: leaseInput})
	if err != nil || directive == nil || directive.Action != "suspend" || directive.SuspendAttemptID == uuid.Nil {
		t.Fatalf("eligible directive = %#v, %v", directive, err)
	}

	var checkpointingSession persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&checkpointingSession).Error; err != nil {
		t.Fatal(err)
	}
	if checkpointingSession.ResourceState != "checkpointing" {
		t.Fatalf("resource state = %q, want checkpointing", checkpointingSession.ResourceState)
	}
	markResourceSuspendQuiescedForTest(
		t, service, firstWorker, fixture.ExecutionID, leaseInput, directive.SuspendAttemptID, "suspend-quiesced",
	)

	completed, err := service.CompleteResourceSuspend(ctx, firstWorker, fixture.ExecutionID, CompleteResourceSuspendInput{
		LeaseInput: leaseInput, SuspendAttemptID: directive.SuspendAttemptID, CheckpointStatus: "unchanged",
	}, "suspend-complete")
	if err != nil || completed.Value.Status != "suspended" || completed.Value.WorkerID != nil {
		t.Fatalf("complete suspend = %#v, %v", completed, err)
	}
	assertExecutionStatus(t, db, fixture, "suspended")
	var leaseCount int64
	if err := db.Model(&persistence.WorkerLease{}).Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).Count(&leaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if leaseCount != 0 {
		t.Fatalf("suspended Execution retained %d Worker leases", leaseCount)
	}
	var suspendedSession persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&suspendedSession).Error; err != nil {
		t.Fatal(err)
	}
	if suspendedSession.ResourceState != "suspended" || suspendedSession.ResourceIdleSince == nil {
		t.Fatalf("Session did not enter resource suspension: %#v", suspendedSession)
	}

	_, err = service.Renew(ctx, firstWorker, fixture.ExecutionID, RenewLeaseInput{LeaseInput: leaseInput}, "suspend-old-renew")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "lease_not_current" {
		t.Fatalf("old Generation renewal was not fenced: %v", err)
	}

	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	blockerExecutionID := seedTenantExecutionQuotaBlocker(t, db, fixture, current)
	_, err = service.ResolveApproval(
		ctx, principal, fixture.ExecutionID, requestID, ResolveApprovalInput{Decision: "accept"},
		"suspend-resolve-quota-rejected", "suspend-resolve-quota-rejected-audit", "127.0.0.1",
	)
	apiError = nil
	if !errors.As(err, &apiError) || apiError.Code != "execution_quota_exceeded" {
		t.Fatalf("suspended interaction quota rejection = %v", err)
	}
	assertExecutionStatus(t, db, fixture, "suspended")
	var unresolved persistence.ExecutionInteraction
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.TenantID, fixture.ExecutionID, requestID,
	).Take(&unresolved).Error; err != nil {
		t.Fatal(err)
	}
	if unresolved.Status != "pending" {
		t.Fatalf("quota rejection committed the interaction resolution: %#v", unresolved)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, blockerExecutionID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveApproval(
		ctx, principal, fixture.ExecutionID, requestID, ResolveApprovalInput{Decision: "accept"},
		"suspend-resolve", "suspend-resolve-audit", "127.0.0.1",
	)
	if err != nil || resolved.Value.DeliveryStatus != "resume-recorded" || resolved.Value.DeliveryWorkerID != nil {
		t.Fatalf("resolve suspended interaction = %#v, %v", resolved, err)
	}
	assertExecutionStatus(t, db, fixture, "recovering")
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", secondWorker.ID).
		Updates(map[string]any{"status": "online", "last_heartbeat_at": current}).Error; err != nil {
		t.Fatal(err)
	}

	resumedClaim, err := service.Claim(ctx, secondWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "suspend-second-claim")
	if err != nil || resumedClaim.Value.Lease == nil || resumedClaim.Value.Workload == nil ||
		resumedClaim.Value.Workload.RecoveryBundle == nil {
		t.Fatalf("resume claim = %#v, %v", resumedClaim, err)
	}
	if resumedClaim.Value.Lease.Generation != firstLease.Generation+1 ||
		resumedClaim.Value.Workload.RecoveryBundle.RecoveryReason != "suspend-resume" ||
		resumedClaim.Value.Workload.RecoveryBundle.PreviousBundleID == nil {
		t.Fatalf("resume Generation lineage is invalid: lease=%#v bundle=%#v", resumedClaim.Value.Lease, resumedClaim.Value.Workload.RecoveryBundle)
	}
	if resumedClaim.Value.Workload.ResumeSnapshot == nil ||
		len(resumedClaim.Value.Workload.ResumeSnapshot.ResumeRecordedInteractions) != 1 ||
		resumedClaim.Value.Workload.ResumeSnapshot.ResumeRecordedInteractions[0].RequestID != requestID ||
		resumedClaim.Value.Workload.ResumeSnapshot.ResumeRecordedInteractions[0].Resolution["decision"] != "accept" {
		t.Fatalf("Recovery Bundle omitted the suspended user resolution: %#v", resumedClaim.Value.Workload.ResumeSnapshot)
	}
	secondLeaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: resumedClaim.Value.Lease.Generation,
		LeaseToken: resumedClaim.Value.Lease.LeaseToken,
	}
	deliveries, err := service.PullInteractionResolutions(ctx, secondWorker, fixture.ExecutionID, PullInteractionResolutionsInput{
		LeaseInput: secondLeaseInput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 0 {
		t.Fatalf("resume-recorded interaction was incorrectly redelivered to a rebuilt Provider: %#v", deliveries)
	}
}

func TestRunningExecutionIdleTimeoutFailsClosedWithoutDurableCheckpointProtocol(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "idle-suspend-fail-closed")
	cleanupWorkers(t, db, worker.ID)

	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "idle-suspend-fail-closed-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	lease := *claim.Value.Lease
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}
	if _, err := service.Start(ctx, worker, fixture.ExecutionID, leaseInput, "idle-suspend-fail-closed-start"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AppendRuntimeEvent(ctx, worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "content.delta", OccurredAt: current,
		Payload: map[string]any{"streamKind": "assistant_text", "delta": "starting a long-running tool"},
	}, "idle-suspend-fail-closed-activity"); err != nil {
		t.Fatal(err)
	}

	current = current.Add(1801 * time.Second)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"heartbeat_at": current, "expires_at": current.Add(service.leaseTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := service.PullResourceDirective(
		ctx, worker, fixture.ExecutionID, PullResourceDirectiveInput{LeaseInput: leaseInput},
	)
	if err != nil || directive != nil {
		t.Fatalf("running Execution received an unsafe idle suspend directive: %#v, %v", directive, err)
	}
	var attempts int64
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&attempts).Error; err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("running Execution created %d suspend attempts without a durable Provider checkpoint protocol", attempts)
	}
}

func TestResourceSuspendRequiresStrictProcessContainmentCapability(t *testing.T) {
	if workerManifestSupportsStrictResourceSuspendContainment(persistence.WorkerManifest{}) {
		t.Fatal("Worker without process containment was allowed to suspend a Kubernetes execution")
	}
	if workerManifestSupportsStrictResourceSuspendContainment(persistence.WorkerManifest{
		ProcessContainmentMode: "process-group",
	}) {
		t.Fatal("Unix process-group containment was treated as a complete process-tree boundary")
	}
	for _, testCase := range []struct {
		mode            string
		operatingSystem string
	}{
		{mode: "cgroup-v2", operatingSystem: "linux"},
		{mode: "job-object", operatingSystem: "windows"},
	} {
		supervisorVersion := "supervisor-test"
		if testCase.mode == "cgroup-v2" {
			supervisorVersion = executiontargets.ProtectedCgroupSupervisorVersionV3
		}
		probeVersion := 1
		if testCase.mode == "cgroup-v2" {
			probeVersion = executiontargets.ProtectedCgroupProbeVersionV3
		}
		probeSHA256 := strings.Repeat("a", 64)
		supervisorIdentity := "supervisor"
		providerIdentity := "provider"
		if !workerManifestSupportsStrictResourceSuspendContainment(persistence.WorkerManifest{
			OperatingSystem: testCase.operatingSystem, ProcessContainmentMode: testCase.mode,
			ProcessContainmentSupervisorVersion: &supervisorVersion,
			ProcessContainmentProbeVersion:      &probeVersion, ProcessContainmentProbeSHA256: &probeSHA256,
			ProcessContainmentSupervisorIdentity: &supervisorIdentity,
			ProcessContainmentProviderIdentity:   &providerIdentity,
		}) {
			t.Fatalf("strict process containment mode %q was rejected", testCase.mode)
		}
	}
	legacySupervisor := "agentd-protected-cgroup-supervisor-v1"
	probeVersion := 1
	probeSHA256 := strings.Repeat("a", 64)
	supervisorIdentity, providerIdentity := "supervisor", "provider"
	if workerManifestSupportsStrictResourceSuspendContainment(persistence.WorkerManifest{
		OperatingSystem: "linux", ProcessContainmentMode: "cgroup-v2",
		ProcessContainmentSupervisorVersion: &legacySupervisor,
		ProcessContainmentProbeVersion:      &probeVersion, ProcessContainmentProbeSHA256: &probeSHA256,
		ProcessContainmentSupervisorIdentity: &supervisorIdentity,
		ProcessContainmentProviderIdentity:   &providerIdentity,
	}) {
		t.Fatal("persisted signed v1 cgroup supervisor retained resource-suspend authority")
	}
	legacySupervisor = "agentd-protected-cgroup-supervisor-v2"
	if workerManifestSupportsStrictResourceSuspendContainment(persistence.WorkerManifest{
		OperatingSystem: "linux", ProcessContainmentMode: "cgroup-v2",
		ProcessContainmentSupervisorVersion: &legacySupervisor,
		ProcessContainmentProbeVersion:      &probeVersion, ProcessContainmentProbeSHA256: &probeSHA256,
		ProcessContainmentSupervisorIdentity: &supervisorIdentity,
		ProcessContainmentProviderIdentity:   &providerIdentity,
	}) {
		t.Fatal("persisted signed v2 cgroup supervisor without resource limits retained resource-suspend authority")
	}
	v3Supervisor := executiontargets.ProtectedCgroupSupervisorVersionV3
	if workerManifestSupportsStrictResourceSuspendContainment(persistence.WorkerManifest{
		OperatingSystem: "linux", ProcessContainmentMode: "cgroup-v2",
		ProcessContainmentSupervisorVersion: &v3Supervisor,
		ProcessContainmentProbeVersion:      &probeVersion, ProcessContainmentProbeSHA256: &probeSHA256,
		ProcessContainmentSupervisorIdentity: &supervisorIdentity,
		ProcessContainmentProviderIdentity:   &providerIdentity,
	}) {
		t.Fatal("persisted v3 cgroup supervisor with the old probe retained resource-suspend authority")
	}
}

func TestSuspendedInteractionExpiryTerminatesWithoutRecreatingPod(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "suspend-expiry")
	cleanupWorkers(t, db, worker.ID)

	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "suspend-expiry-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation, LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := service.Start(ctx, worker, fixture.ExecutionID, leaseInput, "suspend-expiry-start"); err != nil {
		t.Fatal(err)
	}
	requestID := "approval-suspend-expiry-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(ctx, worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "request.opened", OccurredAt: current,
		Payload: map[string]any{
			"requestId": requestID, "requestType": "exec_command_approval", "detail": "Await an inactive user",
		},
	}, "suspend-expiry-opened"); err != nil {
		t.Fatal(err)
	}

	current = current.Add(16 * time.Minute)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"heartbeat_at": current, "expires_at": current.Add(service.leaseTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := service.PullResourceDirective(ctx, worker, fixture.ExecutionID, PullResourceDirectiveInput{LeaseInput: leaseInput})
	if err != nil || directive == nil {
		t.Fatalf("suspend directive = %#v, %v", directive, err)
	}
	markResourceSuspendQuiescedForTest(
		t, service, worker, fixture.ExecutionID, leaseInput, directive.SuspendAttemptID, "suspend-expiry-quiesced",
	)
	completed, err := service.CompleteResourceSuspend(ctx, worker, fixture.ExecutionID, CompleteResourceSuspendInput{
		LeaseInput: leaseInput, SuspendAttemptID: directive.SuspendAttemptID, CheckpointStatus: "unchanged",
	}, "suspend-expiry-complete")
	if err != nil || completed.Value.Status != "suspended" {
		t.Fatalf("complete suspend = %#v, %v", completed, err)
	}

	if err := db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND request_id = ?", fixture.TenantID, fixture.ExecutionID, requestID).
		Update("expires_at", current).Error; err != nil {
		t.Fatal(err)
	}
	expired, err := service.ExpirePendingInteractions(ctx, current, 10)
	if err != nil || expired != 1 {
		t.Fatalf("expire suspended interaction = %d, %v", expired, err)
	}
	assertExecutionStatus(t, db, fixture, "failed")

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.WorkerID != nil || execution.NextRecoveryReason != nil || execution.FailureCode == nil || *execution.FailureCode != "interaction_expired" {
		t.Fatalf("expired suspended Execution retained resumable state: %#v", execution)
	}
	var turn persistence.AgentTurn
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.TurnID).Take(&turn).Error; err != nil {
		t.Fatal(err)
	}
	if turn.Status != "failed" || turn.CompletedAt == nil {
		t.Fatalf("expired suspended Turn was not terminalized: %#v", turn)
	}
	var failedEvents []persistence.SessionEvent
	if err := db.Where(
		"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
		fixture.TenantID, fixture.SessionID, fixture.ExecutionID, "execution.failed",
	).Find(&failedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if len(failedEvents) != 1 || failedEvents[0].WorkerID == nil || *failedEvents[0].WorkerID != worker.ID ||
		failedEvents[0].Generation == nil || *failedEvents[0].Generation != claim.Value.Lease.Generation {
		t.Fatalf("expired suspended interaction lost generation lineage: %#v", failedEvents)
	}
	var recoveryOutbox int64
	if err := db.Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic = ? AND message_key = ?", fixture.TenantID, "execution.recovering", fixture.ExecutionID.String()+":"+formatGeneration(execution.Generation)).
		Count(&recoveryOutbox).Error; err != nil {
		t.Fatal(err)
	}
	if recoveryOutbox != 0 {
		t.Fatalf("expired suspended interaction enqueued %d replacement recoveries", recoveryOutbox)
	}
}

func TestSuspendedInteractionExpiryFailsWholeSetRegardlessResolutionOrder(t *testing.T) {
	for _, resolveOtherFirst := range []bool{false, true} {
		name := "expire-before-other-resolution"
		if resolveOtherFirst {
			name = "resolve-other-before-expiry"
		}
		t.Run(name, func(t *testing.T) {
			fixture := setupActiveResourceSuspendAttempt(t, "suspend-expiry-order-"+name)
			secondRequestID := "approval-second-" + uuid.NewString()
			now := fixture.service.now()
			if _, err := fixture.service.AppendRuntimeEvent(
				context.Background(), fixture.worker, fixture.execution.ExecutionID,
				RuntimeEventInput{
					LeaseInput: fixture.leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
					EventType: "request.opened", OccurredAt: now,
					Payload: map[string]any{
						"requestId": secondRequestID, "requestType": "exec_command_approval",
						"detail": "A second required decision",
					},
				},
				"suspend-expiry-order-second-opened",
			); err != nil {
				t.Fatal(err)
			}
			markResourceSuspendQuiescedForTest(
				t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
				fixture.leaseInput, fixture.directive.SuspendAttemptID, "suspend-expiry-order-quiesced",
			)
			completed, err := fixture.service.CompleteResourceSuspend(
				context.Background(), fixture.worker, fixture.execution.ExecutionID,
				CompleteResourceSuspendInput{
					LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID,
					CheckpointStatus: "unchanged",
				},
				"suspend-expiry-order-complete",
			)
			if err != nil || completed.Value.Status != "suspended" {
				t.Fatalf("complete multi-interaction suspend = %#v, %v", completed, err)
			}

			principal := identity.Principal{
				UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
			}
			if resolveOtherFirst {
				if _, err := fixture.service.ResolveApproval(
					context.Background(), principal, fixture.execution.ExecutionID, secondRequestID,
					ResolveApprovalInput{Decision: "accept"}, "suspend-expiry-order-resolve",
					"suspend-expiry-order-resolve-audit", "127.0.0.1",
				); err != nil {
					t.Fatal(err)
				}
				assertExecutionStatus(t, fixture.db, fixture.execution, "suspended")
			}

			if err := fixture.db.Model(&persistence.ExecutionInteraction{}).
				Where(
					"tenant_id = ? AND execution_id = ? AND request_id = ?",
					fixture.execution.TenantID, fixture.execution.ExecutionID, fixture.requestID,
				).
				Update("expires_at", now).Error; err != nil {
				t.Fatal(err)
			}
			expired, err := fixture.service.ExpirePendingInteractions(context.Background(), now, 10)
			if err != nil || expired != 1 {
				t.Fatalf("expire one required suspended interaction = %d, %v", expired, err)
			}
			assertExecutionStatus(t, fixture.db, fixture.execution, "failed")
			var pending int64
			if err := fixture.db.Model(&persistence.ExecutionInteraction{}).
				Where(
					"tenant_id = ? AND execution_id = ? AND status = ?",
					fixture.execution.TenantID, fixture.execution.ExecutionID, "pending",
				).
				Count(&pending).Error; err != nil {
				t.Fatal(err)
			}
			if pending != 0 {
				t.Fatalf("failed suspended Execution retained %d required pending interactions", pending)
			}
			if !resolveOtherFirst {
				_, err := fixture.service.ResolveApproval(
					context.Background(), principal, fixture.execution.ExecutionID, secondRequestID,
					ResolveApprovalInput{Decision: "accept"}, "suspend-expiry-order-late-resolve",
					"suspend-expiry-order-late-resolve-audit", "127.0.0.1",
				)
				var apiError *problem.Error
				if !errors.As(err, &apiError) || apiError.Code != "interaction_expired" {
					t.Fatalf("late resolution after required expiry error = %v", err)
				}
			}
		})
	}
}

func TestResourceSuspendAttemptDeadlineRestoresWaitingStateWithoutRenewingActivity(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-deadline")
	var checkpointingSession persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.SessionID).
		Take(&checkpointingSession).Error; err != nil {
		t.Fatal(err)
	}
	if checkpointingSession.ResourceState != "checkpointing" {
		t.Fatalf("resource state = %q, want checkpointing", checkpointingSession.ResourceState)
	}
	meaningfulActivityAt := checkpointingSession.MeaningfulActivityAt

	expiredAt := fixture.directive.CheckpointDeadlineAt.Add(time.Second)
	fixture.service.now = func() time.Time { return expiredAt }
	if err := fixture.db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID).
		Updates(map[string]any{"heartbeat_at": expiredAt, "expires_at": expiredAt.Add(fixture.service.leaseTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := fixture.service.PullResourceDirective(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		PullResourceDirectiveInput{LeaseInput: fixture.leaseInput},
	)
	if err != nil || directive != nil {
		t.Fatalf("deadline pull = %#v, %v", directive, err)
	}
	var attempt persistence.ExecutionSuspendAttempt
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.directive.SuspendAttemptID).
		Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "aborted" || attempt.FailureCode == nil || *attempt.FailureCode != "checkpoint_deadline_exceeded" {
		t.Fatalf("expired suspend attempt = %#v", attempt)
	}
	var waitingSession persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.SessionID).
		Take(&waitingSession).Error; err != nil {
		t.Fatal(err)
	}
	if waitingSession.ResourceState != "waiting" || !waitingSession.MeaningfulActivityAt.Equal(meaningfulActivityAt) {
		t.Fatalf("deadline abort changed lifecycle authority incorrectly: before=%s after=%#v", meaningfulActivityAt, waitingSession)
	}
}

func TestInteractionResolutionSupersedesCheckpointingSuspendAttempt(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-resolve-race")
	principal := identity.Principal{UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID}
	resolved, err := fixture.service.ResolveApproval(
		context.Background(), principal, fixture.execution.ExecutionID, fixture.requestID,
		ResolveApprovalInput{Decision: "accept"}, "suspend-race-resolve", "suspend-race-audit", "127.0.0.1",
	)
	if err != nil || resolved.Value.Status != "resolved" {
		t.Fatalf("resolve during suspend checkpoint = %#v, %v", resolved, err)
	}
	assertExecutionStatus(t, fixture.db, fixture.execution, "running")
	var attempt persistence.ExecutionSuspendAttempt
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.directive.SuspendAttemptID).
		Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "superseded" || attempt.FailureCode == nil || *attempt.FailureCode != "interaction_resolved" {
		t.Fatalf("resolved suspend attempt = %#v", attempt)
	}
	var activeSession persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.SessionID).
		Take(&activeSession).Error; err != nil {
		t.Fatal(err)
	}
	if activeSession.ResourceState != "active" {
		t.Fatalf("resolved Session resource state = %q, want active", activeSession.ResourceState)
	}

	_, err = fixture.service.AbortResourceSuspend(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		AbortResourceSuspendInput{
			LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID,
			FailureCode: "late_abort", FailureMessage: "The obsolete Worker loop observed the resolution race.",
		}, "suspend-race-late-abort",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "resource_suspend_attempt_finished" {
		t.Fatalf("late abort error = %v", err)
	}
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.SessionID).
		Take(&activeSession).Error; err != nil {
		t.Fatal(err)
	}
	if activeSession.ResourceState != "active" {
		t.Fatalf("late abort regressed Session resource state to %q", activeSession.ResourceState)
	}
}

func TestQuiescedCheckpointResolutionIsPreservedForRecovery(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-quiesced-race")
	secondWorker := registerManifestTestWorker(
		t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "suspend-quiesced-race-recovery",
	)
	cleanupWorkers(t, fixture.db, secondWorker.ID)
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.leaseInput, fixture.directive.SuspendAttemptID, "suspend-quiesced-race-mark",
	)
	principal := identity.Principal{UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID}
	resolved, err := fixture.service.ResolveApproval(
		context.Background(), principal, fixture.execution.ExecutionID, fixture.requestID,
		ResolveApprovalInput{Decision: "accept"}, "suspend-quiesced-race-resolve", "suspend-quiesced-race-audit", "127.0.0.1",
	)
	if err != nil || resolved.Value.DeliveryStatus != "resume-recorded" || resolved.Value.DeliveryWorkerID != nil {
		t.Fatalf("resolve after durable Provider quiesce = %#v, %v", resolved, err)
	}
	released, err := fixture.service.Release(
		context.Background(),
		fixture.worker,
		fixture.execution.ExecutionID,
		ReleaseLeaseInput{
			LeaseInput:                     fixture.leaseInput,
			Reason:                         "Provider quiesced before resource suspension could be committed.",
			PreserveInteractionResolutions: true,
		},
		"suspend-race-release",
	)
	if err != nil || released.Value.Status != "recovering" {
		t.Fatalf("release after quiesced suspend race = %#v, %v", released, err)
	}
	var preserved persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID, fixture.requestID,
	).Take(&preserved).Error; err != nil {
		t.Fatal(err)
	}
	if preserved.Status != "resolved" || preserved.DeliveryStatus != "resume-recorded" ||
		preserved.DeliveryWorkerID != nil || preserved.DeliveryGeneration != nil {
		t.Fatalf("quiesced resolution was not preserved for recovery: %#v", preserved)
	}

	recoveryClaim, err := fixture.service.Claim(
		context.Background(),
		secondWorker,
		ClaimExecutionInput{
			ExecutionTargetID: fixture.execution.TargetID,
			TargetKind:        fixture.execution.TargetKind,
			ExecutionID:       &fixture.execution.ExecutionID,
		},
		"suspend-race-recovery-claim",
	)
	if err != nil || recoveryClaim.Value.Workload == nil || recoveryClaim.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("claim after quiesced suspend race = %#v, %v", recoveryClaim, err)
	}
	recorded := recoveryClaim.Value.Workload.ResumeSnapshot.ResumeRecordedInteractions
	if len(recorded) != 1 || recorded[0].RequestID != fixture.requestID ||
		recorded[0].Resolution["decision"] != "accept" {
		t.Fatalf("Recovery Bundle lost the quiesced interaction resolution: %#v", recorded)
	}
}

func TestExplicitQuiescedReleaseRecoversPendingInteractionInsteadOfSuspending(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-quiesced-pending-release")
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.leaseInput, fixture.directive.SuspendAttemptID, "suspend-quiesced-pending-release-mark",
	)
	released, err := fixture.service.Release(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		ReleaseLeaseInput{
			LeaseInput:                     fixture.leaseInput,
			Reason:                         "Suspend commit was rejected after Provider quiesce.",
			PreserveInteractionResolutions: true,
		},
		"suspend-quiesced-pending-release",
	)
	if err != nil || released.Value.Status != "recovering" {
		t.Fatalf("explicit quiesced release = %#v, %v; want recovering", released, err)
	}
	var interaction persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID, fixture.requestID,
	).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.Status != "pending" || interaction.DeliveryStatus != "not-ready" {
		t.Fatalf("explicit quiesced release lost its pending callback: %#v", interaction)
	}
	var attempt persistence.ExecutionSuspendAttempt
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.directive.SuspendAttemptID,
	).Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "superseded" || attempt.ProviderQuiescedAt == nil {
		t.Fatalf("explicit quiesced release lost its audit provenance: %#v", attempt)
	}
}

func TestLeaseExpiryAfterSuspendCheckpointDeadlineUsesOrdinaryRecovery(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-quiesced-deadline-expired")
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.leaseInput, fixture.directive.SuspendAttemptID, "suspend-quiesced-deadline-expired-mark",
	)
	recoveryAt := fixture.directive.CheckpointDeadlineAt.Add(time.Second)
	if err := fixture.db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID).
		Update("expires_at", recoveryAt.Add(-time.Millisecond)).Error; err != nil {
		t.Fatal(err)
	}
	fixture.service.now = func() time.Time { return recoveryAt }

	if err := fixture.service.RecoverExpired(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	assertExecutionStatus(t, fixture.db, fixture.execution, "recovering")
	var interaction persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID, fixture.requestID,
	).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.Status != "expired" || interaction.DeliveryStatus != "superseded" {
		t.Fatalf("deadline-expired suspend attempt incorrectly preserved its callback: %#v", interaction)
	}
}

func TestAbsoluteExpiredSessionCannotAdvanceSuspendHandshake(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-absolute-expired")
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.leaseInput, fixture.directive.SuspendAttemptID, "suspend-absolute-expired-initial-mark",
	)
	absoluteExpiry := fixture.service.now().Add(time.Second)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.SessionID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	fixture.service.now = func() time.Time { return absoluteExpiry.Add(time.Millisecond) }

	if _, err := fixture.service.MarkResourceSuspendQuiesced(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		MarkResourceSuspendQuiescedInput{
			LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID,
		},
		"suspend-absolute-expired-late-mark",
	); err == nil {
		t.Fatal("absolute-expired Session accepted a quiesce proof")
	} else {
		assertProblemCode(t, err, "session_absolute_expired")
	}
	if _, err := fixture.service.CompleteResourceSuspend(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		CompleteResourceSuspendInput{
			LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID,
			CheckpointStatus: "unchanged",
		},
		"suspend-absolute-expired-complete",
	); err == nil {
		t.Fatal("absolute-expired Session completed resource suspension")
	} else {
		assertProblemCode(t, err, "session_absolute_expired")
	}
	directive, err := fixture.service.PullResourceDirective(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		PullResourceDirectiveInput{LeaseInput: fixture.leaseInput},
	)
	if err != nil || directive == nil || directive.Action != "terminate" || directive.Reason != sessionAbsoluteExpiryAction {
		t.Fatalf("absolute-expired resource directive did not terminate the Generation: %#v, %v", directive, err)
	}
	assertExecutionStatus(t, fixture.db, fixture.execution, "cancelled")
	var attempt persistence.ExecutionSuspendAttempt
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.directive.SuspendAttemptID,
	).Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "superseded" {
		t.Fatalf("absolute-expired cancellation left a live suspend attempt: %#v", attempt)
	}
}

func TestAbsoluteExpiredSuspendAbortConvergesToCancellation(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-abort-absolute-expired")
	absoluteExpiry := fixture.service.now().Add(time.Second)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.SessionID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	fixture.service.now = func() time.Time { return absoluteExpiry.Add(time.Millisecond) }

	aborted, err := fixture.service.AbortResourceSuspend(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		AbortResourceSuspendInput{
			LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID,
			FailureCode: "absolute_expired", FailureMessage: "Session lifetime elapsed during suspend.",
		},
		"suspend-abort-absolute-expired",
	)
	if err != nil || aborted.Value.Status != "cancelled" {
		t.Fatalf("absolute-expired suspend abort did not converge to cancellation: %#v, %v", aborted, err)
	}
	var attempt persistence.ExecutionSuspendAttempt
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.directive.SuspendAttemptID,
	).Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "superseded" {
		t.Fatalf("absolute-expired cancellation left a live suspend attempt: %#v", attempt)
	}
}

func TestLeaseExpiryBeforeProviderQuiesceFallsBackToOrdinaryRecovery(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-before-quiesce-crash")
	secondWorker := registerManifestTestWorker(
		t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "suspend-before-quiesce-recovery",
	)
	cleanupWorkers(t, fixture.db, secondWorker.ID)

	var firstLease persistence.WorkerLease
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&firstLease).Error; err != nil {
		t.Fatal(err)
	}
	expiredAt := firstLease.ExpiresAt.Add(time.Second)
	fixture.service.now = func() time.Time { return expiredAt }
	if err := fixture.service.RecoverExpired(context.Background(), 10); err != nil {
		t.Fatal(err)
	}

	assertExecutionStatus(t, fixture.db, fixture.execution, "recovering")
	var attempt persistence.ExecutionSuspendAttempt
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.directive.SuspendAttemptID).
		Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "superseded" || attempt.ProviderQuiescedAt != nil {
		t.Fatalf("pre-quiesce crash retained suspend provenance: %#v", attempt)
	}
	claim, err := fixture.service.Claim(
		context.Background(), secondWorker,
		ClaimExecutionInput{
			ExecutionTargetID: fixture.execution.TargetID,
			TargetKind:        fixture.execution.TargetKind,
			ExecutionID:       &fixture.execution.ExecutionID,
		},
		"suspend-before-quiesce-recovery-claim",
	)
	if err != nil || claim.Value.Lease == nil || claim.Value.Workload == nil || claim.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("claim after pre-quiesce crash = %#v, %v", claim, err)
	}
	if len(claim.Value.Workload.ResumeSnapshot.ResumeRecordedInteractions) != 0 {
		t.Fatalf("pre-quiesce crash incorrectly preserved resume-recorded interactions: %#v", claim.Value.Workload.ResumeSnapshot)
	}
}

func TestLeaseExpiryDuringCheckpointingSuspendsAndBindsResolutionOnlyOnce(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-lost-ack")
	secondWorker := registerManifestTestWorker(
		t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "suspend-lost-ack-second",
	)
	thirdWorker := registerManifestTestWorker(
		t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "suspend-lost-ack-third",
	)
	cleanupWorkers(t, fixture.db, secondWorker.ID, thirdWorker.ID)

	var firstLease persistence.WorkerLease
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&firstLease).Error; err != nil {
		t.Fatal(err)
	}
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.leaseInput, fixture.directive.SuspendAttemptID, "suspend-lost-ack-quiesced",
	)
	expiredAt := firstLease.ExpiresAt.Add(time.Second)
	fixture.service.now = func() time.Time { return expiredAt }

	// Two reconcilers racing the same expired generation must produce one
	// suspended transition and must not supersede the pending user decision.
	errorsByWorker := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByWorker <- fixture.service.RecoverExpired(context.Background(), 10)
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent lease-expiry reconciliation failed: %v", err)
		}
	}

	assertExecutionStatus(t, fixture.db, fixture.execution, "suspended")
	var preserved persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID, fixture.requestID,
	).Take(&preserved).Error; err != nil {
		t.Fatal(err)
	}
	if preserved.Status != "pending" || preserved.DeliveryStatus != "not-ready" ||
		preserved.WorkerID != fixture.worker.ID || preserved.Generation != fixture.leaseInput.Generation {
		t.Fatalf("lost suspend acknowledgement did not preserve the pending interaction: %#v", preserved)
	}
	var suspendedEvents int64
	if err := fixture.db.Model(&persistence.SessionEvent{}).
		Where(
			"tenant_id = ? AND execution_id = ? AND event_type = ?",
			fixture.execution.TenantID, fixture.execution.ExecutionID, "execution.suspended",
		).
		Count(&suspendedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if suspendedEvents != 1 {
		t.Fatalf("concurrent reconciliation emitted %d suspended events, want 1", suspendedEvents)
	}
	var recoveryMessages int64
	if err := fixture.db.Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic = ?", fixture.execution.TenantID, "execution.recovering").
		Count(&recoveryMessages).Error; err != nil {
		t.Fatal(err)
	}
	if recoveryMessages != 0 {
		t.Fatalf("lost suspend acknowledgement eagerly recreated %d recovery generations", recoveryMessages)
	}

	principal := identity.Principal{
		UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
	}
	resolved, err := fixture.service.ResolveApproval(
		context.Background(), principal, fixture.execution.ExecutionID, fixture.requestID,
		ResolveApprovalInput{Decision: "accept"}, "suspend-lost-ack-resolve",
		"suspend-lost-ack-resolve-audit", "127.0.0.1",
	)
	if err != nil || resolved.Value.DeliveryStatus != "resume-recorded" {
		t.Fatalf("resolve interaction after reconciled suspension = %#v, %v", resolved, err)
	}
	claim, err := fixture.service.Claim(
		context.Background(), secondWorker,
		ClaimExecutionInput{
			ExecutionTargetID: fixture.execution.TargetID,
			TargetKind:        fixture.execution.TargetKind,
			ExecutionID:       &fixture.execution.ExecutionID,
		},
		"suspend-lost-ack-second-claim",
	)
	if err != nil || claim.Value.Lease == nil || claim.Value.Workload == nil ||
		claim.Value.Workload.RecoveryBundle == nil || claim.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("claim after reconciled suspension = %#v, %v", claim, err)
	}
	if len(claim.Value.Workload.ResumeSnapshot.ResumeRecordedInteractions) != 1 ||
		claim.Value.Workload.ResumeSnapshot.ResumeRecordedInteractions[0].ID != resolved.Value.ID {
		t.Fatalf("recovery Bundle omitted the post-fence resolution: %#v", claim.Value.Workload.ResumeSnapshot)
	}

	var bound persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, resolved.Value.ID,
	).Take(&bound).Error; err != nil {
		t.Fatal(err)
	}
	if bound.DeliveryStatus != "resume-bound" || bound.ResumeBundleID == nil ||
		*bound.ResumeBundleID != claim.Value.Workload.RecoveryBundle.ID ||
		bound.ResumeGeneration == nil || *bound.ResumeGeneration != claim.Value.Lease.Generation ||
		bound.ResumeBoundAt == nil {
		t.Fatalf("resume-recorded resolution was not atomically bound to one Bundle: %#v", bound)
	}

	// There is intentionally no Provider apply ACK for prompt-based recovery.
	// Losing the bound generation therefore fails closed instead of replaying the
	// same resolution in a third Recovery Bundle.
	var secondLease persistence.WorkerLease
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&secondLease).Error; err != nil {
		t.Fatal(err)
	}
	secondExpiredAt := secondLease.ExpiresAt.Add(time.Second)
	fixture.service.now = func() time.Time { return secondExpiredAt }
	if err := fixture.service.RecoverExpired(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	assertExecutionStatus(t, fixture.db, fixture.execution, "failed")
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, resolved.Value.ID,
	).Take(&bound).Error; err != nil {
		t.Fatal(err)
	}
	if bound.DeliveryStatus != "outcome-unknown" || bound.DeliveryError == nil ||
		bound.ResumeBundleID == nil || *bound.ResumeBundleID != claim.Value.Workload.RecoveryBundle.ID {
		t.Fatalf("lost bound Generation did not reach terminal outcome-unknown: %#v", bound)
	}
	thirdClaim, err := fixture.service.Claim(
		context.Background(), thirdWorker,
		ClaimExecutionInput{
			ExecutionTargetID: fixture.execution.TargetID,
			TargetKind:        fixture.execution.TargetKind,
			ExecutionID:       &fixture.execution.ExecutionID,
		},
		"suspend-lost-ack-third-claim",
	)
	if err != nil {
		t.Fatal(err)
	}
	if thirdClaim.Value.Lease != nil || thirdClaim.Value.Workload != nil {
		t.Fatalf("outcome-unknown resolution was replayed in another claim: %#v", thirdClaim)
	}
	var bundleCount int64
	if err := fixture.db.Model(&persistence.ExecutionRecoveryBundle{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID).
		Count(&bundleCount).Error; err != nil {
		t.Fatal(err)
	}
	if bundleCount != 2 {
		t.Fatalf("bound resolution entered %d Recovery Bundles, want exactly 1 recovered Bundle plus initial Bundle", bundleCount)
	}
}

func TestDeliveredInteractionResolutionBlocksLateProviderQuiesceRecording(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-delivered-unknown")
	secondRequestID := "approval-delivered-blocker-" + uuid.NewString()
	if _, err := fixture.service.AppendRuntimeEvent(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		RuntimeEventInput{
			LeaseInput: fixture.leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
			EventType: "request.opened", OccurredAt: fixture.service.now(),
			Payload: map[string]any{
				"requestId": secondRequestID, "requestType": "exec_command_approval", "detail": "keep execution waiting",
			},
		},
		"suspend-delivered-blocker-opened",
	); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
	}
	resolved, err := fixture.service.ResolveApproval(
		context.Background(), principal, fixture.execution.ExecutionID, fixture.requestID,
		ResolveApprovalInput{Decision: "accept"}, "suspend-delivered-resolve",
		"suspend-delivered-resolve-audit", "127.0.0.1",
	)
	if err != nil || resolved.Value.DeliveryStatus != "pending" || resolved.Value.ResolutionCommandID == nil {
		t.Fatalf("resolve before delivered outcome test = %#v, %v", resolved, err)
	}
	delivered, err := fixture.service.MarkInteractionResolutionDelivered(
		context.Background(), fixture.worker, fixture.execution.ExecutionID, resolved.Value.ID,
		InteractionResolutionDeliveryInput{
			LeaseInput: fixture.leaseInput, ResolutionCommandID: *resolved.Value.ResolutionCommandID,
		},
		"suspend-delivered-mark",
	)
	if err != nil || delivered.Value.DeliveryStatus != "delivered" {
		t.Fatalf("mark interaction resolution delivered = %#v, %v", delivered, err)
	}
	_, err = fixture.service.MarkResourceSuspendQuiesced(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		MarkResourceSuspendQuiescedInput{
			LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID,
		},
		"suspend-delivered-quiesced",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "resource_suspend_attempt_finished" {
		t.Fatalf("late Provider quiesce recording error = %v", err)
	}
}

type activeResourceSuspendAttemptFixture struct {
	db         *gorm.DB
	service    *Service
	execution  executionFixture
	worker     persistence.WorkerInstance
	leaseInput LeaseInput
	directive  ResourceDirective
	requestID  string
}

func setupActiveResourceSuspendAttempt(t *testing.T, label string) activeResourceSuspendAttemptFixture {
	t.Helper()
	ctx := context.Background()
	db, service, execution := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	worker := registerManifestTestWorker(t, service, execution.TargetID, execution.TargetKind, label)
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: execution.TargetID, TargetKind: execution.TargetKind, ExecutionID: &execution.ExecutionID,
	}, label+"-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	leaseInput := LeaseInput{
		TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation, LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := service.Start(ctx, worker, execution.ExecutionID, leaseInput, label+"-start"); err != nil {
		t.Fatal(err)
	}
	requestID := "approval-" + label + "-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(ctx, worker, execution.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "request.opened", OccurredAt: current,
		Payload: map[string]any{
			"requestId": requestID, "requestType": "exec_command_approval", "detail": label,
		},
	}, label+"-opened"); err != nil {
		t.Fatal(err)
	}
	current = current.Add(16 * time.Minute)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", execution.TenantID, execution.ExecutionID).
		Updates(map[string]any{"heartbeat_at": current, "expires_at": current.Add(service.leaseTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := service.PullResourceDirective(
		ctx, worker, execution.ExecutionID, PullResourceDirectiveInput{LeaseInput: leaseInput},
	)
	if err != nil || directive == nil {
		t.Fatalf("suspend directive = %#v, %v", directive, err)
	}
	return activeResourceSuspendAttemptFixture{
		db: db, service: service, execution: execution, worker: worker,
		leaseInput: leaseInput, directive: *directive, requestID: requestID,
	}
}

func markResourceSuspendQuiescedForTest(
	t *testing.T,
	service *Service,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	leaseInput LeaseInput,
	attemptID uuid.UUID,
	requestID string,
) ResourceSuspendQuiesceReceipt {
	t.Helper()
	result, err := service.MarkResourceSuspendQuiesced(
		context.Background(), worker, executionID,
		MarkResourceSuspendQuiescedInput{LeaseInput: leaseInput, SuspendAttemptID: attemptID},
		requestID,
	)
	if err != nil {
		t.Fatalf("mark resource suspend quiesced = %#v, %v", result, err)
	}
	if result.Value.SuspendAttemptID != attemptID || result.Value.ProviderQuiescedAt.IsZero() {
		t.Fatalf("invalid resource suspend quiesce receipt: %#v", result.Value)
	}
	return result.Value
}

func TestCompleteResourceSuspendRequiresDurableProviderQuiesce(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-complete-requires-quiesce")
	_, err := fixture.service.CompleteResourceSuspend(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		CompleteResourceSuspendInput{
			LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID, CheckpointStatus: "unchanged",
		},
		"suspend-complete-before-quiesce",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "resource_suspend_quiesce_required" {
		t.Fatalf("complete before quiesce error = %v", err)
	}
}

func TestCompleteResourceSuspendRechecksStrictContainmentAfterQuiesce(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-complete-rechecks-containment")
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID, fixture.leaseInput,
		fixture.directive.SuspendAttemptID, "suspend-complete-rechecks-containment-quiesced",
	)
	workerWithoutCurrentProof := fixture.worker
	workerWithoutCurrentProof.CurrentManifestID = nil
	_, err := fixture.service.CompleteResourceSuspend(
		context.Background(), workerWithoutCurrentProof, fixture.execution.ExecutionID,
		CompleteResourceSuspendInput{
			LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID, CheckpointStatus: "unchanged",
		},
		"suspend-complete-without-current-containment",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "resource_suspend_no_longer_safe" {
		t.Fatalf("complete after containment proof loss error = %v", err)
	}
}

func TestResourceSuspendFailsClosedAfterTargetAttestationKeyRotation(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-target-attestation-key-rotation")
	capabilities := workerManifestTestTargetCapabilities()
	capabilities["processContainmentPolicy"] = map[string]any{
		"trustMode":        "signed-v1",
		"keyId":            "rotated-containment-key",
		"ed25519PublicKey": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("z", 32))),
	}
	rotatedTarget := persistence.ExecutionTarget{Capabilities: capabilities}
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).
		Where("id = ?", fixture.execution.TargetID).
		Select("capabilities").Updates(&rotatedTarget).Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.MarkResourceSuspendQuiesced(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		MarkResourceSuspendQuiescedInput{
			LeaseInput: fixture.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID,
		},
		"suspend-target-attestation-key-rotation",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "resource_suspend_no_longer_safe" {
		t.Fatalf("rotated Target attestation key did not fence the active suspend attempt: %v", err)
	}
}

func TestMarkResourceSuspendQuiescedRejectsStaleOrUnsafeAttempts(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "suspend-mark-quiesced")
	baseNow := fixture.service.now()
	cases := []struct {
		name       string
		worker     persistence.WorkerInstance
		leaseInput LeaseInput
		now        func(time.Time) time.Time
		prepare    func(*testing.T, time.Time)
		wantCode   string
	}{
		{
			name: "fake-worker",
			worker: registerManifestTestWorker(
				t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "suspend-mark-quiesced-fake",
			),
			leaseInput: fixture.leaseInput,
			wantCode:   "generation_fenced",
		},
		{
			name:   "wrong-generation",
			worker: fixture.worker,
			leaseInput: LeaseInput{
				TenantID: fixture.leaseInput.TenantID, Generation: fixture.leaseInput.Generation + 1, LeaseToken: fixture.leaseInput.LeaseToken,
			},
			wantCode: "generation_fenced",
		},
		{
			name:       "deadline-expired",
			worker:     fixture.worker,
			leaseInput: fixture.leaseInput,
			now:        func(_ time.Time) time.Time { return fixture.directive.CheckpointDeadlineAt.Add(time.Second) },
			prepare: func(t *testing.T, targetNow time.Time) {
				t.Helper()
				if err := fixture.db.Model(&persistence.WorkerLease{}).
					Where("tenant_id = ? AND execution_id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID).
					Updates(map[string]any{"heartbeat_at": targetNow, "expires_at": targetNow.Add(fixture.service.leaseTTL)}).Error; err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "resource_suspend_checkpoint_expired",
		},
		{
			name: "containment-revoked",
			worker: func() persistence.WorkerInstance {
				worker := fixture.worker
				worker.CurrentManifestID = nil
				return worker
			}(),
			leaseInput: fixture.leaseInput,
			wantCode:   "resource_suspend_no_longer_safe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := fixture.service
			targetNow := baseNow
			if tc.now != nil {
				targetNow = tc.now(baseNow)
			}
			service.now = func() time.Time { return targetNow }
			if tc.prepare != nil {
				tc.prepare(t, targetNow)
			}
			_, err := service.MarkResourceSuspendQuiesced(
				context.Background(), tc.worker, fixture.execution.ExecutionID,
				MarkResourceSuspendQuiescedInput{
					LeaseInput: tc.leaseInput, SuspendAttemptID: fixture.directive.SuspendAttemptID,
				},
				"suspend-mark-quiesced-"+tc.name,
			)
			var apiError *problem.Error
			if !errors.As(err, &apiError) || apiError.Code != tc.wantCode {
				t.Fatalf("mark quiesced error = %v, want %s", err, tc.wantCode)
			}
		})
	}
}

func TestResourceSuspendWaitsForUnacknowledgedResolutionDelivery(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "suspend-delivery-blocker")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "suspend-blocker-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation, LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := service.Start(ctx, worker, fixture.ExecutionID, leaseInput, "suspend-blocker-start"); err != nil {
		t.Fatal(err)
	}
	for _, requestID := range []string{"approval-blocker-one", "approval-blocker-two"} {
		if _, err := service.AppendRuntimeEvent(ctx, worker, fixture.ExecutionID, RuntimeEventInput{
			LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
			EventType: "request.opened", OccurredAt: current,
			Payload: map[string]any{
				"requestId": requestID, "requestType": "exec_command_approval", "detail": requestID,
			},
		}, "suspend-blocker-"+requestID); err != nil {
			t.Fatal(err)
		}
	}
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	if _, err := service.ResolveApproval(
		ctx, principal, fixture.ExecutionID, "approval-blocker-one", ResolveApprovalInput{Decision: "decline"},
		"suspend-blocker-resolve", "suspend-blocker-audit", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	var partiallyResolvedSession persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Take(&partiallyResolvedSession).Error; err != nil {
		t.Fatal(err)
	}
	if partiallyResolvedSession.ResourceState != "waiting" {
		t.Fatalf("partially resolved Session resource state = %q, want waiting", partiallyResolvedSession.ResourceState)
	}
	current = current.Add(16 * time.Minute)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"heartbeat_at": current, "expires_at": current.Add(service.leaseTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := service.PullResourceDirective(ctx, worker, fixture.ExecutionID, PullResourceDirectiveInput{LeaseInput: leaseInput})
	if err != nil {
		t.Fatal(err)
	}
	if directive != nil {
		t.Fatalf("unacknowledged resolution delivery did not block suspension: %#v", directive)
	}
	var attempts int64
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).Count(&attempts).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("created %d unsafe suspension attempts", attempts)
	}
}
