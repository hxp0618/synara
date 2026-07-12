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
		Where("tenant_id = ? AND status IN ?", fixture.TenantID, []string{"queued", "leased", "running", "recovering"}).
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
