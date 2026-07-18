package executions

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/providercapabilities"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestReviewOperationPersistsAtomicallyAndReplays(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	expected := fixture.lastSequence(t, fixture.sessionID)
	input := StartReviewInput{
		ExpectedLastEventSequence: &expected,
		Target:                    ReviewTarget{Type: "baseBranch"},
		RuntimeMode:               "approval-required",
	}

	first, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID, input,
		"review-replay", "review-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.StatusCode != 202 || first.Value.Type != "review" ||
		first.Value.Turn.TurnKind != "review" || first.Value.Turn.RuntimeMode != "approval-required" ||
		first.Value.ControlCommand.CommandType != "StartReview" || first.Value.ControlCommand.Status != "pending" {
		t.Fatalf("unexpected first Review result: %#v", first)
	}
	if target, ok := first.Value.ControlCommand.Payload["target"].(map[string]any); !ok ||
		target["type"] != "baseBranch" || target["branch"] != "main" {
		t.Fatalf("Review did not resolve the Project default branch: %#v", first.Value.ControlCommand.Payload)
	}

	replayed, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID, input,
		"review-replay", "review-replayed", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.StatusCode != 202 ||
		replayed.Value.ExecutionID != first.Value.ExecutionID ||
		replayed.Value.ControlCommand.ID != first.Value.ControlCommand.ID {
		t.Fatalf("unexpected Review replay: first=%#v replay=%#v", first, replayed)
	}
	fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
}

func TestPrimaryOperationsEnforcePrivateSessionAndSequenceCAS(t *testing.T) {
	t.Run("private session", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		other := fixture.createOperator(t)
		expected := fixture.lastSequence(t, fixture.sessionID)
		_, err := fixture.service.RequestReview(
			context.Background(), other, fixture.sessionID,
			StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
			"review-private", "review-private", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 404, "session_not_found")
		fixture.assertPrimaryOperationCounts(t, 0, 0, 0, 0, 0)
	})

	t.Run("stale sequence", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		if err := fixture.db.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
			Update("last_event_sequence", 3).Error; err != nil {
			t.Fatal(err)
		}
		expected := int64(2)
		_, err := fixture.service.RequestReview(
			context.Background(), fixture.principal, fixture.sessionID,
			StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
			"review-stale", "review-stale", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, "stale_session_sequence")
		fixture.assertPrimaryOperationCounts(t, 0, 0, 0, 0, 0)
	})
}

func TestPrimaryOperationsEnforceQuotaAndObservedCapability(t *testing.T) {
	t.Run("quota", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		maximum := 0
		if err := fixture.db.Create(&persistence.TenantQuota{
			TenantID: fixture.tenantID, MaxConcurrentExecutions: &maximum, UpdatedBy: fixture.principal.UserID,
		}).Error; err != nil {
			t.Fatal(err)
		}
		expected := fixture.lastSequence(t, fixture.sessionID)
		_, err := fixture.service.RequestReview(
			context.Background(), fixture.principal, fixture.sessionID,
			StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
			"review-quota", "review-quota", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, "execution_quota_exceeded")
		fixture.assertPrimaryOperationCounts(t, 0, 0, 0, 0, 0)
	})

	t.Run("capability", func(t *testing.T) {
		capabilities := workerManifestTestCapabilities()
		setTestProviderCapability(capabilities, "codex", "review", "unsupported")
		fixture := newAdvancedOperationFixture(t, capabilities)
		expected := fixture.lastSequence(t, fixture.sessionID)
		_, err := fixture.service.RequestReview(
			context.Background(), fixture.principal, fixture.sessionID,
			StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
			"review-unsupported", "review-unsupported", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, providercapabilities.ReasonCapabilityUnsupported)
		fixture.assertPrimaryOperationCounts(t, 0, 0, 0, 0, 0)
	})
}

func TestPrimaryOperationsUseBoundManifestAcrossExecutionPinnedObservationGap(t *testing.T) {
	for _, test := range []struct {
		name            string
		compactSupport  string
		expectedProblem string
	}{
		{name: "supported", compactSupport: "native"},
		{name: "unsupported remains unobserved", compactSupport: "unsupported", expectedProblem: providercapabilities.ReasonWorkerManifestRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			capabilities := workerManifestTestCapabilities()
			setTestProviderCapability(capabilities, "codex", "compact", test.compactSupport)
			fixture := newAdvancedOperationFixture(t, capabilities)
			expected := fixture.lastSequence(t, fixture.sessionID)
			review, err := fixture.service.RequestReview(
				context.Background(), fixture.principal, fixture.sessionID,
				StartReviewInput{
					ExpectedLastEventSequence: &expected,
					Target:                    ReviewTarget{Type: "uncommittedChanges"},
				},
				"review-observation-gap", "review-observation-gap", "127.0.0.1",
			)
			if err != nil {
				t.Fatal(err)
			}
			fixture.completeExecutionWithCurrentManifest(t, review.Value.ExecutionID)
			fixture.setUsableCursor(t)
			fixture.markWorkersOffline(t)

			expected = fixture.lastSequence(t, fixture.sessionID)
			compact, err := fixture.service.RequestCompact(
				context.Background(), fixture.principal, fixture.sessionID,
				CompactSessionInput{ExpectedLastEventSequence: &expected},
				"compact-observation-gap", "compact-observation-gap", "127.0.0.1",
			)
			if test.expectedProblem != "" {
				assertAdvancedOperationProblem(t, err, 409, test.expectedProblem)
				fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if compact.StatusCode != 202 || compact.Value.Type != "compact" {
				t.Fatalf("unexpected Compact result after observation gap: %#v", compact)
			}
			fixture.assertPrimaryOperationCounts(t, 2, 2, 2, 2, 2)
		})
	}
}

func TestReviewUsesBoundManifestAcrossExecutionPinnedObservationGap(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	expected := fixture.lastSequence(t, fixture.sessionID)
	first, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-observation-gap-first", "review-observation-gap-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.completeExecutionWithCurrentManifest(t, first.Value.ExecutionID)
	fixture.markWorkersOffline(t)

	expected = fixture.lastSequence(t, fixture.sessionID)
	second, err := fixture.service.RequestReview(
		context.Background(), fixture.principal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expected,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"review-observation-gap-second", "review-observation-gap-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.StatusCode != 202 || second.Value.Type != "review" {
		t.Fatalf("unexpected Review result after observation gap: %#v", second)
	}
	fixture.assertPrimaryOperationCounts(t, 2, 2, 2, 2, 2)
}

func TestCompactRequiresUsableCursorAfterNewForkAndRollbackHistory(t *testing.T) {
	t.Run("new session", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		expected := fixture.lastSequence(t, fixture.sessionID)
		_, err := fixture.service.RequestCompact(
			context.Background(), fixture.principal, fixture.sessionID,
			CompactSessionInput{ExpectedLastEventSequence: &expected},
			"compact-new", "compact-new", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, providercapabilities.ReasonProviderCursorRequired)
	})

	t.Run("fork", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		expected := fixture.lastSequence(t, fixture.sessionID)
		forked, _, err := fixture.sessions.Fork(
			context.Background(), fixture.principal, fixture.sessionID,
			sessions.ForkSessionInput{ExpectedLastEventSequence: &expected, Title: "Forked history"},
			"fork-before-compact", "fork-before-compact", "127.0.0.1",
		)
		if err != nil {
			t.Fatal(err)
		}
		forkSequence := forked.Session.LastEventSequence
		_, err = fixture.service.RequestCompact(
			context.Background(), fixture.principal, forked.Session.ID,
			CompactSessionInput{ExpectedLastEventSequence: &forkSequence},
			"compact-fork", "compact-fork", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, providercapabilities.ReasonProviderCursorRequired)
	})

	t.Run("rollback", func(t *testing.T) {
		fixture := newAdvancedOperationFixture(t, nil)
		turnID := fixture.appendCompletedTurn(t)
		fixture.setUsableCursor(t)
		expected := fixture.lastSequence(t, fixture.sessionID)
		rolledBack, _, err := fixture.sessions.Rollback(
			context.Background(), fixture.principal, fixture.sessionID,
			sessions.RollbackSessionInput{ExpectedLastEventSequence: &expected, FromTurnID: turnID},
			"rollback-before-compact", "rollback-before-compact", "127.0.0.1",
		)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fixture.service.RequestCompact(
			context.Background(), fixture.principal, fixture.sessionID,
			CompactSessionInput{ExpectedLastEventSequence: &rolledBack.EventSequence},
			"compact-rollback", "compact-rollback", "127.0.0.1",
		)
		assertAdvancedOperationProblem(t, err, 409, providercapabilities.ReasonProviderCursorRequired)
	})
}

func TestCompactWithUsableCursorQueuesOnePrimaryOperation(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	fixture.setUsableCursor(t)
	expected := fixture.lastSequence(t, fixture.sessionID)
	result, err := fixture.service.RequestCompact(
		context.Background(), fixture.principal, fixture.sessionID,
		CompactSessionInput{ExpectedLastEventSequence: &expected},
		"compact-success", "compact-success", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != 202 || result.Value.Type != "compact" || result.Value.Turn.TurnKind != "compact" ||
		result.Value.Turn.RuntimeMode != "full-access" || result.Value.ControlCommand.CommandType != "CompactSession" {
		t.Fatalf("unexpected Compact result: %#v", result)
	}
	fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
}

func TestConcurrentPrimaryOperationRequestsHaveSingleWinner(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	expected := fixture.lastSequence(t, fixture.sessionID)
	type outcome struct {
		result OperationResult[QueuedSessionOperation]
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			result, err := fixture.service.RequestReview(
				context.Background(), fixture.principal, fixture.sessionID,
				StartReviewInput{ExpectedLastEventSequence: &expected, Target: ReviewTarget{Type: "uncommittedChanges"}},
				"review-concurrent-"+uuid.NewString(), "review-concurrent", "127.0.0.1",
			)
			outcomes <- outcome{result: result, err: err}
		}(index)
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
		if errors.As(item.err, &apiError) &&
			(apiError.Code == "session_execution_active" || apiError.Code == "stale_session_sequence") {
			rejected++
			continue
		}
		t.Fatalf("unexpected concurrent Review failure: %v", item.err)
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent Review outcomes success=%d rejected=%d", succeeded, rejected)
	}
	fixture.assertPrimaryOperationCounts(t, 1, 1, 1, 1, 1)
}

type advancedOperationFixture struct {
	db             *gorm.DB
	service        *Service
	sessions       *sessions.Service
	principal      identity.Principal
	tenantID       uuid.UUID
	organizationID uuid.UUID
	projectID      uuid.UUID
	sessionID      uuid.UUID
	targetID       uuid.UUID
}

func newAdvancedOperationFixture(t *testing.T, capabilities map[string]any) advancedOperationFixture {
	t.Helper()
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "advanced-operation-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectID := uuid.New()
	targetID := uuid.New()
	sessionID := uuid.New()
	models := []any{
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Advanced operations", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
		},
		&persistence.ExecutionTarget{
			ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
			Kind: "kubernetes", Name: "advanced-operation-target", Status: "active", ConfigurationEncrypted: []byte{},
			Capabilities: workerManifestTestTargetCapabilities(), CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Advanced operations", Status: "active",
			Visibility: "private", Provider: "codex", ExecutionTargetID: targetID,
			ProviderResumeCursorState: "absent", CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), profile, nil)
	projectService := projects.NewService(store.DB())
	sessionService := sessions.NewService(store.DB(), projectService, targetService)
	service := NewService(
		store.DB(), sessionService, time.Minute, 90*time.Second, time.Hour, nil, targetService,
		WithProjectService(projectService),
	)
	if capabilities == nil {
		capabilities = workerManifestTestCapabilities()
	}
	registerTestWorkerWithCapabilities(t, service, targetID, "kubernetes", "advanced-operation", capabilities)
	return advancedOperationFixture{
		db: store.DB(), service: service, sessions: sessionService,
		principal: identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		tenantID:  domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, sessionID: sessionID, targetID: targetID,
	}
}

func (f advancedOperationFixture) createOperator(t *testing.T) identity.Principal {
	t.Helper()
	now := time.Now().UTC()
	userID := uuid.New()
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.User{
				ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Advanced operation operator",
				Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.TenantMembership{
				TenantID: f.tenantID, UserID: userID, Role: "member", Status: "active",
				JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.OrganizationMembership{
				TenantID: f.tenantID, OrganizationID: f.organizationID, UserID: userID,
				Role: "agent_operator", Status: "active", CreatedAt: now, UpdatedAt: now,
			},
		}
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return identity.Principal{UserID: userID, ActiveTenantID: &f.tenantID}
}

func (f advancedOperationFixture) lastSequence(t *testing.T, sessionID uuid.UUID) int64 {
	t.Helper()
	var session persistence.AgentSession
	if err := f.db.Select("last_event_sequence").Where("tenant_id = ? AND id = ?", f.tenantID, sessionID).
		Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session.LastEventSequence
}

func (f advancedOperationFixture) setUsableCursor(t *testing.T) {
	t.Helper()
	if err := f.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", f.tenantID, f.sessionID).
		Updates(map[string]any{
			"provider_resume_cursor_state":     "usable",
			"provider_resume_cursor_encrypted": []byte("encrypted-test-cursor"),
		}).Error; err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) completeExecutionWithCurrentManifest(
	t *testing.T,
	executionID uuid.UUID,
) {
	t.Helper()
	var execution persistence.AgentExecution
	if err := f.db.Where("tenant_id = ? AND id = ?", f.tenantID, executionID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	var worker persistence.WorkerInstance
	if err := f.db.Where("execution_target_id = ? AND current_manifest_id IS NOT NULL", f.targetID).
		Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if execution.ProviderRuntimeBindingID == nil || worker.CurrentManifestID == nil {
		t.Fatal("test execution or Worker omitted its Provider runtime manifest binding")
	}
	now := time.Now().UTC()
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", f.tenantID, execution.ID).
			Updates(map[string]any{
				"status": "completed", "worker_id": worker.ID,
				"worker_manifest_id": worker.CurrentManifestID,
				"started_at":         now, "finished_at": now,
			}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND id = ?", f.tenantID, execution.TurnID).
			Updates(map[string]any{"status": "completed", "started_at": now, "completed_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.ExecutionControlCommand{}).
			Where("tenant_id = ? AND execution_id = ?", f.tenantID, execution.ID).
			Updates(map[string]any{"status": "acknowledged", "acknowledged_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.ProviderRuntimeBinding{}).
			Where("tenant_id = ? AND id = ? AND session_id = ?", f.tenantID,
				*execution.ProviderRuntimeBindingID, f.sessionID).
			Updates(map[string]any{
				"worker_manifest_id": worker.CurrentManifestID,
				"last_execution_id":  execution.ID,
				"last_generation":    execution.Generation,
				"status":             "active", "updated_at": now,
			}).Error
	}); err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) markWorkersOffline(t *testing.T) {
	t.Helper()
	if err := f.db.Model(&persistence.WorkerInstance{}).
		Where("execution_target_id = ?", f.targetID).
		Update("status", "offline").Error; err != nil {
		t.Fatal(err)
	}
}

func (f advancedOperationFixture) appendCompletedTurn(t *testing.T) uuid.UUID {
	t.Helper()
	turnID := uuid.New()
	now := time.Now().UTC()
	if err := f.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: f.tenantID, SessionID: f.sessionID, CreatedBy: f.principal.UserID,
		Status: "completed", InputText: "history to roll back", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", CompletedAt: &now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		_, err := f.sessions.AppendInternalEvent(context.Background(), tx, f.tenantID, f.sessionID, sessions.InternalEventInput{
			EventType: "turn.created", ActorType: "user", ActorID: &f.principal.UserID,
			Payload: map[string]any{"turnId": turnID, "status": "completed", "inputText": "history to roll back"},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return turnID
}

func (f advancedOperationFixture) assertPrimaryOperationCounts(
	t *testing.T,
	turns, executions, commands, events, outboxMessages int64,
) {
	t.Helper()
	assertCount := func(model any, query string, expected int64, args ...any) {
		t.Helper()
		var count int64
		if err := f.db.Model(model).Where(query, args...).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != expected {
			t.Fatalf("%T count = %d, want %d", model, count, expected)
		}
	}
	assertCount(&persistence.AgentTurn{}, "tenant_id = ? AND session_id = ?", turns, f.tenantID, f.sessionID)
	assertCount(&persistence.AgentExecution{}, "tenant_id = ? AND session_id = ?", executions, f.tenantID, f.sessionID)
	assertCount(&persistence.ExecutionControlCommand{}, "tenant_id = ? AND session_id = ?", commands, f.tenantID, f.sessionID)
	assertCount(&persistence.SessionEvent{}, "tenant_id = ? AND session_id = ?", events, f.tenantID, f.sessionID)
	assertCount(&persistence.OutboxMessage{}, "tenant_id = ? AND topic = ?", outboxMessages, f.tenantID, "execution.queued")
}

func assertAdvancedOperationProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != status || apiError.Code != code {
		t.Fatalf("error = %#v, want status %d code %q", err, status, code)
	}
}
