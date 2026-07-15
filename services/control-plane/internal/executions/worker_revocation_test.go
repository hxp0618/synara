package executions

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkerRevocationFencesTokenAndRecoversExecutionAndCleanupLeases(t *testing.T) {
	db, service, fixture := newSQLiteWorkerRevocationFixture(t)
	ctx := context.Background()
	registration := RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		InstanceUID: uuid.NewString(), ClusterID: "revocation-cluster", Namespace: "default",
		PodName: "revocation-worker", Version: "worker-test",
		ProtocolVersion: WorkerProtocolVersion, Capabilities: workerManifestTestCapabilities(),
		LeaseSupported: true, FencingSupported: true,
	}
	registered, err := service.Register(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}

	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "worker-revocation-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim before revoke: %#v, %v", claim, err)
	}
	cleanup := seedLeasedWorkspaceCleanup(t, db, service, fixture, worker)

	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	first, err := service.RevokeWorker(ctx, principal, fixture.TenantID, worker.ID, RevokeWorkerInput{
		ExpectedIncarnation: worker.Incarnation, Reason: "operator security response",
	}, "worker-revoke-idempotency", "worker-revoke-request", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.StatusCode != 200 || first.Value.Worker.AdministrativeStatus != "revoked" ||
		first.Value.ReleasedExecutionLeases != 1 || first.Value.RecoveringExecutions != 1 ||
		first.Value.OutcomeUnknownExecutions != 0 || first.Value.CheckpointUnconfirmedExecutions != 0 ||
		first.Value.RequeuedWorkspaceCleanups != 1 {
		t.Fatalf("unexpected Worker revocation result: %#v", first)
	}
	replayed, err := service.RevokeWorker(ctx, principal, fixture.TenantID, worker.ID, RevokeWorkerInput{
		ExpectedIncarnation: worker.Incarnation, Reason: "operator security response",
	}, "worker-revoke-idempotency", "worker-revoke-replayed", "127.0.0.1")
	if err != nil || !replayed.Replayed || !reflect.DeepEqual(replayed.Value, first.Value) {
		t.Fatalf("unexpected Worker revocation replay: %#v, %v", replayed, err)
	}
	_, err = service.RevokeWorker(ctx, principal, fixture.TenantID, worker.ID, RevokeWorkerInput{
		ExpectedIncarnation: worker.Incarnation, Reason: "different reason",
	}, "worker-revoke-idempotency", "worker-revoke-conflict", "127.0.0.1")
	assertWorkerRevocationProblem(t, err, 409, "idempotency_conflict")

	_, err = service.Authenticate(ctx, registered.Token)
	assertWorkerRevocationProblem(t, err, 401, "worker_token_revoked")
	_, err = service.Renew(ctx, worker, fixture.ExecutionID, RenewLeaseInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		},
	}, "worker-renew-after-revoke")
	assertWorkerRevocationProblem(t, err, 401, "worker_token_revoked")
	registration.InstanceUID = uuid.NewString()
	_, err = service.Register(ctx, registration)
	assertWorkerRevocationProblem(t, err, 409, "worker_identity_revoked")

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "recovering" || execution.WorkerID != nil || execution.Generation != claim.Value.Lease.Generation {
		t.Fatalf("revoked Execution did not enter recovery: %#v", execution)
	}
	var leaseCount int64
	if err := db.Model(&persistence.WorkerLease{}).Where("worker_id = ?", worker.ID).Count(&leaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if leaseCount != 0 {
		t.Fatalf("revoked Worker retained %d execution leases", leaseCount)
	}
	var cleanupAfter persistence.WorkspaceCleanupCommand
	if err := db.Where("id = ?", cleanup.ID).Take(&cleanupAfter).Error; err != nil {
		t.Fatal(err)
	}
	if cleanupAfter.Status != "pending" || cleanupAfter.DeliveryWorkerID != nil ||
		cleanupAfter.DeliveryWorkerIncarnation != nil || cleanupAfter.LeaseExpiresAt != nil ||
		cleanupAfter.LastErrorCode == nil || *cleanupAfter.LastErrorCode != "workspace_cleanup_worker_revoked" {
		t.Fatalf("Workspace cleanup lease was not requeued: %#v", cleanupAfter)
	}

	var auditCount, outboxCount int64
	if err := db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND resource_id = ?", fixture.TenantID, "worker.revoked", worker.ID).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic = ? AND message_key = ?", fixture.TenantID, "worker.revoked", worker.ID.String()+":"+formatGeneration(worker.Incarnation)).
		Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || outboxCount != 1 {
		t.Fatalf("Worker revocation audit/outbox counts = %d/%d", auditCount, outboxCount)
	}

	replacement := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "replacement-worker")
	reclaimed, err := service.Claim(ctx, replacement, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "worker-revocation-reclaim")
	if err != nil || reclaimed.Value.Lease == nil || reclaimed.Value.Lease.Generation != claim.Value.Lease.Generation+1 {
		t.Fatalf("recovered Execution was not generation-fenced and reclaimed: %#v, %v", reclaimed, err)
	}
}

func TestWorkerRevocationPreservesDeliveredPrimaryOutcomeUnknown(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	expected := fixture.lastSequence(t, fixture.sessionID)
	queued, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
		"worker-revoke-review", "worker-revoke-review", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	var worker persistence.WorkerInstance
	if err := fixture.db.Where("execution_target_id = ? AND administrative_status = ?", fixture.targetID, "active").Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	claim, err := fixture.service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.targetID, TargetKind: "kubernetes", ExecutionID: &queued.Value.ExecutionID,
	}, "worker-revoke-review-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim Review operation: %#v, %v", claim, err)
	}
	leaseInput := LeaseInput{
		TenantID: fixture.tenantID, Generation: claim.Value.Lease.Generation, LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := fixture.service.MarkControlCommandDelivered(
		context.Background(), worker, queued.Value.ExecutionID, queued.Value.ControlCommand.ID,
		ControlCommandDeliveryInput{LeaseInput: leaseInput, CommandID: queued.Value.ControlCommand.CommandID},
		"worker-revoke-review-delivered",
	); err != nil {
		t.Fatal(err)
	}

	revoked, err := fixture.service.RevokeWorker(
		context.Background(), fixture.principal, fixture.tenantID, worker.ID,
		RevokeWorkerInput{ExpectedIncarnation: worker.Incarnation, Reason: "primary operation Worker compromised"},
		"worker-revoke-review-idempotency", "worker-revoke-review-request", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Value.OutcomeUnknownExecutions != 1 || revoked.Value.RecoveringExecutions != 0 {
		t.Fatalf("delivered primary operation was not terminalized outcome-unknown: %#v", revoked.Value)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, queued.Value.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "failed" || execution.FailureCode == nil || *execution.FailureCode != "provider_operation_outcome_unknown" {
		t.Fatalf("unexpected primary operation terminal state: %#v", execution)
	}
	var command persistence.ExecutionControlCommand
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, queued.Value.ControlCommand.ID).Take(&command).Error; err != nil {
		t.Fatal(err)
	}
	if command.Status != "outcome_unknown" {
		t.Fatalf("delivered primary command status = %q, want outcome_unknown", command.Status)
	}
}

func TestWorkerRevocationCheckpointRiskRequiresExactReadyGeneration(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		readyCheckpoint bool
		wantRisk        bool
	}{
		{name: "checkpoint unconfirmed", wantRisk: true},
		{name: "ready generation", readyCheckpoint: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db, service, fixture, _, lease := setupExpiredManagedLeaseRecovery(t, testCase.readyCheckpoint)
			var worker persistence.WorkerInstance
			if err := db.Where("id = ?", lease.WorkerID).Take(&worker).Error; err != nil {
				t.Fatal(err)
			}
			principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
			result, err := service.RevokeWorker(
				context.Background(), principal, fixture.TenantID, worker.ID,
				RevokeWorkerInput{ExpectedIncarnation: worker.Incarnation, Reason: "managed Workspace Worker revoked"},
				"worker-revoke-checkpoint-"+uuid.NewString(), "worker-revoke-checkpoint", "127.0.0.1",
			)
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if testCase.wantRisk {
				wantCount = 1
			}
			if result.Value.CheckpointUnconfirmedExecutions != wantCount {
				t.Fatalf("checkpoint risk count = %d, want %d", result.Value.CheckpointUnconfirmedExecutions, wantCount)
			}
			var event persistence.SessionEvent
			if err := db.Where(
				"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
				fixture.TenantID, fixture.SessionID, fixture.ExecutionID, "execution.recovering",
			).Order("sequence DESC").Take(&event).Error; err != nil {
				t.Fatal(err)
			}
			if event.Payload["reason"] != "worker_revoked" {
				t.Fatalf("unexpected revoke recovery event: %#v", event.Payload)
			}
			assertRecoveryRisk(t, event.Payload, testCase.wantRisk)
		})
	}
}

func newSQLiteWorkerRevocationFixture(t *testing.T) (*gorm.DB, *Service, executionFixture) {
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
	fixture := seedExecutionFixture(t, store.DB())
	return store.DB(), integrationService(t, store.DB()), fixture
}

func seedLeasedWorkspaceCleanup(
	t *testing.T,
	db *gorm.DB,
	service *Service,
	fixture executionFixture,
	worker persistence.WorkerInstance,
) persistence.WorkspaceCleanupCommand {
	t.Helper()
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	workspaceID, materializationID, incarnationID := uuid.New(), uuid.New(), uuid.New()
	reason := "test-cleanup"
	workspace := persistence.RemoteWorkspace{
		ID: workspaceID, TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: session.ID, ExecutionTargetID: fixture.TargetID,
		WorkspaceMode: "clone", State: "cleanup-pending", DefaultBranch: "main",
		CurrentMaterializationID: &materializationID, CreatedAt: now, UpdatedAt: now,
	}
	materialization := persistence.WorkspaceMaterialization{
		ID: materializationID, TenantID: fixture.TenantID, WorkspaceID: workspaceID,
		OrganizationID: session.OrganizationID, ProjectID: session.ProjectID, SessionID: session.ID,
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		StorageScope: "target", LayoutVersion: workspaceLayoutVersionCurrent,
		IncarnationID: incarnationID, State: "cleanup-pending", CleanupReason: &reason,
		CleanupRequestedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	command := persistence.WorkspaceCleanupCommand{
		ID: uuid.New(), TenantID: fixture.TenantID, MaterializationID: materializationID,
		MaterializationIncarnationID: incarnationID, WorkspaceID: workspaceID,
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		StorageScope: "target", LayoutVersion: workspaceLayoutVersionCurrent,
		Reason: reason, Status: "pending", DeliveryAvailableAt: now,
		RequestedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range []any{&workspace, &materialization, &command} {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := service.ClaimWorkspaceCleanup(context.Background(), worker, WorkspaceCleanupClaimInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
	}, "worker-revoke-cleanup-claim")
	if err != nil || claim.Value.Cleanup == nil || claim.Value.Cleanup.CleanupID != command.ID {
		t.Fatalf("claim Workspace cleanup before Worker revoke: %#v, %v", claim, err)
	}
	if err := db.Where("id = ?", command.ID).Take(&command).Error; err != nil {
		t.Fatal(err)
	}
	return command
}

func assertWorkerRevocationProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != status || apiError.Code != code {
		t.Fatalf("problem = %#v (%v), want status=%d code=%s", apiError, err, status, code)
	}
}
