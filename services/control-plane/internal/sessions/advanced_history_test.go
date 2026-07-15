package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestRollbackReportsConversationOnlyDispositionAndReplaysIdempotently(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	firstTurnID, _ := appendCompletedMessageTurn(t, fixture, fixture.sessionID, "first")
	secondTurnID, expected := appendCompletedMessageTurn(t, fixture, fixture.sessionID, "second")

	result, replayed, err := fixture.service.Rollback(
		ctx,
		fixture.principal,
		fixture.sessionID,
		RollbackSessionInput{ExpectedLastEventSequence: &expected, FromTurnID: secondTurnID},
		"rollback-conversation-only",
		"rollback-request",
		"127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || result.RemovedTurnCount != 1 || result.WorkspaceDisposition != "unchanged" ||
		result.ExternalSideEffectsReverted || result.SupportMode != "emulated" {
		t.Fatalf("unexpected rollback result: replayed=%t result=%#v", replayed, result)
	}

	replayedResult, replayed, err := fixture.service.Rollback(
		ctx,
		fixture.principal,
		fixture.sessionID,
		RollbackSessionInput{ExpectedLastEventSequence: &expected, FromTurnID: secondTurnID},
		"rollback-conversation-only",
		"rollback-request-replay",
		"127.0.0.1",
	)
	if err != nil || !replayed || replayedResult.EventID != result.EventID {
		t.Fatalf("rollback replay = %#v replayed=%t err=%v", replayedResult, replayed, err)
	}

	var rollbackEvent persistence.SessionEvent
	if err := fixture.db.Where(
		"tenant_id = ? AND session_id = ? AND event_type = ?",
		fixture.tenantID,
		fixture.sessionID,
		"session.history.rolled-back",
	).Take(&rollbackEvent).Error; err != nil {
		t.Fatal(err)
	}
	if rollbackEvent.Payload["workspaceDisposition"] != "unchanged" ||
		rollbackEvent.Payload["externalSideEffectsReverted"] != false {
		t.Fatalf("rollback event omitted disposition: %#v", rollbackEvent.Payload)
	}

	logical, err := LoadLogicalEvents(ctx, fixture.db, fixture.tenantID, fixture.sessionID, result.EventSequence)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := EffectiveLogicalEvents(logical)
	if err != nil {
		t.Fatal(err)
	}
	if logicalHistoryContainsTurn(effective, secondTurnID) || !logicalHistoryContainsTurn(effective, firstTurnID) {
		t.Fatalf("rollback effective history is incorrect: %#v", effective)
	}
}

func TestForkOfForkKeepsLogicalPrefixWithoutCopyingEvents(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	turnID, sourceSequence := appendCompletedMessageTurn(t, fixture, fixture.sessionID, "source")

	first, replayed, err := fixture.service.Fork(
		ctx,
		fixture.principal,
		fixture.sessionID,
		ForkSessionInput{ExpectedLastEventSequence: &sourceSequence, Title: "First fork"},
		"fork-first",
		"fork-first-request",
		"127.0.0.1",
	)
	if err != nil || replayed {
		t.Fatalf("first fork = %#v replayed=%t err=%v", first, replayed, err)
	}
	if first.Session.LastEventSequence != sourceSequence+1 || first.Session.ForkSourceTurnID == nil ||
		*first.Session.ForkSourceTurnID != turnID {
		t.Fatalf("first fork lineage = %#v", first.Session)
	}

	firstSequence := first.Session.LastEventSequence
	second, replayed, err := fixture.service.Fork(
		ctx,
		fixture.principal,
		first.Session.ID,
		ForkSessionInput{ExpectedLastEventSequence: &firstSequence, Title: "Second fork"},
		"fork-second",
		"fork-second-request",
		"127.0.0.1",
	)
	if err != nil || replayed {
		t.Fatalf("second fork = %#v replayed=%t err=%v", second, replayed, err)
	}
	if second.Session.ForkSourceTurnID == nil || *second.Session.ForkSourceTurnID != turnID {
		t.Fatalf("fork-of-fork lost its latest logical source Turn: %#v", second.Session)
	}

	var copiedEvents int64
	if err := fixture.db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ?", fixture.tenantID, second.Session.ID).
		Count(&copiedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if copiedEvents != 1 {
		t.Fatalf("fork copied %d physical events; want only session.forked", copiedEvents)
	}
	logical, err := LoadLogicalEvents(
		ctx,
		fixture.db,
		fixture.tenantID,
		second.Session.ID,
		second.Session.LastEventSequence,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !logicalHistoryContainsTurn(logical, turnID) {
		t.Fatalf("fork-of-fork lost its inherited logical Turn: %#v", logical)
	}
	firstPage, err := fixture.service.ListEvents(
		ctx, fixture.principal, second.Session.ID, 0, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 2 || firstPage.Items[0].Sequence != 1 || firstPage.Items[1].Sequence != 2 {
		t.Fatalf("unexpected first logical page: %#v", firstPage)
	}
	secondPage, err := fixture.service.ListEvents(
		ctx, fixture.principal, second.Session.ID, 2, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 1 || secondPage.Items[0].Sequence != 3 ||
		secondPage.Items[0].SessionID != second.Session.ID {
		t.Fatalf("unexpected second logical page: %#v", secondPage)
	}
}

func TestForkDoesNotInheritUnavailableProviderCredential(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	credentialID := uuid.New()
	if err := fixture.db.Create(&persistence.ProviderCredential{
		ID: credentialID, TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "Source-only credential", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: []byte("encrypted"), EncryptedDataKey: []byte("data-key"),
		KMSProvider: "local", KMSKeyID: "test", Version: 1,
		CreatedBy: fixture.principal.UserID, UpdatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Update("provider_credential_id", credentialID).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := fixture.db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, credentialID).
		Updates(map[string]any{"revoked_at": now, "revoked_by": fixture.principal.UserID}).Error; err != nil {
		t.Fatal(err)
	}

	sequence := int64(0)
	result, replayed, err := fixture.service.Fork(
		context.Background(),
		fixture.principal,
		fixture.sessionID,
		ForkSessionInput{ExpectedLastEventSequence: &sequence, Title: "Credential-independent fork"},
		"fork-revoked-credential",
		"fork-revoked-request",
		"127.0.0.1",
	)
	if err != nil || replayed {
		t.Fatalf("Fork should resolve Credential for the new owner instead of inheriting the source binding: result=%#v replayed=%t err=%v", result, replayed, err)
	}
	if result.Session.ProviderCredentialID != nil {
		t.Fatalf("Fork inherited the unavailable source Credential: %#v", result.Session.ProviderCredentialID)
	}

	var sessions int64
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ?", fixture.tenantID).
		Count(&sessions).Error; err != nil {
		t.Fatal(err)
	}
	if sessions != 2 {
		t.Fatalf("Fork persisted %d Sessions; want source plus one fork", sessions)
	}
}

func TestForkBindsOnlyTheCredentialRequestedForTheNewSession(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	requestedCredentialID := uuid.New()
	if err := fixture.db.Create(&persistence.ProviderCredential{
		ID: requestedCredentialID, TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "Fork credential", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: []byte("encrypted"), EncryptedDataKey: []byte("data-key"),
		KMSProvider: "local", KMSKeyID: "test", Version: 1,
		CreatedBy: fixture.principal.UserID, UpdatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	sequence := int64(0)
	result, replayed, err := fixture.service.Fork(
		context.Background(),
		fixture.principal,
		fixture.sessionID,
		ForkSessionInput{
			ExpectedLastEventSequence: &sequence,
			Title:                     "Explicit Credential fork",
			ProviderCredentialID:      &requestedCredentialID,
		},
		"fork-explicit-credential",
		"fork-explicit-request",
		"127.0.0.1",
	)
	if err != nil || replayed {
		t.Fatalf("explicit Credential Fork = %#v replayed=%t err=%v", result, replayed, err)
	}
	if result.Session.ProviderCredentialID == nil || *result.Session.ProviderCredentialID != requestedCredentialID {
		t.Fatalf("Fork omitted the requested Credential: %#v", result.Session.ProviderCredentialID)
	}
}

func TestForkRejectsProspectiveLineageBeyondMaximumDepth(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	_, sequence := appendCompletedMessageTurn(t, fixture, fixture.sessionID, "root")
	sourceSessionID := fixture.sessionID

	for depth := 1; depth < maximumForkLineageDepth; depth++ {
		result, replayed, err := fixture.service.Fork(
			ctx,
			fixture.principal,
			sourceSessionID,
			ForkSessionInput{
				ExpectedLastEventSequence: &sequence,
				Title:                     "Fork depth " + string(rune('A'+depth-1)),
			},
			"fork-depth-"+uuid.NewString(),
			"fork-depth-request",
			"127.0.0.1",
		)
		if err != nil || replayed {
			t.Fatalf("fork depth %d = %#v replayed=%t err=%v", depth, result, replayed, err)
		}
		sourceSessionID = result.Session.ID
		sequence = result.Session.LastEventSequence
	}

	_, _, err := fixture.service.Fork(
		ctx,
		fixture.principal,
		sourceSessionID,
		ForkSessionInput{ExpectedLastEventSequence: &sequence, Title: "Too deep"},
		"fork-depth-rejected",
		"fork-depth-rejected-request",
		"127.0.0.1",
	)
	assertSessionProblemCode(t, err, "fork_lineage_too_deep")

	var count int64
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ?", fixture.tenantID).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != maximumForkLineageDepth {
		t.Fatalf("rejected deep Fork persisted a Session: count=%d want=%d", count, maximumForkLineageDepth)
	}
}

func appendCompletedMessageTurn(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	sessionID uuid.UUID,
	inputText string,
) (uuid.UUID, int64) {
	t.Helper()
	turnID := uuid.New()
	var sequence int64
	err := persistence.InTransaction(context.Background(), fixture.db, func(tx *gorm.DB) error {
		var session persistence.AgentSession
		if err := tx.Where("tenant_id = ? AND id = ?", fixture.tenantID, sessionID).Take(&session).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Create(&persistence.AgentTurn{
			ID: turnID, TenantID: fixture.tenantID, SessionID: sessionID,
			CreatedBy: fixture.principal.UserID, Status: "completed", InputText: inputText,
			TurnKind: "message", RuntimeMode: "full-access", InteractionMode: "default",
			StartedAt: &now, CompletedAt: &now, CreatedAt: now,
		}).Error; err != nil {
			return err
		}
		event, err := appendEvent(context.Background(), tx, &session, eventInput{
			EventType: "turn.created", ActorType: "user", ActorID: &fixture.principal.UserID,
			Payload: map[string]any{
				"turnId": turnID, "status": "completed", "turnKind": "message",
			},
		})
		if err != nil {
			return err
		}
		sequence = event.Sequence
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return turnID, sequence
}

func logicalHistoryContainsTurn(events []LogicalEvent, turnID uuid.UUID) bool {
	for _, item := range events {
		if item.Event.EventType != "turn.created" {
			continue
		}
		candidate, err := logicalPayloadUUID(item.Event.Payload, "turnId")
		if err == nil && candidate == turnID {
			return true
		}
	}
	return false
}
