package executions

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestRecoverExpiredDrainingManagedLeasePersistsCheckpointRisk(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		readyCheckpoint bool
		wantRisk        bool
	}{
		{name: "checkpoint unconfirmed", wantRisk: true},
		{name: "current generation checkpoint ready", readyCheckpoint: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db, service, fixture, execution, lease := setupExpiredManagedLeaseRecovery(
				t, testCase.readyCheckpoint,
			)
			if err := service.RecoverExpired(context.Background(), 10); err != nil {
				t.Fatal(err)
			}

			var event persistence.SessionEvent
			if err := db.Where(
				"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
				fixture.TenantID, fixture.SessionID, fixture.ExecutionID, "execution.recovering",
			).Order("sequence DESC").Take(&event).Error; err != nil {
				t.Fatal(err)
			}
			if event.Payload["reason"] != "lease_expired" {
				t.Fatalf("unexpected recovery reason: %#v", event.Payload)
			}
			assertRecoveryRisk(t, event.Payload, testCase.wantRisk)

			var message persistence.OutboxMessage
			if err := db.Where(
				"tenant_id = ? AND topic = ? AND message_key = ?",
				fixture.TenantID, "execution.recovering",
				execution.ID.String()+":"+formatGeneration(lease.Generation),
			).Take(&message).Error; err != nil {
				t.Fatal(err)
			}
			if message.Payload["reason"] != "lease_expired" {
				t.Fatalf("unexpected recovery Outbox reason: %#v", message.Payload)
			}
			assertRecoveryRisk(t, message.Payload, testCase.wantRisk)
		})
	}
}

func TestRecoverExpiredFailsUploadingCheckpointBeforeNextGeneration(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)

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

	firstWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "checkpoint-recovery-first")
	secondWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "checkpoint-recovery-second")
	cleanupWorkers(t, db, firstWorker.ID, secondWorker.ID)
	firstClaim, err := service.Claim(ctx, firstWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "checkpoint-recovery-first-claim")
	if err != nil || firstClaim.Value.Lease == nil {
		t.Fatalf("claim first Generation: %#v, %v", firstClaim, err)
	}
	firstLease := *firstClaim.Value.Lease
	firstLeaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
	}
	fingerprint := strings.Repeat("a", 64)
	branch := "synara/checkpoint-recovery"
	baseCommit := strings.Repeat("b", 40)
	readyHead := strings.Repeat("c", 40)
	if _, err := service.MarkWorkspaceReady(ctx, firstWorker, fixture.ExecutionID, WorkspaceReadyInput{
		LeaseInput: firstLeaseInput, RepositoryFingerprint: &fingerprint,
		CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &readyHead,
	}, "checkpoint-recovery-workspace-ready"); err != nil {
		t.Fatal(err)
	}
	readyCheckpoint, err := service.CreateWorkspaceCheckpoint(ctx, firstWorker, fixture.ExecutionID, CreateWorkspaceCheckpointInput{
		LeaseInput: firstLeaseInput, IdempotencyKey: "checkpoint-recovery-ready", Strategy: "git-reference",
		BaseCommit: &baseCommit, HeadCommit: &readyHead, CurrentBranch: &branch,
		Manifest: map[string]any{"format": "synara-git-reference-v1", "headCommit": readyHead},
	}, "checkpoint-recovery-create-ready")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkWorkspaceCheckpointReady(
		ctx, firstWorker, fixture.ExecutionID, readyCheckpoint.Value.ID,
		WorkspaceCheckpointReadyInput{LeaseInput: firstLeaseInput}, "checkpoint-recovery-mark-ready",
	); err != nil {
		t.Fatal(err)
	}

	dirtyHead := strings.Repeat("d", 40)
	if _, err := service.MarkWorkspaceDirty(ctx, firstWorker, fixture.ExecutionID, WorkspaceDirtyInput{
		LeaseInput: firstLeaseInput, CurrentBranch: &branch, HeadCommit: &dirtyHead,
	}, "checkpoint-recovery-workspace-dirty"); err != nil {
		t.Fatal(err)
	}
	fileCount, totalBytes := 2, int64(128)
	uploadingCheckpoint, err := service.CreateWorkspaceCheckpoint(ctx, firstWorker, fixture.ExecutionID, CreateWorkspaceCheckpointInput{
		LeaseInput: firstLeaseInput, IdempotencyKey: "checkpoint-recovery-uploading", Strategy: "snapshot",
		BaseCommit: &baseCommit, HeadCommit: &dirtyHead, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-workspace-snapshot-v1", "files": []any{}},
		FileCount: &fileCount, TotalBytes: &totalBytes,
	}, "checkpoint-recovery-create-uploading")
	if err != nil {
		t.Fatal(err)
	}
	artifact := persistence.Artifact{
		ID: uuid.New(), TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: fixture.SessionID, ExecutionID: &fixture.ExecutionID,
		WorkspaceCheckpointID: &uploadingCheckpoint.Value.ID,
		Kind:                  "workspace_snapshot", Status: "pending", Bucket: "test",
		ObjectKey: "checkpoint/" + uuid.NewString(), CreatedByType: "worker", CreatedByID: firstWorker.ID,
		CreatedAt: service.now(),
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&artifact).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, uploadingCheckpoint.Value.ID).
			Updates(map[string]any{"status": "uploading", "artifact_id": artifact.ID}).Error
	}); err != nil {
		t.Fatal(err)
	}
	var bound persistence.WorkspaceCheckpoint
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, uploadingCheckpoint.Value.ID).Take(&bound).Error; err != nil {
		t.Fatal(err)
	}
	if bound.Status != "uploading" || bound.ArtifactID == nil || *bound.ArtifactID != artifact.ID {
		t.Fatalf("first Generation Checkpoint was not bound for upload: %#v", bound)
	}

	recoveryTime := service.now().Add(service.leaseTTL + time.Second)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("execution_id = ?", fixture.ExecutionID).
		Update("expires_at", recoveryTime.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return recoveryTime }
	if err := service.RecoverExpired(ctx, 10); err != nil {
		t.Fatal(err)
	}

	var failed persistence.WorkspaceCheckpoint
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, uploadingCheckpoint.Value.ID).Take(&failed).Error; err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.FailureCode == nil || *failed.FailureCode != "checkpoint_lease_inactive" ||
		failed.FailedAt == nil || failed.ArtifactID == nil || *failed.ArtifactID != artifact.ID {
		t.Fatalf("expired Generation Checkpoint was not failed: %#v", failed)
	}
	var recoveredWorkspace persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, workspace.ID).Take(&recoveredWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	if recoveredWorkspace.CurrentCheckpointID == nil || *recoveredWorkspace.CurrentCheckpointID != readyCheckpoint.Value.ID {
		t.Fatalf("recovery changed the last ready Checkpoint: %#v", recoveredWorkspace.CurrentCheckpointID)
	}
	var recoveredExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&recoveredExecution).Error; err != nil {
		t.Fatal(err)
	}
	if recoveredExecution.RestoreCheckpointID == nil || *recoveredExecution.RestoreCheckpointID != readyCheckpoint.Value.ID {
		t.Fatalf("recovery did not preserve the ready restore Checkpoint: %#v", recoveredExecution.RestoreCheckpointID)
	}

	secondClaim, err := service.Claim(ctx, secondWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "checkpoint-recovery-second-claim")
	if err != nil || secondClaim.Value.Lease == nil {
		t.Fatalf("claim second Generation: %#v, %v", secondClaim, err)
	}
	secondLease := *secondClaim.Value.Lease
	if secondLease.Generation != firstLease.Generation+1 {
		t.Fatalf("second Generation = %d, want %d", secondLease.Generation, firstLease.Generation+1)
	}
	secondLeaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: secondLease.Generation, LeaseToken: secondLease.LeaseToken,
	}
	if _, err := service.MarkWorkspaceReady(ctx, secondWorker, fixture.ExecutionID, WorkspaceReadyInput{
		LeaseInput: secondLeaseInput, RepositoryFingerprint: &fingerprint,
		CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &dirtyHead,
		RestoredCheckpointID: &readyCheckpoint.Value.ID,
	}, "checkpoint-recovery-second-ready"); err != nil {
		t.Fatal(err)
	}
	secondDirtyHead := strings.Repeat("e", 40)
	if _, err := service.MarkWorkspaceDirty(ctx, secondWorker, fixture.ExecutionID, WorkspaceDirtyInput{
		LeaseInput: secondLeaseInput, CurrentBranch: &branch, HeadCommit: &secondDirtyHead,
	}, "checkpoint-recovery-second-dirty"); err != nil {
		t.Fatal(err)
	}
	secondCheckpoint, err := service.CreateWorkspaceCheckpoint(ctx, secondWorker, fixture.ExecutionID, CreateWorkspaceCheckpointInput{
		LeaseInput: secondLeaseInput, IdempotencyKey: "checkpoint-recovery-second", Strategy: "snapshot",
		BaseCommit: &baseCommit, HeadCommit: &secondDirtyHead, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-workspace-snapshot-v1", "files": []any{}},
		FileCount: &fileCount, TotalBytes: &totalBytes,
	}, "checkpoint-recovery-create-second")
	if err != nil || secondCheckpoint.Value.Status != "pending" || secondCheckpoint.Value.Generation != secondLease.Generation {
		t.Fatalf("second Generation Checkpoint create: %#v, %v", secondCheckpoint, err)
	}
}

func setupSQLiteRecoveryService(t *testing.T) (*gorm.DB, *Service, executionFixture) {
	t.Helper()
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	return db, integrationService(t, db), seedExecutionFixture(t, db)
}

func setupExpiredManagedLeaseRecovery(
	t *testing.T,
	readyCheckpoint bool,
) (*gorm.DB, *Service, executionFixture, persistence.AgentExecution, persistence.WorkerLease) {
	t.Helper()
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)

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

	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "expired-draining-worker")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "expired-draining-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim managed Execution: %#v, %v", claim, err)
	}
	lease := *claim.Value.Lease
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}
	fingerprint := strings.Repeat("a", 64)
	branch := "synara/drain-risk"
	baseCommit := strings.Repeat("b", 40)
	headCommit := strings.Repeat("c", 40)
	if _, err := service.MarkWorkspaceReady(ctx, worker, fixture.ExecutionID, WorkspaceReadyInput{
		LeaseInput: leaseInput, RepositoryFingerprint: &fingerprint,
		CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &headCommit,
	}, "expired-draining-workspace-ready"); err != nil {
		t.Fatal(err)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if readyCheckpoint {
		readyAt := time.Now().UTC()
		checkpoint := persistence.WorkspaceCheckpoint{
			ID: uuid.New(), TenantID: fixture.TenantID, WorkspaceID: workspace.ID,
			SessionID: fixture.SessionID, TurnID: &fixture.TurnID, ExecutionID: fixture.ExecutionID,
			Generation: lease.Generation, IdempotencyKey: "drain-risk-ready", Strategy: "git-reference",
			Status: "ready", BaseCommit: &baseCommit, HeadCommit: &headCommit, CurrentBranch: &branch,
			Manifest: map[string]any{
				"format": "synara-git-reference-v1", "headCommit": headCommit, "currentBranch": branch,
			},
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
	}

	now := time.Now().UTC().Add(time.Minute)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).Updates(map[string]any{
			"status": "draining", "draining_at": now, "last_heartbeat_at": now,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkerLease{}).Where("execution_id = ?", fixture.ExecutionID).
			Update("expires_at", now.Add(-time.Second)).Error
	}); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }

	var persistedLease persistence.WorkerLease
	if err := db.Where("execution_id = ?", fixture.ExecutionID).Take(&persistedLease).Error; err != nil {
		t.Fatal(err)
	}
	return db, service, fixture, execution, persistedLease
}

func assertRecoveryRisk(t *testing.T, payload map[string]any, want bool) {
	t.Helper()
	risk, present := payload["risk"]
	if want {
		if !present || risk != workspaceCheckpointUnconfirmedRisk {
			t.Fatalf("recovery risk was not persisted: %#v", payload)
		}
		return
	}
	if present {
		t.Fatalf("ready Checkpoint produced a false data-loss risk: %#v", payload)
	}
}
