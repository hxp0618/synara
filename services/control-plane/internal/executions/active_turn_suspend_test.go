package executions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

type activeTurnSuspendFixture struct {
	db        *gorm.DB
	service   *Service
	execution executionFixture
	worker    persistence.WorkerInstance
	lease     LeaseInput
	directive ResourceDirective
	delivery  ControlCommandDelivery
	current   *time.Time
}

func TestActiveTurnSuspendExplicitResumeBindsReceiptOnce(t *testing.T) {
	fixture := setupDurableActiveTurnSuspend(t, "active-explicit-resume")
	acknowledgeActiveTurnSuspend(t, fixture, "cursor-active-explicit")
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.lease, fixture.directive.SuspendAttemptID, "active-explicit-quiesced",
	)

	completed, err := fixture.service.CompleteResourceSuspend(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		CompleteResourceSuspendInput{
			LeaseInput: fixture.lease, SuspendAttemptID: fixture.directive.SuspendAttemptID,
			CheckpointStatus: "unchanged",
		},
		"active-explicit-complete",
	)
	if err != nil || completed.Value.Status != "suspended" {
		t.Fatalf("complete active-turn suspend = %#v, %v", completed, err)
	}

	principal := identity.Principal{
		UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
	}
	resumed, err := fixture.service.ResumeActiveTurnForSession(
		context.Background(), principal, fixture.execution.SessionID,
		"active-explicit-resume-key", "active-explicit-resume-request", "127.0.0.1",
	)
	if err != nil || resumed.Value.Status != "recovering" {
		t.Fatalf("explicit active-turn resume = %#v, %v", resumed, err)
	}
	recoveryWorker := registerActiveTurnSuspendWorker(
		t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "active-explicit-recovery",
	)
	cleanupWorkers(t, fixture.db, recoveryWorker.ID)

	claim, err := fixture.service.Claim(
		context.Background(), recoveryWorker,
		ClaimExecutionInput{
			ExecutionTargetID: fixture.execution.TargetID, TargetKind: fixture.execution.TargetKind,
			ExecutionID: &fixture.execution.ExecutionID,
		},
		"active-explicit-recovery-claim",
	)
	if err != nil || claim.Value.Lease == nil || claim.Value.Workload == nil ||
		claim.Value.Workload.ResumeSnapshot == nil || claim.Value.ProviderResumeCursor == nil {
		t.Fatalf("active-turn recovery claim = %#v, %v", claim, err)
	}
	checkpoint := claim.Value.Workload.ResumeSnapshot.ActiveTurnCheckpoint
	if checkpoint == nil || checkpoint.SuspendAttemptID != fixture.directive.SuspendAttemptID ||
		checkpoint.SourceGeneration != fixture.lease.Generation ||
		checkpoint.BoundaryMeaningfulActivitySequence <= 0 ||
		*claim.Value.ProviderResumeCursor != "cursor-active-explicit" {
		t.Fatalf("active-turn Recovery Bundle checkpoint = %#v, cursor=%v", checkpoint, claim.Value.ProviderResumeCursor)
	}

	var attempt persistence.ExecutionSuspendAttempt
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.directive.SuspendAttemptID,
	).Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.ResumeBundleID == nil || attempt.ResumeGeneration == nil ||
		*attempt.ResumeGeneration != claim.Value.Lease.Generation || attempt.ResumeBoundAt == nil {
		t.Fatalf("active-turn receipt was not bound once to the Recovery Bundle: %#v", attempt)
	}

	replay, err := fixture.service.ResumeActiveTurnForSession(
		context.Background(), principal, fixture.execution.SessionID,
		"active-explicit-resume-key", "active-explicit-resume-replay", "127.0.0.1",
	)
	if err != nil || !replay.Replayed || replay.Value.ID != resumed.Value.ID {
		t.Fatalf("explicit resume replay after claim = %#v, %v", replay, err)
	}
}

func TestActiveTurnSuspendResumeReacquiresTenantExecutionQuota(t *testing.T) {
	fixture := setupDurableActiveTurnSuspend(t, "active-resume-quota")
	acknowledgeActiveTurnSuspend(t, fixture, "cursor-active-resume-quota")
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.lease, fixture.directive.SuspendAttemptID, "active-resume-quota-quiesced",
	)
	completed, err := fixture.service.CompleteResourceSuspend(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		CompleteResourceSuspendInput{
			LeaseInput: fixture.lease, SuspendAttemptID: fixture.directive.SuspendAttemptID,
			CheckpointStatus: "unchanged",
		},
		"active-resume-quota-complete",
	)
	if err != nil || completed.Value.Status != "suspended" {
		t.Fatalf("complete quota fixture = %#v, %v", completed, err)
	}

	blockerExecutionID := seedTenantExecutionQuotaBlocker(
		t, fixture.db, fixture.execution, fixture.service.now(),
	)

	principal := identity.Principal{
		UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
	}
	_, err = fixture.service.ResumeActiveTurnForSession(
		context.Background(), principal, fixture.execution.SessionID,
		"active-resume-quota-rejected", "active-resume-quota-rejected-request", "127.0.0.1",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "execution_quota_exceeded" {
		t.Fatalf("resume quota rejection = %v", err)
	}
	var suspended persistence.AgentExecution
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&suspended).Error; err != nil {
		t.Fatal(err)
	}
	if suspended.Status != "suspended" {
		t.Fatalf("quota rejection changed suspended Execution: %#v", suspended)
	}

	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, blockerExecutionID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	resumed, err := fixture.service.ResumeActiveTurnForSession(
		context.Background(), principal, fixture.execution.SessionID,
		"active-resume-quota-retry", "active-resume-quota-retry-request", "127.0.0.1",
	)
	if err != nil || resumed.Value.Status != "recovering" {
		t.Fatalf("resume after quota release = %#v, %v", resumed, err)
	}
}

func TestActiveTurnSuspendQueuesCrossedActivityForImmediateRecovery(t *testing.T) {
	fixture := setupDurableActiveTurnSuspend(t, "active-crossed-activity")
	principal := identity.Principal{
		UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
	}
	steer, err := fixture.service.RequestSteer(
		context.Background(), principal, fixture.execution.SessionID,
		SteerActiveTurnInput{InputText: "continue after the checkpoint"},
		"active-crossed-steer", "active-crossed-steer-request", "127.0.0.1",
	)
	if err != nil || steer.Value.Status != "pending" {
		t.Fatalf("queue crossed Steer = %#v, %v", steer, err)
	}

	acknowledgeActiveTurnSuspend(t, fixture, "cursor-active-crossed")
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.lease, fixture.directive.SuspendAttemptID, "active-crossed-quiesced",
	)
	completed, err := fixture.service.CompleteResourceSuspend(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		CompleteResourceSuspendInput{
			LeaseInput: fixture.lease, SuspendAttemptID: fixture.directive.SuspendAttemptID,
			CheckpointStatus: "unchanged",
		},
		"active-crossed-complete",
	)
	if err != nil || completed.Value.Status != "recovering" {
		t.Fatalf("crossed activity did not atomically resume = %#v, %v", completed, err)
	}
	recoveryWorker := registerActiveTurnSuspendWorker(
		t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "active-crossed-recovery",
	)
	cleanupWorkers(t, fixture.db, recoveryWorker.ID)

	claim, err := fixture.service.Claim(
		context.Background(), recoveryWorker,
		ClaimExecutionInput{
			ExecutionTargetID: fixture.execution.TargetID, TargetKind: fixture.execution.TargetKind,
			ExecutionID: &fixture.execution.ExecutionID,
		},
		"active-crossed-recovery-claim",
	)
	if err != nil || claim.Value.Lease == nil || claim.Value.Workload == nil ||
		claim.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("crossed activity recovery claim = %#v, %v", claim, err)
	}
	recoveryLease := LeaseInput{
		TenantID: fixture.execution.TenantID, Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}
	commands, err := fixture.service.PullControlCommands(
		context.Background(), recoveryWorker, fixture.execution.ExecutionID,
		PullControlCommandsInput{LeaseInput: recoveryLease},
	)
	if err != nil || len(commands) != 1 || commands[0].ControlCommandID != steer.Value.ID ||
		commands[0].CommandType != "SteerTurn" {
		t.Fatalf("crossed Steer was not rebound to Recovery generation: %#v, %v", commands, err)
	}
}

func TestActiveTurnSuspendDeliveredWithoutReceiptFailsClosed(t *testing.T) {
	fixture := setupDurableActiveTurnSuspend(t, "active-missing-receipt")
	released, err := fixture.service.Release(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		ReleaseLeaseInput{
			LeaseInput:                     fixture.lease,
			Reason:                         "SuspendTurn delivery stopped the Provider without a durable receipt.",
			PreserveInteractionResolutions: true,
		},
		"active-missing-receipt-release",
	)
	if err != nil || released.Value.Status != "failed" || released.Value.FailureCode == nil ||
		*released.Value.FailureCode != "active_suspend_checkpoint_outcome_unknown" {
		t.Fatalf("missing active suspend receipt did not fail closed: %#v, %v", released, err)
	}
}

func TestActiveTurnSuspendUnavailableCursorTerminalizesRecoveringExecution(t *testing.T) {
	fixture := setupDurableActiveTurnSuspend(t, "active-cursor-unavailable")
	acknowledgeActiveTurnSuspend(t, fixture, "cursor-active-unavailable")
	markResourceSuspendQuiescedForTest(
		t, fixture.service, fixture.worker, fixture.execution.ExecutionID,
		fixture.lease, fixture.directive.SuspendAttemptID, "active-cursor-unavailable-quiesced",
	)
	if completed, err := fixture.service.CompleteResourceSuspend(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		CompleteResourceSuspendInput{
			LeaseInput: fixture.lease, SuspendAttemptID: fixture.directive.SuspendAttemptID,
			CheckpointStatus: "unchanged",
		},
		"active-cursor-unavailable-complete",
	); err != nil || completed.Value.Status != "suspended" {
		t.Fatalf("complete cursor-unavailable fixture = %#v, %v", completed, err)
	}
	principal := identity.Principal{
		UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
	}
	if resumed, err := fixture.service.ResumeActiveTurnForSession(
		context.Background(), principal, fixture.execution.SessionID,
		"active-cursor-unavailable-resume", "active-cursor-unavailable-request", "127.0.0.1",
	); err != nil || resumed.Value.Status != "recovering" {
		t.Fatalf("resume cursor-unavailable fixture = %#v, %v", resumed, err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.SessionID).
		Update("provider_resume_cursor_state", "quarantined").Error; err != nil {
		t.Fatal(err)
	}
	recoveryWorker := registerActiveTurnSuspendWorker(
		t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "active-cursor-unavailable-recovery",
	)
	cleanupWorkers(t, fixture.db, recoveryWorker.ID)
	claim, err := fixture.service.Claim(
		context.Background(), recoveryWorker,
		ClaimExecutionInput{
			ExecutionTargetID: fixture.execution.TargetID, TargetKind: fixture.execution.TargetKind,
			ExecutionID: &fixture.execution.ExecutionID,
		},
		"active-cursor-unavailable-claim",
	)
	if err != nil || claim.Value.Execution != nil || claim.Value.Lease != nil {
		t.Fatalf("unavailable cursor claim should terminalize without a lease: %#v, %v", claim, err)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "failed" || execution.FailureCode == nil ||
		*execution.FailureCode != "active_suspend_resume_unavailable" {
		t.Fatalf("unavailable active checkpoint cursor remained claimable: %#v", execution)
	}
}

func seedTenantExecutionQuotaBlocker(
	t *testing.T,
	db *gorm.DB,
	fixture executionFixture,
	now time.Time,
) uuid.UUID {
	t.Helper()
	var sourceSession persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Take(&sourceSession).Error; err != nil {
		t.Fatal(err)
	}
	limit := 1
	blockerSessionID := uuid.New()
	blockerTurnID := uuid.New()
	blockerExecutionID := uuid.New()
	blockerRuntimeBindingID := uuid.New()
	provider := "codex"
	if err := db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.TenantQuota{
				TenantID: fixture.TenantID, MaxConcurrentExecutions: &limit,
				UpdatedBy: fixture.UserID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentSession{
				ID: blockerSessionID, TenantID: fixture.TenantID,
				OrganizationID: sourceSession.OrganizationID, ProjectID: sourceSession.ProjectID,
				CreatedBy: fixture.UserID, Title: "quota blocker", Status: "active",
				Visibility: "private", Provider: provider,
				ProviderCredentialID:    sourceSession.ProviderCredentialID,
				ExecutionTargetID:       fixture.TargetID,
				CurrentRuntimeBindingID: &blockerRuntimeBindingID,
			},
			&persistence.ProviderRuntimeBinding{
				ID: blockerRuntimeBindingID, TenantID: fixture.TenantID,
				SessionID: blockerSessionID, Provider: provider, Revision: 1,
				Status: "active", ResumeStrategy: "authoritative-history",
				CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentTurn{
				ID: blockerTurnID, TenantID: fixture.TenantID,
				SessionID: blockerSessionID, CreatedBy: fixture.UserID,
				Status: "queued", InputText: "occupy the execution quota",
				RuntimeMode: "approval-required", InteractionMode: "plan",
			},
			&persistence.AgentExecution{
				ID: blockerExecutionID, TenantID: fixture.TenantID,
				SessionID: blockerSessionID, TurnID: blockerTurnID, Attempt: 1,
				Status: "queued", ExecutionTargetID: fixture.TargetID,
				TargetKind: fixture.TargetKind, Provider: &provider,
				ProviderRuntimeBindingID: &blockerRuntimeBindingID,
				Generation:               0, RequestedBy: fixture.UserID, QueuedAt: now,
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
	return blockerExecutionID
}

func setupDurableActiveTurnSuspend(t *testing.T, label string) activeTurnSuspendFixture {
	t.Helper()
	ctx := context.Background()
	db, service, execution := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	worker := registerActiveTurnSuspendWorker(t, service, execution.TargetID, execution.TargetKind, label)
	cleanupWorkers(t, db, worker.ID)

	firstClaim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: execution.TargetID, TargetKind: execution.TargetKind,
		ExecutionID: &execution.ExecutionID,
	}, label+"-initial-claim")
	if err != nil || firstClaim.Value.Lease == nil {
		t.Fatalf("initial claim = %#v, %v", firstClaim, err)
	}
	firstLease := LeaseInput{
		TenantID: execution.TenantID, Generation: firstClaim.Value.Lease.Generation,
		LeaseToken: firstClaim.Value.Lease.LeaseToken,
	}
	if _, err := service.Start(ctx, worker, execution.ExecutionID, firstLease, label+"-initial-start"); err != nil {
		t.Fatal(err)
	}
	seedCursor := "cursor-" + label + "-seed"
	if _, err := service.Renew(ctx, worker, execution.ExecutionID, RenewLeaseInput{
		LeaseInput: firstLease, ProviderResumeCursor: &seedCursor,
	}, label+"-initial-renew"); err != nil {
		t.Fatal(err)
	}
	if released, err := service.Release(ctx, worker, execution.ExecutionID, ReleaseLeaseInput{
		LeaseInput: firstLease, Reason: "seed native Provider cursor for active suspend test",
	}, label+"-initial-release"); err != nil || released.Value.Status != "recovering" {
		t.Fatalf("initial recovery release = %#v, %v", released, err)
	}

	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: execution.TargetID, TargetKind: execution.TargetKind,
		ExecutionID: &execution.ExecutionID,
	}, label+"-active-claim")
	if err != nil || claim.Value.Lease == nil || claim.Value.ProviderResumeCursor == nil ||
		*claim.Value.ProviderResumeCursor != seedCursor {
		t.Fatalf("native-cursor active claim = %#v, %v", claim, err)
	}
	lease := LeaseInput{
		TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}
	var turnCreatedCount int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
			execution.TenantID, execution.SessionID, execution.ExecutionID, "turn.created").
		Count(&turnCreatedCount).Error; err != nil {
		t.Fatal(err)
	}
	if turnCreatedCount == 0 {
		if err := persistence.InTransaction(ctx, db, func(tx *gorm.DB) error {
			_, err := service.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
				EventType: "turn.created", ActorType: "user", ActorID: &execution.UserID,
				ExecutionID: &execution.ExecutionID, WorkerID: &worker.ID, Generation: &lease.Generation,
				Payload: map[string]any{"turnId": execution.TurnID, "inputText": "long-running active work"},
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.Start(ctx, worker, execution.ExecutionID, lease, label+"-active-start"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AppendRuntimeEvent(ctx, worker, execution.ExecutionID, RuntimeEventInput{
		LeaseInput: lease, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "content.delta", OccurredAt: current,
		Payload: map[string]any{"streamKind": "assistant_text", "delta": "long-running active work"},
	}, label+"-active-progress"); err != nil {
		t.Fatal(err)
	}

	current = current.Add(1801 * time.Second)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", execution.TenantID, execution.ExecutionID).
		Updates(map[string]any{"heartbeat_at": current, "expires_at": current.Add(service.leaseTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := service.PullResourceDirective(
		ctx, worker, execution.ExecutionID, PullResourceDirectiveInput{LeaseInput: lease},
	)
	if err != nil || directive == nil || directive.Reason != resourceSuspendReasonActiveIdleTimeout ||
		directive.ControlCommandID == uuid.Nil {
		t.Fatalf("active-turn suspend directive = %#v, %v", directive, err)
	}
	commands, err := service.PullControlCommands(
		ctx, worker, execution.ExecutionID, PullControlCommandsInput{LeaseInput: lease},
	)
	if err != nil || len(commands) != 1 || commands[0].ControlCommandID != directive.ControlCommandID ||
		commands[0].CommandType != activeTurnSuspendCommandType {
		t.Fatalf("durable SuspendTurn delivery = %#v, %v", commands, err)
	}
	delivery := commands[0]
	if _, err := service.MarkControlCommandDelivered(
		ctx, worker, execution.ExecutionID, delivery.ControlCommandID,
		ControlCommandDeliveryInput{LeaseInput: lease, CommandID: delivery.CommandID},
		label+"-suspend-delivered",
	); err != nil {
		t.Fatal(err)
	}
	return activeTurnSuspendFixture{
		db: db, service: service, execution: execution, worker: worker,
		lease: lease, directive: *directive, delivery: delivery, current: &current,
	}
}

func acknowledgeActiveTurnSuspend(t *testing.T, fixture activeTurnSuspendFixture, cursor string) {
	t.Helper()
	result, err := fixture.service.AcknowledgeControlCommand(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		fixture.delivery.ControlCommandID,
		ControlCommandDeliveryInput{
			LeaseInput: fixture.lease, CommandID: fixture.delivery.CommandID,
			ProviderResumeCursor: &cursor,
			Result: map[string]any{
				"quiesced": true, "targetCommandId": "send-active-" + fixture.directive.SuspendAttemptID.String(),
				"checkpointProtocol": activeTurnSuspendCheckpointProtocol,
			},
		},
		"active-suspend-ack-"+fixture.directive.SuspendAttemptID.String(),
	)
	if err != nil || result.Value.Status != "acknowledged" {
		t.Fatalf("acknowledge durable SuspendTurn = %#v, %v", result, err)
	}
}

func registerActiveTurnSuspendWorker(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	targetKind string,
	label string,
) persistence.WorkerInstance {
	t.Helper()
	capabilities := workerManifestTestCapabilities()
	host := capabilities["providerHost"].(map[string]any)
	host["protocolVersion"].(map[string]any)["minor"] = activeTurnSuspendProviderHostMinor
	for _, raw := range host["providers"].(map[string]any) {
		raw.(map[string]any)["protocolVersion"].(map[string]any)["minor"] = activeTurnSuspendProviderHostMinor
	}
	addWorkerManifestTestContainmentEvidence(capabilities)
	return registerTestWorkerWithCapabilities(t, service, targetID, targetKind, label, capabilities)
}
