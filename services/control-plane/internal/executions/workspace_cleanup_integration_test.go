package executions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestWorkspaceCleanupClaimIsConcurrentFencedAndAcknowledged(t *testing.T) {
	db := integrationDB(t)
	fixture, workspace, materialization := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }

	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands created = %d, want 1", created)
	}
	workerA := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-a")
	workerB := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-b")
	cleanupWorkers(t, db, workerA.ID, workerB.ID)

	type claimOutcome struct {
		result OperationResult[WorkspaceCleanupClaimResult]
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan claimOutcome, 2)
	var wait sync.WaitGroup
	for _, worker := range []persistence.WorkerInstance{workerA, workerB} {
		worker := worker
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := service.ClaimWorkspaceCleanup(context.Background(), worker, WorkspaceCleanupClaimInput{}, uuid.NewString())
			outcomes <- claimOutcome{result: result, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)

	var claimed *WorkspaceCleanupClaim
	var claimedBy persistence.WorkerInstance
	emptyClaims := 0
	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.result.Value.Cleanup == nil {
			emptyClaims++
			continue
		}
		claimed = outcome.result.Value.Cleanup
		var command persistence.WorkspaceCleanupCommand
		if err := db.Where("id = ?", claimed.CleanupID).Take(&command).Error; err != nil {
			t.Fatal(err)
		}
		if command.DeliveryWorkerID == nil {
			t.Fatal("claimed cleanup has no delivery Worker")
		}
		if *command.DeliveryWorkerID == workerA.ID {
			claimedBy = workerA
		} else {
			claimedBy = workerB
		}
	}
	if claimed == nil || emptyClaims != 1 {
		t.Fatalf("expected one claimed and one empty cleanup result, claimed=%#v empty=%d", claimed, emptyClaims)
	}
	if claimed.MaterializationID != materialization.ID || claimed.IncarnationID != materialization.IncarnationID ||
		claimed.LogicalWorkspaceID != workspace.ID || claimed.StorageScope != "target" || claimed.LayoutVersion != 3 {
		t.Fatalf("claim omitted or changed physical fencing metadata: %#v", claimed)
	}
	if claimed.Lease.LeaseToken == "" || claimed.Lease.DispatchGeneration != claimed.DispatchGeneration {
		t.Fatalf("invalid cleanup lease envelope: %#v", claimed.Lease)
	}

	leaseInput := WorkspaceCleanupLeaseInput{
		DispatchGeneration: claimed.DispatchGeneration, LeaseToken: claimed.Lease.LeaseToken,
	}
	started, err := service.StartWorkspaceCleanup(context.Background(), claimedBy, claimed.CleanupID, leaseInput, "cleanup-start-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if started.Value.Status != "running" {
		t.Fatalf("cleanup status after start = %q", started.Value.Status)
	}
	requestID := "cleanup-ack-" + uuid.NewString()
	acknowledged, err := service.AcknowledgeWorkspaceCleanup(context.Background(), claimedBy, claimed.CleanupID, leaseInput, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.Value.Status != "acknowledged" {
		t.Fatalf("cleanup status after acknowledgement = %q", acknowledged.Value.Status)
	}
	replayed, err := service.AcknowledgeWorkspaceCleanup(context.Background(), claimedBy, claimed.CleanupID, leaseInput, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Value.Status != "acknowledged" {
		t.Fatalf("cleanup acknowledgement did not replay idempotently: %#v", replayed)
	}
	if err := db.Where("id = ?", materialization.ID).Take(&materialization).Error; err != nil {
		t.Fatal(err)
	}
	if materialization.State != "cleaned" || materialization.CleanedAt == nil {
		t.Fatalf("materialization was not cleaned: %#v", materialization)
	}
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.State != "cleaned" || workspace.CleanedAt == nil {
		t.Fatalf("current logical Workspace was not cleaned: %#v", workspace)
	}
}

func TestWorkspaceCleanupClaimRequiresCurrentOnlineWorker(t *testing.T) {
	db := integrationDB(t)
	fixture, _, materialization := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }

	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands created = %d, want 1", created)
	}
	legacyWorker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-protocol-v1")
	offlineWorker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-offline")
	drainingWorker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-draining")
	onlineWorker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-online")
	cleanupWorkers(t, db, legacyWorker.ID, offlineWorker.ID, drainingWorker.ID, onlineWorker.ID)
	if err := db.Model(&persistence.WorkerInstance{}).
		Where("id = ? AND incarnation = ?", legacyWorker.ID, legacyWorker.Incarnation).
		Update("protocol_version", 1).Error; err != nil {
		t.Fatal(err)
	}
	legacyWorker.ProtocolVersion = 1
	if err := db.Model(&persistence.WorkerInstance{}).
		Where("id = ? AND incarnation = ?", offlineWorker.ID, offlineWorker.Incarnation).
		Update("status", "offline").Error; err != nil {
		t.Fatal(err)
	}
	draining := true
	if _, err := service.Heartbeat(context.Background(), drainingWorker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion, Draining: &draining,
	}); err != nil {
		t.Fatal(err)
	}

	assertCommandPending := func() {
		t.Helper()
		var command persistence.WorkspaceCleanupCommand
		if err := db.Where("tenant_id = ? AND materialization_id = ?", fixture.TenantID, materialization.ID).
			Take(&command).Error; err != nil {
			t.Fatal(err)
		}
		if command.Status != "pending" || command.DispatchGeneration != 0 || command.DeliveryAttempts != 0 ||
			command.DeliveryWorkerID != nil || command.DeliveryWorkerIncarnation != nil ||
			len(command.LeaseTokenHash) != 0 || command.LeaseExpiresAt != nil {
			t.Fatalf("ineligible Worker changed the pending cleanup command: %#v", command)
		}
	}
	_, err = service.ClaimWorkspaceCleanup(
		context.Background(), legacyWorker, WorkspaceCleanupClaimInput{}, "cleanup-claim-protocol-v1",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != 426 || apiError.Code != "worker_protocol_version_unsupported" ||
		apiError.Details["received"] != 1 || apiError.Details["minimumSupported"] != WorkerProtocolVersion {
		t.Fatalf("legacy Worker cleanup claim returned unexpected error: %#v", apiError)
	}
	assertCommandPending()
	_, err = service.ClaimWorkspaceCleanup(
		context.Background(), offlineWorker, WorkspaceCleanupClaimInput{}, "cleanup-claim-offline",
	)
	assertProblemCode(t, err, "worker_not_claimable")
	assertCommandPending()
	_, err = service.ClaimWorkspaceCleanup(
		context.Background(), drainingWorker, WorkspaceCleanupClaimInput{}, "cleanup-claim-draining",
	)
	assertProblemCode(t, err, "worker_not_claimable")
	assertCommandPending()

	claimed, err := service.ClaimWorkspaceCleanup(
		context.Background(), onlineWorker, WorkspaceCleanupClaimInput{}, "cleanup-claim-online",
	)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Value.Cleanup == nil || claimed.Value.Cleanup.MaterializationID != materialization.ID {
		t.Fatalf("current online Worker did not claim the pending cleanup: %#v", claimed.Value)
	}
	var command persistence.WorkspaceCleanupCommand
	if err := db.Where("tenant_id = ? AND materialization_id = ?", fixture.TenantID, materialization.ID).
		Take(&command).Error; err != nil {
		t.Fatal(err)
	}
	if command.Status != "leased" || command.DeliveryWorkerID == nil || *command.DeliveryWorkerID != onlineWorker.ID ||
		command.DeliveryWorkerIncarnation == nil || *command.DeliveryWorkerIncarnation != onlineWorker.Incarnation ||
		command.DispatchGeneration != 1 || command.DeliveryAttempts != 1 {
		t.Fatalf("online Worker claim did not fence the cleanup lease: %#v", command)
	}
	if _, err := service.Heartbeat(context.Background(), onlineWorker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion, Draining: &draining,
	}); err != nil {
		t.Fatal(err)
	}
	leaseInput := WorkspaceCleanupLeaseInput{
		DispatchGeneration: claimed.Value.Cleanup.DispatchGeneration,
		LeaseToken:         claimed.Value.Cleanup.Lease.LeaseToken,
	}
	if _, err := service.StartWorkspaceCleanup(
		context.Background(), onlineWorker, claimed.Value.Cleanup.CleanupID, leaseInput, "cleanup-start-after-drain",
	); err != nil {
		t.Fatalf("same Worker incarnation could not start its existing cleanup lease after draining: %v", err)
	}
	if _, err := service.AcknowledgeWorkspaceCleanup(
		context.Background(), onlineWorker, claimed.Value.Cleanup.CleanupID, leaseInput, "cleanup-ack-after-drain",
	); err != nil {
		t.Fatalf("same Worker incarnation could not finish its existing cleanup lease after draining: %v", err)
	}
}

func TestWorkspaceCleanupLocksLogicalWorkspaceBeforeMaterialization(t *testing.T) {
	db := integrationDB(t)
	_, workspace, materialization := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	var lockedWorkspace persistence.RemoteWorkspace
	if err := persistence.WithLocking(tx, "UPDATE", "").Where("id = ?", workspace.ID).Take(&lockedWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		created bool
		err     error
	}
	started := make(chan struct{})
	done := make(chan outcome, 1)
	go func() {
		close(started)
		created, err := service.enqueueWorkspaceCleanup(context.Background(), materialization.ID, now)
		done <- outcome{created: created, err: err}
	}()
	<-started
	time.Sleep(100 * time.Millisecond)
	if err := tx.Exec("SET LOCAL lock_timeout = '500ms'").Error; err != nil {
		t.Fatal(err)
	}
	var lockedMaterialization persistence.WorkspaceMaterialization
	if err := persistence.WithLocking(tx, "UPDATE", "").Where("id = ?", materialization.ID).Take(&lockedMaterialization).Error; err != nil {
		t.Fatalf("cleanup locked Materialization before the logical Workspace: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || !result.created {
		t.Fatalf("cleanup after ordered lock release: created=%v err=%v", result.created, result.err)
	}
}

func TestWorkspaceCleanupBlocksActiveExecutionAndFencesExpiredLease(t *testing.T) {
	db := integrationDB(t)
	fixture, _, materialization := seedWorkspaceCleanupFixture(t, db, true)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }

	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("cleanup was queued with an active Execution: %d", created)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"status": "completed", "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	created, err = service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands created after Execution completion = %d, want 1", created)
	}

	workerA := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-expiry-a")
	workerB := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-expiry-b")
	cleanupWorkers(t, db, workerA.ID, workerB.ID)
	first, err := service.ClaimWorkspaceCleanup(context.Background(), workerA, WorkspaceCleanupClaimInput{}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if first.Value.Cleanup == nil {
		t.Fatal("first Worker did not claim cleanup")
	}
	firstClaim := first.Value.Cleanup
	if err := db.Model(&persistence.WorkspaceCleanupCommand{}).Where("id = ?", firstClaim.CleanupID).
		Update("lease_expires_at", now.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	second, err := service.ClaimWorkspaceCleanup(context.Background(), workerB, WorkspaceCleanupClaimInput{}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if second.Value.Cleanup == nil {
		t.Fatal("second Worker did not reclaim the expired cleanup")
	}
	if second.Value.Cleanup.DispatchGeneration != firstClaim.DispatchGeneration+1 {
		t.Fatalf("dispatch generation after reclaim = %d, want %d",
			second.Value.Cleanup.DispatchGeneration, firstClaim.DispatchGeneration+1)
	}
	_, err = service.RenewWorkspaceCleanup(context.Background(), workerA, firstClaim.CleanupID, WorkspaceCleanupLeaseInput{
		DispatchGeneration: firstClaim.DispatchGeneration, LeaseToken: firstClaim.Lease.LeaseToken,
	}, uuid.NewString())
	assertProblemCode(t, err, "workspace_cleanup_lease_fenced")

	var current persistence.WorkspaceMaterialization
	if err := db.Where("id = ?", materialization.ID).Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.State != "cleanup-pending" {
		t.Fatalf("materialization state after lease recovery = %q, want cleanup-pending", current.State)
	}
}

func TestDirtyWorkspaceCleanupRequiresExactReadyCheckpoint(t *testing.T) {
	db := integrationDB(t)
	fixture, workspace, _ := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	if err := db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
		Update("state", "dirty").Error; err != nil {
		t.Fatal(err)
	}

	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("dirty Workspace cleanup was queued without a ready Checkpoint: %d", created)
	}
	headCommit := "0123456789abcdef0123456789abcdef01234567"
	branch := "synara/cleanup-checkpoint"
	readyAt := now.Add(-time.Minute)
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: fixture.TenantID, WorkspaceID: workspace.ID,
		SessionID: fixture.SessionID, TurnID: &fixture.TurnID, ExecutionID: fixture.ExecutionID,
		Generation: 1, IdempotencyKey: "cleanup-ready-checkpoint", Strategy: "git-reference",
		Status: "ready", HeadCommit: &headCommit, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-git-reference-v1", "headCommit": headCommit},
		CreatedAt: readyAt, ReadyAt: &readyAt,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&checkpoint).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
			Update("current_checkpoint_id", checkpoint.ID).Error
	}); err != nil {
		t.Fatal(err)
	}
	created, err = service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("dirty Workspace cleanup commands after exact ready Checkpoint = %d, want 1", created)
	}
}

func TestWorkspaceCleanupBlocksIncompleteCheckpoint(t *testing.T) {
	db := integrationDB(t)
	fixture, workspace, _ := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: fixture.TenantID, WorkspaceID: workspace.ID,
		SessionID: fixture.SessionID, TurnID: &fixture.TurnID, ExecutionID: fixture.ExecutionID,
		Generation: 1, IdempotencyKey: "cleanup-pending-checkpoint", Strategy: "git-reference",
		Status: "pending", Manifest: map[string]any{"format": "synara-git-reference-v1"}, CreatedAt: now,
	}
	if err := db.Create(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("cleanup was queued with an incomplete Checkpoint: %d", created)
	}
	failureCode := "checkpoint-test-failed"
	failedAt := now.Add(time.Second)
	if err := db.Model(&persistence.WorkspaceCheckpoint{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, checkpoint.ID).
		Updates(map[string]any{
			"status": "failed", "failure_code": failureCode,
			"failure_message": "test completed", "failed_at": failedAt,
		}).Error; err != nil {
		t.Fatal(err)
	}
	created, err = service.ReconcileWorkspaceCleanup(context.Background(), failedAt, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands after Checkpoint failure = %d, want 1", created)
	}
}

func TestWorkspaceCleanupBlocksAnyPendingExecutionArtifact(t *testing.T) {
	db := integrationDB(t)
	fixture, workspace, _ := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	artifact := persistence.Artifact{
		ID: uuid.New(), TenantID: fixture.TenantID, OrganizationID: workspace.OrganizationID,
		ProjectID: workspace.ProjectID, SessionID: fixture.SessionID, ExecutionID: &fixture.ExecutionID,
		Kind: "generated_file", Status: "pending", Bucket: "cleanup-test",
		ObjectKey: "pending/" + uuid.NewString(), CreatedByType: "user", CreatedByID: fixture.UserID,
		CreatedAt: now,
	}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("cleanup was queued with a pending generated-file Artifact: %d", created)
	}
	if err := db.Model(&persistence.Artifact{}).Where("id = ?", artifact.ID).Update("status", "failed").Error; err != nil {
		t.Fatal(err)
	}
	created, err = service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands after Artifact failure = %d, want 1", created)
	}
}

func TestWorkspaceCleanupTerminalFailureDoesNotResetAttempts(t *testing.T) {
	db := integrationDB(t)
	fixture, _, materialization := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	if created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10); err != nil || created != 1 {
		t.Fatalf("initial cleanup reconcile = %d, %v", created, err)
	}
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-terminal-failure")
	cleanupWorkers(t, db, worker.ID)
	claimed, err := service.ClaimWorkspaceCleanup(context.Background(), worker, WorkspaceCleanupClaimInput{}, uuid.NewString())
	if err != nil || claimed.Value.Cleanup == nil {
		t.Fatalf("claim cleanup: %#v, %v", claimed.Value, err)
	}
	claim := claimed.Value.Cleanup
	failed, err := service.FailWorkspaceCleanup(context.Background(), worker, claim.CleanupID, WorkspaceCleanupFailedInput{
		WorkspaceCleanupLeaseInput: WorkspaceCleanupLeaseInput{
			DispatchGeneration: claim.DispatchGeneration, LeaseToken: claim.Lease.LeaseToken,
		},
		ErrorCode: "workspace_manifest_mismatch", ErrorMessage: "manual reconciliation required", Retryable: false,
	}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if failed.Value.Status != "failed" {
		t.Fatalf("cleanup status = %q, want failed", failed.Value.Status)
	}
	if created, err := service.ReconcileWorkspaceCleanup(context.Background(), now.Add(time.Minute), 10); err != nil || created != 0 {
		t.Fatalf("terminal cleanup failure was automatically re-enqueued: created=%d err=%v", created, err)
	}
	var commands int64
	if err := db.Model(&persistence.WorkspaceCleanupCommand{}).
		Where("tenant_id = ? AND materialization_id = ?", fixture.TenantID, materialization.ID).
		Count(&commands).Error; err != nil {
		t.Fatal(err)
	}
	if commands != 1 {
		t.Fatalf("cleanup command count = %d, want 1", commands)
	}
}

func TestWorkspaceCleanupExpiredFinalAttemptBecomesTerminal(t *testing.T) {
	db := integrationDB(t)
	fixture, _, materialization := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	if created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10); err != nil || created != 1 {
		t.Fatalf("initial cleanup reconcile = %d, %v", created, err)
	}
	if err := db.Model(&persistence.WorkspaceCleanupCommand{}).
		Where("tenant_id = ? AND materialization_id = ?", fixture.TenantID, materialization.ID).
		Update("delivery_attempts", workspaceCleanupMaxAttempts-1).Error; err != nil {
		t.Fatal(err)
	}
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-final-expiry")
	cleanupWorkers(t, db, worker.ID)
	claimed, err := service.ClaimWorkspaceCleanup(context.Background(), worker, WorkspaceCleanupClaimInput{}, uuid.NewString())
	if err != nil || claimed.Value.Cleanup == nil {
		t.Fatalf("claim final cleanup attempt: %#v, %v", claimed.Value, err)
	}
	service.now = func() time.Time { return now.Add(time.Minute) }
	if recovered, err := service.RecoverExpiredWorkspaceCleanupLeases(context.Background(), now.Add(time.Minute), 10); err != nil || recovered != 1 {
		t.Fatalf("recover final expired cleanup lease = %d, %v", recovered, err)
	}
	var command persistence.WorkspaceCleanupCommand
	if err := db.Where("id = ?", claimed.Value.Cleanup.CleanupID).Take(&command).Error; err != nil {
		t.Fatal(err)
	}
	if command.Status != "failed" || command.LastErrorCode == nil || *command.LastErrorCode != "workspace_cleanup_attempts_exhausted" {
		t.Fatalf("final expired cleanup attempt was requeued: %#v", command)
	}
	if created, err := service.ReconcileWorkspaceCleanup(context.Background(), now.Add(2*time.Minute), 10); err != nil || created != 0 {
		t.Fatalf("exhausted cleanup was replaced by a new command: created=%d err=%v", created, err)
	}
}

func TestPodCleanupNoOpsOnlyForProvablyUnmaterializedV3(t *testing.T) {
	db := integrationDB(t)
	fixture, workspace, _ := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	if err := db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
		Update("retention_until", now.Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	reason := "tenant-delete"
	requestedAt := now.Add(-time.Minute)
	newPod := persistence.WorkspaceMaterialization{
		ID: uuid.New(), TenantID: fixture.TenantID, WorkspaceID: workspace.ID,
		OrganizationID: workspace.OrganizationID, ProjectID: workspace.ProjectID, SessionID: workspace.SessionID,
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, StorageScope: "pod",
		LayoutVersion: 3, IncarnationID: uuid.New(), State: "retired",
		CleanupReason: &reason, CleanupRequestedAt: &requestedAt, CreatedAt: now, UpdatedAt: now,
	}
	legacyPod := newPod
	legacyPod.ID = uuid.New()
	legacyPod.IncarnationID = uuid.New()
	legacyPod.LayoutVersion = 2
	if err := db.Create(&newPod).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyPod).Error; err != nil {
		t.Fatal(err)
	}
	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 2 {
		t.Fatalf("Pod cleanup commands created = %d, want 2", created)
	}
	if err := db.Where("id = ?", newPod.ID).Take(&newPod).Error; err != nil {
		t.Fatal(err)
	}
	if newPod.State != "cleaned" || newPod.CleanedAt == nil {
		t.Fatalf("provably unmaterialized v3 Pod was not no-op acknowledged: %#v", newPod)
	}
	if err := db.Where("id = ?", legacyPod.ID).Take(&legacyPod).Error; err != nil {
		t.Fatal(err)
	}
	if legacyPod.State != "failed" || legacyPod.FailureCode == nil || *legacyPod.FailureCode != "workspace_cleanup_placement_unknown" {
		t.Fatalf("uncertain legacy Pod cleanup did not require reconciliation: %#v", legacyPod)
	}
}

func TestEphemeralWorkspaceCleanupRequiresAbsentExactPodUID(t *testing.T) {
	db := integrationDB(t)
	fixture, workspace, _ := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	worker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-ephemeral")
	cleanupWorkers(t, db, worker.ID)
	reason := "worker-instance-replaced"
	requestedAt := now.Add(-time.Minute)
	podMaterialization := persistence.WorkspaceMaterialization{
		ID: uuid.New(), TenantID: fixture.TenantID, WorkspaceID: workspace.ID,
		OrganizationID: workspace.OrganizationID, ProjectID: workspace.ProjectID, SessionID: workspace.SessionID,
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		StorageScope: "pod", LayoutVersion: 3, IncarnationID: uuid.New(),
		WorkerID: &worker.ID, WorkerIncarnation: &worker.Incarnation, WorkerInstanceUID: &worker.InstanceUID,
		LastExecutionID: &fixture.ExecutionID, LastGeneration: pointerInt64(1), State: "retired",
		CleanupReason: &reason, CleanupRequestedAt: &requestedAt, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}
	if err := db.Create(&podMaterialization).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
		Update("retention_until", now.Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("ephemeral cleanup commands created = %d, want 1", created)
	}
	acknowledged, err := service.ReconcileEphemeralWorkspaceCleanup(
		context.Background(), fixture.TargetID, []string{worker.InstanceUID}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged != 0 {
		t.Fatalf("present Pod materialization was acknowledged: %d", acknowledged)
	}
	acknowledged, err = service.ReconcileEphemeralWorkspaceCleanup(
		context.Background(), fixture.TargetID, nil, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged != 1 {
		t.Fatalf("absent Pod materializations acknowledged = %d, want 1", acknowledged)
	}
	if err := db.Where("id = ?", podMaterialization.ID).Take(&podMaterialization).Error; err != nil {
		t.Fatal(err)
	}
	if podMaterialization.State != "cleaned" {
		t.Fatalf("ephemeral materialization state = %q, want cleaned", podMaterialization.State)
	}
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.State == "cleaned" {
		t.Fatal("acknowledging a retired Pod materialization cleaned the current logical Workspace")
	}
}

func TestDirectClaimRematerializesLegacyPodWithUnknownWorkerUID(t *testing.T) {
	db := integrationDB(t)
	fixture, workspace, seededMaterialization := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }

	legacyMaterialization := seededMaterialization
	legacyMaterialization.ID = uuid.New()
	legacyMaterialization.IncarnationID = uuid.New()
	legacyMaterialization.StorageScope = "pod"
	legacyMaterialization.LayoutVersion = 2
	legacyMaterialization.WorkerID = nil
	legacyMaterialization.WorkerIncarnation = nil
	legacyMaterialization.WorkerInstanceUID = nil
	legacyMaterialization.State = "active"
	legacyMaterialization.CleanupReason = nil
	legacyMaterialization.CleanupRequestedAt = nil
	legacyMaterialization.FailureCode = nil
	legacyMaterialization.FailureMessage = nil
	legacyMaterialization.FailedAt = nil
	legacyMaterialization.CleanedAt = nil

	queuedTurnID := uuid.New()
	queuedExecutionID := uuid.New()
	queuedTurn := persistence.AgentTurn{
		ID: queuedTurnID, TenantID: fixture.TenantID, SessionID: fixture.SessionID,
		CreatedBy: fixture.UserID, Status: "queued", InputText: "Claim migrated legacy Workspace",
		RuntimeMode: "approval-required", InteractionMode: "plan", CreatedAt: now,
	}
	queuedExecution := persistence.AgentExecution{
		ID: queuedExecutionID, TenantID: fixture.TenantID, SessionID: fixture.SessionID,
		TurnID: queuedTurnID, Attempt: 1, Status: "queued",
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		RemoteWorkspaceID: &workspace.ID, WorkspaceMaterializationID: &legacyMaterialization.ID,
		Generation: 0, RequestedBy: fixture.UserID, QueuedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Update("workspace_materialization_id", nil).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
			Updates(map[string]any{
				"state": "dirty", "current_materialization_id": nil, "current_checkpoint_id": nil,
			}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&seededMaterialization).Error; err != nil {
			return err
		}
		if err := tx.Create(&legacyMaterialization).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
			Update("current_materialization_id", legacyMaterialization.ID).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Update("workspace_materialization_id", legacyMaterialization.ID).Error; err != nil {
			return err
		}
		if err := tx.Create(&queuedTurn).Error; err != nil {
			return err
		}
		return tx.Create(&queuedExecution).Error
	}); err != nil {
		t.Fatalf("seed migrated legacy Workspace: %v", err)
	}

	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "legacy-workspace-claim")
	cleanupWorkers(t, db, worker.ID)
	claimInput := ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &queuedExecutionID,
	}
	_, err := service.Claim(context.Background(), worker, claimInput, "legacy-workspace-claim-without-checkpoint")
	assertProblemCode(t, err, "workspace_recovery_checkpoint_required")

	var rejectedExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, queuedExecutionID).
		Take(&rejectedExecution).Error; err != nil {
		t.Fatal(err)
	}
	if rejectedExecution.Status != "queued" || rejectedExecution.Generation != 0 || rejectedExecution.WorkerID != nil ||
		rejectedExecution.WorkspaceMaterializationID == nil ||
		*rejectedExecution.WorkspaceMaterializationID != legacyMaterialization.ID {
		t.Fatalf("failed legacy rematerialization changed the queued Execution: %#v", rejectedExecution)
	}
	var rejectedLegacy persistence.WorkspaceMaterialization
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, legacyMaterialization.ID).
		Take(&rejectedLegacy).Error; err != nil {
		t.Fatal(err)
	}
	if rejectedLegacy.State != "active" || rejectedLegacy.WorkerInstanceUID != nil {
		t.Fatalf("failed legacy rematerialization changed the unknown placement: %#v", rejectedLegacy)
	}

	headCommit := "0123456789abcdef0123456789abcdef01234567"
	branch := "synara/legacy-workspace-recovery"
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: fixture.TenantID, WorkspaceID: workspace.ID,
		SessionID: fixture.SessionID, TurnID: &fixture.TurnID, ExecutionID: fixture.ExecutionID,
		Generation: 1, IdempotencyKey: "legacy-workspace-ready-checkpoint", Strategy: "git-reference",
		Status: "ready", HeadCommit: &headCommit, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-git-reference-v1", "headCommit": headCommit},
		CreatedAt: now, ReadyAt: &now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&checkpoint).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
			Update("current_checkpoint_id", checkpoint.ID).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, queuedExecutionID).
			Update("restore_checkpoint_id", checkpoint.ID).Error
	}); err != nil {
		t.Fatalf("seed exact ready recovery Checkpoint: %v", err)
	}

	now = now.Add(time.Second)
	claimed, err := service.Claim(context.Background(), worker, claimInput, "legacy-workspace-direct-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Value.Execution == nil || claimed.Value.Execution.ID != queuedExecutionID ||
		claimed.Value.Lease == nil || claimed.Value.Workload == nil {
		t.Fatalf("legacy queued Execution was not claimed: %#v", claimed.Value)
	}
	workload := claimed.Value.Workload
	if workload.WorkspaceMaterializationID == nil || *workload.WorkspaceMaterializationID == legacyMaterialization.ID ||
		workload.WorkspaceMaterializationIncarnationID == nil ||
		workload.WorkspaceLayoutVersion != workspaceLayoutVersionCurrent ||
		workload.RestoreCheckpointID == nil || *workload.RestoreCheckpointID != checkpoint.ID ||
		workload.RestoreCheckpoint == nil || workload.RestoreCheckpoint.ID != checkpoint.ID {
		t.Fatalf("legacy direct claim omitted the new fenced materialization or recovery Checkpoint: %#v", workload)
	}

	var retired persistence.WorkspaceMaterialization
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, legacyMaterialization.ID).
		Take(&retired).Error; err != nil {
		t.Fatal(err)
	}
	if retired.State != "retired" || retired.CleanupReason == nil || *retired.CleanupReason != "worker-instance-replaced" ||
		retired.CleanupRequestedAt == nil || retired.WorkerInstanceUID != nil {
		t.Fatalf("legacy unknown Pod materialization was not retired safely: %#v", retired)
	}
	var current persistence.WorkspaceMaterialization
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, *workload.WorkspaceMaterializationID).
		Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.State != "active" || current.StorageScope != "pod" || current.LayoutVersion != workspaceLayoutVersionCurrent ||
		current.WorkerID == nil || *current.WorkerID != worker.ID ||
		current.WorkerIncarnation == nil || *current.WorkerIncarnation != worker.Incarnation ||
		current.WorkerInstanceUID == nil || *current.WorkerInstanceUID != worker.InstanceUID ||
		current.LastExecutionID == nil || *current.LastExecutionID != queuedExecutionID ||
		current.LastGeneration == nil || *current.LastGeneration != claimed.Value.Lease.Generation {
		t.Fatalf("replacement materialization was not fenced to the claiming Worker: %#v", current)
	}
	var currentWorkspace persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
		Take(&currentWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	if currentWorkspace.CurrentMaterializationID == nil || *currentWorkspace.CurrentMaterializationID != current.ID ||
		currentWorkspace.CurrentCheckpointID == nil || *currentWorkspace.CurrentCheckpointID != checkpoint.ID ||
		currentWorkspace.State != "recovering" {
		t.Fatalf("logical Workspace did not advance to the replacement materialization: %#v", currentWorkspace)
	}
}

func TestPodReplacementRejectsDirtyWorkspaceWithoutExactReadyCheckpoint(t *testing.T) {
	db := integrationDB(t)
	fixture, workspace, materialization := seedWorkspaceCleanupFixture(t, db, false)
	service := integrationService(t, db)
	oldWorker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-old-pod")
	newWorker := registerTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cleanup-new-pod")
	cleanupWorkers(t, db, oldWorker.ID, newWorker.ID)
	podMaterialization := materialization
	podMaterialization.ID = uuid.New()
	podMaterialization.IncarnationID = uuid.New()
	podMaterialization.StorageScope = "pod"
	podMaterialization.WorkerID = &oldWorker.ID
	podMaterialization.WorkerIncarnation = &oldWorker.Incarnation
	podMaterialization.WorkerInstanceUID = &oldWorker.InstanceUID
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&podMaterialization).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
			Updates(map[string]any{"state": "dirty", "current_materialization_id": podMaterialization.ID}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Update("workspace_materialization_id", podMaterialization.ID).Error
	}); err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		_, err := ensureExecutionWorkspaceMaterialization(context.Background(), tx, newWorker, &execution, time.Now().UTC())
		return err
	})
	assertProblemCode(t, err, "workspace_recovery_checkpoint_required")

	var current persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.CurrentMaterializationID == nil || *current.CurrentMaterializationID != podMaterialization.ID {
		t.Fatalf("dirty Workspace current materialization changed after rejected replacement: %#v", current.CurrentMaterializationID)
	}
}

func seedWorkspaceCleanupFixture(
	t *testing.T,
	db *gorm.DB,
	activeExecution bool,
) (executionFixture, persistence.RemoteWorkspace, persistence.WorkspaceMaterialization) {
	t.Helper()
	fixture := seedExecutionFixture(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	createdAt := now.Add(-48 * time.Hour)
	retentionUntil := now.Add(-time.Hour)
	workspace := persistence.RemoteWorkspace{
		ID: uuid.New(), TenantID: fixture.TenantID, SessionID: fixture.SessionID,
		ExecutionTargetID: fixture.TargetID, WorkspaceMode: "clone", State: "ready",
		DefaultBranch: "main", LastExecutionID: &fixture.ExecutionID,
		LastGeneration: pointerInt64(1), RetentionUntil: &retentionUntil,
		LastUsedAt: &createdAt, CreatedAt: createdAt, UpdatedAt: now,
	}
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	workspace.OrganizationID = session.OrganizationID
	workspace.ProjectID = session.ProjectID
	materialization := persistence.WorkspaceMaterialization{
		ID: uuid.New(), TenantID: fixture.TenantID, WorkspaceID: workspace.ID,
		OrganizationID: session.OrganizationID, ProjectID: session.ProjectID, SessionID: fixture.SessionID,
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		StorageScope: "target", LayoutVersion: 3, IncarnationID: uuid.New(),
		LastExecutionID: &fixture.ExecutionID, LastGeneration: pointerInt64(1), State: "active",
		CreatedAt: createdAt, UpdatedAt: now,
	}
	executionStatus := "completed"
	var finishedAt any = now
	if activeExecution {
		executionStatus = "queued"
		finishedAt = nil
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Updates(map[string]any{
				"status": executionStatus, "generation": 1, "finished_at": finishedAt,
			}).Error; err != nil {
			return err
		}
		if err := tx.Create(&workspace).Error; err != nil {
			return err
		}
		if err := tx.Create(&materialization).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).
			Update("current_materialization_id", materialization.ID).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Updates(map[string]any{
				"remote_workspace_id":          workspace.ID,
				"workspace_materialization_id": materialization.ID,
			}).Error
	}); err != nil {
		t.Fatalf("seed Workspace cleanup fixture: %v", err)
	}
	return fixture, workspace, materialization
}

func pointerInt64(value int64) *int64 { return &value }

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected problem code %q, got nil", code)
	}
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("problem error = %v, want code %q", err, code)
	}
}
