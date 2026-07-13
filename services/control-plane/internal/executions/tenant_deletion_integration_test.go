package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

func TestTenantDeletionInterruptsLiveLeaseAndCancelsInsteadOfRecovering(t *testing.T) {
	db := integrationDB(t)
	fixture, _, _ := seedWorkspaceCleanupFixture(t, db, true)
	service := integrationService(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "tenant-delete-live")
	cleanupWorkers(t, db, worker.ID)
	plainToken, tokenHash, err := secret.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if worker.CurrentManifestID == nil {
		t.Fatal("manifest Worker has no current Manifest")
	}
	provider := "codex"
	const activeGeneration int64 = 2
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Updates(map[string]any{
				"status": "running", "worker_id": worker.ID, "worker_manifest_id": *worker.CurrentManifestID,
				"provider": provider, "generation": activeGeneration, "started_at": now,
			}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.TurnID).
			Updates(map[string]any{"status": "running", "started_at": now}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.WorkerLease{
			ExecutionID: fixture.ExecutionID, TenantID: fixture.TenantID, WorkerID: worker.ID,
			Generation: activeGeneration, LeaseTokenHash: tokenHash, AcquiredAt: now, HeartbeatAt: now,
			ExpiresAt: now.Add(30 * time.Second),
		}).Error
	}); err != nil {
		t.Fatal(err)
	}

	var deletionEvents []persistence.SessionEvent
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		deletionEvents, err = service.PrepareTenantDeletion(
			context.Background(), tx, fixture.TenantID, fixture.UserID, now,
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(deletionEvents) != 1 || deletionEvents[0].EventType != "turn.interrupt-requested" {
		t.Fatalf("Tenant deletion events = %#v, want one interrupt request", deletionEvents)
	}
	var execution persistence.AgentExecution
	if err := db.Where("id = ?", fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "running" || execution.WorkerID == nil {
		t.Fatalf("live Execution was unsafely terminalized before its lease ended: %#v", execution)
	}
	var interruptCommands int64
	if err := db.Model(&persistence.ExecutionControlCommand{}).
		Where("tenant_id = ? AND execution_id = ? AND command_type = ? AND status = ?",
			fixture.TenantID, fixture.ExecutionID, "InterruptTurn", "pending").
		Count(&interruptCommands).Error; err != nil {
		t.Fatal(err)
	}
	if interruptCommands != 1 {
		t.Fatalf("Tenant deletion interrupt commands = %d, want 1", interruptCommands)
	}
	if err := db.Model(&persistence.Tenant{}).Where("id = ?", fixture.TenantID).
		Updates(map[string]any{"status": "deleting", "deleted_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.Renew(context.Background(), worker, fixture.ExecutionID, RenewLeaseInput{
		LeaseInput: LeaseInput{TenantID: fixture.TenantID, Generation: activeGeneration, LeaseToken: plainToken},
	}, uuid.NewString())
	assertProblemCode(t, err, "tenant_deleting")
	service.now = func() time.Time { return now.Add(time.Minute) }
	if err := service.RecoverExpired(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	execution = persistence.AgentExecution{}
	if err := db.Where("id = ?", fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "cancelled" || execution.FinishedAt == nil || execution.WorkerID != nil {
		t.Fatalf("expired Tenant deletion Execution recovered instead of cancelling: %#v", execution)
	}
	var recoveringEvents, cancelledEvents int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.ExecutionID, "execution.recovering").
		Count(&recoveringEvents).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type = ?", fixture.TenantID, fixture.ExecutionID, "execution.cancelled").
		Count(&cancelledEvents).Error; err != nil {
		t.Fatal(err)
	}
	if recoveringEvents != 0 || cancelledEvents != 1 {
		t.Fatalf("Tenant expiry events: recovering=%d cancelled=%d", recoveringEvents, cancelledEvents)
	}
}
