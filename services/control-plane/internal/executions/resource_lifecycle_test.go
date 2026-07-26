package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestEnforceResourceLifecycleCancelsAbsoluteExpiredLeasedExecution(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "absolute-expiry-live")
	cleanupWorkers(t, db, worker.ID)

	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "absolute-expiry-live-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("claim did not return a lease")
	}
	lease := claim.Value.Lease

	absoluteExpiry := time.Now().UTC().Add(time.Minute)
	enforcedAt := absoluteExpiry.Add(time.Second)
	resolvedAt := absoluteExpiry.Add(-time.Minute)
	deliveryAvailableAt := resolvedAt
	deliveredAt := resolvedAt.Add(time.Second)
	resolutionKind := "accept"
	resolutionCommandID := "absolute-expiry-delivered:resolution"
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionInteraction{
		ID: uuid.New(), TenantID: fixture.TenantID, ExecutionID: fixture.ExecutionID,
		SessionID: fixture.SessionID, TurnID: fixture.TurnID, WorkerID: worker.ID,
		Generation: lease.Generation, Provider: "codex", RequestID: "absolute-expiry-delivered",
		EventVersion: 1, Kind: "approval", Status: "resolved", Payload: map[string]any{"label": "continue?"},
		RequestedAt: resolvedAt.Add(-time.Minute), ExpiresAt: absoluteExpiry.Add(time.Hour),
		Resolution: map[string]any{"decision": "accept"}, ResolvedAt: &resolvedAt,
		ResolvedBy: &fixture.UserID, ResolutionKind: &resolutionKind, ResolutionCommandID: &resolutionCommandID,
		DeliveryStatus: "delivered", DeliveryWorkerID: &worker.ID, DeliveryGeneration: &lease.Generation,
		DeliveryAvailableAt: &deliveryAvailableAt, DeliveredAt: &deliveredAt,
	}).Error; err != nil {
		t.Fatal(err)
	}

	changed, err := service.EnforceResourceLifecycle(ctx, enforcedAt, 10)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("expected one absolute-expiry cancellation, got %d", changed)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "cancelled" || execution.WorkerID != nil || execution.FinishedAt == nil {
		t.Fatalf("absolute-expiry controller did not cancel the leased Execution: %#v", execution)
	}

	var leases int64
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("absolute-expiry controller left %d Worker leases behind", leases)
	}
	_, release := loadExecutionClaimReleaseFactForTest(t, db, fixture.ExecutionID, lease.Generation)
	if release.ReleaseReason != workerClaimReleaseSessionAbsoluteExpired ||
		!release.ReleasedAt.Equal(absoluteExpiry) || !release.RecordedAt.Equal(enforcedAt) {
		t.Fatalf("absolute-expiry release fact = %#v", release)
	}
	var delivered persistence.ExecutionInteraction
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.TenantID, fixture.ExecutionID, "absolute-expiry-delivered",
	).Take(&delivered).Error; err != nil {
		t.Fatal(err)
	}
	if delivered.DeliveryStatus != "outcome-unknown" || delivered.DeliveryError == nil {
		t.Fatalf("absolute expiry mislabeled an ambiguous delivered resolution: %#v", delivered)
	}
	changed, err = service.EnforceResourceLifecycle(ctx, enforcedAt.Add(time.Second), 10)
	if err != nil || changed != 0 {
		t.Fatalf("replayed absolute-expiry sweep = %d, %v; want no-op", changed, err)
	}
}

func TestClaimRejectsAbsoluteExpiredSessionBeforeCreatingLease(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "absolute-expiry-claim")
	cleanupWorkers(t, db, worker.ID)
	absoluteExpiry := service.now().Add(time.Second)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return absoluteExpiry.Add(time.Millisecond) }

	_, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "absolute-expiry-direct-claim")
	assertProblemCode(t, err, "session_absolute_expired")

	var leases int64
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("absolute-expired Session created %d Worker leases", leases)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "queued" || execution.Generation != 0 || execution.WorkerID != nil {
		t.Fatalf("absolute-expired Claim advanced the Execution: %#v", execution)
	}
}

func TestAbsoluteExpiredSessionWithholdsGenerationScopedRuntimeAuthority(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "absolute-expiry-runtime-authority")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "absolute-expiry-runtime-authority-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("Claim before absolute expiry: %#v, %v", claim, err)
	}
	expiresAt := service.now().Add(time.Minute)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("absolute_expires_at", expiresAt).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Update("expires_at", expiresAt.Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return expiresAt.Add(time.Millisecond) }
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		_, authorizeErr := service.AuthorizeLeaseWithinSessionLifetime(
			ctx, tx, worker, fixture.ExecutionID, leaseInput,
		)
		return authorizeErr
	})
	assertProblemCode(t, err, "session_absolute_expired")
}

func TestClaimReceiptReplayDoesNotRotateLeaseAfterAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "absolute-expiry-claim-replay")
	cleanupWorkers(t, db, worker.ID)
	absoluteExpiry := service.now().Add(time.Second)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	requestID := "absolute-expiry-claim-replay"
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, requestID)
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("initial Claim before absolute expiry: %#v, %v", claim, err)
	}
	var before persistence.WorkerLease
	if err := db.Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return absoluteExpiry.Add(time.Millisecond) }

	_, err = service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, requestID)
	assertProblemCode(t, err, "session_absolute_expired")
	var after persistence.WorkerLease
	if err := db.Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) || string(after.LeaseTokenHash) != string(before.LeaseTokenHash) {
		t.Fatalf("absolute-expired Claim replay rotated its lease: before=%#v after=%#v", before, after)
	}
}

func TestAbsoluteExpiredSessionRejectsRunningStartReplayAndRuntimeEvents(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "absolute-expiry-running")
	cleanupWorkers(t, db, worker.ID)
	absoluteExpiry := service.now().Add(time.Second)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "absolute-expiry-running-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("Claim before absolute expiry: %#v, %v", claim, err)
	}
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := service.Start(ctx, worker, fixture.ExecutionID, leaseInput, "absolute-expiry-running-start"); err != nil {
		t.Fatal(err)
	}
	postExpiry := absoluteExpiry.Add(time.Millisecond)
	service.now = func() time.Time { return postExpiry }

	if _, err := service.Start(ctx, worker, fixture.ExecutionID, leaseInput, "absolute-expiry-running-start-retry"); err == nil {
		t.Fatal("absolute-expired running Execution accepted Start")
	} else {
		assertProblemCode(t, err, "session_absolute_expired")
	}
	eventID := uuid.New()
	if _, err := service.AppendRuntimeEvent(ctx, worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: eventID, EventVersion: RuntimeEventVersionV1,
		EventType: "runtime.output.delta", Payload: map[string]any{"text": "after expiry"}, OccurredAt: postExpiry,
	}, "absolute-expiry-runtime-event"); err == nil {
		t.Fatal("absolute-expired Execution accepted a Runtime Event")
	} else {
		assertProblemCode(t, err, "session_absolute_expired")
	}
	var eventCount int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND event_id = ?", fixture.TenantID, eventID).
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("absolute-expired Runtime Event committed %d rows", eventCount)
	}
}

func TestAbsoluteExpiredSessionCancelsLateWorkerTerminalReports(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		eventType string
		terminal  func(context.Context, *Service, persistence.WorkerInstance, executionFixture, LeaseInput) (Execution, error)
	}{
		{
			name: "complete", eventType: "execution.completed",
			terminal: func(ctx context.Context, service *Service, worker persistence.WorkerInstance, fixture executionFixture, lease LeaseInput) (Execution, error) {
				result, err := service.Complete(ctx, worker, fixture.ExecutionID, CompleteExecutionInput{
					LeaseInput: lease, Output: map[string]any{"text": "too late"},
				}, "absolute-expiry-late-complete")
				return result.Value, err
			},
		},
		{
			name: "fail", eventType: "execution.failed",
			terminal: func(ctx context.Context, service *Service, worker persistence.WorkerInstance, fixture executionFixture, lease LeaseInput) (Execution, error) {
				result, err := service.Fail(ctx, worker, fixture.ExecutionID, FailExecutionInput{
					LeaseInput: lease, FailureCode: "provider_failed", FailureMessage: "too late",
				}, "absolute-expiry-late-fail")
				return result.Value, err
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			db, service, fixture := setupSQLiteRecoveryService(t)
			worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "absolute-expiry-late-"+testCase.name)
			cleanupWorkers(t, db, worker.ID)
			absoluteExpiry := service.now().Add(time.Second)
			if err := db.Model(&persistence.AgentSession{}).
				Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
				Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
				t.Fatal(err)
			}
			claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
				ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
			}, "absolute-expiry-late-claim-"+testCase.name)
			if err != nil || claim.Value.Lease == nil {
				t.Fatalf("Claim before absolute expiry: %#v, %v", claim, err)
			}
			leaseInput := LeaseInput{
				TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
				LeaseToken: claim.Value.Lease.LeaseToken,
			}
			if _, err := service.Start(ctx, worker, fixture.ExecutionID, leaseInput, "absolute-expiry-late-start-"+testCase.name); err != nil {
				t.Fatal(err)
			}
			service.now = func() time.Time { return absoluteExpiry.Add(time.Millisecond) }

			terminal, err := testCase.terminal(ctx, service, worker, fixture, leaseInput)
			if err != nil || terminal.Status != "cancelled" {
				t.Fatalf("late %s did not converge to cancellation: %#v, %v", testCase.name, terminal, err)
			}
			var forbiddenEvents int64
			if err := db.Model(&persistence.SessionEvent{}).
				Where("tenant_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.ExecutionID, testCase.eventType).
				Count(&forbiddenEvents).Error; err != nil {
				t.Fatal(err)
			}
			if forbiddenEvents != 0 {
				t.Fatalf("late %s committed %d %s events", testCase.name, forbiddenEvents, testCase.eventType)
			}
		})
	}
}

func TestRecoverExpiredCancelsAbsoluteExpiredSessionWithoutEnqueueingRecovery(t *testing.T) {
	db, service, fixture, _, _ := setupExpiredManagedLeaseRecovery(t, false)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("absolute_expires_at", service.now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}

	if err := service.RecoverExpired(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "cancelled" || execution.WorkerID != nil || execution.FinishedAt == nil {
		t.Fatalf("absolute-expired lease recovery did not cancel the Execution: %#v", execution)
	}
	var recoveryEvents int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
			fixture.TenantID, fixture.SessionID, fixture.ExecutionID, "execution.recovering").
		Count(&recoveryEvents).Error; err != nil {
		t.Fatal(err)
	}
	if recoveryEvents != 0 {
		t.Fatalf("absolute-expired lease recovery emitted %d recovery events", recoveryEvents)
	}
	var recoveryMessages int64
	if err := db.Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic = ?", fixture.TenantID, "execution.recovering").
		Count(&recoveryMessages).Error; err != nil {
		t.Fatal(err)
	}
	if recoveryMessages != 0 {
		t.Fatalf("absolute-expired lease recovery enqueued %d recovery messages", recoveryMessages)
	}
}

func TestEnforceResourceLifecycleIncludesOperationallySuspendedSession(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "absolute-expiry-operational-suspend")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "absolute-expiry-operational-suspend-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim operationally suspended fixture: %#v, %v", claim, err)
	}
	expiry := service.now().Add(time.Minute)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Updates(map[string]any{"status": "suspended", "absolute_expires_at": expiry}).Error; err != nil {
		t.Fatal(err)
	}

	changed, err := service.EnforceResourceLifecycle(ctx, expiry.Add(time.Second), 10)
	if err != nil || changed != 1 {
		t.Fatalf("operationally suspended absolute-expiry sweep = %d, %v; want one cancellation", changed, err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "cancelled" {
		t.Fatalf("operationally suspended Session retained nonterminal Execution: %#v", execution)
	}
}

func TestEnforceResourceLifecycleCancelsAbsoluteExpiredSuspendedExecution(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "absolute-expiry-suspended")
	cleanupWorkers(t, db, worker.ID)

	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "absolute-expiry-suspended-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("claim did not return a lease")
	}
	lease := claim.Value.Lease
	absoluteExpiry := time.Now().UTC().Add(time.Minute)
	enforcedAt := absoluteExpiry.Add(time.Second)
	pendingExpiry := time.Now().UTC().Add(time.Hour)
	requestedAt := time.Now().UTC().Add(-2 * time.Minute)
	idleSince := absoluteExpiry.Add(-30 * time.Second)
	resolvedAt := requestedAt.Add(time.Second)
	resolutionKind := "accept"
	resolutionCommandID := "absolute-expiry-recorded:resolution"
	deliveryAvailableAt := resolvedAt

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
			Updates(map[string]any{
				"absolute_expires_at": absoluteExpiry,
				"resource_state":      "suspended",
				"resource_idle_since": idleSince,
			}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&persistence.WorkerLease{}, "tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Updates(map[string]any{
				"status":               "suspended",
				"worker_id":            nil,
				"next_recovery_reason": "suspend-resume",
			}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.ExecutionInteraction{
			ID: uuid.New(), TenantID: fixture.TenantID, ExecutionID: fixture.ExecutionID,
			SessionID: fixture.SessionID, TurnID: fixture.TurnID, WorkerID: worker.ID,
			Generation: lease.Generation, Provider: "codex", RequestID: "absolute-expiry-pending",
			EventVersion: 1, Kind: "approval", Status: "pending", Payload: map[string]any{"label": "resume?"},
			RequestedAt: requestedAt, ExpiresAt: pendingExpiry, DeliveryStatus: "not-ready",
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.ExecutionInteraction{
			ID: uuid.New(), TenantID: fixture.TenantID, ExecutionID: fixture.ExecutionID,
			SessionID: fixture.SessionID, TurnID: fixture.TurnID, WorkerID: worker.ID,
			Generation: lease.Generation, Provider: "codex", RequestID: "absolute-expiry-recorded",
			EventVersion: 1, Kind: "approval", Status: "resolved", Payload: map[string]any{"label": "continue?"},
			RequestedAt: requestedAt, ExpiresAt: pendingExpiry,
			Resolution: map[string]any{"decision": "accept"}, ResolvedAt: &resolvedAt,
			ResolvedBy: &fixture.UserID, ResolutionKind: &resolutionKind, ResolutionCommandID: &resolutionCommandID,
			DeliveryStatus: "resume-recorded", DeliveryAvailableAt: &deliveryAvailableAt,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}

	changed, err := service.EnforceResourceLifecycle(ctx, enforcedAt, 10)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("expected one absolute-expiry suspended cancellation, got %d", changed)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "cancelled" || execution.FinishedAt == nil {
		t.Fatalf("absolute-expiry controller did not cancel the suspended Execution: %#v", execution)
	}

	var interaction persistence.ExecutionInteraction
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.TenantID, fixture.ExecutionID, "absolute-expiry-pending",
	).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.Status != "expired" || interaction.DeliveryStatus != "superseded" {
		t.Fatalf("absolute-expiry controller did not supersede suspended interactions: %#v", interaction)
	}
	var recorded persistence.ExecutionInteraction
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.TenantID, fixture.ExecutionID, "absolute-expiry-recorded",
	).Take(&recorded).Error; err != nil {
		t.Fatal(err)
	}
	if recorded.Status != "resolved" || recorded.DeliveryStatus != "resume-recorded" {
		t.Fatalf("absolute expiry corrupted a durable unbound resolution: %#v", recorded)
	}
}

func TestAbsoluteExpiredSessionRejectsNewWorkButAllowsInterrupt(t *testing.T) {
	fixture := setupActiveResourceSuspendAttempt(t, "absolute-expiry-writes")
	now := fixture.service.now()
	absoluteExpiry := now.Add(-time.Second)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.SessionID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
	}
	if _, err := fixture.service.ResolveApproval(
		context.Background(), principal, fixture.execution.ExecutionID, fixture.requestID,
		ResolveApprovalInput{Decision: "accept"}, "absolute-expiry-resolve",
		"absolute-expiry-resolve-audit", "127.0.0.1",
	); err == nil {
		t.Fatal("absolute-expired Session accepted an interaction resolution")
	} else {
		assertProblemCode(t, err, "session_absolute_expired")
	}
	if _, err := fixture.service.RequestSteer(
		context.Background(), principal, fixture.execution.SessionID,
		SteerActiveTurnInput{InputText: "continue after expiry"},
		"absolute-expiry-steer", "absolute-expiry-steer-audit", "127.0.0.1",
	); err == nil {
		t.Fatal("absolute-expired Session accepted a Steer command")
	} else {
		assertProblemCode(t, err, "session_absolute_expired")
	}
	interrupt, err := fixture.service.RequestInterrupt(
		context.Background(), principal, fixture.execution.SessionID,
		"absolute-expiry-interrupt", "absolute-expiry-interrupt-audit", "127.0.0.1",
	)
	if err != nil || interrupt.Value.CommandType != "InterruptTurn" {
		t.Fatalf("absolute-expired Session could not be interrupted safely: %#v, %v", interrupt, err)
	}
}
