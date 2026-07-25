package executions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestPostgresConcurrentSuspendCompletionHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	services := [2]*Service{integrationService(t, db), integrationService(t, db)}
	firstWorker := registerManifestTestWorker(t, services[0], fixture.TargetID, fixture.TargetKind, "pg-suspend-first")
	secondWorker := registerManifestTestWorker(t, services[0], fixture.TargetID, fixture.TargetKind, "pg-suspend-second")
	cleanupWorkers(t, db, firstWorker.ID, secondWorker.ID)
	claim, err := services[0].Claim(ctx, firstWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "pg-suspend-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	firstLease := *claim.Value.Lease
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: firstLease.Generation, LeaseToken: firstLease.LeaseToken,
	}
	if _, err := services[0].Start(ctx, firstWorker, fixture.ExecutionID, leaseInput, "pg-suspend-start"); err != nil {
		t.Fatal(err)
	}
	requestID := "pg-suspend-approval-" + uuid.NewString()
	if _, err := services[0].AppendRuntimeEvent(ctx, firstWorker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "request.opened", OccurredAt: time.Now().UTC(),
		Payload: map[string]any{
			"requestId": requestID, "requestType": "exec_command_approval", "detail": "Concurrent suspend",
		},
	}, "pg-suspend-opened"); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND request_id = ?", fixture.TenantID, fixture.ExecutionID, requestID).
		Update("requested_at", time.Now().UTC().Add(-16*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := services[0].PullResourceDirective(ctx, firstWorker, fixture.ExecutionID, PullResourceDirectiveInput{LeaseInput: leaseInput})
	if err != nil || directive == nil {
		t.Fatalf("directive = %#v, %v", directive, err)
	}
	if _, err := services[0].MarkResourceSuspendQuiesced(ctx, firstWorker, fixture.ExecutionID, MarkResourceSuspendQuiescedInput{
		LeaseInput: leaseInput, SuspendAttemptID: directive.SuspendAttemptID,
	}, "pg-suspend-quiesced"); err != nil {
		t.Fatalf("mark quiesced = %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range services {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := services[index].CompleteResourceSuspend(ctx, firstWorker, fixture.ExecutionID, CompleteResourceSuspendInput{
				LeaseInput: leaseInput, SuspendAttemptID: directive.SuspendAttemptID, CheckpointStatus: "unchanged",
			}, "pg-suspend-complete-"+string(rune('a'+index)))
			results <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded := 0
	for result := range results {
		if result == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent suspend completions succeeded %d times, want exactly 1", succeeded)
	}
	assertExecutionStatus(t, db, fixture, "suspended")
	var completedAttempts int64
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND execution_id = ? AND status = ?", fixture.TenantID, fixture.ExecutionID, "completed").
		Count(&completedAttempts).Error; err != nil {
		t.Fatal(err)
	}
	if completedAttempts != 1 {
		t.Fatalf("completed suspend attempts = %d, want 1", completedAttempts)
	}

	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	if _, err := services[1].ResolveApproval(
		ctx, principal, fixture.ExecutionID, requestID, ResolveApprovalInput{Decision: "accept"},
		"pg-suspend-resolve", "pg-suspend-resolve-audit", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	resumed, err := services[1].Claim(ctx, secondWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "pg-suspend-resume-claim")
	if err != nil || resumed.Value.Lease == nil || resumed.Value.Workload == nil || resumed.Value.Workload.RecoveryBundle == nil {
		t.Fatalf("resume claim = %#v, %v", resumed, err)
	}
	if resumed.Value.Workload.RecoveryBundle.RecoveryReason != "suspend-resume" ||
		resumed.Value.Lease.Generation != firstLease.Generation+1 {
		t.Fatalf("unexpected resumed lineage: lease=%#v bundle=%#v", resumed.Value.Lease, resumed.Value.Workload.RecoveryBundle)
	}
	var boundResolution persistence.ExecutionInteraction
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.TenantID, fixture.ExecutionID, requestID,
	).Take(&boundResolution).Error; err != nil {
		t.Fatal(err)
	}
	if boundResolution.DeliveryStatus != "resume-bound" || boundResolution.ResumeBundleID == nil ||
		*boundResolution.ResumeBundleID != resumed.Value.Workload.RecoveryBundle.ID ||
		boundResolution.ResumeGeneration == nil || *boundResolution.ResumeGeneration != resumed.Value.Lease.Generation {
		t.Fatalf("Postgres did not bind the resolution to its exact Recovery Bundle: %#v", boundResolution)
	}
	if err := db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, boundResolution.ID).
		Update("resume_bundle_id", *resumed.Value.Workload.RecoveryBundle.PreviousBundleID).Error; err == nil {
		t.Fatal("Postgres allowed a recovery-bound resolution to be rebound to another Bundle")
	}
	var resumedLease persistence.WorkerLease
	if err := db.Where(
		"tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID,
	).Take(&resumedLease).Error; err != nil {
		t.Fatal(err)
	}
	services[1].now = func() time.Time { return resumedLease.ExpiresAt.Add(time.Second) }
	if err := services[1].RecoverExpired(ctx, 10); err != nil {
		t.Fatal(err)
	}
	assertExecutionStatus(t, db, fixture, "failed")
	if err := db.Where(
		"tenant_id = ? AND id = ?", fixture.TenantID, boundResolution.ID,
	).Take(&boundResolution).Error; err != nil {
		t.Fatal(err)
	}
	if boundResolution.DeliveryStatus != "outcome-unknown" || boundResolution.ResumeBundleID == nil ||
		*boundResolution.ResumeBundleID != resumed.Value.Workload.RecoveryBundle.ID {
		t.Fatalf("Postgres did not fail closed after losing a recovery-bound generation: %#v", boundResolution)
	}
}
