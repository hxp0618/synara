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

func setupExpiredManagedLeaseRecovery(
	t *testing.T,
	readyCheckpoint bool,
) (*gorm.DB, *Service, executionFixture, persistence.AgentExecution, persistence.WorkerLease) {
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
