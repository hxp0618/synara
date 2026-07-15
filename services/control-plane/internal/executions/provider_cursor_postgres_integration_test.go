package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func TestProviderCursorMaximumAgeBoundaryOnPostgres(t *testing.T) {
	t.Run("usable immediately before expiry", func(t *testing.T) {
		ctx := context.Background()
		db := integrationDB(t)
		fixture := seedExecutionFixture(t, db)
		service := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
		service.providerCursorMaximumAge = time.Hour
		service.heartbeatTimeout = 24 * time.Hour
		current := time.Now().UTC().Truncate(time.Second)
		service.now = func() time.Time { return current }
		worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cursor-age-before-expiry")
		cleanupWorkers(t, db, worker.ID)

		const cursor = "postgres-cursor-before-expiry"
		seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, cursor)
		execution := createNextCursorExecution(t, db, fixture, "resume immediately before Cursor expiry")
		current = current.Add(time.Hour - time.Nanosecond)
		claim := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "cursor-age-before-expiry-claim")
		if claim.Value.ProviderResumeCursor == nil || *claim.Value.ProviderResumeCursor != cursor {
			t.Fatalf("pre-expiry Claim did not receive the native Cursor: %#v", claim.Value.ProviderResumeCursor)
		}
		decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
		if decision["selectedStrategy"] != "native-cursor" || decision["reasonCode"] != "cursor_usable" ||
			jsonInt64(t, decision["authoritativeHistorySequence"]) != claim.Value.Workload.ResumeSnapshot.AuthoritativeHistorySequence {
			t.Fatalf("pre-expiry Claim decision = %#v", decision)
		}
		lease := *claim.Value.Lease
		if _, err := service.Complete(ctx, worker, execution.ID, CompleteExecutionInput{
			LeaseInput: LeaseInput{
				TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			ProviderResumeCursor: pointer(cursor), Output: map[string]any{"boundary": "before-expiry"},
		}, "cursor-age-before-expiry-complete"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("expired at exact boundary", func(t *testing.T) {
		ctx := context.Background()
		db := integrationDB(t)
		fixture := seedExecutionFixture(t, db)
		service := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
		service.providerCursorMaximumAge = time.Hour
		service.heartbeatTimeout = 24 * time.Hour
		current := time.Now().UTC().Truncate(time.Second)
		service.now = func() time.Time { return current }
		worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cursor-age-at-expiry")
		cleanupWorkers(t, db, worker.ID)

		const cursor = "postgres-cursor-at-expiry"
		seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, cursor)
		before := loadProviderCursorSessionForTest(t, db, fixture)
		execution := createNextCursorExecution(t, db, fixture, "resume at exact Cursor expiry")
		current = current.Add(time.Hour)
		claim := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "cursor-age-at-expiry-claim")
		if claim.Value.ProviderResumeCursor != nil {
			t.Fatalf("exact-expiry Claim received the stale Cursor: %#v", claim.Value.ProviderResumeCursor)
		}
		assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
		decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
		if decision["selectedStrategy"] != "authoritative-history" || decision["reasonCode"] != "cursor_expired" ||
			decision["cursorState"] != providerCursorStateQuarantined ||
			jsonInt64(t, decision["authoritativeHistorySequence"]) != claim.Value.Workload.ResumeSnapshot.AuthoritativeHistorySequence {
			t.Fatalf("exact-expiry Claim decision = %#v", decision)
		}
		lease := *claim.Value.Lease
		if _, err := service.Fail(ctx, worker, execution.ID, FailExecutionInput{
			LeaseInput: LeaseInput{
				TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			FailureCode: "test_cursor_expired", FailureMessage: "exact Cursor expiry boundary",
		}, "cursor-age-at-expiry-fail"); err != nil {
			t.Fatal(err)
		}
		assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	})
}

func TestProviderCursorExpiredQuarantineCannotBeRevivedByPolicyChangeOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	key := bytes.Repeat([]byte{0x42}, 32)
	expiring := cursorIntegrationService(t, db, key)
	expiring.providerCursorMaximumAge = time.Hour
	expiring.heartbeatTimeout = 24 * time.Hour
	current := time.Now().UTC().Truncate(time.Second)
	expiring.now = func() time.Time { return current }
	worker := registerManifestTestWorker(t, expiring, fixture.TargetID, fixture.TargetKind, "cursor-expiry-no-revive")
	cleanupWorkers(t, db, worker.ID)

	seedUsableProviderCursor(t, ctx, db, expiring, worker, fixture, fixture.ExecutionID, "postgres-expiring-cursor")
	before := loadProviderCursorSessionForTest(t, db, fixture)
	expiredExecution := createNextCursorExecution(t, db, fixture, "expire Provider Cursor")
	current = current.Add(time.Hour)
	expiredClaim := claimCursorExecution(t, ctx, expiring, worker, fixture, expiredExecution.ID, "cursor-expiry-no-revive-expire")
	if expiredClaim.Value.ProviderResumeCursor != nil {
		t.Fatalf("expired Cursor reached the Worker: %#v", expiredClaim.Value.ProviderResumeCursor)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	expiredLease := *expiredClaim.Value.Lease
	if _, err := expiring.Fail(ctx, worker, expiredExecution.ID, FailExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: expiredLease.Generation, LeaseToken: expiredLease.LeaseToken,
		},
		FailureCode: "test_cursor_expired", FailureMessage: "preserve quarantined ciphertext",
	}, "cursor-expiry-no-revive-fail"); err != nil {
		t.Fatal(err)
	}

	extended := cursorIntegrationService(t, db, key)
	extended.providerCursorMaximumAge = 365 * 24 * time.Hour
	extended.heartbeatTimeout = 24 * time.Hour
	extended.now = func() time.Time { return current }
	rebuildExecution := createNextCursorExecution(t, db, fixture, "rebuild after Cursor expiry")
	rebuildClaim := claimCursorExecution(t, ctx, extended, worker, fixture, rebuildExecution.ID, "cursor-expiry-no-revive-rebuild")
	if rebuildClaim.Value.ProviderResumeCursor != nil {
		t.Fatalf("longer maximum age revived a quarantined Cursor: %#v", rebuildClaim.Value.ProviderResumeCursor)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	rebuildDecision := loadProviderResumeDecision(t, db, fixture, rebuildExecution.ID)
	if rebuildDecision["reasonCode"] != "cursor_quarantined" || rebuildDecision["selectedStrategy"] != "authoritative-history" {
		t.Fatalf("quarantined Cursor rebuild decision = %#v", rebuildDecision)
	}

	rebuildLease := *rebuildClaim.Value.Lease
	const freshCursor = "postgres-fresh-cursor-after-expiry"
	if _, err := extended.Complete(ctx, worker, rebuildExecution.ID, CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: rebuildLease.Generation, LeaseToken: rebuildLease.LeaseToken,
		},
		ProviderResumeCursor: pointer(freshCursor), Output: map[string]any{"mode": "fresh-cursor"},
	}, "cursor-expiry-no-revive-complete"); err != nil {
		t.Fatal(err)
	}
	stored := loadProviderCursorSessionForTest(t, db, fixture)
	if stored.ProviderResumeCursorState != providerCursorStateUsable ||
		stored.ProviderResumeCursorSourceExecutionID == nil || *stored.ProviderResumeCursorSourceExecutionID != rebuildExecution.ID ||
		stored.ProviderResumeCursorSourceGeneration == nil || *stored.ProviderResumeCursorSourceGeneration != rebuildLease.Generation ||
		bytes.Equal(stored.ProviderResumeCursorEncrypted, before.ProviderResumeCursorEncrypted) {
		t.Fatalf("fresh current-Generation Cursor did not replace quarantine: %#v", stored)
	}
	consumeExecution := createNextCursorExecution(t, db, fixture, "consume fresh Cursor after expiry")
	consumeClaim := claimCursorExecution(t, ctx, extended, worker, fixture, consumeExecution.ID, "cursor-expiry-no-revive-consume")
	if consumeClaim.Value.ProviderResumeCursor == nil || *consumeClaim.Value.ProviderResumeCursor != freshCursor {
		t.Fatalf("fresh Cursor did not restore native resume: %#v", consumeClaim.Value.ProviderResumeCursor)
	}
}

func TestProviderCursorIssuedBeyondClockSkewIsQuarantinedOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
	service.providerCursorMaximumAge = time.Hour
	service.heartbeatTimeout = 24 * time.Hour
	issuedAt := time.Now().UTC().Truncate(time.Second).Add(10 * time.Minute)
	current := issuedAt
	service.now = func() time.Time { return current }
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cursor-future-issued")
	cleanupWorkers(t, db, worker.ID)

	seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, "postgres-future-issued-cursor")
	before := loadProviderCursorSessionForTest(t, db, fixture)
	execution := createNextCursorExecution(t, db, fixture, "resume with future-issued Cursor")
	current = issuedAt.Add(-providerCursorMaximumFutureSkew - time.Nanosecond)
	claim := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "cursor-future-issued-claim")
	if claim.Value.ProviderResumeCursor != nil {
		t.Fatalf("future-issued Cursor reached the Worker: %#v", claim.Value.ProviderResumeCursor)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
	if decision["reasonCode"] != "cursor_issued_in_future" || decision["selectedStrategy"] != "authoritative-history" {
		t.Fatalf("future-issued Cursor decision = %#v", decision)
	}
}

func TestProviderCursorMaximumFutureClockSkewRemainsUsableOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
	service.providerCursorMaximumAge = time.Hour
	service.heartbeatTimeout = 24 * time.Hour
	issuedAt := time.Now().UTC().Truncate(time.Second).Add(10 * time.Minute)
	current := issuedAt
	service.now = func() time.Time { return current }
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cursor-maximum-future-skew")
	cleanupWorkers(t, db, worker.ID)

	const cursor = "postgres-maximum-future-skew-cursor"
	seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, cursor)
	execution := createNextCursorExecution(t, db, fixture, "resume at maximum future Cursor skew")
	current = issuedAt.Add(-providerCursorMaximumFutureSkew)
	claim := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "cursor-maximum-future-skew-claim")
	if claim.Value.ProviderResumeCursor == nil || *claim.Value.ProviderResumeCursor != cursor {
		t.Fatalf("maximum allowed future skew rejected the Cursor: %#v", claim.Value.ProviderResumeCursor)
	}
	decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
	if decision["reasonCode"] != "cursor_usable" || decision["selectedStrategy"] != "native-cursor" {
		t.Fatalf("maximum future skew Cursor decision = %#v", decision)
	}
	lease := *claim.Value.Lease
	if _, err := service.Complete(ctx, worker, execution.ID, CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		ProviderResumeCursor: pointer(cursor), Output: map[string]any{"boundary": "maximum-future-skew"},
	}, "cursor-maximum-future-skew-complete"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderCursorClaimReplayAcrossServicesPreservesDecisionPastExpiryOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	replicaDB := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	key := bytes.Repeat([]byte{0x42}, 32)
	services := [2]*Service{cursorIntegrationService(t, db, key), cursorIntegrationService(t, replicaDB, key)}
	current := time.Now().UTC().Truncate(time.Second)
	for _, service := range services {
		service.providerCursorMaximumAge = time.Hour
		service.heartbeatTimeout = 24 * time.Hour
		service.now = func() time.Time { return current }
	}
	worker := registerManifestTestWorker(t, services[0], fixture.TargetID, fixture.TargetKind, "cursor-replay-past-expiry")
	cleanupWorkers(t, db, worker.ID)
	const cursor = "postgres-replay-past-expiry-cursor"
	seedUsableProviderCursor(t, ctx, db, services[0], worker, fixture, fixture.ExecutionID, cursor)

	execution := createNextCursorExecution(t, db, fixture, "Claim replay across Cursor expiry")
	const requestID = "cursor-replay-past-expiry"
	current = current.Add(time.Hour - time.Nanosecond)
	first := claimCursorExecution(t, ctx, services[0], worker, fixture, execution.ID, requestID)
	if first.Value.ProviderResumeCursor == nil || *first.Value.ProviderResumeCursor != cursor || first.Replayed {
		t.Fatalf("pre-expiry Claim did not select the native Cursor: %#v", first)
	}
	current = current.Add(2 * time.Nanosecond)
	replayed := claimCursorExecution(t, ctx, services[1], worker, fixture, execution.ID, requestID)
	if replayed.Value.ProviderResumeCursor == nil || *replayed.Value.ProviderResumeCursor != cursor || !replayed.Replayed {
		t.Fatalf("cross-Service Claim replay changed the committed native decision: %#v", replayed)
	}
	if state := loadProviderCursorSessionForTest(t, db, fixture); state.ProviderResumeCursorState != providerCursorStateUsable {
		t.Fatalf("cross-Service Claim replay quarantined the committed Cursor: %#v", state)
	}
	var eventCount int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND generation = ? AND event_type = ?",
			fixture.TenantID, fixture.SessionID, execution.ID, replayed.Value.Execution.Generation, "execution.leased").
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("cross-Service Claim replay appended %d execution.leased decisions", eventCount)
	}
	decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
	if decision["selectedStrategy"] != "native-cursor" || decision["reasonCode"] != "cursor_usable" {
		t.Fatalf("cross-Service replay decision = %#v", decision)
	}
}

func TestProviderCursorExpiredClaimRetryAcrossServicesAppendsOneDecisionOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	replicaDB := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	key := bytes.Repeat([]byte{0x42}, 32)
	services := [2]*Service{cursorIntegrationService(t, db, key), cursorIntegrationService(t, replicaDB, key)}
	current := time.Now().UTC().Truncate(time.Second)
	for _, service := range services {
		service.providerCursorMaximumAge = time.Hour
		service.heartbeatTimeout = 24 * time.Hour
		service.now = func() time.Time { return current }
	}
	worker := registerManifestTestWorker(t, services[0], fixture.TargetID, fixture.TargetKind, "cursor-claim-retry")
	cleanupWorkers(t, db, worker.ID)
	const cursor = "postgres-concurrent-expired-cursor"
	seedUsableProviderCursor(t, ctx, db, services[0], worker, fixture, fixture.ExecutionID, cursor)
	before := loadProviderCursorSessionForTest(t, db, fixture)
	current = current.Add(time.Hour)
	execution := createNextCursorExecution(t, db, fixture, "concurrent expired Provider Cursor Claim")
	const requestID = "cursor-concurrent-expired-claim-retry"

	type outcome struct {
		result OperationResult[ClaimResult]
		err    error
	}
	start := make(chan struct{})
	completed := make(chan outcome, len(services))
	var wait sync.WaitGroup
	for _, service := range services {
		wait.Add(1)
		go func(service *Service) {
			defer wait.Done()
			<-start
			result, err := service.Claim(ctx, worker, ClaimExecutionInput{
				ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &execution.ID,
			}, requestID)
			completed <- outcome{result: result, err: err}
		}(service)
	}
	close(start)
	wait.Wait()
	close(completed)

	successes := 0
	for item := range completed {
		if item.err != nil {
			var apiError *problem.Error
			if !errors.As(item.err, &apiError) || apiError.Status != 409 || apiError.Code != "request_receipt_conflict" {
				t.Fatalf("concurrent Claim failed unexpectedly: %#v, result=%#v", apiError, item.result)
			}
			continue
		}
		successes++
		if item.result.Value.ProviderResumeCursor != nil || item.result.Value.Workload == nil ||
			item.result.Value.Workload.ResumeSnapshot == nil {
			t.Fatalf("concurrent Claim returned an invalid resume decision: %#v", item.result)
		}
	}
	if successes == 0 {
		t.Fatal("concurrent Claims produced no successful response")
	}

	var replayed OperationResult[ClaimResult]
	for index, service := range services {
		replayed = claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, requestID)
		if !replayed.Replayed || replayed.Value.ProviderResumeCursor != nil {
			t.Fatalf("Service %d did not converge on the committed authoritative-history decision: %#v", index, replayed)
		}
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	var eventCount int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND generation = ? AND event_type = ?",
			fixture.TenantID, fixture.SessionID, execution.ID, replayed.Value.Execution.Generation, "execution.leased").
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("cross-Service Claim retry appended %d execution.leased decisions", eventCount)
	}
	decision := loadProviderResumeDecision(t, db, fixture, execution.ID)
	if decision["selectedStrategy"] != "authoritative-history" || decision["reasonCode"] != "cursor_expired" ||
		decision["cursorState"] != providerCursorStateQuarantined ||
		jsonInt64(t, decision["authoritativeHistorySequence"]) != replayed.Value.Workload.ResumeSnapshot.AuthoritativeHistorySequence {
		t.Fatalf("cross-Service Claim decision = %#v", decision)
	}
}

func TestProviderCursorWrongKeyQuarantinesWithoutBlockingLifecycleAndFreshCursorRecovers(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	primary := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
	wrongKey := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x73}, 32))
	worker := registerManifestTestWorker(t, primary, fixture.TargetID, fixture.TargetKind, "cursor-wrong-key")
	cleanupWorkers(t, db, worker.ID)

	seedUsableProviderCursor(t, ctx, db, primary, worker, fixture, fixture.ExecutionID, "cursor-before-key-drift")
	before := loadProviderCursorSessionForTest(t, db, fixture)
	second := createNextCursorExecution(t, db, fixture, "authoritative fallback under wrong key")
	claim := claimCursorExecution(t, ctx, wrongKey, worker, fixture, second.ID, "cursor-wrong-key-claim")
	if claim.Value.ProviderResumeCursor != nil {
		t.Fatalf("wrong-key claim received a Provider Cursor: %#v", claim.Value.ProviderResumeCursor)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	lease := *claim.Value.Lease
	if _, err := wrongKey.Renew(ctx, worker, second.ID, RenewLeaseInput{LeaseInput: LeaseInput{
		TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}}, "cursor-wrong-key-renew"); err != nil {
		t.Fatalf("wrong-key Cursor blocked Renew: %v", err)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	if _, err := wrongKey.Complete(ctx, worker, second.ID, CompleteExecutionInput{
		LeaseInput: LeaseInput{TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken},
		Output:     map[string]any{"mode": "authoritative-history"},
	}, "cursor-wrong-key-complete"); err != nil {
		t.Fatalf("wrong-key Cursor blocked Complete: %v", err)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)

	third := createNextCursorExecution(t, db, fixture, "fresh cursor after quarantine")
	thirdClaim := claimCursorExecution(t, ctx, primary, worker, fixture, third.ID, "cursor-fresh-claim")
	if thirdClaim.Value.ProviderResumeCursor != nil {
		t.Fatalf("restored key resurrected the stale Cursor: %#v", thirdClaim.Value.ProviderResumeCursor)
	}
	thirdLease := *thirdClaim.Value.Lease
	fresh := "cursor-after-authoritative-rebuild"
	if _, err := primary.Complete(ctx, worker, third.ID, CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: thirdLease.Generation, LeaseToken: thirdLease.LeaseToken,
		},
		ProviderResumeCursor: &fresh, Output: map[string]any{"mode": "fresh-native-cursor"},
	}, "cursor-fresh-complete"); err != nil {
		t.Fatalf("fresh Cursor did not recover quarantined state: %v", err)
	}
	stored := loadProviderCursorSessionForTest(t, db, fixture)
	if stored.ProviderResumeCursorState != providerCursorStateUsable ||
		stored.ProviderResumeCursorSourceExecutionID == nil || *stored.ProviderResumeCursorSourceExecutionID != third.ID ||
		stored.ProviderResumeCursorSourceGeneration == nil || *stored.ProviderResumeCursorSourceGeneration != thirdLease.Generation {
		t.Fatalf("fresh Cursor did not restore usable lineage: %#v", stored)
	}
	fourth := createNextCursorExecution(t, db, fixture, "consume fresh cursor")
	fourthClaim := claimCursorExecution(t, ctx, primary, worker, fixture, fourth.ID, "cursor-fresh-resume")
	if fourthClaim.Value.ProviderResumeCursor == nil || *fourthClaim.Value.ProviderResumeCursor != fresh {
		t.Fatalf("fresh Cursor was not resumed: %#v", fourthClaim.Value.ProviderResumeCursor)
	}
	fourthLease := *fourthClaim.Value.Lease
	if _, err := primary.Fail(ctx, worker, fourth.ID, FailExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: fourthLease.Generation, LeaseToken: fourthLease.LeaseToken,
		},
		FailureCode: "test_complete", FailureMessage: "cleanup",
	}, "cursor-fresh-cleanup"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderCursorMissingCipherDoesNotBlockFailAndCannotResurrect(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	primary := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
	withoutCipher := cursorIntegrationService(t, db, nil)
	worker := registerManifestTestWorker(t, primary, fixture.TargetID, fixture.TargetKind, "cursor-no-cipher")
	cleanupWorkers(t, db, worker.ID)

	seedUsableProviderCursor(t, ctx, db, primary, worker, fixture, fixture.ExecutionID, "cursor-before-cipher-loss")
	before := loadProviderCursorSessionForTest(t, db, fixture)
	second := createNextCursorExecution(t, db, fixture, "authoritative fallback without cipher")
	claim := claimCursorExecution(t, ctx, withoutCipher, worker, fixture, second.ID, "cursor-no-cipher-claim")
	if claim.Value.ProviderResumeCursor != nil {
		t.Fatalf("nil Cipher claim received a Provider Cursor: %#v", claim.Value.ProviderResumeCursor)
	}
	lease := *claim.Value.Lease
	if _, err := withoutCipher.Fail(ctx, worker, second.ID, FailExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		FailureCode: "provider_unavailable", FailureMessage: "test nil Cipher fallback",
	}, "cursor-no-cipher-fail"); err != nil {
		t.Fatalf("nil Cipher blocked Fail: %v", err)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
	third := createNextCursorExecution(t, db, fixture, "correct key after cipher outage")
	thirdClaim := claimCursorExecution(t, ctx, primary, worker, fixture, third.ID, "cursor-after-cipher-outage")
	if thirdClaim.Value.ProviderResumeCursor != nil {
		t.Fatalf("correct key resurrected a Cursor skipped during the outage: %#v", thirdClaim.Value.ProviderResumeCursor)
	}
}

func TestProviderCursorRenewAndSteerWithoutFreshCursorRemainUsableButInterruptQuarantines(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cursor-control")
	cleanupWorkers(t, db, worker.ID)
	seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, "cursor-before-control")
	before := loadProviderCursorSessionForTest(t, db, fixture)
	execution := createNextCursorExecution(t, db, fixture, "control lifecycle")
	claim := claimCursorExecution(t, ctx, service, worker, fixture, execution.ID, "cursor-control-claim")
	lease := *claim.Value.Lease
	leaseInput := LeaseInput{TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken}
	if _, err := service.Renew(ctx, worker, execution.ID, RenewLeaseInput{LeaseInput: leaseInput}, "cursor-control-renew"); err != nil {
		t.Fatal(err)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateUsable, before.ProviderResumeCursorEncrypted)
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	steer, err := service.RequestSteer(
		ctx, principal, fixture.SessionID, SteerActiveTurnInput{InputText: "continue safely"},
		"cursor-steer", "cursor-steer", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := service.PullControlCommands(ctx, worker, execution.ID, PullControlCommandsInput{
		LeaseInput: leaseInput, Limit: 1,
	})
	if err != nil || len(deliveries) != 1 || deliveries[0].ControlCommandID != steer.Value.ID {
		t.Fatalf("Steer delivery failed: %#v, %v", deliveries, err)
	}
	delivery := ControlCommandDeliveryInput{LeaseInput: leaseInput, CommandID: steer.Value.CommandID}
	if _, err := service.MarkControlCommandDelivered(ctx, worker, execution.ID, steer.Value.ID, delivery, "cursor-steer-delivered"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcknowledgeControlCommand(ctx, worker, execution.ID, steer.Value.ID, delivery, "cursor-steer-acknowledged"); err != nil {
		t.Fatal(err)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateUsable, before.ProviderResumeCursorEncrypted)

	interrupt, err := service.RequestInterrupt(
		ctx, principal, fixture.SessionID, "cursor-interrupt", "cursor-interrupt", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err = service.PullControlCommands(ctx, worker, execution.ID, PullControlCommandsInput{
		LeaseInput: leaseInput, Limit: 1,
	})
	if err != nil || len(deliveries) != 1 || deliveries[0].ControlCommandID != interrupt.Value.ID {
		t.Fatalf("Interrupt delivery failed: %#v, %v", deliveries, err)
	}
	delivery = ControlCommandDeliveryInput{LeaseInput: leaseInput, CommandID: interrupt.Value.CommandID}
	if _, err := service.MarkControlCommandDelivered(ctx, worker, execution.ID, interrupt.Value.ID, delivery, "cursor-interrupt-delivered"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcknowledgeControlCommand(ctx, worker, execution.ID, interrupt.Value.ID, delivery, "cursor-interrupt-acknowledged"); err != nil {
		t.Fatalf("Interrupt without a fresh Cursor failed: %v", err)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, before.ProviderResumeCursorEncrypted)
}

func TestProviderCursorRuntimeAndCredentialDriftCannotResurrectOldCursor(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
	stableWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cursor-stable-runtime")
	driftCapabilities := workerManifestTestCapabilities()
	setTestProviderCapability(driftCapabilities, "codex", "resume-session", "emulated")
	driftWorker := registerTestWorkerWithCapabilities(
		t, service, fixture.TargetID, fixture.TargetKind, "cursor-authoritative-runtime", driftCapabilities,
	)
	cleanupWorkers(t, db, stableWorker.ID, driftWorker.ID)
	seedUsableProviderCursor(t, ctx, db, service, stableWorker, fixture, fixture.ExecutionID, "cursor-before-runtime-drift")
	oldCiphertext := loadProviderCursorSessionForTest(t, db, fixture).ProviderResumeCursorEncrypted

	driftExecution := createNextCursorExecution(t, db, fixture, "runtime without native resume")
	driftClaim := claimCursorExecution(t, ctx, service, driftWorker, fixture, driftExecution.ID, "cursor-runtime-drift-claim")
	if driftClaim.Value.ProviderResumeCursor != nil {
		t.Fatalf("authoritative-only runtime received a native Cursor: %#v", driftClaim.Value.ProviderResumeCursor)
	}
	assertProviderCursorState(t, db, fixture, providerCursorStateQuarantined, oldCiphertext)
	driftLease := *driftClaim.Value.Lease
	if _, err := service.Fail(ctx, driftWorker, driftExecution.ID, FailExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: driftLease.Generation, LeaseToken: driftLease.LeaseToken,
		},
		FailureCode: "test_runtime_drift", FailureMessage: "authoritative runtime completed",
	}, "cursor-runtime-drift-fail"); err != nil {
		t.Fatal(err)
	}

	rebuildExecution := createNextCursorExecution(t, db, fixture, "rebuild after runtime drift")
	rebuildClaim := claimCursorExecution(t, ctx, service, stableWorker, fixture, rebuildExecution.ID, "cursor-runtime-return")
	if rebuildClaim.Value.ProviderResumeCursor != nil {
		t.Fatalf("returning runtime resurrected the pre-drift Cursor: %#v", rebuildClaim.Value.ProviderResumeCursor)
	}
	rebuildLease := *rebuildClaim.Value.Lease
	rebuiltCursor := "cursor-after-runtime-rebuild"
	if _, err := service.Complete(ctx, stableWorker, rebuildExecution.ID, CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: rebuildLease.Generation, LeaseToken: rebuildLease.LeaseToken,
		},
		ProviderResumeCursor: &rebuiltCursor,
	}, "cursor-runtime-rebuild-complete"); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ProviderCredentialID).
		Updates(map[string]any{
			"version":            2,
			"encrypted_payload":  []byte("rotated-provider-payload"),
			"encrypted_data_key": []byte("rotated-provider-data-key"),
			"updated_at":         time.Now().UTC(),
		}).Error; err != nil {
		t.Fatal(err)
	}
	credentialExecution := createNextCursorExecution(t, db, fixture, "credential version drift")
	credentialClaim := claimCursorExecution(t, ctx, service, stableWorker, fixture, credentialExecution.ID, "cursor-credential-drift")
	if credentialClaim.Value.ProviderResumeCursor != nil {
		t.Fatalf("rotated Credential received the old Cursor: %#v", credentialClaim.Value.ProviderResumeCursor)
	}
	cleared := loadProviderCursorSessionForTest(t, db, fixture)
	if cleared.ProviderResumeCursorState != providerCursorStateAbsent || len(cleared.ProviderResumeCursorEncrypted) != 0 {
		t.Fatalf("Credential drift did not discard the incompatible Cursor: %#v", cleared)
	}
}

func TestProviderCursorCASDoesNotOverwriteNewerCiphertext(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := cursorIntegrationService(t, db, bytes.Repeat([]byte{0x42}, 32))
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "cursor-cas")
	cleanupWorkers(t, db, worker.ID)
	seedUsableProviderCursor(t, ctx, db, service, worker, fixture, fixture.ExecutionID, "cursor-before-cas")
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	binding, available := providerCursorBindingFromExecution(execution)
	if !available {
		t.Fatal("Cursor CAS fixture omitted a native binding")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		stale, err := lockProviderCursorSession(ctx, tx, execution)
		if err != nil {
			return err
		}
		payload := providerCursorPayloadV1{
			Cursor: "newer-cursor", SourceExecutionID: execution.ID, SourceGeneration: execution.Generation,
			AuthoritativeHistorySequence: stale.LastEventSequence, IssuedAt: time.Now().UTC(),
		}
		plain, err := jsonMarshalProviderCursorPayload(payload)
		if err != nil {
			return err
		}
		newer, err := service.cursorCipher.SealV2(plain, binding.Version, binding.Digest)
		if err != nil {
			return err
		}
		replaced, err := replaceProviderCursorCAS(ctx, tx, execution, stale, newer, payload)
		if err != nil || !replaced {
			t.Fatalf("first Cursor CAS failed: replaced=%t err=%v", replaced, err)
		}
		payload.Cursor = "stale-overwrite"
		plain, err = jsonMarshalProviderCursorPayload(payload)
		if err != nil {
			return err
		}
		staleReplacement, err := service.cursorCipher.SealV2(plain, binding.Version, binding.Digest)
		if err != nil {
			return err
		}
		replaced, err = replaceProviderCursorCAS(ctx, tx, execution, stale, staleReplacement, payload)
		if err != nil {
			return err
		}
		if replaced {
			t.Fatal("stale Cursor CAS overwrote newer ciphertext")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stored := loadProviderCursorSessionForTest(t, db, fixture)
	plain, status, err := service.cursorCipher.OpenV2(stored.ProviderResumeCursorEncrypted, binding.Version, binding.Digest)
	if err != nil || status != secret.CursorOpenValid {
		t.Fatalf("newer Cursor was not retained: status=%s err=%v", status, err)
	}
	var payload providerCursorPayloadV1
	if err := json.Unmarshal(plain, &payload); err != nil || payload.Cursor != "newer-cursor" {
		t.Fatalf("stale CAS changed the Cursor payload: %#v err=%v", payload, err)
	}
}

func cursorIntegrationService(t *testing.T, db *gorm.DB, key []byte) *Service {
	t.Helper()
	cipher, err := secret.NewCursorCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(db, testPlatformConfig(), cipher)
	return NewService(
		db, sessions.NewService(db, projects.NewService(db), targetService),
		30*time.Second, 2*time.Minute, time.Hour, cipher, targetService,
	)
}

func seedUsableProviderCursor(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	service *Service,
	worker persistence.WorkerInstance,
	fixture executionFixture,
	executionID uuid.UUID,
	cursor string,
) {
	t.Helper()
	claim := claimCursorExecution(t, ctx, service, worker, fixture, executionID, "seed-cursor-"+uuid.NewString())
	lease := *claim.Value.Lease
	if _, err := service.Complete(ctx, worker, executionID, CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		ProviderResumeCursor: &cursor, Output: map[string]any{"cursor": "seeded"},
	}, "seed-cursor-complete-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	stored := loadProviderCursorSessionForTest(t, db, fixture)
	if stored.ProviderResumeCursorState != providerCursorStateUsable || len(stored.ProviderResumeCursorEncrypted) == 0 {
		t.Fatalf("seed Cursor was not usable: %#v", stored)
	}
}

func createNextCursorExecution(
	t *testing.T,
	db *gorm.DB,
	fixture executionFixture,
	inputText string,
) persistence.AgentExecution {
	t.Helper()
	var session persistence.AgentSession
	if err := db.Select("current_runtime_binding_id", "provider").
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.CurrentRuntimeBindingID == nil {
		t.Fatal("Cursor fixture Session omitted a runtime binding")
	}
	now := time.Now().UTC()
	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: fixture.TenantID, SessionID: fixture.SessionID,
		CreatedBy: fixture.UserID, Status: "queued", InputText: inputText,
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now,
	}
	provider := session.Provider
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.TenantID, SessionID: fixture.SessionID, TurnID: turn.ID,
		Attempt: 1, Status: "queued", ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		Provider: &provider, ProviderRuntimeBindingID: session.CurrentRuntimeBindingID,
		Generation: 0, RequestedBy: fixture.UserID, QueuedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&turn).Error; err != nil {
			return err
		}
		return tx.Create(&execution).Error
	}); err != nil {
		t.Fatal(err)
	}
	return execution
}

func claimCursorExecution(
	t *testing.T,
	ctx context.Context,
	service *Service,
	worker persistence.WorkerInstance,
	fixture executionFixture,
	executionID uuid.UUID,
	requestID string,
) OperationResult[ClaimResult] {
	t.Helper()
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &executionID,
	}, requestID)
	if err != nil || claim.Value.Lease == nil || claim.Value.Execution == nil || claim.Value.Workload == nil {
		t.Fatalf("Cursor claim failed: result=%#v err=%v", claim, err)
	}
	return claim
}

func loadProviderCursorSessionForTest(t *testing.T, db *gorm.DB, fixture executionFixture) persistence.AgentSession {
	t.Helper()
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session
}

func assertProviderCursorState(
	t *testing.T,
	db *gorm.DB,
	fixture executionFixture,
	wantState string,
	wantCiphertext []byte,
) {
	t.Helper()
	session := loadProviderCursorSessionForTest(t, db, fixture)
	if session.ProviderResumeCursorState != wantState ||
		!bytes.Equal(session.ProviderResumeCursorEncrypted, wantCiphertext) {
		t.Fatalf("Provider Cursor state = %q/%x, want %q/%x", session.ProviderResumeCursorState,
			session.ProviderResumeCursorEncrypted, wantState, wantCiphertext)
	}
}

func jsonMarshalProviderCursorPayload(payload providerCursorPayloadV1) ([]byte, error) {
	return json.Marshal(payload)
}
