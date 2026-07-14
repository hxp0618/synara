package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestExpiredProviderCursorFallsBackToAuditedAuthoritativeHistoryOnSQLite(t *testing.T) {
	ctx := context.Background()
	db, service, worker, fixture := sqliteProviderCursorPolicyFixture(t)
	current := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	service.providerCursorMaximumAge = time.Hour
	service.now = func() time.Time { return current }
	const cursor = "sqlite-expiring-provider-cursor"
	seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, cursor)
	before := loadProviderCursorSessionForTest(t, db, fixture)

	execution := createNextCursorExecution(t, db, fixture, "resume after Cursor expiry")
	current = current.Add(time.Hour)
	claim := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "sqlite-expired-cursor-claim")
	if claim.Value.ProviderResumeCursor != nil {
		t.Fatalf("expired Cursor reached the Worker: %#v", claim.Value.ProviderResumeCursor)
	}
	if claim.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("expired Cursor fallback omitted authoritative history: %#v", claim.Value.Workload)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)

	decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
	authoritativeHistorySequence := claim.Value.Workload.ResumeSnapshot.AuthoritativeHistorySequence
	if decision["requestedStrategy"] != "native-cursor" ||
		decision["selectedStrategy"] != "authoritative-history" ||
		decision["reasonCode"] != "cursor_expired" ||
		decision["cursorState"] != providerCursorStateQuarantined ||
		decision["cursorSourceExecutionId"] != fixture.ExecutionID.String() ||
		jsonInt64(t, decision["cursorSourceGeneration"]) != 1 ||
		jsonInt64(t, decision["authoritativeHistorySequence"]) != authoritativeHistorySequence ||
		jsonInt64(t, decision["maximumAgeSeconds"]) != int64(time.Hour/time.Second) {
		t.Fatalf("unexpected expired Cursor decision: %#v", decision)
	}
	var session persistence.AgentSession
	if err := db.Select("last_event_sequence").Where(
		"tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID,
	).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if authoritativeHistorySequence == session.LastEventSequence {
		t.Fatalf("fixture did not distinguish Resume Snapshot sequence %d from Session tail %d", authoritativeHistorySequence, session.LastEventSequence)
	}
	if decision["cursorIssuedAt"] != "2026-07-14T12:00:00Z" ||
		decision["cursorExpiresAt"] != "2026-07-14T13:00:00Z" {
		t.Fatalf("unexpected Cursor expiry boundary: %#v", decision)
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(cursor)) || bytes.Contains(encoded, before.ProviderResumeCursorEncrypted) {
		t.Fatal("Provider Resume decision leaked Cursor material")
	}
}

func TestProviderCursorClaimReplayPreservesGenerationDecisionPastExpiry(t *testing.T) {
	ctx := context.Background()
	db, service, worker, fixture := sqliteProviderCursorPolicyFixture(t)
	current := time.Date(2026, 7, 14, 14, 0, 0, 0, time.UTC)
	service.providerCursorMaximumAge = time.Hour
	service.now = func() time.Time { return current }
	const cursor = "sqlite-replayed-provider-cursor"
	seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, cursor)

	execution := createNextCursorExecution(t, db, fixture, "claim replay across Cursor expiry")
	current = current.Add(time.Hour - time.Nanosecond)
	first := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "sqlite-cursor-replay")
	if first.Value.ProviderResumeCursor == nil || *first.Value.ProviderResumeCursor != cursor || first.Replayed {
		t.Fatalf("pre-expiry Claim did not select the native Cursor: %#v", first)
	}

	current = current.Add(2 * time.Nanosecond)
	replayed := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "sqlite-cursor-replay")
	if replayed.Value.ProviderResumeCursor == nil || *replayed.Value.ProviderResumeCursor != cursor || !replayed.Replayed {
		t.Fatalf("idempotent Claim replay changed the Generation decision: %#v", replayed)
	}
	if state := loadProviderCursorSessionForTest(t, db, fixture); state.ProviderResumeCursorState != providerCursorStateUsable {
		t.Fatalf("Claim replay quarantined a Cursor selected by the same Generation: %#v", state)
	}

	decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
	if decision["selectedStrategy"] != "native-cursor" || decision["reasonCode"] != "cursor_usable" {
		t.Fatalf("native Cursor selection was not audited: %#v", decision)
	}
	var eventCount int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
			fixture.TenantID, fixture.SessionID, execution.ID, "execution.leased").
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("Claim replay appended %d execution.leased Events", eventCount)
	}
}

func TestProviderCursorClaimReplayRejectsCursorWrittenAfterInitialClaim(t *testing.T) {
	ctx := context.Background()
	db, service, worker, fixture := sqliteProviderCursorPolicyFixture(t)
	current := time.Date(2026, 7, 14, 15, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return current }
	const initialCursor = "sqlite-initial-provider-cursor"
	seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, initialCursor)

	execution := createNextCursorExecution(t, db, fixture, "claim replay after current generation Cursor update")
	const requestID = "sqlite-cursor-replay-after-renew"
	first := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, requestID)
	if first.Value.ProviderResumeCursor == nil || *first.Value.ProviderResumeCursor != initialCursor || first.Replayed {
		t.Fatalf("initial Claim did not select the prior Cursor: %#v", first)
	}
	decisionBefore := loadProviderResumeDecision(t, db, fixture, execution.ID)
	lease := *first.Value.Lease
	const freshCursor = "sqlite-current-generation-provider-cursor"
	if _, err := service.Renew(ctx, worker, execution.ID, RenewLeaseInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		ProviderResumeCursor: pointer(freshCursor),
	}, "sqlite-current-generation-cursor-renew"); err != nil {
		t.Fatal(err)
	}
	stored := loadProviderCursorSessionForTest(t, db, fixture)
	if stored.ProviderResumeCursorState != providerCursorStateUsable ||
		stored.ProviderResumeCursorSourceExecutionID == nil || *stored.ProviderResumeCursorSourceExecutionID != execution.ID ||
		stored.ProviderResumeCursorSourceGeneration == nil || *stored.ProviderResumeCursorSourceGeneration != lease.Generation {
		t.Fatalf("Renew did not store the current Generation Cursor: %#v", stored)
	}

	replayed, replayErr := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &execution.ID,
	}, requestID)
	var apiError *problem.Error
	if !errors.As(replayErr, &apiError) || apiError.Status != 409 || apiError.Code != "claim_replay_resume_cursor_unavailable" {
		t.Fatalf("Claim replay with a replaced Cursor error = %#v, result=%#v", apiError, replayed)
	}
	if replayed.Replayed || replayed.Value.Execution != nil || replayed.Value.Lease != nil || replayed.Value.Workload != nil ||
		replayed.Value.ProviderResumeCursor != nil {
		t.Fatalf("rejected Claim replay returned execution state: %#v", replayed)
	}
	afterReplay := loadProviderCursorSessionForTest(t, db, fixture)
	if afterReplay.ProviderResumeCursorSourceExecutionID == nil ||
		*afterReplay.ProviderResumeCursorSourceExecutionID != execution.ID ||
		afterReplay.ProviderResumeCursorSourceGeneration == nil ||
		*afterReplay.ProviderResumeCursorSourceGeneration != lease.Generation ||
		!bytes.Equal(afterReplay.ProviderResumeCursorEncrypted, stored.ProviderResumeCursorEncrypted) {
		t.Fatalf("rejected Claim replay changed the current Generation Cursor: %#v", afterReplay)
	}
	decisionAfter := loadProviderResumeDecision(t, db, fixture, execution.ID)
	beforeJSON, err := json.Marshal(decisionBefore)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(decisionAfter)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("Claim replay changed the committed Resume decision: before=%s after=%s", beforeJSON, afterJSON)
	}
	var eventCount int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
			fixture.TenantID, fixture.SessionID, execution.ID, "execution.leased").
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("Claim replay appended %d execution.leased Events", eventCount)
	}
}

func TestProviderCursorIssuedBeyondClockSkewIsQuarantinedOnSQLite(t *testing.T) {
	ctx := context.Background()
	db, service, worker, fixture := sqliteProviderCursorPolicyFixture(t)
	issuedAt := time.Date(2026, 7, 14, 16, 0, 0, 0, time.UTC)
	current := issuedAt
	service.providerCursorMaximumAge = time.Hour
	service.now = func() time.Time { return current }
	seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, "future-provider-cursor")
	before := loadProviderCursorSessionForTest(t, db, fixture)

	execution := createNextCursorExecution(t, db, fixture, "resume with future Cursor timestamp")
	current = issuedAt.Add(-providerCursorMaximumFutureSkew - time.Nanosecond)
	claim := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "sqlite-future-cursor-claim")
	if claim.Value.ProviderResumeCursor != nil {
		t.Fatalf("future-issued Cursor reached the Worker: %#v", claim.Value.ProviderResumeCursor)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
	if decision["reasonCode"] != "cursor_issued_in_future" ||
		decision["selectedStrategy"] != "authoritative-history" {
		t.Fatalf("future-issued Cursor decision = %#v", decision)
	}
}

func sqliteProviderCursorPolicyFixture(
	t *testing.T,
) (*gorm.DB, *Service, persistence.WorkerInstance, executionFixture) {
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
	fixture := seedExecutionFixture(t, store.DB())
	service := cursorIntegrationService(t, store.DB(), bytes.Repeat([]byte{0x42}, 32))
	service.heartbeatTimeout = 24 * time.Hour
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "sqlite-cursor-policy")
	cleanupWorkers(t, store.DB(), worker.ID)
	return store.DB(), service, worker, fixture
}

func loadProviderResumeDecision(
	t *testing.T,
	db *gorm.DB,
	fixture executionFixture,
	executionID uuid.UUID,
) map[string]any {
	t.Helper()
	var event persistence.SessionEvent
	if err := db.Where(
		"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
		fixture.TenantID, fixture.SessionID, executionID, "execution.leased",
	).Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	decision, ok := event.Payload["providerResume"].(map[string]any)
	if !ok {
		t.Fatalf("execution.leased omitted providerResume decision: %#v", event.Payload)
	}
	return decision
}

func jsonInt64(t *testing.T, value any) int64 {
	t.Helper()
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		t.Fatalf("JSON number %T(%v) is not numeric", value, value)
		return 0
	}
}
