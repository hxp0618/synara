package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
)

func TestExpireInteractiveColdStartsUsesExactUnclaimedPoolSnapshot(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	now := time.Now().UTC().Truncate(time.Second)
	fixture.service.now = func() time.Time { return now }

	poolID := uuid.New()
	otherPoolID := uuid.New()
	for _, pool := range []persistence.WorkerPool{
		coldStartTestPool(poolID, fixture.tenantID, fixture.targetID, now),
		coldStartTestPool(otherPoolID, fixture.tenantID, fixture.targetID, now),
	} {
		if err := fixture.db.Create(&pool).Error; err != nil {
			t.Fatal(err)
		}
	}

	var worker persistence.WorkerInstance
	if err := fixture.db.Model(&persistence.WorkerInstance{}).
		Where("execution_target_id = ?", fixture.targetID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	oldInteractive := fixture.createColdStartExecution(t, poolID, "interactive", "queued", nil, now.Add(-21*time.Second))
	freshInteractive := fixture.createColdStartExecution(t, poolID, "interactive", "queued", nil, now.Add(-19*time.Second))
	oldBatch := fixture.createColdStartExecution(t, poolID, "batch", "queued", nil, now.Add(-time.Minute))
	wrongPool := fixture.createColdStartExecution(t, otherPoolID, "interactive", "queued", nil, now.Add(-time.Minute))
	claimed := fixture.createColdStartExecution(t, poolID, "interactive", "leased", &worker.ID, now.Add(-time.Minute))

	changed, err := fixture.service.ExpireInteractiveColdStarts(context.Background(), ColdStartDeadlineScope{
		TenantID: fixture.tenantID, ExecutionTargetID: fixture.targetID,
		WorkerPoolID: poolID, WorkerPoolVersion: 1,
		Cutoff: now.Add(-20 * time.Second), Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("expired executions = %d, want 1", changed)
	}
	fixture.assertColdStartExecutionStatus(t, oldInteractive, "cancelled")
	fixture.assertColdStartExecutionStatus(t, freshInteractive, "queued")
	fixture.assertColdStartExecutionStatus(t, oldBatch, "queued")
	fixture.assertColdStartExecutionStatus(t, wrongPool, "queued")
	fixture.assertColdStartExecutionStatus(t, claimed, "leased")

	var event persistence.SessionEvent
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND event_type = ?",
		fixture.tenantID, oldInteractive, "execution.cancelled",
	).Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.Payload["reason"] != "interactive-cold-start-deadline" {
		t.Fatalf("cold-start cancellation payload = %#v", event.Payload)
	}
	var outboxCount int64
	if err := fixture.db.Model(&persistence.OutboxMessage{}).Where(
		"tenant_id = ? AND topic = ? AND message_key = ?",
		fixture.tenantID, "execution.cancelled", oldInteractive.String(),
	).Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("cold-start cancellation outbox count = %d, want 1", outboxCount)
	}
}

func coldStartTestPool(id, tenantID, targetID uuid.UUID, now time.Time) persistence.WorkerPool {
	return persistence.WorkerPool{
		ID: id, TenantID: &tenantID, ExecutionTargetID: targetID,
		Name: "cold-start-" + id.String()[:8], Mode: placement.PoolModeWarm,
		CapacityClass: placement.CapacityClassInteractive, TenantIsolation: placement.TenantIsolationPinned,
		DesiredIdleUnits: 1, MinIdleUnits: 1, MaxActiveUnits: 5,
		SchedulingTemplate: map[string]any{}, Status: placement.PoolStatusActive,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func (f advancedOperationFixture) createColdStartExecution(
	t *testing.T,
	poolID uuid.UUID,
	queueClass, status string,
	workerID *uuid.UUID,
	queuedAt time.Time,
) uuid.UUID {
	t.Helper()
	sessionID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	poolVersion := int64(1)
	capacityClass := placement.CapacityClassInteractive
	models := []any{
		&persistence.AgentSession{
			ID: sessionID, TenantID: f.tenantID, OrganizationID: f.organizationID,
			ProjectID: f.projectID, CreatedBy: f.principal.UserID, Title: "Cold-start test",
			Status: "active", Visibility: "private", Provider: "codex",
			ExecutionTargetID: f.targetID, RequestedExecutionTargetID: f.targetID,
			ResourceState: "provisioning", CreatedAt: queuedAt, UpdatedAt: queuedAt,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: f.tenantID, SessionID: sessionID, CreatedBy: f.principal.UserID,
			Status: "queued", InputText: "cold-start test", TurnKind: "message",
			RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: queuedAt,
		},
		&persistence.AgentExecution{
			ID: executionID, TenantID: f.tenantID, SessionID: sessionID, TurnID: turnID,
			QueueClass: queueClass, QueuePriority: 0, QuotaUnits: 1,
			Attempt: 1, Status: status, ExecutionTargetID: f.targetID, TargetKind: "kubernetes",
			WorkerPoolID: &poolID, WorkerPoolVersion: &poolVersion, CapacityClass: &capacityClass,
			WorkerID: workerID, Generation: 0, RequestedBy: f.principal.UserID, QueuedAt: queuedAt,
		},
	}
	for _, model := range models {
		if err := f.db.Create(model).Error; err != nil {
			t.Fatalf("create cold-start fixture %T: %v", model, err)
		}
	}
	return executionID
}

func (f advancedOperationFixture) assertColdStartExecutionStatus(t *testing.T, executionID uuid.UUID, want string) {
	t.Helper()
	var execution persistence.AgentExecution
	if err := f.db.Where("tenant_id = ? AND id = ?", f.tenantID, executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != want {
		t.Fatalf("execution %s status = %q, want %q", executionID, execution.Status, want)
	}
}
