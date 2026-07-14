package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSwitchModelRotatesBindingsCursorEventAndAuditAtomically(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	previousModel := "gpt-5"
	nextModel := "gpt-5.6"
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Update("model", previousModel).Error; err != nil {
		t.Fatal(err)
	}
	turn, err := fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID, CreateTurnInput{
		InputText: "establish the first runtime binding",
	}, "model-switch-setup", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND turn_id = ?", fixture.tenantID, turn.ID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	completeSessionExecutionForNextTurn(t, fixture, execution)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, execution.ID).
		Update("generation", 1).Error; err != nil {
		t.Fatal(err)
	}

	var sessionBefore persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&sessionBefore).Error; err != nil {
		t.Fatal(err)
	}
	if sessionBefore.CurrentRuntimeBindingID == nil {
		t.Fatal("setup did not attach the first runtime binding")
	}
	now := time.Now().UTC()
	recoveringBindingID := uuid.New()
	incompatibleBindingID := uuid.New()
	alreadyReleasedBindingID := uuid.New()
	releasedBeforeSwitch := now.Add(-time.Hour)
	for _, binding := range []persistence.ProviderRuntimeBinding{
		{
			ID: recoveringBindingID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
			Provider: "codex", Revision: 2, Status: "recovering", ResumeStrategy: "native-cursor",
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: incompatibleBindingID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
			Provider: "codex", Revision: 3, Status: "incompatible", ResumeStrategy: "native-cursor",
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: alreadyReleasedBindingID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
			Provider: "codex", Revision: 4, Status: "released", ResumeStrategy: "authoritative-history",
			CreatedAt: now, UpdatedAt: releasedBeforeSwitch, ReleasedAt: &releasedBeforeSwitch,
		},
	} {
		binding := binding
		if err := fixture.db.Create(&binding).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"current_runtime_binding_id":                 recoveringBindingID,
			"provider_resume_cursor_encrypted":           []byte("encrypted-provider-cursor"),
			"provider_resume_cursor_state":               "usable",
			"provider_resume_cursor_source_execution_id": execution.ID,
			"provider_resume_cursor_source_generation":   int64(1),
			"provider_resume_cursor_history_sequence":    sessionBefore.LastEventSequence,
		}).Error; err != nil {
		t.Fatal(err)
	}
	seedSessionCapabilityWorker(t, fixture, sessionCapabilityManifestOptions{})

	_, published, cancel, err := fixture.service.SubscribeEvents(ctx, fixture.principal, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	result, replayed, err := fixture.service.SwitchModelWithIdempotency(
		ctx, fixture.principal, fixture.sessionID,
		SwitchModelInput{Model: nextModel, ExpectedModel: &previousModel, ExpectedModelProvided: true},
		"model-switch-success", "model-switch-success", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("first model switch was marked as replayed")
	}
	if result.Model == nil || *result.Model != nextModel {
		t.Fatalf("model switch result = %#v", result)
	}
	if result.LastEventSequence != sessionBefore.LastEventSequence+1 {
		t.Fatalf("result lastEventSequence = %d, want %d", result.LastEventSequence, sessionBefore.LastEventSequence+1)
	}

	select {
	case event := <-published:
		if event.EventType != "session.model.changed" || event.Sequence != result.LastEventSequence {
			t.Fatalf("published event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("model switch event was not published after commit")
	}

	var stored persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Model == nil || *stored.Model != nextModel || stored.CurrentRuntimeBindingID == nil {
		t.Fatalf("stored Session model/current binding = %#v", stored)
	}
	if len(stored.ProviderResumeCursorEncrypted) != 0 || stored.ProviderResumeCursorState != "absent" ||
		stored.ProviderResumeCursorSourceExecutionID != nil || stored.ProviderResumeCursorSourceGeneration != nil ||
		stored.ProviderResumeCursorHistorySequence != nil {
		t.Fatalf("provider cursor lineage was not cleared: %#v", stored)
	}

	bindings := make([]persistence.ProviderRuntimeBinding, 0)
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Order("revision").Find(&bindings).Error; err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 5 {
		t.Fatalf("runtime binding count = %d, want 5", len(bindings))
	}
	for _, binding := range bindings[:4] {
		if binding.Status != "released" || binding.ReleasedAt == nil {
			t.Fatalf("old runtime binding was not released: %#v", binding)
		}
	}
	newBinding := bindings[4]
	if newBinding.Revision != 5 || newBinding.Status != "active" ||
		newBinding.ResumeStrategy != "authoritative-history" || newBinding.ReleasedAt != nil {
		t.Fatalf("new runtime binding = %#v", newBinding)
	}
	if *stored.CurrentRuntimeBindingID != newBinding.ID {
		t.Fatalf("current runtime binding = %s, want %s", *stored.CurrentRuntimeBindingID, newBinding.ID)
	}

	var event persistence.SessionEvent
	if err := fixture.db.Where(
		"tenant_id = ? AND session_id = ? AND event_type = ?",
		fixture.tenantID, fixture.sessionID, "session.model.changed",
	).Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.Payload["previousModel"] != previousModel || event.Payload["model"] != nextModel ||
		event.Payload["provider"] != "codex" || event.Payload["supportMode"] != modelSwitchSupportMode {
		t.Fatalf("model switch event payload = %#v", event.Payload)
	}
	assertCount(t, fixture, &persistence.SessionEvent{},
		"tenant_id = ? AND session_id = ? AND event_type = ?", 1,
		fixture.tenantID, fixture.sessionID, "session.model.changed")

	var auditLog persistence.AuditLog
	if err := fixture.db.Where(
		"tenant_id = ? AND resource_id = ? AND action = ?",
		fixture.tenantID, fixture.sessionID, "session.model.changed",
	).Take(&auditLog).Error; err != nil {
		t.Fatal(err)
	}
	if auditLog.Metadata["previousModel"] != previousModel || auditLog.Metadata["model"] != nextModel ||
		auditLog.Metadata["provider"] != "codex" || auditLog.Metadata["supportMode"] != modelSwitchSupportMode {
		t.Fatalf("model switch audit metadata = %#v", auditLog.Metadata)
	}
	assertCount(t, fixture, &persistence.AuditLog{},
		"tenant_id = ? AND resource_id = ? AND action = ?", 1,
		fixture.tenantID, fixture.sessionID, "session.model.changed")
}

func TestSwitchModelStrictCASPrecedesNoOpAndNoOpSkipsExecutionAndCapabilityChecks(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	model := "gpt-5"
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Update("model", model).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID, CreateTurnInput{
		InputText: "leave an active execution while testing no-op",
	}, "model-switch-noop-turn", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	stale := "gpt-4"
	_, err := fixture.service.SwitchModel(ctx, fixture.principal, fixture.sessionID, SwitchModelInput{
		Model: model, ExpectedModel: &stale, ExpectedModelProvided: true,
	}, "model-switch-stale-noop", "127.0.0.1")
	assertSessionProblemCode(t, err, "session_model_conflict")

	before := loadSessionForModelSwitchTest(t, fixture)
	result, err := fixture.service.SwitchModel(ctx, fixture.principal, fixture.sessionID, SwitchModelInput{
		Model: model, ExpectedModel: &model, ExpectedModelProvided: true,
	}, "model-switch-noop", "127.0.0.1")
	if err != nil {
		t.Fatalf("same-model no-op was rejected by active Execution or unobserved capability: %v", err)
	}
	after := loadSessionForModelSwitchTest(t, fixture)
	if result.LastEventSequence != before.LastEventSequence || after.LastEventSequence != before.LastEventSequence ||
		after.CurrentRuntimeBindingID == nil || before.CurrentRuntimeBindingID == nil ||
		*after.CurrentRuntimeBindingID != *before.CurrentRuntimeBindingID {
		t.Fatalf("same-model no-op mutated Session state: before=%#v after=%#v result=%#v", before, after, result)
	}
	assertCount(t, fixture, &persistence.SessionEvent{},
		"tenant_id = ? AND session_id = ? AND event_type = ?", 0,
		fixture.tenantID, fixture.sessionID, "session.model.changed")
	assertCount(t, fixture, &persistence.AuditLog{},
		"tenant_id = ? AND resource_id = ? AND action = ?", 0,
		fixture.tenantID, fixture.sessionID, "session.model.changed")
}

func TestSwitchModelRejectsEveryActiveExecutionStatus(t *testing.T) {
	for _, status := range activeSessionExecutionStatuses {
		t.Run(status, func(t *testing.T) {
			fixture := newTenantExecutionPolicyFixture(t)
			ctx := context.Background()
			previousModel := "gpt-5"
			if err := fixture.db.Model(&persistence.AgentSession{}).
				Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
				Update("model", previousModel).Error; err != nil {
				t.Fatal(err)
			}
			turn, err := fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID, CreateTurnInput{
				InputText: "active status " + status,
			}, "model-switch-active-"+status, "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			if status != "queued" {
				if err := fixture.db.Model(&persistence.AgentExecution{}).
					Where("tenant_id = ? AND turn_id = ?", fixture.tenantID, turn.ID).
					Update("status", status).Error; err != nil {
					t.Fatal(err)
				}
			}
			seedSessionCapabilityWorker(t, fixture, sessionCapabilityManifestOptions{})
			_, err = fixture.service.SwitchModel(ctx, fixture.principal, fixture.sessionID, SwitchModelInput{
				Model: "gpt-5.6", ExpectedModel: &previousModel, ExpectedModelProvided: true,
			}, "model-switch-active-"+status, "127.0.0.1")
			assertSessionProblemCode(t, err, "session_execution_active")
			stored := loadSessionForModelSwitchTest(t, fixture)
			if stored.Model == nil || *stored.Model != previousModel {
				t.Fatalf("active %s execution allowed model mutation: %#v", status, stored)
			}
			assertCount(t, fixture, &persistence.SessionEvent{},
				"tenant_id = ? AND session_id = ? AND event_type = ?", 0,
				fixture.tenantID, fixture.sessionID, "session.model.changed")
		})
	}
}

func TestSwitchModelRequiresExecutionCreateAndPreservesPrivateIsolation(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	other := createSessionModelSwitchPrincipal(t, fixture, "agent_operator")
	_, err := fixture.service.SwitchModel(context.Background(), other, fixture.sessionID, SwitchModelInput{
		Model: "gpt-5.6", ExpectedModelProvided: true,
	}, "private-model-switch", "127.0.0.1")
	assertSessionProblemCode(t, err, "session_not_found")

	viewer := createSessionModelSwitchPrincipal(t, fixture, "viewer")
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Update("visibility", "organization").Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.SwitchModel(context.Background(), viewer, fixture.sessionID, SwitchModelInput{
		Model: "gpt-5.6", ExpectedModelProvided: true,
	}, "viewer-model-switch", "127.0.0.1")
	assertSessionProblemCode(t, err, "organization_forbidden")
}

func TestSwitchModelValidatesModelAndExpectedModel(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	_, err := fixture.service.SwitchModel(ctx, fixture.principal, fixture.sessionID, SwitchModelInput{
		Model: "gpt-5.6",
	}, "missing-expected", "127.0.0.1")
	assertSessionProblemCode(t, err, "expected_model_required")
	_, err = fixture.service.SwitchModel(ctx, fixture.principal, fixture.sessionID, SwitchModelInput{
		Model: " ", ExpectedModelProvided: true,
	}, "empty-model", "127.0.0.1")
	assertSessionProblemCode(t, err, "invalid_model")
	empty := " "
	_, err = fixture.service.SwitchModel(ctx, fixture.principal, fixture.sessionID, SwitchModelInput{
		Model: "gpt-5.6", ExpectedModel: &empty, ExpectedModelProvided: true,
	}, "empty-expected", "127.0.0.1")
	assertSessionProblemCode(t, err, "invalid_expected_model")
}

func loadSessionForModelSwitchTest(t *testing.T, fixture tenantExecutionPolicyFixture) persistence.AgentSession {
	t.Helper()
	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session
}

func createSessionModelSwitchPrincipal(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	organizationRole string,
) identity.Principal {
	t.Helper()
	userID := uuid.New()
	now := time.Now().UTC()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Model switch user",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.TenantMembership{
			TenantID: fixture.tenantID, UserID: userID, Role: "member", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.OrganizationMembership{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, UserID: userID,
			Role: organizationRole, Status: "active", CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	return identity.Principal{UserID: userID, ActiveTenantID: &fixture.tenantID}
}
