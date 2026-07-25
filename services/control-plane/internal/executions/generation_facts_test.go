package executions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestGenerationFactTracksInitialLifecycleAndFallback(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	ensureGenerationFactTable(t, db)

	base := time.Date(2026, time.July, 24, 9, 0, 0, 0, time.UTC)
	queuedAt := base
	claimAt := base.Add(10 * time.Second)
	startAt := base.Add(15 * time.Second)
	readyAt := base.Add(20 * time.Second)
	fallbackAt := base.Add(22 * time.Second)
	completeAt := base.Add(25 * time.Second)

	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Update("queued_at", queuedAt).Error; err != nil {
		t.Fatal(err)
	}
	current := claimAt
	service.now = func() time.Time { return current }

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "generation-fact-initial", WorkerModeGeneralPool,
	)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "generation-fact-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("claim lease is nil")
	}

	leaseInput := LeaseInput{
		TenantID:   fixture.TenantID,
		Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}

	current = startAt
	if _, err := service.Start(ctx, worker, fixture.ExecutionID, leaseInput, "generation-fact-start"); err != nil {
		t.Fatal(err)
	}

	current = readyAt
	if _, err := service.AppendRuntimeEvent(ctx, worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput:   leaseInput,
		EventID:      uuid.New(),
		EventVersion: RuntimeEventVersionV2,
		EventType:    "session.started",
		Payload:      map[string]any{"message": "Provider session ready"},
		OccurredAt:   readyAt,
	}, "generation-fact-ready"); err != nil {
		t.Fatal(err)
	}

	fallbackPayload := map[string]any{
		"message": "Provider resume cursor was invalid; falling back to authoritative history.",
		"detail": map[string]any{
			"kind":                         "session_resume",
			"attemptedStrategy":            "native-cursor",
			"selectedStrategy":             "authoritative-history",
			"outcome":                      "fallback_selected",
			"reasonCode":                   "session_resume_invalid",
			"fallbackSafety":               "before_turn_activity",
			"authoritativeHistorySequence": float64(42),
			"provider":                     "codex",
		},
	}
	if _, err := service.AppendRuntimeEvent(ctx, worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput:   leaseInput,
		EventID:      uuid.New(),
		EventVersion: RuntimeEventVersionV2,
		EventType:    "runtime.warning",
		Payload:      fallbackPayload,
		OccurredAt:   fallbackAt,
	}, "generation-fact-fallback"); err != nil {
		t.Fatal(err)
	}

	current = completeAt
	if _, err := service.Complete(ctx, worker, fixture.ExecutionID, CompleteExecutionInput{
		LeaseInput: leaseInput,
	}, "generation-fact-complete"); err != nil {
		t.Fatal(err)
	}

	fact := loadGenerationFactForTest(t, db, fixture, claim.Value.Lease.Generation)
	if fact.RecoveryReason != "initial-claim" {
		t.Fatalf("recovery reason = %q, want initial-claim", fact.RecoveryReason)
	}
	if fact.WarmPoolMode != "disabled" {
		t.Fatalf("warm pool mode = %q, want disabled", fact.WarmPoolMode)
	}
	if fact.WarmPoolResult != generationWarmPoolResultNotRequested {
		t.Fatalf("warm pool result = %q, want %q", fact.WarmPoolResult, generationWarmPoolResultNotRequested)
	}
	assertTimeEqual(t, fact.DispatchRequestedAt, queuedAt, "dispatch_requested_at")
	assertTimeEqual(t, fact.BundleCreatedAt, claimAt, "bundle_created_at")
	assertTimeEqual(t, fact.LeasedAt, claimAt, "leased_at")
	assertTimeEqual(t, fact.ExecutionStartedAt, startAt, "execution_started_at")
	assertTimeEqual(t, fact.ProviderReadyAt, readyAt, "provider_ready_at")
	assertTimeEqual(t, fact.TerminalAt, completeAt, "terminal_at")
	assertStringEqual(t, fact.TerminalOutcome, generationTerminalOutcomeCompleted, "terminal_outcome")
	assertStringEqual(t, fact.ResumeAttemptedStrategy, "native-cursor", "resume_attempted_strategy")
	assertStringEqual(t, fact.ResumeSelectedStrategy, "authoritative-history", "resume_selected_strategy")
	assertStringEqual(t, fact.ResumeFallbackOutcome, "fallback_selected", "resume_fallback_outcome")
	assertStringEqual(t, fact.ResumeFallbackReasonCode, "session_resume_invalid", "resume_fallback_reason_code")
	assertStringEqual(t, fact.ResumeFallbackSafety, "before_turn_activity", "resume_fallback_safety")
	assertStringEqual(t, fact.ResumeFallbackProvider, "codex", "resume_fallback_provider")
	if fact.ResumeAuthoritativeHistorySequence == nil || *fact.ResumeAuthoritativeHistorySequence != 42 {
		t.Fatalf("resume_authoritative_history_sequence = %#v, want 42", fact.ResumeAuthoritativeHistorySequence)
	}
}

func TestGenerationFactTracksRecoveryDispatchAndInterruptedTerminal(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	ensureGenerationFactTable(t, db)

	base := time.Date(2026, time.July, 24, 10, 0, 0, 0, time.UTC)
	queuedAt := base
	claimOneAt := base.Add(10 * time.Second)
	recoveringAt := base.Add(20 * time.Second)
	claimTwoAt := base.Add(25 * time.Second)
	startTwoAt := base.Add(27 * time.Second)
	interruptAt := base.Add(29 * time.Second)

	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Update("queued_at", queuedAt).Error; err != nil {
		t.Fatal(err)
	}

	current := claimOneAt
	service.now = func() time.Time { return current }

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "generation-fact-recovery", WorkerModeGeneralPool,
	)
	firstClaim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "generation-fact-first-claim")
	if err != nil {
		t.Fatal(err)
	}
	if firstClaim.Value.Lease == nil {
		t.Fatal("first claim lease is nil")
	}
	firstLease := LeaseInput{
		TenantID:   fixture.TenantID,
		Generation: firstClaim.Value.Lease.Generation,
		LeaseToken: firstClaim.Value.Lease.LeaseToken,
	}

	current = recoveringAt
	if _, err := service.Release(ctx, worker, fixture.ExecutionID, ReleaseLeaseInput{
		LeaseInput: firstLease,
		Reason:     "worker-draining",
	}, "generation-fact-release"); err != nil {
		t.Fatal(err)
	}

	firstFact := loadGenerationFactForTest(t, db, fixture, firstClaim.Value.Lease.Generation)
	assertStringEqual(t, firstFact.TerminalOutcome, generationTerminalOutcomeRecovering, "generation1 terminal_outcome")
	assertTimeEqual(t, firstFact.TerminalAt, recoveringAt, "generation1 terminal_at")

	secondFact := loadGenerationFactForTest(t, db, fixture, firstClaim.Value.Lease.Generation+1)
	if secondFact.RecoveryReason != "execution-recovery" {
		t.Fatalf("generation2 recovery reason = %q, want execution-recovery", secondFact.RecoveryReason)
	}
	assertTimeEqual(t, secondFact.DispatchRequestedAt, recoveringAt, "generation2 dispatch_requested_at")
	if secondFact.DispatchRequestedAt == nil || secondFact.DispatchRequestedAt.Equal(queuedAt) {
		t.Fatalf("generation2 dispatch_requested_at = %#v, should not fall back to queued_at %s", secondFact.DispatchRequestedAt, queuedAt)
	}

	current = claimTwoAt
	secondClaim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "generation-fact-second-claim")
	if err != nil {
		t.Fatal(err)
	}
	if secondClaim.Value.Lease == nil {
		t.Fatal("second claim lease is nil")
	}
	secondLease := LeaseInput{
		TenantID:   fixture.TenantID,
		Generation: secondClaim.Value.Lease.Generation,
		LeaseToken: secondClaim.Value.Lease.LeaseToken,
	}
	assertTimeEqual(t, loadGenerationFactForTest(t, db, fixture, secondClaim.Value.Lease.Generation).DispatchRequestedAt, recoveringAt, "generation2 dispatch_requested_at after claim")

	current = startTwoAt
	if _, err := service.Start(ctx, worker, fixture.ExecutionID, secondLease, "generation-fact-second-start"); err != nil {
		t.Fatal(err)
	}

	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	requested, err := service.RequestInterrupt(
		ctx, principal, fixture.SessionID, "generation-fact-interrupt", "generation-fact-interrupt", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := service.PullControlCommands(ctx, worker, fixture.ExecutionID, PullControlCommandsInput{
		LeaseInput: secondLease,
		Limit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].CommandID != requested.Value.CommandID {
		t.Fatalf("unexpected control command deliveries: %#v", deliveries)
	}
	deliveryInput := ControlCommandDeliveryInput{
		LeaseInput: secondLease,
		CommandID:  deliveries[0].CommandID,
	}
	if _, err := service.MarkControlCommandDelivered(
		ctx, worker, fixture.ExecutionID, requested.Value.ID, deliveryInput, "generation-fact-interrupt-delivered",
	); err != nil {
		t.Fatal(err)
	}

	current = interruptAt
	cursor := "provider-cursor-after-interrupt"
	if _, err := service.AcknowledgeControlCommand(ctx, worker, fixture.ExecutionID, requested.Value.ID, ControlCommandDeliveryInput{
		LeaseInput:           secondLease,
		CommandID:            deliveries[0].CommandID,
		ProviderResumeCursor: &cursor,
	}, "generation-fact-interrupt-acknowledged"); err != nil {
		t.Fatal(err)
	}

	secondFact = loadGenerationFactForTest(t, db, fixture, secondClaim.Value.Lease.Generation)
	assertStringEqual(t, secondFact.TerminalOutcome, generationTerminalOutcomeInterrupted, "generation2 terminal_outcome")
	assertTimeEqual(t, secondFact.TerminalAt, interruptAt, "generation2 terminal_at")
}

func TestGenerationFactTerminalizesProspectiveRecoveryWhenCancelledBeforeClaim(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	ensureGenerationFactTable(t, db)

	claimAt := time.Date(2026, time.July, 24, 11, 0, 0, 0, time.UTC)
	recoveringAt := claimAt.Add(10 * time.Second)
	cancelledAt := recoveringAt.Add(5 * time.Second)
	current := claimAt
	service.now = func() time.Time { return current }
	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "generation-fact-cancel-recovery", WorkerModeGeneralPool,
	)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ExecutionID: &fixture.ExecutionID,
	}, "generation-fact-cancel-recovery-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	current = recoveringAt
	if _, err := service.Release(ctx, worker, fixture.ExecutionID, ReleaseLeaseInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		},
		Reason: "test-recovery",
	}, "generation-fact-cancel-recovery-release"); err != nil {
		t.Fatal(err)
	}

	current = cancelledAt
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	if _, err := service.Cancel(
		ctx, principal, fixture.ExecutionID,
		"generation-fact-cancel-recovery", "generation-fact-cancel-recovery", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	prospective := loadGenerationFactForTest(t, db, fixture, claim.Value.Lease.Generation+1)
	assertStringEqual(t, prospective.TerminalOutcome, generationTerminalOutcomeCancelled, "prospective terminal_outcome")
	assertTimeEqual(t, prospective.TerminalAt, cancelledAt, "prospective terminal_at")
}

func TestUpdateGenerationFactRowRejectsMissingGenerationFact(t *testing.T) {
	ctx := context.Background()
	db, _, fixture := setupSQLiteRecoveryService(t)
	ensureGenerationFactTable(t, db)

	err := updateGenerationFactRow(
		ctx,
		db,
		fixture.TenantID,
		fixture.ExecutionID,
		99,
		map[string]any{"warm_pool_result": generationWarmPoolResultFallback},
		"execution_generation_fact_test_update_failed",
		"The test generation fact could not be updated.",
	)
	if err == nil {
		t.Fatal("expected the missing generation fact update to fail")
	}
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "execution_generation_fact_missing" {
		t.Fatalf("error = %#v, want execution_generation_fact_missing", err)
	}
}

func loadGenerationFactForTest(
	t *testing.T,
	db *gorm.DB,
	fixture executionFixture,
	generation int64,
) persistence.ExecutionGenerationFact {
	t.Helper()
	var fact persistence.ExecutionGenerationFact
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?",
		fixture.TenantID,
		fixture.ExecutionID,
		generation,
	).Take(&fact).Error; err != nil {
		t.Fatal(err)
	}
	return fact
}

func ensureGenerationFactTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&persistence.ExecutionGenerationFact{}); err != nil {
		t.Fatal(err)
	}
}

func assertTimeEqual(t *testing.T, got *time.Time, want time.Time, field string) {
	t.Helper()
	if got == nil || !got.Equal(want) {
		t.Fatalf("%s = %#v, want %s", field, got, want)
	}
}

func assertStringEqual(t *testing.T, got *string, want, field string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %#v, want %q", field, got, want)
	}
}
