package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestConcurrentClaimHasSingleWinner(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	first := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-a")
	second := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-b")
	cleanupWorkers(t, db, first.ID, second.ID)

	type outcome struct {
		result OperationResult[ClaimResult]
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for index, worker := range []persistence.WorkerInstance{first, second} {
		wait.Add(1)
		go func(index int, worker persistence.WorkerInstance) {
			defer wait.Done()
			<-start
			result, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
				ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
			}, "claim-"+uuid.NewString())
			outcomes <- outcome{result: result, err: err}
		}(index, worker)
	}
	close(start)
	wait.Wait()
	close(outcomes)

	winners := 0
	for item := range outcomes {
		if item.err != nil {
			t.Fatalf("claim failed: %v", item.err)
		}
		if item.result.Value.Lease != nil {
			winners++
			if item.result.Value.Execution == nil || item.result.Value.Execution.ID != fixture.ExecutionID {
				t.Fatalf("claimed unexpected execution: %#v", item.result.Value.Execution)
			}
			if item.result.Value.Workload == nil || item.result.Value.Workload.InputText != "Run integration test" ||
				item.result.Value.Workload.Provider != "codex" || item.result.Value.Workload.DefaultBranch != "main" {
				t.Fatalf("claim omitted execution workload: %#v", item.result.Value.Workload)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one claim winner, got %d", winners)
	}
	var leaseCount int64
	if err := db.Model(&persistence.WorkerLease{}).Where("execution_id = ?", fixture.ExecutionID).Count(&leaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if leaseCount != 1 {
		t.Fatalf("expected one persisted lease, got %d", leaseCount)
	}
}

func TestSuspendedTenantExecutionIsNotClaimed(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-suspended")
	cleanupWorkers(t, db, worker.ID)
	if err := db.Model(&persistence.Tenant{}).Where("id = ?", fixture.TenantID).Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
	}, "claim-suspended")
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.Execution != nil || result.Value.Lease != nil {
		t.Fatalf("suspended tenant execution was claimed: %#v", result.Value)
	}
}

func TestLeaseRecoveryFencingEventsAndIdempotentCompletion(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	current := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return current }
	first := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-c")
	second := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-d")
	cleanupWorkers(t, db, first.ID, second.ID)

	claimInput := ClaimExecutionInput{ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind}
	firstClaim, err := service.Claim(context.Background(), first, claimInput, "claim-first")
	if err != nil {
		t.Fatal(err)
	}
	firstLease := *firstClaim.Value.Lease
	firstEventID := uuid.New()
	firstEvent, err := service.AppendRuntimeEvent(context.Background(), first, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
		},
		EventID: firstEventID, EventVersion: 1, EventType: "runtime.output.delta",
		Payload: map[string]any{"text": "hello"}, OccurredAt: current,
	}, "event-first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(context.Background(), first, fixture.ExecutionID, RenewLeaseInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
		},
		ProviderResumeCursor: pointer("resume-before-crash"),
	}, "renew-checkpoint"); err != nil {
		t.Fatalf("persist runtime resume checkpoint: %v", err)
	}

	current = current.Add(service.leaseTTL + time.Second)
	secondClaim, err := service.Claim(context.Background(), second, claimInput, "claim-second")
	if err != nil {
		t.Fatal(err)
	}
	secondLease := *secondClaim.Value.Lease
	if secondLease.Generation != firstLease.Generation+1 {
		t.Fatalf("expected generation %d, got %d", firstLease.Generation+1, secondLease.Generation)
	}
	if secondClaim.Value.ProviderResumeCursor == nil || *secondClaim.Value.ProviderResumeCursor != "resume-before-crash" {
		t.Fatalf("recovery claim did not receive the persisted resume cursor: %#v", secondClaim.Value.ProviderResumeCursor)
	}
	_, err = service.AppendRuntimeEvent(context.Background(), first, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
		},
		EventID: uuid.New(), EventVersion: 1, EventType: "runtime.output.delta",
		Payload: map[string]any{"text": "stale"}, OccurredAt: current,
	}, "event-stale")
	if err == nil || (!strings.Contains(err.Error(), "generation") && !strings.Contains(err.Error(), "lease")) {
		t.Fatalf("expected stale generation rejection, got %v", err)
	}

	duplicate, err := service.AppendRuntimeEvent(context.Background(), first, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
		},
		EventID: firstEventID, EventVersion: 1, EventType: "runtime.output.delta",
		Payload: map[string]any{"text": "hello"}, OccurredAt: current.Add(-time.Second),
	}, "event-duplicate-new-request")
	if err != nil {
		t.Fatalf("duplicate event id should be idempotent: %v", err)
	}
	if duplicate.Value.Sequence != firstEvent.Value.Sequence {
		t.Fatalf("duplicate event changed sequence: %d != %d", duplicate.Value.Sequence, firstEvent.Value.Sequence)
	}

	completeInput := CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: secondLease.Generation, LeaseToken: secondLease.LeaseToken,
		},
		ProviderResumeCursor: pointer("resume-secret"), Output: map[string]any{"summary": "done"},
	}
	completed, err := service.Complete(context.Background(), second, fixture.ExecutionID, completeInput, "complete-once")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Complete(context.Background(), second, fixture.ExecutionID, completeInput, "complete-once")
	if err != nil {
		t.Fatalf("idempotent completion replay failed: %v", err)
	}
	if !replayed.Replayed || replayed.Value.ID != completed.Value.ID || replayed.Value.Status != "completed" {
		t.Fatalf("unexpected completion replay: %#v", replayed)
	}
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if len(session.ProviderResumeCursorEncrypted) == 0 || bytes.Contains(session.ProviderResumeCursorEncrypted, []byte("resume-secret")) {
		t.Fatal("provider resume cursor was not stored as ciphertext")
	}
	var leaseCount int64
	if err := db.Model(&persistence.WorkerLease{}).Where("execution_id = ?", fixture.ExecutionID).Count(&leaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if leaseCount != 0 {
		t.Fatalf("terminal execution retained %d lease rows", leaseCount)
	}
}

func TestClaimReplayRotatesTokenWithoutPersistingPlaintext(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-e")
	cleanupWorkers(t, db, worker.ID)

	claimInput := ClaimExecutionInput{ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind}
	first, err := service.Claim(context.Background(), worker, claimInput, "same-claim-request")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Claim(context.Background(), worker, claimInput, "same-claim-request")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || first.Value.Execution.ID != second.Value.Execution.ID || first.Value.Lease.Generation != second.Value.Lease.Generation {
		t.Fatalf("claim replay changed logical result: first=%#v second=%#v", first, second)
	}
	if first.Value.Lease.LeaseToken == second.Value.Lease.LeaseToken {
		t.Fatal("claim replay should rotate the unrecoverable lease token")
	}
	var receipt persistence.WorkerRequestReceipt
	if err := db.Where("worker_id = ? AND request_id = ?", worker.ID, "same-claim-request").Take(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(receipt.Response)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(first.Value.Lease.LeaseToken)) || bytes.Contains(encoded, []byte(second.Value.Lease.LeaseToken)) {
		t.Fatal("claim receipt persisted a plaintext lease token")
	}
	_, err = service.Renew(context.Background(), worker, fixture.ExecutionID, RenewLeaseInput{LeaseInput: LeaseInput{
		TenantID: fixture.TenantID, Generation: first.Value.Lease.Generation, LeaseToken: first.Value.Lease.LeaseToken,
	}}, "renew-old-token")
	if err == nil {
		t.Fatal("rotated lease token remained valid")
	}
	if _, err := service.Renew(context.Background(), worker, fixture.ExecutionID, RenewLeaseInput{LeaseInput: LeaseInput{
		TenantID: fixture.TenantID, Generation: second.Value.Lease.Generation, LeaseToken: second.Value.Lease.LeaseToken,
	}}, "renew-current-token"); err != nil {
		t.Fatalf("current rotated lease token was rejected: %v", err)
	}
}

func TestCreateTurnQueuesExecutionAndOutboxAtomically(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	rollback := errors.New("rollback create turn integration test")
	err := db.Transaction(func(tx *gorm.DB) error {
		targetService := executiontargets.NewService(tx, testPlatformConfig(), nil)
		sessionService := sessions.NewService(tx, projects.NewService(tx), targetService)
		turn, err := sessionService.CreateTurn(context.Background(), identity.Principal{
			UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID,
		}, fixture.SessionID, sessions.CreateTurnInput{InputText: "Queue another execution"}, "turn-request", "127.0.0.1")
		if err != nil {
			return err
		}
		var execution persistence.AgentExecution
		if err := tx.Where("tenant_id = ? AND turn_id = ?", fixture.TenantID, turn.ID).Take(&execution).Error; err != nil {
			t.Fatalf("queued execution missing: %v", err)
		}
		if execution.Status != "queued" || execution.ExecutionTargetID != fixture.TargetID || execution.TargetKind != fixture.TargetKind || execution.Generation != 0 {
			t.Fatalf("unexpected queued execution: %#v", execution)
		}
		if execution.QueuedAt.Year() < 2020 {
			t.Fatalf("execution queued_at was persisted as a zero timestamp: %s", execution.QueuedAt)
		}
		var outbox persistence.OutboxMessage
		if err := tx.Where("topic = ? AND message_key = ?", "execution.queued", execution.ID.String()).Take(&outbox).Error; err != nil {
			t.Fatalf("execution outbox missing: %v", err)
		}
		if outbox.TenantID == nil || *outbox.TenantID != fixture.TenantID || outbox.PublishedAt != nil {
			t.Fatalf("unexpected execution outbox: %#v", outbox)
		}
		if outbox.AvailableAt.Year() < 2020 || outbox.CreatedAt.Year() < 2020 {
			t.Fatalf("execution outbox timestamps were persisted as zero values: %#v", outbox)
		}
		var event persistence.SessionEvent
		if err := tx.Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.SessionID, execution.ID, "turn.created").Take(&event).Error; err != nil {
			t.Fatalf("turn event is not linked to execution: %v", err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("create turn transaction: %v", err)
	}
}

func TestExecutionCancelIsIdempotentAndRemovesLease(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-cancel")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "cancel-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("cancel test execution was not leased")
	}
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}

	first, err := service.Cancel(
		context.Background(), principal, fixture.ExecutionID, "cancel-key", "cancel-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Cancel(
		context.Background(), principal, fixture.ExecutionID, "cancel-key", "cancel-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || !second.Replayed || first.Value.ID != second.Value.ID || second.Value.Status != "cancelled" {
		t.Fatalf("unexpected cancel replay: first=%#v second=%#v", first, second)
	}
	var leases int64
	if err := db.Model(&persistence.WorkerLease{}).Where("execution_id = ?", fixture.ExecutionID).Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("cancelled execution retained %d leases", leases)
	}
	var events, messages int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.ExecutionID, "execution.cancelled").
		Count(&events).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic = ? AND message_key = ?", fixture.TenantID, "execution.cancelled", fixture.ExecutionID.String()).
		Count(&messages).Error; err != nil {
		t.Fatal(err)
	}
	if events != 1 || messages != 1 {
		t.Fatalf("cancel lifecycle duplicated event/outbox: events=%d messages=%d", events, messages)
	}
}

func TestConcurrentCancelAndCompleteHasOneTerminalWinner(t *testing.T) {
	for iteration := range 5 {
		t.Run("race-"+string(rune('a'+iteration)), func(t *testing.T) {
			db := integrationDB(t)
			fixture := seedExecutionFixture(t, db)
			service := integrationService(t, db)
			worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-terminal-race")
			cleanupWorkers(t, db, worker.ID)
			claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
				ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
			}, "terminal-race-claim")
			if err != nil {
				t.Fatal(err)
			}
			lease := claim.Value.Lease
			if lease == nil {
				t.Fatal("terminal race execution was not leased")
			}
			principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
			start := make(chan struct{})
			outcomes := make(chan error, 2)
			var wait sync.WaitGroup
			wait.Add(2)
			go func() {
				defer wait.Done()
				<-start
				_, err := service.Cancel(
					context.Background(), principal, fixture.ExecutionID,
					"terminal-race-cancel", "terminal-race-cancel", "127.0.0.1",
				)
				outcomes <- err
			}()
			go func() {
				defer wait.Done()
				<-start
				_, err := service.Complete(context.Background(), worker, fixture.ExecutionID, CompleteExecutionInput{
					LeaseInput: LeaseInput{
						TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
					},
				}, "terminal-race-complete")
				outcomes <- err
			}()
			close(start)
			wait.Wait()
			close(outcomes)

			succeeded := 0
			for err := range outcomes {
				if err == nil {
					succeeded++
					continue
				}
				var apiError *problem.Error
				if !errors.As(err, &apiError) || (apiError.Code != "execution_terminal" && apiError.Code != "lease_not_current") {
					t.Fatalf("unexpected terminal race error: %v", err)
				}
			}
			if succeeded != 1 {
				t.Fatalf("terminal race successes = %d, want 1", succeeded)
			}
			var execution persistence.AgentExecution
			if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
				t.Fatal(err)
			}
			if execution.Status != "completed" && execution.Status != "cancelled" {
				t.Fatalf("terminal race left status %q", execution.Status)
			}
			var leases int64
			if err := db.Model(&persistence.WorkerLease{}).Where("execution_id = ?", fixture.ExecutionID).Count(&leases).Error; err != nil {
				t.Fatal(err)
			}
			if leases != 0 {
				t.Fatalf("terminal race retained %d leases", leases)
			}
		})
	}
}

func TestApprovalAndUserInputPersistResolveReplayAndResumeExecution(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-interaction")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "interaction-claim")
	if err != nil {
		t.Fatal(err)
	}
	lease := claim.Value.Lease
	if lease == nil {
		t.Fatal("interaction test execution was not leased")
	}
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}
	if _, err := service.Start(context.Background(), worker, fixture.ExecutionID, leaseInput, "interaction-start"); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}

	approvalRequestID := "approval-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(context.Background(), worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: 1, EventType: "approval.requested",
		Payload: map[string]any{
			"requestId": approvalRequestID, "requestType": "exec_command_approval", "summary": "Run command",
		}, OccurredAt: time.Now().UTC(),
	}, "approval-requested"); err != nil {
		t.Fatal(err)
	}
	assertExecutionStatus(t, db, fixture, "waiting-for-approval")
	if _, err := service.Complete(context.Background(), worker, fixture.ExecutionID, CompleteExecutionInput{
		LeaseInput: leaseInput,
	}, "complete-while-approval-pending"); err == nil {
		t.Fatal("execution completed while approval was pending")
	}

	resolved, err := service.ResolveApproval(
		context.Background(), principal, fixture.ExecutionID, approvalRequestID,
		ResolveApprovalInput{Decision: "accept"}, "approval-resolve-key", "approval-resolve", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.ResolveApproval(
		context.Background(), principal, fixture.ExecutionID, approvalRequestID,
		ResolveApprovalInput{Decision: "accept"}, "approval-resolve-key", "approval-resolve-replay", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Replayed || !replayed.Replayed || replayed.Value.ID != resolved.Value.ID || resolved.Value.Status != "resolved" {
		t.Fatalf("unexpected approval replay: first=%#v second=%#v", resolved, replayed)
	}
	assertExecutionStatus(t, db, fixture, "running")

	userInputRequestID := "user-input-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(context.Background(), worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: 1, EventType: "user-input.requested",
		Payload: map[string]any{
			"requestId": userInputRequestID,
			"questions": []any{map[string]any{"id": "environment", "question": "Which environment?"}},
		}, OccurredAt: time.Now().UTC(),
	}, "user-input-requested"); err != nil {
		t.Fatal(err)
	}
	userInput, err := service.ResolveUserInput(
		context.Background(), principal, fixture.ExecutionID, userInputRequestID,
		ResolveUserInputInput{Answers: map[string]any{"environment": "staging"}},
		"user-input-resolve-key", "user-input-resolve", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if userInput.Value.Status != "resolved" {
		t.Fatalf("user input was not resolved: %#v", userInput)
	}
	assertExecutionStatus(t, db, fixture, "running")

	interactions, err := service.ListInteractions(context.Background(), principal, fixture.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(interactions) != 2 || interactions[0].Status != "resolved" || interactions[1].Status != "resolved" {
		t.Fatalf("unexpected persisted interactions: %#v", interactions)
	}
	var requestedEvents, resolvedEvents int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type IN ?", fixture.TenantID, fixture.ExecutionID,
			[]string{"approval.requested", "user-input.requested"}).Count(&requestedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type IN ?", fixture.TenantID, fixture.ExecutionID,
			[]string{"approval.resolved", "user-input.resolved"}).Count(&resolvedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if requestedEvents != 2 || resolvedEvents != 2 {
		t.Fatalf("interaction replay duplicated events: requested=%d resolved=%d", requestedEvents, resolvedEvents)
	}
}

func TestInteractionResolutionRejectsExpiredLease(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-expired-interaction")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "expired-interaction-claim")
	if err != nil {
		t.Fatal(err)
	}
	lease := claim.Value.Lease
	if lease == nil {
		t.Fatal("expired interaction execution was not leased")
	}
	leaseInput := LeaseInput{TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken}
	requestID := "approval-expired-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(context.Background(), worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: 1, EventType: "approval.requested",
		Payload: map[string]any{"requestId": requestID}, OccurredAt: now,
	}, "expired-approval-requested"); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(service.leaseTTL + time.Second) }
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	_, err = service.ResolveApproval(
		context.Background(), principal, fixture.ExecutionID, requestID,
		ResolveApprovalInput{Decision: "accept"}, "expired-approval-key", "expired-approval", "127.0.0.1",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "interaction_lease_expired" {
		t.Fatalf("expected interaction_lease_expired, got %v", err)
	}
}

func assertExecutionStatus(t *testing.T, db *gorm.DB, fixture executionFixture, want string) {
	t.Helper()
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != want {
		t.Fatalf("execution status = %q, want %q", execution.Status, want)
	}
}

func TestConcurrentTurnCreationCannotOversubscribeTenantQuota(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	maxExecutions := 2
	if err := db.Create(&persistence.TenantQuota{
		TenantID: fixture.TenantID, MaxConcurrentExecutions: &maxExecutions, UpdatedBy: fixture.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(db, testPlatformConfig(), nil)
	service := sessions.NewService(db, projects.NewService(db), targetService)
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}

	type outcome struct {
		err error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := service.CreateTurn(context.Background(), principal, fixture.SessionID, sessions.CreateTurnInput{
				InputText: "Concurrent quota request " + uuid.NewString(),
			}, "quota-concurrent-"+uuid.NewString(), "127.0.0.1")
			outcomes <- outcome{err: err}
		}()
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
		if errors.As(item.err, &apiError) && apiError.Code == "execution_quota_exceeded" {
			rejected++
			continue
		}
		t.Fatalf("concurrent quota request failed unexpectedly: %v", item.err)
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("expected one success and one quota rejection, got success=%d rejected=%d", succeeded, rejected)
	}
	var activeExecutions int64
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND status IN ?", fixture.TenantID, []string{"queued", "leased", "running", "waiting-for-approval", "recovering"}).
		Count(&activeExecutions).Error; err != nil {
		t.Fatal(err)
	}
	if activeExecutions != 2 {
		t.Fatalf("tenant quota was oversubscribed: %d active executions", activeExecutions)
	}
}

type executionFixture struct {
	UserID      uuid.UUID
	TenantID    uuid.UUID
	SessionID   uuid.UUID
	ExecutionID uuid.UUID
	TargetID    uuid.UUID
	TargetKind  string
}

func integrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return db
}

func integrationService(t *testing.T, db *gorm.DB) *Service {
	t.Helper()
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(db, testPlatformConfig(), cipher)
	return NewService(
		db, sessions.NewService(db, projects.NewService(db), targetService),
		30*time.Second, 2*time.Minute, time.Hour, cipher, targetService,
	)
}

func seedExecutionFixture(t *testing.T, db *gorm.DB) executionFixture {
	t.Helper()
	now := time.Now().UTC()
	userID := uuid.New()
	tenantID := uuid.New()
	organizationID := uuid.New()
	projectID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	targetID := uuid.New()
	slug := "exec-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	models := []any{
		&persistence.User{ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Execution test", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: slug, Name: "Execution test tenant", Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now},
		&persistence.Organization{ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.OrganizationMembership{TenantID: tenantID, OrganizationID: organizationID, UserID: userID, Role: "owner", Status: "active"},
		&persistence.Project{ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Execution project", DefaultBranch: "main", Visibility: "organization", CreatedBy: userID},
		&persistence.ExecutionTarget{ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID, Kind: "kubernetes", Name: "test-target", Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}},
		&persistence.AgentSession{ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID, CreatedBy: userID, Title: "Execution session", Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: targetID},
		&persistence.AgentTurn{ID: turnID, TenantID: tenantID, SessionID: sessionID, CreatedBy: userID, Status: "queued", InputText: "Run integration test"},
		&persistence.AgentExecution{ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "queued", ExecutionTargetID: targetID, TargetKind: "kubernetes", Generation: 0, RequestedBy: userID, QueuedAt: now},
		&persistence.OutboxMessage{ID: uuid.New(), TenantID: &tenantID, Topic: "execution.queued", MessageKey: executionID.String(), Payload: map[string]any{"executionId": executionID}, Headers: map[string]any{"eventVersion": 1}, CreatedAt: now, AvailableAt: now},
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed execution fixture: %v", err)
	}
	t.Cleanup(func() {
		cleanupFixture(db, tenantID)
	})
	return executionFixture{UserID: userID, TenantID: tenantID, SessionID: sessionID, ExecutionID: executionID, TargetID: targetID, TargetKind: "kubernetes"}
}

func registerTestWorker(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	targetKind string,
	podName string,
) persistence.WorkerInstance {
	t.Helper()
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: targetID, TargetKind: targetKind,
		ClusterID: "test-cluster", Namespace: "default", PodName: podName + "-" + uuid.NewString(),
		Version: "test", Capabilities: map[string]any{"codex": true}, LeaseSupported: true, FencingSupported: true,
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

func cleanupWorkers(t *testing.T, db *gorm.DB, workerIDs ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if len(workerIDs) == 0 {
			return
		}
		_ = db.Where("worker_id IN ?", workerIDs).Delete(&persistence.WorkerRequestReceipt{}).Error
		_ = db.Where("worker_id IN ?", workerIDs).Delete(&persistence.WorkerLease{}).Error
		_ = db.Model(&persistence.AgentExecution{}).Where("worker_id IN ?", workerIDs).
			Updates(map[string]any{"status": "recovering", "worker_id": nil}).Error
		_ = db.Where("id IN ?", workerIDs).Delete(&persistence.WorkerInstance{}).Error
	})
}

func cleanupFixture(db *gorm.DB, tenantID uuid.UUID) {
	_ = db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.WorkerLease{}, &persistence.SessionEvent{}, &persistence.OutboxMessage{},
			&persistence.APIIdempotencyKey{}, &persistence.ExecutionInteraction{},
			&persistence.TenantQuota{}, &persistence.AgentExecution{}, &persistence.AgentTurn{},
			&persistence.AgentSession{}, &persistence.Project{}, &persistence.ExecutionTarget{},
		}
		for _, model := range models {
			if err := tx.Where("tenant_id = ?", tenantID).Delete(model).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("ALTER TABLE audit_logs DISABLE TRIGGER trg_audit_logs_append_only").Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM audit_logs WHERE tenant_id = ?", tenantID).Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE audit_logs ENABLE TRIGGER trg_audit_logs_append_only").Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE tenants SET status = 'deleting', deleted_at = now() WHERE id = ?", tenantID).Error; err != nil {
			return err
		}
		return nil
	})
}

func pointer(value string) *string { return &value }

func testPlatformConfig() platform.Config {
	config, _ := platform.Defaults(platform.ProfileSingleNode)
	return config
}
