package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/lifecyclepolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestSessionResourceLifecycleUsesServerEventsNotReadTraffic(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	var persisted persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	initialMeaningfulSequence := persisted.MeaningfulActivitySequence
	anchor := time.Now().UTC()
	if !anchor.After(persisted.CreatedAt) {
		anchor = persisted.CreatedAt.Add(time.Millisecond)
	}
	oldActivity := anchor.Add(-2 * time.Hour)
	idleSince := anchor
	absoluteExpiry := anchor.Add(24 * time.Hour)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"meaningful_activity_at":       oldActivity,
			"meaningful_activity_sequence": initialMeaningfulSequence,
			"resource_idle_since":          idleSince,
			"absolute_expires_at":          absoluteExpiry,
		}).Error; err != nil {
		t.Fatal(err)
	}

	turn, err := fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID, CreateTurnInput{
		InputText: "wake this Session with meaningful activity",
	}, "resource-lifecycle-turn", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var active persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&active).Error; err != nil {
		t.Fatal(err)
	}
	if !active.MeaningfulActivityAt.After(oldActivity) || active.ResourceState != "provisioning" || active.ResourceIdleSince != nil ||
		active.AbsoluteExpiresAt == nil || !active.AbsoluteExpiresAt.Equal(absoluteExpiry) {
		t.Fatalf("meaningful Turn activity did not activate the Session without sliding its hard expiry: %#v", active)
	}
	activityAfterTurn := active.MeaningfulActivityAt
	if active.MeaningfulActivitySequence <= initialMeaningfulSequence {
		t.Fatalf("user Turn did not advance meaningful activity sequence: before=%d after=%d", initialMeaningfulSequence, active.MeaningfulActivitySequence)
	}
	sequenceAfterTurn := active.MeaningfulActivitySequence

	if _, err := fixture.service.ListEvents(ctx, fixture.principal, fixture.sessionID, 0, 100); err != nil {
		t.Fatal(err)
	}
	var afterRead persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&afterRead).Error; err != nil {
		t.Fatal(err)
	}
	if !afterRead.MeaningfulActivityAt.Equal(activityAfterTurn) || afterRead.ResourceState != "provisioning" || afterRead.ResourceIdleSince != nil {
		t.Fatalf("read traffic incorrectly renewed Session activity: before=%s after=%#v", activityAfterTurn, afterRead)
	}
	if afterRead.MeaningfulActivitySequence != sequenceAfterTurn {
		t.Fatalf("read traffic changed meaningful activity sequence: before=%d after=%d", sequenceAfterTurn, afterRead.MeaningfulActivitySequence)
	}

	var execution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND session_id = ? AND turn_id = ?", fixture.tenantID, fixture.sessionID, turn.ID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, err := fixture.service.AppendInternalEvent(ctx, tx, fixture.tenantID, fixture.sessionID, InternalEventInput{
			EventType: "execution.suspend-checkpointing", ActorType: "system", ExecutionID: &execution.ID,
			Payload: map[string]any{"turnId": turn.ID},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var afterSystemMaintenance persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&afterSystemMaintenance).Error; err != nil {
		t.Fatal(err)
	}
	if !afterSystemMaintenance.MeaningfulActivityAt.Equal(activityAfterTurn) ||
		afterSystemMaintenance.ResourceState != "checkpointing" || afterSystemMaintenance.ResourceIdleSince != nil {
		t.Fatalf("system lifecycle maintenance incorrectly renewed Session activity: before=%s after=%#v", activityAfterTurn, afterSystemMaintenance)
	}
	if afterSystemMaintenance.MeaningfulActivitySequence != sequenceAfterTurn {
		t.Fatalf("system lifecycle maintenance changed meaningful activity sequence: before=%d after=%d", sequenceAfterTurn, afterSystemMaintenance.MeaningfulActivitySequence)
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		for _, eventType := range []string{"execution.leased", "execution.started", "workspace.ready", "checkpoint.ready"} {
			if _, err := fixture.service.AppendInternalEvent(ctx, tx, fixture.tenantID, fixture.sessionID, InternalEventInput{
				EventType: eventType, ActorType: "worker", ExecutionID: &execution.ID,
				Payload: map[string]any{"turnId": turn.ID},
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var afterWorkerMaintenance persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&afterWorkerMaintenance).Error; err != nil {
		t.Fatal(err)
	}
	if !afterWorkerMaintenance.MeaningfulActivityAt.Equal(activityAfterTurn) {
		t.Fatalf("Worker lifecycle bookkeeping renewed Session activity: before=%s after=%s", activityAfterTurn, afterWorkerMaintenance.MeaningfulActivityAt)
	}
	if afterWorkerMaintenance.MeaningfulActivitySequence != sequenceAfterTurn {
		t.Fatalf("worker lifecycle bookkeeping changed meaningful activity sequence: before=%d after=%d", sequenceAfterTurn, afterWorkerMaintenance.MeaningfulActivitySequence)
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, err := fixture.service.AppendInternalEvent(ctx, tx, fixture.tenantID, fixture.sessionID, InternalEventInput{
			EventType: "content.delta", ActorType: "worker", MeaningfulActivity: true,
			ExecutionID: &execution.ID, Payload: map[string]any{"streamKind": "assistant_text", "delta": "working"},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var afterRuntimeProgress persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&afterRuntimeProgress).Error; err != nil {
		t.Fatal(err)
	}
	if !afterRuntimeProgress.MeaningfulActivityAt.After(activityAfterTurn) {
		t.Fatalf("semantic Worker runtime progress did not renew Session activity: %#v", afterRuntimeProgress)
	}
	activityAfterRuntime := afterRuntimeProgress.MeaningfulActivityAt
	if afterRuntimeProgress.MeaningfulActivitySequence <= sequenceAfterTurn {
		t.Fatalf("semantic Worker runtime progress did not advance meaningful activity sequence: before=%d after=%d", sequenceAfterTurn, afterRuntimeProgress.MeaningfulActivitySequence)
	}
	sequenceAfterRuntime := afterRuntimeProgress.MeaningfulActivitySequence
	finishedAt := time.Now().UTC()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, execution.ID).
			Updates(map[string]any{"status": "completed", "finished_at": finishedAt}).Error; err != nil {
			return err
		}
		_, err := fixture.service.AppendInternalEvent(ctx, tx, fixture.tenantID, fixture.sessionID, InternalEventInput{
			EventType: "execution.completed", ActorType: "worker", ExecutionID: &execution.ID,
			Payload: map[string]any{"turnId": turn.ID},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var idle persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&idle).Error; err != nil {
		t.Fatal(err)
	}
	if idle.ResourceState != "idle" || idle.ResourceIdleSince == nil || idle.ResourceIdleSince.Before(activityAfterRuntime) ||
		!idle.MeaningfulActivityAt.Equal(activityAfterRuntime) {
		t.Fatalf("terminal Execution did not start the resource-idle interval: %#v", idle)
	}
	if idle.MeaningfulActivitySequence != sequenceAfterRuntime {
		t.Fatalf("terminal bookkeeping changed meaningful activity sequence: before=%d after=%d", sequenceAfterRuntime, idle.MeaningfulActivitySequence)
	}
}

func TestAppendEventClampsMeaningfulActivityTimestampAndSequence(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	futureActivity := time.Now().UTC().Add(time.Minute)

	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
			Updates(map[string]any{
				"last_event_sequence":          4,
				"meaningful_activity_sequence": 2,
				"meaningful_activity_at":       futureActivity,
			}).Error; err != nil {
			return err
		}
		var session persistence.AgentSession
		if err := tx.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&session).Error; err != nil {
			return err
		}
		event, err := appendEvent(ctx, tx, &session, eventInput{
			EventType: "turn.created", ActorType: "user", Payload: map[string]any{"turnId": uuid.New()},
		})
		if err != nil {
			return err
		}
		if event.Sequence != 5 {
			t.Fatalf("event sequence = %d, want 5", event.Sequence)
		}
		if session.LastEventSequence != 5 || session.MeaningfulActivitySequence != 5 {
			t.Fatalf("in-memory session watermark = %#v", session)
		}
		if !session.MeaningfulActivityAt.Equal(futureActivity) {
			t.Fatalf("in-memory meaningful activity = %s, want clamped %s", session.MeaningfulActivityAt, futureActivity)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var persisted persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.LastEventSequence != 5 || persisted.MeaningfulActivitySequence != 5 {
		t.Fatalf("persisted session watermark = %#v", persisted)
	}
	if !persisted.MeaningfulActivityAt.Equal(futureActivity) {
		t.Fatalf("persisted meaningful activity = %s, want clamped %s", persisted.MeaningfulActivityAt, futureActivity)
	}
}

func TestSessionCreationFreezesEffectiveLifecyclePolicyAndAbsoluteExpiry(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	policyService, err := lifecyclepolicy.NewService(
		fixture.db, lifecyclepolicy.DefaultConfig(platform.ProfilePersonal),
	)
	if err != nil {
		t.Fatal(err)
	}
	waiting := 1200
	absoluteLifetime := 7200
	if _, err := policyService.UpdateTenant(ctx, fixture.principal, fixture.tenantID, lifecyclepolicy.UpdateInput{
		ExpectedVersion: 0,
		Overrides: lifecyclepolicy.Overrides{
			WaitingKeepAliveSeconds:        &waiting,
			AbsoluteSessionLifetimeSeconds: &absoluteLifetime,
		},
	}, "session-policy-tenant", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	suspendAfterIdle := 600
	if _, err := policyService.UpdateProject(ctx, fixture.principal, fixture.tenantID, fixture.projectID, lifecyclepolicy.UpdateInput{
		ExpectedVersion: 0,
		Overrides:       lifecyclepolicy.Overrides{SuspendAfterIdleSeconds: &suspendAfterIdle},
	}, "session-policy-project", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	fixture.service.lifecyclePolicies = policyService
	workspaceRetention := 7
	createdAtLowerBound := time.Now().UTC()
	created, err := fixture.service.Create(ctx, fixture.principal, fixture.projectID, CreateSessionInput{
		Title: "Frozen lifecycle", Provider: "codex",
		ResourceLifecyclePolicy: &lifecyclepolicy.Overrides{WorkspaceRetentionDays: &workspaceRetention},
	}, "session-policy-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	policy := created.ResourceLifecyclePolicy
	if policy.WaitingKeepAliveSeconds != waiting || policy.SuspendAfterIdleSeconds != suspendAfterIdle ||
		policy.WorkspaceRetentionDays != workspaceRetention ||
		policy.AbsoluteSessionLifetimeSeconds == nil || *policy.AbsoluteSessionLifetimeSeconds != absoluteLifetime {
		t.Fatalf("Session did not freeze the resolved policy: %#v", created)
	}
	if created.AbsoluteExpiresAt == nil {
		t.Fatal("absolute Session lifetime did not produce a hard expiry")
	}
	expectedMinimum := createdAtLowerBound.Add(time.Duration(absoluteLifetime) * time.Second)
	if created.AbsoluteExpiresAt.Before(expectedMinimum) || created.AbsoluteExpiresAt.After(expectedMinimum.Add(5*time.Second)) {
		t.Fatalf("unexpected absolute expiry: %s", created.AbsoluteExpiresAt)
	}

	newWaiting := 1800
	if _, err := policyService.UpdateTenant(ctx, fixture.principal, fixture.tenantID, lifecyclepolicy.UpdateInput{
		ExpectedVersion: 1, Overrides: lifecyclepolicy.Overrides{WaitingKeepAliveSeconds: &newWaiting},
	}, "session-policy-parent-update", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	loaded, err := fixture.service.Get(ctx, fixture.principal, fixture.tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ResourceLifecyclePolicy.WaitingKeepAliveSeconds != waiting ||
		loaded.AbsoluteExpiresAt == nil || !loaded.AbsoluteExpiresAt.Equal(*created.AbsoluteExpiresAt) {
		t.Fatalf("parent policy edit mutated an existing Session snapshot: %#v", loaded)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, created.ID).
		Update("waiting_keep_alive_seconds", newWaiting).Error; err == nil {
		t.Fatal("database allowed mutation of a frozen Session lifecycle snapshot")
	}
}

func TestCreateTurnRejectsAbsoluteExpiredSession(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	absoluteExpiry := time.Now().UTC().Add(time.Minute)
	fixture.service.now = func() time.Time { return absoluteExpiry.Add(time.Second) }
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"absolute_expires_at": absoluteExpiry,
		}).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID, CreateTurnInput{
		InputText: "this should be rejected after absolute expiry",
	}, "expired-session-turn", "127.0.0.1"); err == nil {
		t.Fatal("CreateTurn succeeded after the Session absolute expiry elapsed")
	} else {
		assertSessionProblemCode(t, err, "session_absolute_expired")
	}
}
