package executions

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestRenewExpiresTimedOutInteractionAndRecoversGenerationOnce(t *testing.T) {
	fixture := setupExpiredInteraction(t)
	_, err := fixture.service.Renew(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		RenewLeaseInput{LeaseInput: fixture.leaseInput}, "interaction-expiry-renew",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "interaction_expired" {
		t.Fatalf("expired interaction renew error = %v", err)
	}
	assertExpiredInteractionRecovery(t, fixture)
}

func TestRenewRejectsWrongWorkerWithoutExpiringInteraction(t *testing.T) {
	fixture := setupExpiredInteraction(t)
	wrongWorker := registerTestWorker(
		t, fixture.service, fixture.execution.TargetID, fixture.execution.TargetKind, "worker-interaction-expiry-wrong",
	)
	cleanupWorkers(t, fixture.db, wrongWorker.ID)

	assertRenewRejectedWithoutExpiry(
		t, fixture, wrongWorker, fixture.leaseInput,
		"interaction-expiry-wrong-worker", "generation_fenced",
	)
}

func TestRenewRejectsWrongLeaseTokenWithoutExpiringInteraction(t *testing.T) {
	fixture := setupExpiredInteraction(t)
	leaseInput := fixture.leaseInput
	leaseInput.LeaseToken = "wrong-interaction-expiry-lease-token"

	assertRenewRejectedWithoutExpiry(
		t, fixture, fixture.worker, leaseInput,
		"interaction-expiry-wrong-token", "invalid_lease_token",
	)
}

func TestRenewRejectsCrossTenantWorkerWithoutExpiringInteraction(t *testing.T) {
	caller := setupExpiredInteraction(t)
	target := setupExpiredInteraction(t)

	assertRenewRejectedWithoutExpiry(
		t, target, caller.worker, target.leaseInput,
		"interaction-expiry-cross-tenant", "generation_fenced",
	)
}

func TestRenewRejectsInvalidRequestIDWithoutExpiringInteraction(t *testing.T) {
	fixture := setupExpiredInteraction(t)

	assertRenewRejectedWithoutExpiry(
		t, fixture, fixture.worker, fixture.leaseInput, "", "invalid_request_id",
	)
}

func TestExpiredInteractionSweepRecoversGenerationOnce(t *testing.T) {
	fixture := setupExpiredInteraction(t)
	expired, err := fixture.service.ExpirePendingInteractions(context.Background(), fixture.now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatalf("expired interaction sweep count = %d, want 1", expired)
	}
	assertExpiredInteractionRecovery(t, fixture)
}

type expiredInteractionFixture struct {
	db         *gorm.DB
	execution  executionFixture
	service    *Service
	worker     persistence.WorkerInstance
	leaseInput LeaseInput
	requestID  string
	now        time.Time
}

func setupExpiredInteraction(t *testing.T) expiredInteractionFixture {
	t.Helper()
	db := integrationDB(t)
	execution := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	worker := registerTestWorker(t, service, execution.TargetID, execution.TargetKind, "worker-interaction-expiry")
	cleanupWorkers(t, db, worker.ID)

	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: execution.TargetID, TargetKind: execution.TargetKind, ExecutionID: &execution.ExecutionID,
	}, "interaction-expiry-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("interaction expiry execution was not leased")
	}
	leaseInput := LeaseInput{
		TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := service.Start(
		context.Background(), worker, execution.ExecutionID, leaseInput, "interaction-expiry-start",
	); err != nil {
		t.Fatal(err)
	}
	requestID := "approval-expiry-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(context.Background(), worker, execution.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "request.opened", OccurredAt: now,
		Payload: map[string]any{
			"requestId": requestID, "requestType": "exec_command_approval", "detail": "Deploy release",
		},
	}, "interaction-expiry-requested"); err != nil {
		t.Fatal(err)
	}

	requestedAt := now.Add(-2 * time.Hour)
	if err := db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND request_id = ?", execution.TenantID, execution.ExecutionID, requestID).
		Updates(map[string]any{"requested_at": requestedAt, "expires_at": requestedAt.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	return expiredInteractionFixture{
		db: db, execution: execution, service: service, worker: worker,
		leaseInput: leaseInput, requestID: requestID, now: now,
	}
}

func assertExpiredInteractionRecovery(t *testing.T, fixture expiredInteractionFixture) {
	t.Helper()
	var interaction persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID, fixture.requestID,
	).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.Status != "expired" || interaction.DeliveryStatus != "superseded" ||
		interaction.DeliveryError == nil || !strings.Contains(*interaction.DeliveryError, "maximum waiting time") {
		t.Fatalf("expired interaction state = %#v", interaction)
	}

	var execution persistence.AgentExecution
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "recovering" || execution.WorkerID != nil {
		t.Fatalf("expired interaction did not fence the Execution generation: %#v", execution)
	}
	var turn persistence.AgentTurn
	if err := fixture.db.Where(
		"tenant_id = ? AND session_id = ? AND id = ?",
		fixture.execution.TenantID, fixture.execution.SessionID, fixture.execution.TurnID,
	).Take(&turn).Error; err != nil {
		t.Fatal(err)
	}
	if turn.Status != "queued" {
		t.Fatalf("expired interaction did not requeue the Turn: %#v", turn)
	}
	var leases int64
	if err := fixture.db.Model(&persistence.WorkerLease{}).
		Where("execution_id = ?", fixture.execution.ExecutionID).Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("expired interaction retained %d Worker leases", leases)
	}

	var recoveryEvents []persistence.SessionEvent
	if err := fixture.db.Where(
		"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
		fixture.execution.TenantID, fixture.execution.SessionID, fixture.execution.ExecutionID,
		"execution.recovering",
	).Find(&recoveryEvents).Error; err != nil {
		t.Fatal(err)
	}
	if len(recoveryEvents) != 1 || recoveryEvents[0].Payload["reason"] != "interaction_expired" {
		t.Fatalf("interaction expiry recovery Events = %#v", recoveryEvents)
	}
	var recoveryOutbox []persistence.OutboxMessage
	if err := fixture.db.
		Where(
			"tenant_id = ? AND topic = ? AND message_key = ?", fixture.execution.TenantID,
			"execution.recovering", fixture.execution.ExecutionID.String()+":"+formatGeneration(execution.Generation),
		).
		Find(&recoveryOutbox).Error; err != nil {
		t.Fatal(err)
	}
	if len(recoveryOutbox) != 1 || recoveryOutbox[0].Payload["reason"] != "interaction_expired" {
		t.Fatalf("interaction expiry recovery Outbox rows = %#v", recoveryOutbox)
	}

	principal := identity.Principal{
		UserID: fixture.execution.UserID, ActiveTenantID: &fixture.execution.TenantID,
	}
	_, err := fixture.service.ResolveApproval(
		context.Background(), principal, fixture.execution.ExecutionID, fixture.requestID,
		ResolveApprovalInput{Decision: "accept"}, "interaction-expiry-resolve-"+uuid.NewString(),
		"interaction-expiry-resolve", "127.0.0.1",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "interaction_expired" {
		t.Fatalf("expired interaction resolve error = %v", err)
	}

	expiredAgain, err := fixture.service.ExpirePendingInteractions(context.Background(), fixture.now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if expiredAgain != 0 {
		t.Fatalf("idempotent interaction expiry changed %d rows", expiredAgain)
	}
	var recoveryEventsAfter int64
	if err := fixture.db.Model(&persistence.SessionEvent{}).Where(
		"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
		fixture.execution.TenantID, fixture.execution.SessionID, fixture.execution.ExecutionID,
		"execution.recovering",
	).Count(&recoveryEventsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if recoveryEventsAfter != 1 {
		t.Fatalf("idempotent interaction expiry produced %d recovery Events", recoveryEventsAfter)
	}
}

type interactionExpiryState struct {
	Interaction    persistence.ExecutionInteraction
	Execution      persistence.AgentExecution
	Turn           persistence.AgentTurn
	Lease          persistence.WorkerLease
	RecoveryEvents int64
	RecoveryOutbox int64
}

func assertRenewRejectedWithoutExpiry(
	t *testing.T,
	fixture expiredInteractionFixture,
	worker persistence.WorkerInstance,
	leaseInput LeaseInput,
	requestID, expectedCode string,
) {
	t.Helper()
	before := loadInteractionExpiryState(t, fixture)

	_, err := fixture.service.Renew(
		context.Background(), worker, fixture.execution.ExecutionID,
		RenewLeaseInput{LeaseInput: leaseInput}, requestID,
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != expectedCode {
		t.Fatalf("rejected interaction expiry Renew error = %v, want %s", err, expectedCode)
	}

	after := loadInteractionExpiryState(t, fixture)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected interaction expiry Renew changed state:\nbefore = %#v\nafter  = %#v", before, after)
	}
}

func loadInteractionExpiryState(t *testing.T, fixture expiredInteractionFixture) interactionExpiryState {
	t.Helper()
	var state interactionExpiryState
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.execution.TenantID, fixture.execution.ExecutionID, fixture.requestID,
	).Take(&state.Interaction).Error; err != nil {
		t.Fatalf("load interaction expiry state interaction: %v", err)
	}
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&state.Execution).Error; err != nil {
		t.Fatalf("load interaction expiry state Execution: %v", err)
	}
	if err := fixture.db.Where(
		"tenant_id = ? AND session_id = ? AND id = ?",
		fixture.execution.TenantID, fixture.execution.SessionID, fixture.execution.TurnID,
	).Take(&state.Turn).Error; err != nil {
		t.Fatalf("load interaction expiry state Turn: %v", err)
	}
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&state.Lease).Error; err != nil {
		t.Fatalf("load interaction expiry state Lease: %v", err)
	}
	if err := fixture.db.Model(&persistence.SessionEvent{}).Where(
		"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
		fixture.execution.TenantID, fixture.execution.SessionID, fixture.execution.ExecutionID,
		"execution.recovering",
	).Count(&state.RecoveryEvents).Error; err != nil {
		t.Fatalf("load interaction expiry state recovery Events: %v", err)
	}
	if err := fixture.db.Model(&persistence.OutboxMessage{}).Where(
		"tenant_id = ? AND topic = ? AND message_key = ?", fixture.execution.TenantID,
		"execution.recovering", fixture.execution.ExecutionID.String()+":"+formatGeneration(state.Execution.Generation),
	).Count(&state.RecoveryOutbox).Error; err != nil {
		t.Fatalf("load interaction expiry state recovery Outbox: %v", err)
	}
	return state
}
