package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
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
				item.result.Value.Workload.Provider != "codex" || item.result.Value.Workload.DefaultBranch != "main" ||
				item.result.Value.Workload.GitCredentialID == nil ||
				*item.result.Value.Workload.GitCredentialID != fixture.GitCredentialID ||
				item.result.Value.Workload.RuntimeMode != "approval-required" ||
				item.result.Value.Workload.InteractionMode != "plan" {
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

func TestWorkerProtocolV1CannotClaimButV2Can(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	legacyWorker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-protocol-v1")
	currentWorker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-protocol-v2")
	cleanupWorkers(t, db, legacyWorker.ID, currentWorker.ID)
	if err := db.Model(&persistence.WorkerInstance{}).
		Where("id = ? AND incarnation = ?", legacyWorker.ID, legacyWorker.Incarnation).
		Update("protocol_version", 1).Error; err != nil {
		t.Fatal(err)
	}
	legacyWorker.ProtocolVersion = 1
	claimInput := ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}
	_, err := service.Claim(context.Background(), legacyWorker, claimInput, "claim-worker-protocol-v1")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != 426 || apiError.Code != "worker_protocol_version_unsupported" ||
		apiError.Details["received"] != 1 || apiError.Details["minimumSupported"] != WorkerProtocolVersion ||
		apiError.Details["maximumSupported"] != WorkerProtocolVersion {
		t.Fatalf("legacy Worker claim returned unexpected error: %#v", apiError)
	}
	var queued persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued.Status != "queued" || queued.Generation != 0 || queued.WorkerID != nil {
		t.Fatalf("legacy Worker Protocol claim changed the queued Execution: %#v", queued)
	}
	claimed, err := service.Claim(context.Background(), currentWorker, claimInput, "claim-worker-protocol-v2")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Value.Execution == nil || claimed.Value.Execution.ID != fixture.ExecutionID ||
		claimed.Value.Lease == nil || claimed.Value.Lease.WorkerID != currentWorker.ID ||
		claimed.Value.Workload == nil {
		t.Fatalf("Worker Protocol v2 did not claim the Execution: %#v", claimed.Value)
	}
}

func TestReregisteredWorkerFencesAuthenticatedHeartbeatAndLeaseRequests(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	current := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return current }
	initialCapabilities := workerManifestTestCapabilities()
	initialCapabilities["workerGeneration"] = "old"
	registration := RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		InstanceUID:       uuid.NewString(),
		ClusterID:         "test-cluster",
		Namespace:         "default",
		PodName:           "worker-incarnation-" + uuid.NewString(),
		Version:           "current-v1",
		ProtocolVersion:   WorkerProtocolVersion,
		Capabilities:      initialCapabilities,
		LeaseSupported:    true,
		FencingSupported:  true,
	}
	registered, err := service.Register(context.Background(), registration)
	if err != nil {
		t.Fatal(err)
	}
	staleWorker, err := service.Authenticate(context.Background(), registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	cleanupWorkers(t, db, staleWorker.ID)

	claim, err := service.Claim(context.Background(), staleWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
	}, "claim-before-worker-reregistration")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("worker did not receive an execution lease")
	}
	leaseInput := LeaseInput{
		TenantID:   fixture.TenantID,
		Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}

	current = current.Add(time.Second)
	registration.InstanceUID = uuid.NewString()
	registration.Version = "current-v2"
	currentCapabilities := workerManifestTestCapabilities()
	currentCapabilities["workerGeneration"] = "current"
	registration.Capabilities = currentCapabilities
	reregistered, err := service.Register(context.Background(), registration)
	if err != nil {
		t.Fatal(err)
	}
	currentWorker, err := service.Authenticate(context.Background(), reregistered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if currentWorker.ID != staleWorker.ID || currentWorker.Incarnation != staleWorker.Incarnation+1 ||
		currentWorker.InstanceUID != registration.InstanceUID {
		t.Fatalf("worker re-registration did not replace the authenticated incarnation: %#v", currentWorker)
	}

	var leaseBefore persistence.WorkerLease
	if err := db.Where("execution_id = ?", fixture.ExecutionID).Take(&leaseBefore).Error; err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Second)
	draining := true
	staleCapabilities := workerManifestTestCapabilities()
	staleCapabilities["workerGeneration"] = "stale"
	_, err = service.Heartbeat(context.Background(), staleWorker, HeartbeatInput{
		Version:         "stale-version",
		ProtocolVersion: WorkerProtocolVersion,
		Capabilities:    staleCapabilities,
		Draining:        &draining,
	})
	assertWorkerIncarnationFenced := func(operation string, err error) {
		t.Helper()
		var apiError *problem.Error
		if !errors.As(err, &apiError) || apiError.Code != "worker_incarnation_fenced" {
			t.Fatalf("%s returned %v, want worker_incarnation_fenced", operation, err)
		}
	}
	assertWorkerIncarnationFenced("stale heartbeat", err)

	leaseRequests := []struct {
		name string
		run  func() error
	}{
		{
			name: "pull control commands",
			run: func() error {
				_, err := service.PullControlCommands(context.Background(), staleWorker, fixture.ExecutionID, PullControlCommandsInput{
					LeaseInput: leaseInput,
				})
				return err
			},
		},
		{
			name: "pull interaction resolutions",
			run: func() error {
				_, err := service.PullInteractionResolutions(context.Background(), staleWorker, fixture.ExecutionID, PullInteractionResolutionsInput{
					LeaseInput: leaseInput,
				})
				return err
			},
		},
		{
			name: "authorize lease",
			run: func() error {
				return db.Transaction(func(tx *gorm.DB) error {
					_, err := service.AuthorizeLease(context.Background(), tx, staleWorker, fixture.ExecutionID, leaseInput)
					return err
				})
			},
		},
		{
			name: "authorize artifact write",
			run: func() error {
				return db.Transaction(func(tx *gorm.DB) error {
					_, err := service.AuthorizeArtifactWrite(context.Background(), tx, staleWorker, fixture.ExecutionID, leaseInput)
					return err
				})
			},
		},
	}
	for _, request := range leaseRequests {
		assertWorkerIncarnationFenced(request.name, request.run())
	}
	_, err = service.Renew(context.Background(), staleWorker, fixture.ExecutionID, RenewLeaseInput{
		LeaseInput: leaseInput,
	}, "renew-stale-worker-incarnation")
	assertWorkerIncarnationFenced("renew lease", err)

	var workerAfter persistence.WorkerInstance
	if err := db.Where("id = ?", currentWorker.ID).Take(&workerAfter).Error; err != nil {
		t.Fatal(err)
	}
	if workerAfter.Incarnation != currentWorker.Incarnation || workerAfter.InstanceUID != currentWorker.InstanceUID ||
		workerAfter.Status != "online" || workerAfter.Version != registration.Version || workerAfter.DrainingAt != nil ||
		!workerAfter.LastHeartbeatAt.Equal(currentWorker.LastHeartbeatAt) ||
		workerAfter.Capabilities["workerGeneration"] != "current" {
		t.Fatalf("stale Worker request overwrote the current incarnation: before=%#v after=%#v", currentWorker, workerAfter)
	}
	var leaseAfter persistence.WorkerLease
	if err := db.Where("execution_id = ?", fixture.ExecutionID).Take(&leaseAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !leaseAfter.HeartbeatAt.Equal(leaseBefore.HeartbeatAt) || !leaseAfter.ExpiresAt.Equal(leaseBefore.ExpiresAt) ||
		!bytes.Equal(leaseAfter.LeaseTokenHash, leaseBefore.LeaseTokenHash) {
		t.Fatalf("stale Worker request changed the execution lease: before=%#v after=%#v", leaseBefore, leaseAfter)
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

func TestDurableInterruptCommandTerminatesTurn(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}

	first, err := service.RequestInterrupt(
		context.Background(), principal, fixture.SessionID,
		"interrupt-key", "interrupt-request", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.RequestInterrupt(
		context.Background(), principal, fixture.SessionID,
		"interrupt-key", "interrupt-replay", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || !replayed.Replayed || first.Value.ID != replayed.Value.ID || first.StatusCode != 202 {
		t.Fatalf("unexpected interrupt idempotency result: first=%#v replayed=%#v", first, replayed)
	}
	if first.Value.DeliveryWorkerID != nil || first.Value.DeliveryGeneration != nil {
		t.Fatalf("queued interrupt was bound before claim: %#v", first.Value)
	}

	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-interrupt")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "interrupt-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("interrupt test execution was not leased")
	}
	lease := *claim.Value.Lease
	leaseInput := LeaseInput{TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken}
	deliveries, err := service.PullControlCommands(context.Background(), worker, fixture.ExecutionID, PullControlCommandsInput{
		LeaseInput: leaseInput, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].CommandType != "InterruptTurn" || deliveries[0].CommandID != first.Value.CommandID {
		t.Fatalf("unexpected interrupt delivery: %#v", deliveries)
	}
	deliveryInput := ControlCommandDeliveryInput{LeaseInput: leaseInput, CommandID: deliveries[0].CommandID}
	if _, err := service.MarkControlCommandDelivered(
		context.Background(), worker, fixture.ExecutionID, first.Value.ID, deliveryInput, "interrupt-delivered",
	); err != nil {
		t.Fatal(err)
	}
	cursor := "provider-cursor-after-interrupt"
	deliveryInput.ProviderResumeCursor = &cursor
	acknowledged, err := service.AcknowledgeControlCommand(
		context.Background(), worker, fixture.ExecutionID, first.Value.ID, deliveryInput, "interrupt-acknowledged",
	)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.Value.Status != "acknowledged" || acknowledged.Value.AcknowledgedAt == nil {
		t.Fatalf("interrupt command was not acknowledged: %#v", acknowledged.Value)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "interrupted" || execution.FinishedAt == nil {
		t.Fatalf("execution was not interrupted: %#v", execution)
	}
	var turn persistence.AgentTurn
	if err := db.Where("tenant_id = ? AND session_id = ? AND id = ?", fixture.TenantID, fixture.SessionID, execution.TurnID).Take(&turn).Error; err != nil {
		t.Fatal(err)
	}
	if turn.Status != "interrupted" || turn.CompletedAt == nil {
		t.Fatalf("turn was not interrupted: %#v", turn)
	}
	var leases int64
	if err := db.Model(&persistence.WorkerLease{}).Where("execution_id = ?", fixture.ExecutionID).Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("interrupted execution retained %d leases", leases)
	}
	var requestedEvents, interruptedEvents int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.ExecutionID, "turn.interrupt-requested").
		Count(&requestedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.ExecutionID, "execution.interrupted").
		Count(&interruptedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if requestedEvents != 1 || interruptedEvents != 1 {
		t.Fatalf("unexpected interrupt event counts: requested=%d interrupted=%d", requestedEvents, interruptedEvents)
	}
}

func TestInterruptedTurnContinuesOnReplacementWorkerWithCursorHistoryAndSequence(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ? AND message_key = ?", fixture.TenantID, fixture.ExecutionID.String()).
			Delete(&persistence.OutboxMessage{}).Error; err != nil {
			return err
		}
		if err := tx.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Delete(&persistence.AgentExecution{}).Error; err != nil {
			return err
		}
		return tx.Where("tenant_id = ? AND session_id = ? AND id = ?", fixture.TenantID, fixture.SessionID, fixture.TurnID).
			Delete(&persistence.AgentTurn{}).Error
	}); err != nil {
		t.Fatal(err)
	}

	firstTurn, err := service.sessions.CreateTurn(
		ctx, principal, fixture.SessionID,
		sessions.CreateTurnInput{InputText: "Investigate the failing integration test"},
		"continuity-first-turn", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	var firstExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND turn_id = ?", fixture.TenantID, firstTurn.ID).
		Take(&firstExecution).Error; err != nil {
		t.Fatal(err)
	}
	firstWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-continuity-first")
	secondWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-continuity-second")
	cleanupWorkers(t, db, firstWorker.ID, secondWorker.ID)
	firstClaim, err := service.Claim(ctx, firstWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &firstExecution.ID,
	}, "continuity-first-claim")
	if err != nil || firstClaim.Value.Lease == nil {
		t.Fatalf("first claim failed: result=%#v err=%v", firstClaim, err)
	}
	firstLease := *firstClaim.Value.Lease
	firstLeaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
	}
	if _, err := service.Start(ctx, firstWorker, firstExecution.ID, firstLeaseInput, "continuity-first-start"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AppendRuntimeEvent(ctx, firstWorker, firstExecution.ID, RuntimeEventInput{
		LeaseInput: firstLeaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV1,
		EventType: "runtime.output.delta", Payload: map[string]any{
			"turnId": firstTurn.ID.String(), "text": "The first attempt found a race.",
		},
	}, "continuity-first-output"); err != nil {
		t.Fatal(err)
	}
	interrupt, err := service.RequestInterrupt(
		ctx, principal, fixture.SessionID,
		"continuity-interrupt", "continuity-interrupt", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := service.PullControlCommands(ctx, firstWorker, firstExecution.ID, PullControlCommandsInput{
		LeaseInput: firstLeaseInput, Limit: 1,
	})
	if err != nil || len(deliveries) != 1 || deliveries[0].ControlCommandID != interrupt.Value.ID {
		t.Fatalf("interrupt delivery failed: deliveries=%#v err=%v", deliveries, err)
	}
	deliveryInput := ControlCommandDeliveryInput{
		LeaseInput: firstLeaseInput, CommandID: interrupt.Value.CommandID,
	}
	if _, err := service.MarkControlCommandDelivered(
		ctx, firstWorker, firstExecution.ID, interrupt.Value.ID, deliveryInput, "continuity-interrupt-delivered",
	); err != nil {
		t.Fatal(err)
	}
	cursor := "cursor-after-interrupted-first-turn"
	deliveryInput.ProviderResumeCursor = &cursor
	if _, err := service.AcknowledgeControlCommand(
		ctx, firstWorker, firstExecution.ID, interrupt.Value.ID, deliveryInput, "continuity-interrupt-acknowledged",
	); err != nil {
		t.Fatal(err)
	}

	secondTurn, err := service.sessions.CreateTurn(
		ctx, principal, fixture.SessionID,
		sessions.CreateTurnInput{InputText: "Continue from the race diagnosis"},
		"continuity-second-turn", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	var secondExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND turn_id = ?", fixture.TenantID, secondTurn.ID).
		Take(&secondExecution).Error; err != nil {
		t.Fatal(err)
	}
	if secondExecution.ID == firstExecution.ID || secondTurn.ID == firstTurn.ID {
		t.Fatal("later Turn did not create a distinct Execution boundary")
	}
	secondClaim, err := service.Claim(ctx, secondWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &secondExecution.ID,
	}, "continuity-second-claim")
	if err != nil || secondClaim.Value.Lease == nil || secondClaim.Value.Workload == nil {
		t.Fatalf("replacement claim failed: result=%#v err=%v", secondClaim, err)
	}
	if secondClaim.Value.ProviderResumeCursor == nil || *secondClaim.Value.ProviderResumeCursor != cursor {
		t.Fatalf("replacement claim omitted the persisted Cursor: %#v", secondClaim.Value.ProviderResumeCursor)
	}
	expectedHistory := []ConversationMessage{
		{Role: "user", Text: "Investigate the failing integration test"},
		{Role: "assistant", Text: "The first attempt found a race."},
	}
	if !reflect.DeepEqual(secondClaim.Value.Workload.ConversationHistory, expectedHistory) {
		t.Fatalf("replacement claim received unexpected authoritative history: %#v", secondClaim.Value.Workload.ConversationHistory)
	}

	var events []persistence.SessionEvent
	if err := db.Where("tenant_id = ? AND session_id = ?", fixture.TenantID, fixture.SessionID).
		Order("sequence").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	expectedTypes := []string{
		"turn.created", "execution.leased", "execution.started", "runtime.output.delta",
		"turn.interrupt-requested", "execution.interrupted", "turn.created", "execution.leased",
	}
	if len(events) != len(expectedTypes) {
		t.Fatalf("unexpected continuity Event count: got=%d events=%#v", len(events), events)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) || event.EventType != expectedTypes[index] {
			t.Fatalf("Session Event continuity broke at index %d: %#v", index, event)
		}
	}
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.LastEventSequence != int64(len(events)) {
		t.Fatalf("Session sequence cursor is not continuous: got=%d want=%d", session.LastEventSequence, len(events))
	}
}

func TestWorkspacePreparationIsGenerationFencedAcrossWorkerRecovery(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	repositoryURL := "https://git.example.com/team/repository.git"
	if err := db.Model(&persistence.Project{}).Where("tenant_id = ? AND id = ?", fixture.TenantID, session.ProjectID).
		Update("repository_url", repositoryURL).Error; err != nil {
		t.Fatal(err)
	}
	workspaceID := uuid.New()
	workspace := persistence.RemoteWorkspace{
		ID: workspaceID, TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: fixture.SessionID, ExecutionTargetID: fixture.TargetID,
		WorkspaceMode: "clone", State: "pending", DefaultBranch: "main",
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&workspace).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Update("remote_workspace_id", workspaceID).Error
	}); err != nil {
		t.Fatal(err)
	}
	first := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-workspace-first")
	second := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-workspace-second")
	cleanupWorkers(t, db, first.ID, second.ID)
	firstClaim, err := service.Claim(ctx, first, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "workspace-first-claim")
	if err != nil || firstClaim.Value.Lease == nil || firstClaim.Value.Workload == nil ||
		firstClaim.Value.Workload.RemoteWorkspaceID == nil || *firstClaim.Value.Workload.RemoteWorkspaceID != workspaceID {
		t.Fatalf("first Workspace claim failed: result=%#v err=%v", firstClaim, err)
	}
	if firstClaim.Value.Workload.WorkspaceMaterializationID == nil ||
		firstClaim.Value.Workload.WorkspaceMaterializationIncarnationID == nil ||
		firstClaim.Value.Workload.WorkspaceLayoutVersion != workspaceLayoutVersionCurrent {
		t.Fatalf("first Workspace claim omitted materialization fencing: %#v", firstClaim.Value.Workload)
	}
	firstLease := *firstClaim.Value.Lease
	firstLeaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
	}
	fingerprint := strings.Repeat("a", 64)
	branch := "synara/session-test"
	baseCommit, headCommit := strings.Repeat("b", 40), strings.Repeat("c", 40)
	ready, err := service.MarkWorkspaceReady(ctx, first, fixture.ExecutionID, WorkspaceReadyInput{
		LeaseInput: firstLeaseInput, RepositoryFingerprint: &fingerprint,
		CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &headCommit,
	}, "workspace-ready")
	if err != nil || ready.Value.State != "ready" {
		t.Fatalf("Workspace ready transition failed: result=%#v err=%v", ready, err)
	}
	replayed, err := service.MarkWorkspaceReady(ctx, first, fixture.ExecutionID, WorkspaceReadyInput{
		LeaseInput: firstLeaseInput, RepositoryFingerprint: &fingerprint,
		CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &headCommit,
	}, "workspace-ready")
	if err != nil || !replayed.Replayed {
		t.Fatalf("Workspace ready transition was not idempotent: result=%#v err=%v", replayed, err)
	}
	dirtyHead := strings.Repeat("d", 40)
	dirty, err := service.MarkWorkspaceDirty(ctx, first, fixture.ExecutionID, WorkspaceDirtyInput{
		LeaseInput: firstLeaseInput, CurrentBranch: &branch, HeadCommit: &dirtyHead,
	}, "workspace-dirty")
	if err != nil || dirty.Value.State != "dirty" || dirty.Value.HeadCommit == nil || *dirty.Value.HeadCommit != dirtyHead {
		t.Fatalf("Workspace dirty transition failed: result=%#v err=%v", dirty, err)
	}
	dirtyReplay, err := service.MarkWorkspaceDirty(ctx, first, fixture.ExecutionID, WorkspaceDirtyInput{
		LeaseInput: firstLeaseInput, CurrentBranch: &branch, HeadCommit: &dirtyHead,
	}, "workspace-dirty")
	if err != nil || !dirtyReplay.Replayed {
		t.Fatalf("Workspace dirty transition was not idempotent: result=%#v err=%v", dirtyReplay, err)
	}
	checkpointReadyAt := time.Now().UTC()
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: fixture.TenantID, WorkspaceID: workspaceID,
		SessionID: fixture.SessionID, TurnID: &fixture.TurnID, ExecutionID: fixture.ExecutionID,
		Generation: firstLease.Generation, IdempotencyKey: "workspace-recovery-fence", Strategy: "git-reference",
		Status: "ready", BaseCommit: &baseCommit, HeadCommit: &dirtyHead, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-git-reference-v1", "headCommit": dirtyHead},
		CreatedAt: checkpointReadyAt, ReadyAt: &checkpointReadyAt,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&checkpoint).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, workspaceID).
			Update("current_checkpoint_id", checkpoint.ID).Error
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(ctx, first, fixture.ExecutionID, ReleaseLeaseInput{
		LeaseInput: firstLeaseInput, Reason: "replace Worker after Workspace preparation",
	}, "workspace-release"); err != nil {
		t.Fatal(err)
	}
	secondClaim, err := service.Claim(ctx, second, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "workspace-second-claim")
	if err != nil || secondClaim.Value.Lease == nil || secondClaim.Value.Workload == nil ||
		secondClaim.Value.Workload.WorkspaceRepositoryFingerprint == nil ||
		*secondClaim.Value.Workload.WorkspaceRepositoryFingerprint != fingerprint {
		t.Fatalf("replacement Workspace claim failed: result=%#v err=%v", secondClaim, err)
	}
	if _, err := service.MarkWorkspaceReady(ctx, first, fixture.ExecutionID, WorkspaceReadyInput{
		LeaseInput: firstLeaseInput,
	}, "workspace-stale-ready"); err == nil {
		t.Fatal("obsolete Worker Generation updated Workspace state")
	}
	if _, err := service.MarkWorkspaceDirty(ctx, first, fixture.ExecutionID, WorkspaceDirtyInput{
		LeaseInput: firstLeaseInput, CurrentBranch: &branch, HeadCommit: &dirtyHead,
	}, "workspace-stale-dirty"); err == nil {
		t.Fatal("obsolete Worker Generation updated dirty Workspace state")
	}
	secondLease := *secondClaim.Value.Lease
	failed, err := service.MarkWorkspaceFailed(ctx, second, fixture.ExecutionID, WorkspaceFailedInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: secondLease.Generation, LeaseToken: secondLease.LeaseToken,
		},
		FailureCode: "workspace_invalid", FailureMessage: "Repository policy rejected the remote",
	}, "workspace-failed")
	if err != nil || failed.Value.State != "failed" {
		t.Fatalf("replacement Workspace failure transition failed: result=%#v err=%v", failed, err)
	}
	var persisted persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspaceID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.State != "failed" || persisted.LastWorkerID == nil || *persisted.LastWorkerID != second.ID ||
		persisted.LastGeneration == nil || *persisted.LastGeneration != secondLease.Generation {
		t.Fatalf("Workspace generation binding is incorrect: %#v", persisted)
	}
	var readyEvents, dirtyEvents, failedEvents int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.TenantID, fixture.SessionID, "workspace.ready").
		Count(&readyEvents).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.TenantID, fixture.SessionID, "workspace.dirty").
		Count(&dirtyEvents).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.TenantID, fixture.SessionID, "workspace.failed").
		Count(&failedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if readyEvents != 1 || dirtyEvents != 1 || failedEvents != 1 {
		t.Fatalf("Workspace lifecycle events are not idempotent: ready=%d dirty=%d failed=%d", readyEvents, dirtyEvents, failedEvents)
	}
}

func TestWorkspaceCheckpointLifecyclePreservesLastReadyRecoveryPoint(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	workspace := persistence.RemoteWorkspace{
		ID: uuid.New(), TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: fixture.SessionID, ExecutionTargetID: fixture.TargetID,
		WorkspaceMode: "clone", State: "pending", DefaultBranch: "main",
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&workspace).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Update("remote_workspace_id", workspace.ID).Error
	}); err != nil {
		t.Fatal(err)
	}
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-checkpoint")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "checkpoint-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("Checkpoint claim failed: result=%#v err=%v", claim, err)
	}
	lease := *claim.Value.Lease
	leaseInput := LeaseInput{TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken}
	fingerprint := strings.Repeat("a", 64)
	branch := "synara/session-checkpoint"
	baseCommit, headCommit := strings.Repeat("b", 40), strings.Repeat("c", 40)
	if _, err := service.MarkWorkspaceReady(ctx, worker, fixture.ExecutionID, WorkspaceReadyInput{
		LeaseInput: leaseInput, RepositoryFingerprint: &fingerprint, CurrentBranch: &branch,
		BaseCommit: &baseCommit, HeadCommit: &headCommit,
	}, "checkpoint-workspace-ready"); err != nil {
		t.Fatal(err)
	}
	gitReferenceInput := CreateWorkspaceCheckpointInput{
		LeaseInput: leaseInput, IdempotencyKey: "turn-terminal-git-reference", Strategy: "git-reference",
		BaseCommit: &baseCommit, HeadCommit: &headCommit, CurrentBranch: &branch,
		Manifest: map[string]any{"format": "synara-git-reference-v1", "headCommit": headCommit},
	}
	created, err := service.CreateWorkspaceCheckpoint(ctx, worker, fixture.ExecutionID, gitReferenceInput, "checkpoint-create-git")
	if err != nil || created.Value.Status != "pending" {
		t.Fatalf("Git-reference Checkpoint create failed: result=%#v err=%v", created, err)
	}
	replayed, err := service.CreateWorkspaceCheckpoint(ctx, worker, fixture.ExecutionID, gitReferenceInput, "checkpoint-create-git-replay")
	if err != nil || replayed.Value.ID != created.Value.ID {
		t.Fatalf("Git-reference domain idempotency failed: result=%#v err=%v", replayed, err)
	}
	conflictingHead := strings.Repeat("d", 40)
	conflictInput := gitReferenceInput
	conflictInput.HeadCommit = &conflictingHead
	if _, err := service.CreateWorkspaceCheckpoint(ctx, worker, fixture.ExecutionID, conflictInput, "checkpoint-create-git-conflict"); err == nil {
		t.Fatal("Checkpoint idempotency key accepted different content")
	}
	gitReady, err := service.MarkWorkspaceCheckpointReady(ctx, worker, fixture.ExecutionID, created.Value.ID, WorkspaceCheckpointReadyInput{
		LeaseInput: leaseInput,
	}, "checkpoint-ready-git")
	if err != nil || gitReady.Value.Status != "ready" || gitReady.Value.ArtifactID != nil {
		t.Fatalf("Git-reference Checkpoint ready failed: result=%#v err=%v", gitReady, err)
	}

	dirtyHead := strings.Repeat("e", 40)
	if _, err := service.MarkWorkspaceDirty(ctx, worker, fixture.ExecutionID, WorkspaceDirtyInput{
		LeaseInput: leaseInput, CurrentBranch: &branch, HeadCommit: &dirtyHead,
	}, "checkpoint-workspace-dirty"); err != nil {
		t.Fatal(err)
	}
	fileCount, totalBytes := 2, int64(128)
	snapshotInput := CreateWorkspaceCheckpointInput{
		LeaseInput: leaseInput, IdempotencyKey: "turn-terminal-snapshot", Strategy: "snapshot",
		BaseCommit: &baseCommit, HeadCommit: &dirtyHead, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-workspace-snapshot-v1", "files": []any{}},
		FileCount: &fileCount, TotalBytes: &totalBytes,
	}
	snapshot, err := service.CreateWorkspaceCheckpoint(ctx, worker, fixture.ExecutionID, snapshotInput, "checkpoint-create-snapshot")
	if err != nil || snapshot.Value.Status != "pending" {
		t.Fatalf("Snapshot Checkpoint create failed: result=%#v err=%v", snapshot, err)
	}
	secondInput := snapshotInput
	secondInput.IdempotencyKey = "turn-terminal-snapshot-second"
	if _, err := service.CreateWorkspaceCheckpoint(ctx, worker, fixture.ExecutionID, secondInput, "checkpoint-create-snapshot-second"); err == nil {
		t.Fatal("Workspace accepted more than one active Checkpoint")
	}
	now := time.Now().UTC()
	sha := strings.Repeat("f", 64)
	contentType := "application/x-tar"
	size := int64(256)
	artifact := persistence.Artifact{
		ID: uuid.New(), TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: fixture.SessionID, ExecutionID: &fixture.ExecutionID,
		WorkspaceCheckpointID: &snapshot.Value.ID,
		Kind:                  "workspace_snapshot", Status: "ready", Bucket: "test", ObjectKey: "checkpoint/" + uuid.NewString(),
		ContentType: &contentType, SizeBytes: &size, SHA256: &sha,
		CreatedByType: "worker", CreatedByID: worker.ID, ReadyAt: &now, CreatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&artifact).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, snapshot.Value.ID).
			Updates(map[string]any{"status": "uploading", "artifact_id": artifact.ID}).Error
	}); err != nil {
		t.Fatal(err)
	}
	snapshotReady, err := service.MarkWorkspaceCheckpointReady(ctx, worker, fixture.ExecutionID, snapshot.Value.ID, WorkspaceCheckpointReadyInput{
		LeaseInput: leaseInput, ArtifactID: &artifact.ID, SHA256: &sha,
	}, "checkpoint-ready-snapshot")
	if err != nil || snapshotReady.Value.Status != "ready" || snapshotReady.Value.ArtifactID == nil ||
		*snapshotReady.Value.ArtifactID != artifact.ID {
		t.Fatalf("Snapshot Checkpoint ready failed: result=%#v err=%v", snapshotReady, err)
	}
	var persistedWorkspace persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).Take(&persistedWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	if persistedWorkspace.State != "dirty" || persistedWorkspace.CurrentCheckpointID == nil ||
		*persistedWorkspace.CurrentCheckpointID != snapshot.Value.ID {
		t.Fatalf("Snapshot did not become the last ready recovery point: %#v", persistedWorkspace)
	}
	var persistedExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&persistedExecution).Error; err != nil {
		t.Fatal(err)
	}
	if persistedExecution.RestoreCheckpointID == nil || *persistedExecution.RestoreCheckpointID != snapshot.Value.ID {
		t.Fatalf("Execution did not bind the last ready Checkpoint: %#v", persistedExecution.RestoreCheckpointID)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&persistence.Artifact{}).Where("tenant_id = ? AND id = ?", fixture.TenantID, artifact.ID).
			Update("status", "deleting").Error
	}); err == nil {
		t.Fatal("Ready Checkpoint Artifact could be invalidated")
	}
	patchFileCount, patchTotalBytes := 3, int64(96)
	patchInput := CreateWorkspaceCheckpointInput{
		LeaseInput: leaseInput, IdempotencyKey: "turn-terminal-patch", Strategy: "patch",
		BaseCommit: &baseCommit, HeadCommit: &dirtyHead, CurrentBranch: &branch,
		Manifest: map[string]any{
			"format": "synara-workspace-patch-v1", "baseCommit": baseCommit,
			"currentBranch": branch, "trackedPatch": map[string]any{"path": "tracked.patch", "sizeBytes": 32, "sha256": strings.Repeat("1", 64)},
		},
		FileCount: &patchFileCount, TotalBytes: &patchTotalBytes,
	}
	patch, err := service.CreateWorkspaceCheckpoint(ctx, worker, fixture.ExecutionID, patchInput, "checkpoint-create-patch")
	if err != nil || patch.Value.Status != "pending" {
		t.Fatalf("Patch Checkpoint create failed: result=%#v err=%v", patch, err)
	}
	patchSHA := strings.Repeat("1", 64)
	patchArtifact := persistence.Artifact{
		ID: uuid.New(), TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: fixture.SessionID, ExecutionID: &fixture.ExecutionID,
		WorkspaceCheckpointID: &patch.Value.ID,
		Kind:                  "checkpoint", Status: "ready", Bucket: "test", ObjectKey: "checkpoint/" + uuid.NewString(),
		ContentType: &contentType, SizeBytes: &size, SHA256: &patchSHA,
		CreatedByType: "worker", CreatedByID: worker.ID, ReadyAt: &now, CreatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&patchArtifact).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, patch.Value.ID).
			Updates(map[string]any{"status": "uploading", "artifact_id": patchArtifact.ID}).Error
	}); err != nil {
		t.Fatal(err)
	}
	patchReady, err := service.MarkWorkspaceCheckpointReady(ctx, worker, fixture.ExecutionID, patch.Value.ID, WorkspaceCheckpointReadyInput{
		LeaseInput: leaseInput, ArtifactID: &patchArtifact.ID, SHA256: &patchSHA,
	}, "checkpoint-ready-patch")
	if err != nil || patchReady.Value.Status != "ready" || patchReady.Value.ArtifactID == nil ||
		*patchReady.Value.ArtifactID != patchArtifact.ID {
		t.Fatalf("Patch Checkpoint ready failed: result=%#v err=%v", patchReady, err)
	}
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).Take(&persistedWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	if persistedWorkspace.CurrentCheckpointID == nil || *persistedWorkspace.CurrentCheckpointID != patch.Value.ID {
		t.Fatalf("Patch did not replace the current recovery point: %#v", persistedWorkspace.CurrentCheckpointID)
	}
	failedInput := patchInput
	failedInput.IdempotencyKey = "turn-terminal-patch-failed"
	failedCheckpoint, err := service.CreateWorkspaceCheckpoint(ctx, worker, fixture.ExecutionID, failedInput, "checkpoint-create-failed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkWorkspaceCheckpointFailed(ctx, worker, fixture.ExecutionID, failedCheckpoint.Value.ID, WorkspaceCheckpointFailedInput{
		LeaseInput: leaseInput, FailureCode: "checkpoint_persist_failed", FailureMessage: "object storage unavailable",
	}, "checkpoint-failed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).Take(&persistedWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	if persistedWorkspace.CurrentCheckpointID == nil || *persistedWorkspace.CurrentCheckpointID != patch.Value.ID {
		t.Fatalf("Failed Checkpoint replaced the last ready recovery point: %#v", persistedWorkspace.CurrentCheckpointID)
	}
	if _, err := service.Complete(ctx, worker, fixture.ExecutionID, CompleteExecutionInput{
		LeaseInput: leaseInput, Output: map[string]any{"status": "checkpointed"},
	}, "checkpoint-complete"); err != nil {
		t.Fatalf("Checkpointed dirty Workspace could not complete: %v", err)
	}
	abandonedID := uuid.New()
	abandonedCreatedAt := time.Now().UTC()
	abandonedExpiresAt := abandonedCreatedAt.Add(time.Minute)
	if err := db.Create(&persistence.WorkspaceCheckpoint{
		ID: abandonedID, TenantID: fixture.TenantID, WorkspaceID: workspace.ID,
		SessionID: fixture.SessionID, TurnID: &fixture.TurnID, ExecutionID: fixture.ExecutionID,
		Generation: lease.Generation, IdempotencyKey: "abandoned-after-complete", Strategy: "snapshot",
		Status: "pending", BaseCommit: &baseCommit, HeadCommit: &dirtyHead, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-workspace-snapshot-v1", "files": []any{}},
		FileCount: &fileCount, TotalBytes: &totalBytes, CreatedAt: abandonedCreatedAt, ExpiresAt: &abandonedExpiresAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.RemoteWorkspace{}).Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
		Update("state", "checkpointing").Error; err != nil {
		t.Fatal(err)
	}
	failedAbandoned, err := service.FailAbandonedWorkspaceCheckpoints(ctx, time.Now().UTC(), 10)
	if err != nil || failedAbandoned != 1 {
		t.Fatalf("abandoned Checkpoint cleanup = %d, %v", failedAbandoned, err)
	}
	var abandoned persistence.WorkspaceCheckpoint
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, abandonedID).Take(&abandoned).Error; err != nil {
		t.Fatal(err)
	}
	if abandoned.Status != "failed" || abandoned.FailureCode == nil || *abandoned.FailureCode != "checkpoint_lease_inactive" {
		t.Fatalf("abandoned Checkpoint was not failed: %#v", abandoned)
	}
	retained, err := service.ApplyWorkspaceCheckpointRetention(
		ctx, fixture.TenantID, abandonedCreatedAt.Add(-time.Hour), abandonedExpiresAt.Add(time.Second), 10,
	)
	if err != nil || retained.CheckpointsExpired != 1 {
		t.Fatalf("failed Checkpoint retention = %#v, %v", retained, err)
	}
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, abandonedID).Take(&abandoned).Error; err != nil {
		t.Fatal(err)
	}
	if abandoned.Status != "expired" || abandoned.FailureCode == nil || *abandoned.FailureCode != "checkpoint_lease_inactive" {
		t.Fatalf("expired Checkpoint lost immutable failure evidence: %#v", abandoned)
	}
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).Take(&persistedWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	if persistedWorkspace.State != "dirty" || persistedWorkspace.CurrentCheckpointID == nil ||
		*persistedWorkspace.CurrentCheckpointID != patch.Value.ID {
		t.Fatalf("abandoned Checkpoint damaged the last ready recovery point: %#v", persistedWorkspace)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(db, testPlatformConfig(), cipher)
	sessionService := sessions.NewService(db, projects.NewService(db), targetService)
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	nextTurn, err := sessionService.CreateTurn(ctx, principal, fixture.SessionID, sessions.CreateTurnInput{
		InputText: "Continue from the durable Workspace Checkpoint",
	}, "checkpoint-next-turn", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var nextExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND turn_id = ?", fixture.TenantID, nextTurn.ID).Take(&nextExecution).Error; err != nil {
		t.Fatal(err)
	}
	if nextExecution.RestoreCheckpointID == nil || *nextExecution.RestoreCheckpointID != patch.Value.ID {
		t.Fatalf("new Execution did not freeze the last ready Checkpoint: %#v", nextExecution.RestoreCheckpointID)
	}
	nextClaim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &nextExecution.ID,
	}, "checkpoint-next-claim")
	if err != nil || nextClaim.Value.Workload == nil || nextClaim.Value.Workload.RestoreCheckpoint == nil ||
		nextClaim.Value.Workload.RestoreCheckpoint.ID != patch.Value.ID ||
		nextClaim.Value.Workload.RestoreCheckpoint.ArtifactID == nil {
		t.Fatalf("replacement Workload omitted the frozen Checkpoint: result=%#v err=%v", nextClaim, err)
	}
}

func TestDurableInterruptCommandRebindsAfterWorkerRelease(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	requested, err := service.RequestInterrupt(
		context.Background(), principal, fixture.SessionID,
		"interrupt-rebind", "interrupt-rebind", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}

	firstWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-interrupt-first")
	legacyWorker := registerLegacyTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-interrupt-legacy")
	unsupportedCapabilities := workerManifestTestCapabilities()
	setTestProviderCapability(unsupportedCapabilities, "codex", "interrupt-turn", "unsupported")
	unsupportedWorker := registerTestWorkerWithCapabilities(
		t, service, fixture.TargetID, fixture.TargetKind, "worker-interrupt-unsupported", unsupportedCapabilities,
	)
	secondWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-interrupt-second")
	cleanupWorkers(t, db, firstWorker.ID, legacyWorker.ID, unsupportedWorker.ID, secondWorker.ID)
	firstClaim, err := service.Claim(context.Background(), firstWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "interrupt-first-claim")
	if err != nil || firstClaim.Value.Lease == nil {
		t.Fatalf("first claim failed: result=%#v err=%v", firstClaim, err)
	}
	firstLease := *firstClaim.Value.Lease
	firstLeaseInput := LeaseInput{TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken}
	firstDeliveries, err := service.PullControlCommands(context.Background(), firstWorker, fixture.ExecutionID, PullControlCommandsInput{
		LeaseInput: firstLeaseInput, Limit: 1,
	})
	if err != nil || len(firstDeliveries) != 1 {
		t.Fatalf("first interrupt pull failed: deliveries=%#v err=%v", firstDeliveries, err)
	}
	if _, err := service.MarkControlCommandDelivered(
		context.Background(), firstWorker, fixture.ExecutionID, requested.Value.ID,
		ControlCommandDeliveryInput{LeaseInput: firstLeaseInput, CommandID: requested.Value.CommandID},
		"interrupt-first-delivered",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(
		context.Background(), firstWorker, fixture.ExecutionID,
		ReleaseLeaseInput{LeaseInput: firstLeaseInput, Reason: "worker draining"},
		"interrupt-first-release",
	); err != nil {
		t.Fatal(err)
	}

	for _, attempt := range []struct {
		name      string
		worker    persistence.WorkerInstance
		requestID string
	}{
		{name: "legacy", worker: legacyWorker, requestID: "interrupt-legacy-claim"},
		{name: "unsupported", worker: unsupportedWorker, requestID: "interrupt-unsupported-claim"},
	} {
		t.Run(attempt.name+" replacement is fenced", func(t *testing.T) {
			broadClaim, broadErr := service.Claim(context.Background(), attempt.worker, ClaimExecutionInput{
				ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
			}, attempt.requestID+"-broad")
			if broadErr != nil {
				t.Fatal(broadErr)
			}
			if broadClaim.Value.Execution != nil || broadClaim.Value.Lease != nil {
				t.Fatalf("incompatible replacement claimed filtered work: %#v", broadClaim.Value)
			}
			_, claimErr := service.Claim(context.Background(), attempt.worker, ClaimExecutionInput{
				ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
			}, attempt.requestID)
			var apiError *problem.Error
			expectedCode := "capability_unsupported"
			if attempt.name == "legacy" {
				expectedCode = "worker_manifest_required"
			}
			if !errors.As(claimErr, &apiError) || apiError.Code != expectedCode {
				t.Fatalf("expected %s, got %v", expectedCode, claimErr)
			}
		})
	}
	var fencedExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&fencedExecution).Error; err != nil {
		t.Fatal(err)
	}
	if fencedExecution.Status != "recovering" || fencedExecution.WorkerID != nil || fencedExecution.Generation != firstLease.Generation {
		t.Fatalf("incompatible replacement mutated the Execution: %#v", fencedExecution)
	}
	var fencedCommand persistence.ExecutionControlCommand
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, requested.Value.ID).Take(&fencedCommand).Error; err != nil {
		t.Fatal(err)
	}
	if fencedCommand.Status != "pending" || fencedCommand.DeliveryWorkerID != nil ||
		fencedCommand.DeliveryGeneration != nil || fencedCommand.CommandID != requested.Value.CommandID {
		t.Fatalf("incompatible replacement mutated the pending command: %#v", fencedCommand)
	}

	secondClaim, err := service.Claim(context.Background(), secondWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "interrupt-second-claim")
	if err != nil || secondClaim.Value.Lease == nil {
		t.Fatalf("second claim failed: result=%#v err=%v", secondClaim, err)
	}
	secondLease := *secondClaim.Value.Lease
	if secondLease.Generation != firstLease.Generation+1 {
		t.Fatalf("interrupt command did not advance generation: first=%d second=%d", firstLease.Generation, secondLease.Generation)
	}
	secondDeliveries, err := service.PullControlCommands(context.Background(), secondWorker, fixture.ExecutionID, PullControlCommandsInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: secondLease.Generation, LeaseToken: secondLease.LeaseToken,
		},
		Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondDeliveries) != 1 || secondDeliveries[0].ControlCommandID != requested.Value.ID ||
		secondDeliveries[0].CommandID != requested.Value.CommandID || secondDeliveries[0].DeliveryStatus != "pending" {
		t.Fatalf("interrupt command was not rebound to replacement generation: %#v", secondDeliveries)
	}
}

func TestProviderExecutionRejectsWorkerWithoutManifest(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerLegacyTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-without-manifest")
	cleanupWorkers(t, db, worker.ID)
	broad, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
	}, "worker-without-manifest-broad-claim")
	if err != nil || broad.Value.Execution != nil || broad.Value.Lease != nil {
		t.Fatalf("Worker without a manifest claimed Provider work: result=%#v err=%v", broad, err)
	}
	_, err = service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "worker-without-manifest-explicit-claim")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "worker_manifest_required" {
		t.Fatalf("expected worker_manifest_required, got %v", err)
	}
}

func TestDurableSteerCommandKeepsExecutionRunning(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-steer")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "steer-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("steer claim failed: result=%#v err=%v", claim, err)
	}
	lease := *claim.Value.Lease
	leaseInput := LeaseInput{TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken}
	if _, err := service.Start(context.Background(), worker, fixture.ExecutionID, leaseInput, "steer-start"); err != nil {
		t.Fatal(err)
	}

	requested, err := service.RequestSteer(
		context.Background(), principal, fixture.SessionID,
		SteerActiveTurnInput{InputText: "focus on the failing test"},
		"steer-key", "steer-request", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.RequestSteer(
		context.Background(), principal, fixture.SessionID,
		SteerActiveTurnInput{InputText: "focus on the failing test"},
		"steer-key", "steer-replay", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if requested.Replayed || !replayed.Replayed || requested.Value.ID != replayed.Value.ID ||
		requested.Value.DeliveryWorkerID == nil || *requested.Value.DeliveryWorkerID != worker.ID ||
		requested.Value.DeliveryGeneration == nil || *requested.Value.DeliveryGeneration != lease.Generation {
		t.Fatalf("unexpected Steer request/replay: requested=%#v replayed=%#v", requested, replayed)
	}

	deliveries, err := service.PullControlCommands(context.Background(), worker, fixture.ExecutionID, PullControlCommandsInput{
		LeaseInput: leaseInput, Limit: 1,
	})
	if err != nil || len(deliveries) != 1 || deliveries[0].CommandType != "SteerTurn" ||
		deliveries[0].Payload["inputText"] != "focus on the failing test" {
		t.Fatalf("unexpected Steer delivery: deliveries=%#v err=%v", deliveries, err)
	}
	deliveryInput := ControlCommandDeliveryInput{LeaseInput: leaseInput, CommandID: requested.Value.CommandID}
	if _, err := service.MarkControlCommandDelivered(
		context.Background(), worker, fixture.ExecutionID, requested.Value.ID, deliveryInput, "steer-delivered",
	); err != nil {
		t.Fatal(err)
	}
	acknowledged, err := service.AcknowledgeControlCommand(
		context.Background(), worker, fixture.ExecutionID, requested.Value.ID, deliveryInput, "steer-acknowledged",
	)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.Value.Status != "acknowledged" || acknowledged.Value.AcknowledgedAt == nil {
		t.Fatalf("Steer command was not acknowledged: %#v", acknowledged.Value)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "running" || execution.FinishedAt != nil {
		t.Fatalf("Steer changed the Execution terminal state: %#v", execution)
	}
	var leases int64
	if err := db.Model(&persistence.WorkerLease{}).Where("execution_id = ?", fixture.ExecutionID).Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 1 {
		t.Fatalf("Steer unexpectedly released the active Lease: %d", leases)
	}
	var requestedEvents, acknowledgedEvents int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.ExecutionID, "turn.steer-requested").
		Count(&requestedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.ExecutionID, "turn.steered").
		Count(&acknowledgedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if requestedEvents != 1 || acknowledgedEvents != 1 {
		t.Fatalf("unexpected Steer event counts: requested=%d acknowledged=%d", requestedEvents, acknowledgedEvents)
	}
}

func TestDurableSteerRejectsWorkerWithoutCapability(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	capabilities := workerManifestTestCapabilities()
	setTestProviderCapability(capabilities, "codex", "steer-turn", "unsupported")
	worker := registerTestWorkerWithCapabilities(t, service, fixture.TargetID, fixture.TargetKind, "worker-no-steer", capabilities)
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "no-steer-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim without Steer capability failed unexpectedly: result=%#v err=%v", claim, err)
	}
	lease := *claim.Value.Lease
	if _, err := service.Start(context.Background(), worker, fixture.ExecutionID, LeaseInput{
		TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}, "no-steer-start"); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	_, err = service.RequestSteer(
		context.Background(), principal, fixture.SessionID, SteerActiveTurnInput{InputText: "redirect"},
		"no-steer", "no-steer", "127.0.0.1",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "capability_unsupported" {
		t.Fatalf("expected capability_unsupported, got %v", err)
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

func TestPostgresConcurrentApprovalResolveDifferentDecisionsHasSingleWinner(t *testing.T) {
	fixture := setupConcurrentApprovalResolution(t)
	outcomes := resolveApprovalConcurrently(
		fixture,
		[2]string{"accept", "decline"},
		[2]string{"approval-different-a-" + uuid.NewString(), "approval-different-b-" + uuid.NewString()},
	)

	var winner *concurrentApprovalResolutionOutcome
	conflicts := 0
	for index := range outcomes {
		outcome := &outcomes[index]
		if outcome.err == nil {
			if winner != nil {
				t.Fatalf("different concurrent decisions both succeeded: first=%#v second=%#v", winner, outcome)
			}
			winner = outcome
			continue
		}
		var apiError *problem.Error
		if !errors.As(outcome.err, &apiError) || apiError.Code != "interaction_resolution_conflict" {
			t.Fatalf("different concurrent decision returned unexpected error: %v", outcome.err)
		}
		conflicts++
	}
	if winner == nil || conflicts != 1 {
		t.Fatalf("different concurrent decisions produced winner=%#v conflicts=%d", winner, conflicts)
	}
	if winner.result.Replayed || winner.result.Value.Status != "resolved" ||
		winner.result.Value.Resolution["decision"] != winner.decision {
		t.Fatalf("different concurrent decision winner was not canonical: %#v", winner)
	}
	assertSingleConcurrentApprovalResolutionEffects(t, fixture, winner.decision)
}

func TestPostgresConcurrentApprovalResolveSameDecisionHasNoDuplicateEffects(t *testing.T) {
	fixture := setupConcurrentApprovalResolution(t)
	outcomes := resolveApprovalConcurrently(
		fixture,
		[2]string{"accept", "accept"},
		[2]string{"approval-same-a-" + uuid.NewString(), "approval-same-b-" + uuid.NewString()},
	)

	for _, outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("same concurrent decision failed: %v", outcome.err)
		}
		if outcome.result.Replayed {
			t.Fatalf("different idempotency keys unexpectedly replayed: %#v", outcome.result)
		}
		if outcome.result.Value.Status != "resolved" || outcome.result.Value.Resolution["decision"] != "accept" {
			t.Fatalf("same concurrent decision returned non-canonical result: %#v", outcome.result)
		}
	}
	if outcomes[0].result.Value.ID != outcomes[1].result.Value.ID ||
		outcomes[0].result.Value.ResolutionCommandID == nil ||
		outcomes[1].result.Value.ResolutionCommandID == nil ||
		*outcomes[0].result.Value.ResolutionCommandID != *outcomes[1].result.Value.ResolutionCommandID {
		t.Fatalf("same concurrent decision did not converge on one resolution: %#v", outcomes)
	}
	assertSingleConcurrentApprovalResolutionEffects(t, fixture, "accept")
}

type concurrentApprovalResolutionFixture struct {
	db         *gorm.DB
	execution  executionFixture
	services   [2]*Service
	worker     persistence.WorkerInstance
	leaseInput LeaseInput
	principal  identity.Principal
	requestID  string
}

type concurrentApprovalResolutionOutcome struct {
	decision string
	result   OperationResult[Interaction]
	err      error
}

func setupConcurrentApprovalResolution(t *testing.T) concurrentApprovalResolutionFixture {
	t.Helper()
	db := integrationDB(t)
	execution := seedExecutionFixture(t, db)
	services := [2]*Service{integrationService(t, db), integrationService(t, db)}
	worker := registerTestWorker(t, services[0], execution.TargetID, execution.TargetKind, "worker-interaction-resolve-race")
	cleanupWorkers(t, db, worker.ID)
	claim, err := services[0].Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: execution.TargetID, TargetKind: execution.TargetKind, ExecutionID: &execution.ExecutionID,
	}, "interaction-resolve-race-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("interaction resolve race execution was not leased")
	}
	leaseInput := LeaseInput{
		TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation, LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := services[0].Start(
		context.Background(), worker, execution.ExecutionID, leaseInput, "interaction-resolve-race-start",
	); err != nil {
		t.Fatal(err)
	}
	requestID := "approval-race-" + uuid.NewString()
	if _, err := services[0].AppendRuntimeEvent(context.Background(), worker, execution.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2, EventType: "request.opened",
		Payload: map[string]any{
			"requestId": requestID, "requestType": "exec_command_approval", "detail": "Run concurrent command",
		}, OccurredAt: time.Now().UTC(),
	}, "interaction-resolve-race-requested"); err != nil {
		t.Fatal(err)
	}
	assertExecutionStatus(t, db, execution, "waiting-for-approval")
	return concurrentApprovalResolutionFixture{
		db: db, execution: execution, services: services, worker: worker, leaseInput: leaseInput,
		principal: identity.Principal{UserID: execution.UserID, ActiveTenantID: &execution.TenantID},
		requestID: requestID,
	}
}

func resolveApprovalConcurrently(
	fixture concurrentApprovalResolutionFixture,
	decisions, idempotencyKeys [2]string,
) [2]concurrentApprovalResolutionOutcome {
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	completed := make(chan concurrentApprovalResolutionOutcome, 2)
	var wait sync.WaitGroup
	for index := range decisions {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			ready <- struct{}{}
			<-start
			result, err := fixture.services[index].ResolveApproval(
				context.Background(), fixture.principal, fixture.execution.ExecutionID, fixture.requestID,
				ResolveApprovalInput{Decision: decisions[index]}, idempotencyKeys[index],
				"interaction-resolve-race-audit-"+string(rune('a'+index)), "127.0.0.1",
			)
			completed <- concurrentApprovalResolutionOutcome{decision: decisions[index], result: result, err: err}
		}(index)
	}
	<-ready
	<-ready
	close(start)
	wait.Wait()
	close(completed)

	var outcomes [2]concurrentApprovalResolutionOutcome
	index := 0
	for outcome := range completed {
		outcomes[index] = outcome
		index++
	}
	return outcomes
}

func assertSingleConcurrentApprovalResolutionEffects(
	t *testing.T,
	fixture concurrentApprovalResolutionFixture,
	decision string,
) {
	t.Helper()
	var interaction persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ? AND kind = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID, fixture.requestID, "approval",
	).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	expectedResolutionKind := "denied"
	if decision == "accept" {
		expectedResolutionKind = "approved"
	}
	if interaction.Status != "resolved" || interaction.Resolution["decision"] != decision ||
		interaction.ResolutionKind == nil || *interaction.ResolutionKind != expectedResolutionKind ||
		interaction.ResolutionCommandID == nil || *interaction.ResolutionCommandID != fixture.requestID+":resolution" ||
		interaction.DeliveryStatus != "pending" || interaction.DeliveryAttempts != 0 ||
		interaction.DeliveryWorkerID == nil || *interaction.DeliveryWorkerID != fixture.worker.ID ||
		interaction.DeliveryGeneration == nil || *interaction.DeliveryGeneration != fixture.leaseInput.Generation {
		t.Fatalf("concurrent approval did not persist one canonical resolution: %#v", interaction)
	}
	assertExecutionStatus(t, fixture.db, fixture.execution, "running")

	var resolvedEvents []persistence.SessionEvent
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND event_type = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID, "request.resolved",
	).Find(&resolvedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if len(resolvedEvents) != 1 || resolvedEvents[0].Payload["requestId"] != fixture.requestID ||
		resolvedEvents[0].Payload["decision"] != decision {
		t.Fatalf("concurrent approval resolved Events = %#v, want one canonical Event", resolvedEvents)
	}

	var audits []persistence.AuditLog
	if err := fixture.db.Where(
		"tenant_id = ? AND action = ? AND resource_type = ? AND resource_id = ?",
		fixture.execution.TenantID, "execution.approval_resolved", "execution_interaction", interaction.ID,
	).Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].Metadata["requestId"] != fixture.requestID {
		t.Fatalf("concurrent approval Audit rows = %#v, want one canonical Audit", audits)
	}

	deliveries, err := fixture.services[0].PullInteractionResolutions(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		PullInteractionResolutionsInput{LeaseInput: fixture.leaseInput},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].InteractionID != interaction.ID ||
		deliveries[0].CommandID != fixture.requestID+":resolution" ||
		deliveries[0].Resolution["decision"] != decision || deliveries[0].DeliveryStatus != "pending" {
		t.Fatalf("concurrent approval Resolution deliveries = %#v, want one canonical delivery", deliveries)
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
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2, EventType: "request.opened",
		Payload: map[string]any{
			"requestId": approvalRequestID, "requestType": "exec_command_approval", "detail": "Run command",
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
	if resolved.Replayed || !replayed.Replayed || replayed.Value.ID != resolved.Value.ID || resolved.Value.Status != "resolved" ||
		resolved.Value.ResolutionKind == nil || *resolved.Value.ResolutionKind != "approved" ||
		resolved.Value.DeliveryStatus != "pending" || resolved.Value.DeliveryWorkerID == nil ||
		resolved.Value.DeliveryGeneration == nil || resolved.Value.DeliveryAvailableAt == nil {
		t.Fatalf("unexpected approval replay: first=%#v second=%#v", resolved, replayed)
	}
	assertExecutionStatus(t, db, fixture, "running")
	approvalDeliveries, err := service.PullInteractionResolutions(context.Background(), worker, fixture.ExecutionID,
		PullInteractionResolutionsInput{LeaseInput: leaseInput})
	if err != nil {
		t.Fatal(err)
	}
	if len(approvalDeliveries) != 1 || approvalDeliveries[0].CommandType != "ResolveApproval" ||
		approvalDeliveries[0].CommandID != approvalRequestID+":resolution" ||
		approvalDeliveries[0].ResolutionKind != "approved" || approvalDeliveries[0].DeliveryStatus != "pending" {
		t.Fatalf("unexpected approval delivery: %#v", approvalDeliveries)
	}
	approvalDelivery := approvalDeliveries[0]
	deliveryInput := InteractionResolutionDeliveryInput{
		LeaseInput: leaseInput, ResolutionCommandID: approvalDelivery.CommandID,
	}
	delivered, err := service.MarkInteractionResolutionDelivered(
		context.Background(), worker, fixture.ExecutionID, approvalDelivery.InteractionID,
		deliveryInput, "approval-delivered",
	)
	if err != nil {
		t.Fatal(err)
	}
	deliveryReplay, err := service.MarkInteractionResolutionDelivered(
		context.Background(), worker, fixture.ExecutionID, approvalDelivery.InteractionID,
		deliveryInput, "approval-delivered",
	)
	if err != nil {
		t.Fatal(err)
	}
	deliveryAgain, err := service.MarkInteractionResolutionDelivered(
		context.Background(), worker, fixture.ExecutionID, approvalDelivery.InteractionID,
		deliveryInput, "approval-delivered-again",
	)
	if err != nil {
		t.Fatal(err)
	}
	if delivered.Value.DeliveryStatus != "delivered" || delivered.Value.DeliveryAttempts != 1 ||
		delivered.Value.DeliveredAt == nil || !deliveryReplay.Replayed ||
		deliveryAgain.Value.DeliveryAttempts != 1 {
		t.Fatalf("approval delivery was not idempotent: first=%#v replay=%#v again=%#v", delivered, deliveryReplay, deliveryAgain)
	}
	acknowledged, err := service.AcknowledgeInteractionResolution(
		context.Background(), worker, fixture.ExecutionID, approvalDelivery.InteractionID,
		deliveryInput, "approval-acknowledged",
	)
	if err != nil {
		t.Fatal(err)
	}
	acknowledgedAgain, err := service.AcknowledgeInteractionResolution(
		context.Background(), worker, fixture.ExecutionID, approvalDelivery.InteractionID,
		deliveryInput, "approval-acknowledged-again",
	)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.Value.DeliveryStatus != "acknowledged" || acknowledged.Value.AcknowledgedAt == nil ||
		acknowledgedAgain.Value.DeliveryAttempts != 1 {
		t.Fatalf("approval acknowledgement was not idempotent: first=%#v again=%#v", acknowledged, acknowledgedAgain)
	}

	userInputRequestID := "user-input-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(context.Background(), worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2, EventType: "user-input.requested",
		Payload: map[string]any{
			"requestId": userInputRequestID,
			"questions": []any{map[string]any{
				"id": "environment", "header": "Environment", "question": "Which environment?", "options": []any{},
			}},
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
	if userInput.Value.Status != "resolved" || userInput.Value.ResolutionKind == nil ||
		*userInput.Value.ResolutionKind != "answered" || userInput.Value.DeliveryStatus != "pending" {
		t.Fatalf("user input was not resolved: %#v", userInput)
	}
	assertExecutionStatus(t, db, fixture, "running")
	userInputDeliveries, err := service.PullInteractionResolutions(context.Background(), worker, fixture.ExecutionID,
		PullInteractionResolutionsInput{LeaseInput: leaseInput})
	if err != nil {
		t.Fatal(err)
	}
	if len(userInputDeliveries) != 1 || userInputDeliveries[0].CommandType != "ResolveUserInput" ||
		userInputDeliveries[0].ResolutionKind != "answered" {
		t.Fatalf("unexpected user-input delivery: %#v", userInputDeliveries)
	}
	userInputDelivery := userInputDeliveries[0]
	userInputDeliveryInput := InteractionResolutionDeliveryInput{
		LeaseInput: leaseInput, ResolutionCommandID: userInputDelivery.CommandID,
	}
	if _, err := service.MarkInteractionResolutionDelivered(
		context.Background(), worker, fixture.ExecutionID, userInputDelivery.InteractionID,
		userInputDeliveryInput, "user-input-delivered",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcknowledgeInteractionResolution(
		context.Background(), worker, fixture.ExecutionID, userInputDelivery.InteractionID,
		userInputDeliveryInput, "user-input-acknowledged",
	); err != nil {
		t.Fatal(err)
	}
	remainingDeliveries, err := service.PullInteractionResolutions(context.Background(), worker, fixture.ExecutionID,
		PullInteractionResolutionsInput{LeaseInput: leaseInput})
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingDeliveries) != 0 {
		t.Fatalf("acknowledged resolutions remained pullable: %#v", remainingDeliveries)
	}

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
			[]string{"request.opened", "user-input.requested"}).Count(&requestedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type IN ?", fixture.TenantID, fixture.ExecutionID,
			[]string{"request.resolved", "user-input.resolved"}).Count(&resolvedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if requestedEvents != 2 || resolvedEvents != 2 {
		t.Fatalf("interaction replay duplicated events: requested=%d resolved=%d", requestedEvents, resolvedEvents)
	}
	var nonCanonicalEvents int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_version <> ?", fixture.TenantID, fixture.ExecutionID, RuntimeEventVersionV2).
		Where("event_type IN ?", []string{"request.opened", "request.resolved", "user-input.requested", "user-input.resolved"}).
		Count(&nonCanonicalEvents).Error; err != nil {
		t.Fatal(err)
	}
	if nonCanonicalEvents != 0 {
		t.Fatalf("canonical interaction lifecycle persisted %d non-v2 events", nonCanonicalEvents)
	}
	var resolutionEvents []persistence.SessionEvent
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND event_type IN ?",
		fixture.TenantID, fixture.ExecutionID, []string{"request.resolved", "user-input.resolved"},
	).Find(&resolutionEvents).Error; err != nil {
		t.Fatal(err)
	}
	for _, event := range resolutionEvents {
		if event.WorkerID == nil || *event.WorkerID != worker.ID || event.Generation == nil || *event.Generation != lease.Generation {
			t.Fatalf("canonical resolution Event lost Worker generation correlation: %#v", event)
		}
	}
}

func TestPendingSessionInteractionSnapshotSurvivesServiceRestartAndEnforcesApprovalPermission(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-pending-snapshot")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "pending-snapshot-claim")
	if err != nil {
		t.Fatal(err)
	}
	lease := claim.Value.Lease
	if lease == nil {
		t.Fatal("pending snapshot execution was not leased")
	}
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}
	if _, err := service.Start(context.Background(), worker, fixture.ExecutionID, leaseInput, "pending-snapshot-start"); err != nil {
		t.Fatal(err)
	}
	requestID := "approval-snapshot-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(context.Background(), worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2, EventType: "request.opened",
		Payload: map[string]any{
			"requestId": requestID, "requestType": "exec_command_approval", "detail": "Deploy release",
		}, OccurredAt: time.Now().UTC(),
	}, "pending-snapshot-requested"); err != nil {
		t.Fatal(err)
	}

	restarted := integrationService(t, db)
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	snapshot, err := restarted.ListPendingInteractions(context.Background(), principal, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 1 || snapshot.Items[0].ExecutionID != fixture.ExecutionID ||
		snapshot.Items[0].RequestID != requestID || snapshot.Items[0].Kind != "approval" {
		t.Fatalf("unexpected pending interaction snapshot: %#v", snapshot)
	}
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if snapshot.SnapshotSequence != session.LastEventSequence {
		t.Fatalf("snapshot watermark=%d, want %d", snapshot.SnapshotSequence, session.LastEventSequence)
	}
	expiredRequestedAt := time.Now().UTC().Add(-2 * time.Hour)
	if err := db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND request_id = ?", fixture.TenantID, fixture.ExecutionID, requestID).
		Updates(map[string]any{
			"requested_at": expiredRequestedAt,
			"expires_at":   expiredRequestedAt.Add(time.Hour),
		}).Error; err != nil {
		t.Fatal(err)
	}
	expiredSnapshot, err := restarted.ListPendingInteractions(context.Background(), principal, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(expiredSnapshot.Items) != 0 {
		t.Fatalf("expired interaction remained actionable: %#v", expiredSnapshot.Items)
	}

	viewerID := uuid.New()
	now := time.Now().UTC()
	models := []any{
		&persistence.User{
			ID: viewerID, Email: uuid.NewString() + "@example.com", DisplayName: "Interaction viewer",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.TenantMembership{
			TenantID: fixture.TenantID, UserID: viewerID, Role: "member", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.OrganizationMembership{
			TenantID: fixture.TenantID, OrganizationID: session.OrganizationID, UserID: viewerID,
			Role: "viewer", Status: "active", CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("visibility", "organization").Error; err != nil {
		t.Fatal(err)
	}
	viewer := identity.Principal{UserID: viewerID, ActiveTenantID: &fixture.TenantID}
	_, err = restarted.ListPendingInteractions(context.Background(), viewer, fixture.SessionID)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "organization_forbidden" {
		t.Fatalf("viewer loaded pending interaction details: %v", err)
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

func TestInteractionResolutionDeliveryFencesReplacedGeneration(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	current := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return current }
	first := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-interaction-old")
	second := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "worker-interaction-new")
	cleanupWorkers(t, db, first.ID, second.ID)
	claimInput := ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}
	firstClaim, err := service.Claim(context.Background(), first, claimInput, "interaction-fence-first-claim")
	if err != nil {
		t.Fatal(err)
	}
	firstLease := *firstClaim.Value.Lease
	firstLeaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
	}
	if _, err := service.Start(context.Background(), first, fixture.ExecutionID, firstLeaseInput, "interaction-fence-start"); err != nil {
		t.Fatal(err)
	}
	requestID := "approval-fence-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(context.Background(), first, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: firstLeaseInput, EventID: uuid.New(), EventVersion: 1, EventType: "approval.requested",
		Payload: map[string]any{"requestId": requestID}, OccurredAt: current,
	}, "interaction-fence-requested"); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	resolved, err := service.ResolveApproval(
		context.Background(), principal, fixture.ExecutionID, requestID,
		ResolveApprovalInput{Decision: "accept"}, "interaction-fence-resolve", "interaction-fence-resolve", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}

	current = current.Add(service.leaseTTL + time.Second)
	secondClaim, err := service.Claim(context.Background(), second, claimInput, "interaction-fence-second-claim")
	if err != nil {
		t.Fatal(err)
	}
	if secondClaim.Value.Lease == nil || secondClaim.Value.Lease.Generation != firstLease.Generation+1 {
		t.Fatalf("replacement Worker did not receive a new generation: %#v", secondClaim.Value.Lease)
	}
	_, err = service.PullInteractionResolutions(context.Background(), first, fixture.ExecutionID,
		PullInteractionResolutionsInput{LeaseInput: firstLeaseInput})
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "generation_fenced" {
		t.Fatalf("obsolete Worker generation was not fenced: %v", err)
	}
	var interaction persistence.ExecutionInteraction
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, resolved.Value.ID).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.DeliveryStatus != "superseded" || interaction.DeliveryError == nil {
		t.Fatalf("obsolete interaction delivery was not superseded: %#v", interaction)
	}
	secondLeaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: secondClaim.Value.Lease.Generation,
		LeaseToken: secondClaim.Value.Lease.LeaseToken,
	}
	newGenerationDeliveries, err := service.PullInteractionResolutions(context.Background(), second, fixture.ExecutionID,
		PullInteractionResolutionsInput{LeaseInput: secondLeaseInput})
	if err != nil {
		t.Fatal(err)
	}
	if len(newGenerationDeliveries) != 0 {
		t.Fatalf("replacement Worker received an obsolete resolution: %#v", newGenerationDeliveries)
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
	UserID               uuid.UUID
	TenantID             uuid.UUID
	SessionID            uuid.UUID
	TurnID               uuid.UUID
	ExecutionID          uuid.UUID
	ProviderCredentialID uuid.UUID
	GitCredentialID      uuid.UUID
	TargetID             uuid.UUID
	TargetKind           string
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
	runtimeBindingID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	targetID := uuid.New()
	providerCredentialID := uuid.New()
	gitCredentialID := uuid.New()
	repositoryURL := "https://git.example.com/team/private.git"
	provider := "codex"
	slug := "exec-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	models := []any{
		&persistence.User{ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Execution test", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: slug, Name: "Execution test tenant", Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now},
		&persistence.Organization{ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.OrganizationMembership{TenantID: tenantID, OrganizationID: organizationID, UserID: userID, Role: "owner", Status: "active"},
		&persistence.ProviderCredential{
			ID: providerCredentialID, TenantID: tenantID, OrganizationID: &organizationID,
			Name: "Execution Provider", Purpose: "provider", Provider: provider, CredentialType: "api_key",
			EncryptedPayload: []byte("encrypted-provider-payload"), EncryptedDataKey: []byte("encrypted-provider-data-key"),
			KMSProvider: "local", KMSKeyID: "test", Version: 1,
			CreatedBy: userID, UpdatedBy: userID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.ProviderCredential{
			ID: gitCredentialID, TenantID: tenantID, OrganizationID: &organizationID,
			Name: "Execution Git", Purpose: "git", Provider: "git", CredentialType: "https_token",
			EncryptedPayload: []byte("encrypted-git-payload"), EncryptedDataKey: []byte("encrypted-git-data-key"),
			KMSProvider: "local", KMSKeyID: "test", Version: 1,
			CreatedBy: userID, UpdatedBy: userID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Project{
			ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Execution project",
			RepositoryURL: &repositoryURL, DefaultBranch: "main", GitCredentialID: &gitCredentialID,
			Visibility: "organization", CreatedBy: userID,
		},
		&persistence.ExecutionTarget{ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID, Kind: "kubernetes", Name: "test-target", Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: workerManifestTestTargetCapabilities()},
		&persistence.AgentSession{ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID, CreatedBy: userID, Title: "Execution session", Status: "active", Visibility: "private", Provider: provider, ProviderCredentialID: &providerCredentialID, ExecutionTargetID: targetID, CurrentRuntimeBindingID: &runtimeBindingID},
		&persistence.ProviderRuntimeBinding{
			ID: runtimeBindingID, TenantID: tenantID, SessionID: sessionID, Provider: provider,
			Revision: 1, Status: "active", ResumeStrategy: "authoritative-history",
			CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: tenantID, SessionID: sessionID, CreatedBy: userID,
			Status: "queued", InputText: "Run integration test",
			RuntimeMode: "approval-required", InteractionMode: "plan",
		},
		&persistence.AgentExecution{ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "queued", ExecutionTargetID: targetID, TargetKind: "kubernetes", Provider: &provider, ProviderRuntimeBindingID: &runtimeBindingID, Generation: 0, RequestedBy: userID, QueuedAt: now},
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
	return executionFixture{
		UserID: userID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
		ExecutionID: executionID, ProviderCredentialID: providerCredentialID, GitCredentialID: gitCredentialID,
		TargetID: targetID, TargetKind: "kubernetes",
	}
}

func registerTestWorker(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	targetKind string,
	podName string,
) persistence.WorkerInstance {
	t.Helper()
	return registerManifestTestWorker(t, service, targetID, targetKind, podName)
}

func registerLegacyTestWorker(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	targetKind string,
	podName string,
) persistence.WorkerInstance {
	t.Helper()
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: targetID, TargetKind: targetKind,
		InstanceUID: uuid.NewString(),
		ClusterID:   "test-cluster", Namespace: "default", PodName: podName + "-" + uuid.NewString(),
		Version: "test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: map[string]any{"codex": true}, LeaseSupported: true, FencingSupported: true,
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

func registerManifestTestWorker(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	targetKind string,
	podName string,
) persistence.WorkerInstance {
	t.Helper()
	return registerTestWorkerWithCapabilities(
		t, service, targetID, targetKind, podName, workerManifestTestCapabilities(),
	)
}

func registerTestWorkerWithCapabilities(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	targetKind string,
	podName string,
	capabilities map[string]any,
) persistence.WorkerInstance {
	t.Helper()
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: targetID, TargetKind: targetKind,
		InstanceUID: uuid.NewString(),
		ClusterID:   "test-cluster", Namespace: "default", PodName: podName + "-" + uuid.NewString(),
		Version: "test", ProtocolVersion: WorkerProtocolVersion, Capabilities: capabilities,
		LeaseSupported: true, FencingSupported: true,
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

func setTestProviderCapability(
	capabilities map[string]any,
	provider, capabilityID, support string,
) {
	providerHost := capabilities["providerHost"].(map[string]any)
	providers := providerHost["providers"].(map[string]any)
	descriptor := providers[provider].(map[string]any)["capabilityDescriptor"].(map[string]any)
	descriptor["capabilities"].(map[string]any)[capabilityID] = support
}

func cleanupWorkers(t *testing.T, db *gorm.DB, workerIDs ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if len(workerIDs) == 0 {
			return
		}
		_ = db.Where("worker_id IN ?", workerIDs).Delete(&persistence.WorkerRequestReceipt{}).Error
		_ = db.Where("worker_id IN ?", workerIDs).Delete(&persistence.WorkerLease{}).Error
		_ = db.Where("delivery_worker_id IN ? OR worker_id IN ?", workerIDs, workerIDs).
			Delete(&persistence.ExecutionInteraction{}).Error
		_ = db.Where("delivery_worker_id IN ?", workerIDs).
			Delete(&persistence.ExecutionControlCommand{}).Error
		_ = db.Where("delivery_worker_id IN ?", workerIDs).
			Delete(&persistence.WorkspaceCleanupCommand{}).Error
		_ = db.Model(&persistence.WorkspaceMaterialization{}).Where("worker_id IN ?", workerIDs).
			Updates(map[string]any{
				"worker_id": nil, "worker_incarnation": nil, "worker_instance_uid": nil,
			}).Error
		_ = db.Model(&persistence.AgentExecution{}).Where("worker_id IN ?", workerIDs).
			Updates(map[string]any{"status": "recovering", "worker_id": nil}).Error
		_ = db.Where("id IN ?", workerIDs).Delete(&persistence.WorkerInstance{}).Error
	})
}

func cleanupFixture(db *gorm.DB, tenantID uuid.UUID) {
	_ = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).Where("tenant_id = ?", tenantID).
			Updates(map[string]any{"restore_checkpoint_id": nil, "workspace_materialization_id": nil}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).Where("tenant_id = ?", tenantID).
			Updates(map[string]any{"current_checkpoint_id": nil, "current_materialization_id": nil}).Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE artifacts DISABLE TRIGGER trg_artifacts_protect_checkpoint_reference").Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.Artifact{}).Where("tenant_id = ?", tenantID).
			Update("workspace_checkpoint_id", nil).Error; err != nil {
			return err
		}
		if err := tx.Exec("SET CONSTRAINTS ALL IMMEDIATE").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE artifacts ENABLE TRIGGER trg_artifacts_protect_checkpoint_reference").Error; err != nil {
			return err
		}
		if err := tx.Exec("SET CONSTRAINTS ALL DEFERRED").Error; err != nil {
			return err
		}
		models := []any{
			&persistence.WorkerLease{}, &persistence.SessionEvent{}, &persistence.OutboxMessage{},
			&persistence.APIIdempotencyKey{}, &persistence.ExecutionInteraction{}, &persistence.ExecutionControlCommand{},
			&persistence.WorkspaceCleanupCommand{}, &persistence.WorkspaceMaterialization{},
			&persistence.WorkspaceCheckpoint{}, &persistence.Artifact{},
			&persistence.TenantQuota{}, &persistence.AgentExecution{}, &persistence.RemoteWorkspace{}, &persistence.AgentTurn{},
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
