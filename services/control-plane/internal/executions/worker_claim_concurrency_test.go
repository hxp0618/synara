package executions

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

type executionClaimOutcome struct {
	result OperationResult[ClaimResult]
	err    error
}

type workspaceCleanupClaimOutcome struct {
	result OperationResult[WorkspaceCleanupClaimResult]
	err    error
}

func TestPostgresConcurrentExecutionClaimSameRequestIDReplaysSingleClaimLedgerEntry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	primaryDB, replicaDB := isolatedPostgresClaimTestDBs(t)
	fixture := seedExecutionFixtureWithoutCleanup(t, primaryDB)
	services := [2]*Service{integrationService(t, primaryDB), integrationService(t, replicaDB)}
	worker := registerManifestTestWorker(t, services[0], fixture.TargetID, fixture.TargetKind, "claim-concurrency-execution")

	requestID := "claim-concurrency-execution-" + uuid.NewString()
	firstReceiptReady := make(chan struct{})
	releaseFirstReceipt := make(chan struct{})
	secondWorkerLockAttempt := make(chan struct{})
	installWorkerRequestReceiptCreateBarrier(t, services[0].db, requestID, firstReceiptReady, releaseFirstReceipt)
	installWorkerLockAttemptSignal(t, services[1].db, secondWorkerLockAttempt)

	firstDone := make(chan executionClaimOutcome, 1)
	secondDone := make(chan executionClaimOutcome, 1)

	go func() {
		result, err := services[0].Claim(ctx, worker, ClaimExecutionInput{
			ExecutionTargetID: fixture.TargetID,
			TargetKind:        fixture.TargetKind,
			ExecutionID:       &fixture.ExecutionID,
		}, requestID)
		firstDone <- executionClaimOutcome{result: result, err: err}
	}()
	waitForConcurrencySignal(t, ctx, firstReceiptReady, "first execution claim receipt creation")

	go func() {
		result, err := services[1].Claim(ctx, worker, ClaimExecutionInput{
			ExecutionTargetID: fixture.TargetID,
			TargetKind:        fixture.TargetKind,
			ExecutionID:       &fixture.ExecutionID,
		}, requestID)
		secondDone <- executionClaimOutcome{result: result, err: err}
	}()
	waitForConcurrencySignal(t, ctx, secondWorkerLockAttempt, "second execution claim Worker lock")
	close(releaseFirstReceipt)

	first := waitForExecutionClaimOutcome(t, ctx, firstDone, "first execution claim")
	assertExecutionClaimOutcome(t, "first execution claim", first, fixture.ExecutionID, requestID, false)
	second := waitForExecutionClaimOutcome(t, ctx, secondDone, "second execution claim")
	assertExecutionClaimOutcome(t, "second execution claim", second, fixture.ExecutionID, requestID, true)

	assertConcurrentExecutionClaimReplay(t, []OperationResult[ClaimResult]{first.result, second.result}, fixture.ExecutionID, requestID)
	assertWorkerClaimLedgerState(t, primaryDB, worker, requestID, workerClaimKindExecution)
}

func TestPostgresConcurrentWorkspaceCleanupClaimSameRequestIDReplaysSingleClaimLedgerEntry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	primaryDB, replicaDB := isolatedPostgresClaimTestDBs(t)
	fixture, _, _ := seedWorkspaceCleanupFixtureWithoutCleanup(t, primaryDB, false)
	services := [2]*Service{integrationService(t, primaryDB), integrationService(t, replicaDB)}
	worker := registerManifestTestWorker(t, services[0], fixture.TargetID, fixture.TargetKind, "claim-concurrency-cleanup")

	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, service := range services {
		service.now = func() time.Time { return now }
	}
	created, err := services[0].ReconcileWorkspaceCleanup(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands created = %d, want 1", created)
	}

	requestID := "claim-concurrency-cleanup-" + uuid.NewString()
	firstReceiptReady := make(chan struct{})
	releaseFirstReceipt := make(chan struct{})
	secondWorkerLockAttempt := make(chan struct{})
	installWorkerRequestReceiptCreateBarrier(t, services[0].db, requestID, firstReceiptReady, releaseFirstReceipt)
	installWorkerLockAttemptSignal(t, services[1].db, secondWorkerLockAttempt)

	firstDone := make(chan workspaceCleanupClaimOutcome, 1)
	secondDone := make(chan workspaceCleanupClaimOutcome, 1)

	go func() {
		result, err := services[0].ClaimWorkspaceCleanup(ctx, worker, WorkspaceCleanupClaimInput{}, requestID)
		firstDone <- workspaceCleanupClaimOutcome{result: result, err: err}
	}()
	waitForConcurrencySignal(t, ctx, firstReceiptReady, "first cleanup claim receipt creation")

	go func() {
		result, err := services[1].ClaimWorkspaceCleanup(ctx, worker, WorkspaceCleanupClaimInput{}, requestID)
		secondDone <- workspaceCleanupClaimOutcome{result: result, err: err}
	}()
	waitForConcurrencySignal(t, ctx, secondWorkerLockAttempt, "second cleanup claim Worker lock")
	close(releaseFirstReceipt)

	first := waitForWorkspaceCleanupClaimOutcome(t, ctx, firstDone, "first cleanup claim")
	assertWorkspaceCleanupClaimOutcome(t, "first cleanup claim", first, requestID, false)
	second := waitForWorkspaceCleanupClaimOutcome(t, ctx, secondDone, "second cleanup claim")
	assertWorkspaceCleanupClaimOutcome(t, "second cleanup claim", second, requestID, true)

	assertConcurrentWorkspaceCleanupClaimReplay(t, []OperationResult[WorkspaceCleanupClaimResult]{first.result, second.result}, requestID)
	assertWorkerClaimLedgerState(t, primaryDB, worker, requestID, workerClaimKindWorkspaceCleanup)
}

func installWorkerRequestReceiptCreateBarrier(
	t *testing.T,
	db *gorm.DB,
	requestID string,
	ready chan<- struct{},
	release <-chan struct{},
) {
	t.Helper()

	callbackName := "worker-claim-receipt-create-barrier-" + uuid.NewString()
	var once sync.Once
	if err := db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		receipt, ok := workerRequestReceiptCreateTarget(tx)
		if !ok || receipt.RequestID != requestID {
			return
		}
		once.Do(func() { close(ready) })
		select {
		case <-release:
		case <-tx.Statement.Context.Done():
			_ = tx.AddError(tx.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatalf("register receipt barrier callback: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Callback().Create().Remove(callbackName); err != nil {
			t.Errorf("remove receipt barrier callback: %v", err)
		}
	})
}

func installWorkerLockAttemptSignal(t *testing.T, db *gorm.DB, attempted chan<- struct{}) {
	t.Helper()

	callbackName := "worker-claim-worker-lock-signal-" + uuid.NewString()
	var once sync.Once
	if err := db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx == nil || tx.Statement == nil || tx.Statement.Schema == nil ||
			tx.Statement.Schema.Table != "worker_instances" {
			return
		}
		if _, ok := tx.Statement.Dest.(*persistence.WorkerInstance); !ok {
			return
		}
		lockingClause, ok := tx.Statement.Clauses["FOR"]
		if !ok {
			return
		}
		locking, ok := lockingClause.Expression.(clause.Locking)
		if !ok || locking.Strength != "UPDATE" {
			return
		}
		once.Do(func() { close(attempted) })
	}); err != nil {
		t.Fatalf("register Worker lock signal callback: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Callback().Query().Remove(callbackName); err != nil {
			t.Errorf("remove Worker lock signal callback: %v", err)
		}
	})
}

func workerRequestReceiptCreateTarget(tx *gorm.DB) (*persistence.WorkerRequestReceipt, bool) {
	if tx == nil || tx.Statement == nil || tx.Statement.Schema == nil ||
		tx.Statement.Schema.Table != "worker_request_receipts" {
		return nil, false
	}
	switch receipt := tx.Statement.Dest.(type) {
	case *persistence.WorkerRequestReceipt:
		return receipt, true
	case persistence.WorkerRequestReceipt:
		return &receipt, true
	default:
		return nil, false
	}
}

func waitForConcurrencySignal(t *testing.T, ctx context.Context, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s: %v", label, ctx.Err())
	}
}

func waitForExecutionClaimOutcome(
	t *testing.T,
	ctx context.Context,
	done <-chan executionClaimOutcome,
	label string,
) executionClaimOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s: %v", label, ctx.Err())
		return executionClaimOutcome{}
	}
}

func waitForWorkspaceCleanupClaimOutcome(
	t *testing.T,
	ctx context.Context,
	done <-chan workspaceCleanupClaimOutcome,
	label string,
) workspaceCleanupClaimOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s: %v", label, ctx.Err())
		return workspaceCleanupClaimOutcome{}
	}
}

func assertExecutionClaimOutcome(
	t *testing.T,
	outcomeLabel string,
	outcome executionClaimOutcome,
	executionID uuid.UUID,
	requestID string,
	wantReplay bool,
) {
	t.Helper()
	if outcome.err != nil {
		t.Fatalf("%s returned err=%s result=%#v", outcomeLabel, describeClaimTestError(outcome.err), outcome.result)
	}
	if outcome.result.Replayed != wantReplay {
		t.Fatalf("%s replay=%v, want %v; result=%#v", outcomeLabel, outcome.result.Replayed, wantReplay, outcome.result)
	}
	if outcome.result.Value.Execution == nil || outcome.result.Value.Execution.ID != executionID {
		t.Fatalf("%s returned wrong execution for %s: %#v", outcomeLabel, requestID, outcome.result.Value.Execution)
	}
	if outcome.result.Value.Lease == nil {
		t.Fatalf("%s returned nil lease for %s: %#v", outcomeLabel, requestID, outcome.result)
	}
	if outcome.result.Value.Workload == nil || outcome.result.Value.Workload.RecoveryBundle == nil {
		t.Fatalf("%s omitted frozen workload for %s: %#v", outcomeLabel, requestID, outcome.result.Value.Workload)
	}
}

func assertWorkspaceCleanupClaimOutcome(
	t *testing.T,
	outcomeLabel string,
	outcome workspaceCleanupClaimOutcome,
	requestID string,
	wantReplay bool,
) {
	t.Helper()
	if outcome.err != nil {
		t.Fatalf("%s returned err=%s result=%#v", outcomeLabel, describeClaimTestError(outcome.err), outcome.result)
	}
	if outcome.result.Replayed != wantReplay {
		t.Fatalf("%s replay=%v, want %v; result=%#v", outcomeLabel, outcome.result.Replayed, wantReplay, outcome.result)
	}
	if outcome.result.Value.Cleanup == nil {
		t.Fatalf("%s returned nil cleanup for %s: %#v", outcomeLabel, requestID, outcome.result)
	}
}

func assertConcurrentExecutionClaimReplay(
	t *testing.T,
	outcomes []OperationResult[ClaimResult],
	executionID uuid.UUID,
	requestID string,
) {
	t.Helper()

	replayed := 0
	original := 0
	for _, outcome := range outcomes {
		if outcome.Replayed {
			replayed++
		} else {
			original++
		}
		if outcome.Value.Execution == nil || outcome.Value.Execution.ID != executionID {
			t.Fatalf("execution claim returned wrong execution for request %s: %#v", requestID, outcome.Value.Execution)
		}
		if outcome.Value.Lease == nil {
			t.Fatalf("execution claim returned no lease for request %s: %#v", requestID, outcome)
		}
		if outcome.Value.Workload == nil || outcome.Value.Workload.RecoveryBundle == nil {
			t.Fatalf("execution claim omitted frozen workload for request %s: %#v", requestID, outcome.Value.Workload)
		}
	}
	if replayed != 1 || original != 1 {
		t.Fatalf("execution claim replay/original split = %d/%d, want 1/1; outcomes=%#v", replayed, original, outcomes)
	}
	if outcomes[0].Value.Lease.Generation != outcomes[1].Value.Lease.Generation {
		t.Fatalf("execution claim generations diverged: first=%#v second=%#v", outcomes[0].Value.Lease, outcomes[1].Value.Lease)
	}
}

func assertConcurrentWorkspaceCleanupClaimReplay(
	t *testing.T,
	outcomes []OperationResult[WorkspaceCleanupClaimResult],
	requestID string,
) {
	t.Helper()

	replayed := 0
	original := 0
	var cleanupID uuid.UUID
	var dispatchGeneration int64
	for index, outcome := range outcomes {
		if outcome.Replayed {
			replayed++
		} else {
			original++
		}
		if outcome.Value.Cleanup == nil {
			t.Fatalf("cleanup claim %d returned no cleanup for request %s: %#v", index, requestID, outcome)
		}
		if index == 0 {
			cleanupID = outcome.Value.Cleanup.CleanupID
			dispatchGeneration = outcome.Value.Cleanup.DispatchGeneration
			continue
		}
		if outcome.Value.Cleanup.CleanupID != cleanupID ||
			outcome.Value.Cleanup.DispatchGeneration != dispatchGeneration {
			t.Fatalf("cleanup claim changed canonical cleanup identity: first=%#v second=%#v", outcomes[0].Value.Cleanup, outcome.Value.Cleanup)
		}
	}
	if replayed != 1 || original != 1 {
		t.Fatalf("cleanup claim replay/original split = %d/%d, want 1/1; outcomes=%#v", replayed, original, outcomes)
	}
}

func assertWorkerClaimLedgerState(
	t *testing.T,
	db *gorm.DB,
	worker persistence.WorkerInstance,
	requestID string,
	claimKind string,
) {
	t.Helper()

	fact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.ClaimCount != 1 {
		t.Fatalf("worker incarnation fact claim_count = %d, want 1; fact=%#v", fact.ClaimCount, fact)
	}
	claims := loadWorkerClaimFactsForTest(t, db, worker.ID, worker.Incarnation)
	if len(claims) != 1 || claims[0].RequestID != requestID || claims[0].ClaimKind != claimKind {
		t.Fatalf("worker claim facts = %#v, want one %s row for %s", claims, claimKind, requestID)
	}
	var receiptCount int64
	if err := db.Model(&persistence.WorkerRequestReceipt{}).
		Where("worker_id = ? AND worker_incarnation = ? AND request_id = ?",
			worker.ID, worker.Incarnation, requestID).
		Count(&receiptCount).Error; err != nil {
		t.Fatal(err)
	}
	if receiptCount != 1 {
		t.Fatalf("worker request receipt count = %d, want 1", receiptCount)
	}
}

func describeClaimTestError(err error) string {
	if err == nil {
		return "<nil>"
	}
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return fmt.Sprintf("problem(status=%d code=%s details=%#v): %s", apiError.Status, apiError.Code, apiError.Details, apiError.Message)
	}
	return fmt.Sprintf("%T: %v", err, err)
}

func isolatedPostgresClaimTestDBs(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()

	databaseURL := strings.TrimSpace(os.Getenv("SYNARA_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}

	ctx := context.Background()
	adminDB, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open postgres admin db: %v", err)
	}

	schemaName := "claim_concurrency_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	if err := adminDB.WithContext(ctx).Exec(fmt.Sprintf(`CREATE SCHEMA "%s"`, schemaName)).Error; err != nil {
		t.Fatalf("create isolated schema %s: %v", schemaName, err)
	}
	var primaryDB *gorm.DB
	var replicaDB *gorm.DB
	t.Cleanup(func() {
		if replicaDB != nil {
			if sqlDB, err := replicaDB.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
		if primaryDB != nil {
			if sqlDB, err := primaryDB.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
		if err := adminDB.WithContext(context.Background()).
			Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE`, schemaName)).Error; err != nil {
			t.Errorf("drop isolated schema %s: %v", schemaName, err)
		}
		if sqlDB, err := adminDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	schemaURL := schemaScopedDatabaseURL(t, databaseURL, schemaName)
	primaryDB, err = database.Open(ctx, schemaURL)
	if err != nil {
		t.Fatalf("open isolated primary db: %v", err)
	}
	if err := database.Migrate(ctx, primaryDB, migrations.Files); err != nil {
		t.Fatalf("migrate isolated schema %s: %v", schemaName, err)
	}
	replicaDB, err = database.Open(ctx, schemaURL)
	if err != nil {
		t.Fatalf("open isolated replica db: %v", err)
	}

	return primaryDB, replicaDB
}

func schemaScopedDatabaseURL(t *testing.T, databaseURL, schemaName string) string {
	t.Helper()

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse SYNARA_TEST_DATABASE_URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schemaName)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
