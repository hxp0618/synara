package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestExecutionClaimReleaseFactRecordsCompletionAndRejectsConflict(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "claim-release-complete")
	cleanupWorkers(t, db, worker.ID)

	claimAt := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return claimAt }
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "claim-release-complete-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim Execution: %#v, %v", claim.Value, err)
	}

	completedAt := claimAt.Add(5 * time.Second)
	service.now = func() time.Time { return completedAt }
	requestID := "claim-release-complete-request"
	if _, err := service.Complete(ctx, worker, fixture.ExecutionID, CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		},
	}, requestID); err != nil {
		t.Fatal(err)
	}

	claimFact, releaseFact := loadExecutionClaimReleaseFactForTest(
		t, db, fixture.ExecutionID, claim.Value.Lease.Generation,
	)
	if claimFact.ClaimKind != workerClaimKindExecution || releaseFact.ClaimFactID != claimFact.ID ||
		releaseFact.ReleaseReason != workerClaimReleaseExecutionCompleted ||
		!releaseFact.ReleasedAt.Equal(completedAt) || !releaseFact.RecordedAt.Equal(completedAt) ||
		releaseFact.AuthorityKind != workerClaimReleaseAuthorityWorker || releaseFact.AuthorityID == nil ||
		*releaseFact.AuthorityID != worker.ID.String() || releaseFact.RequestID == nil || *releaseFact.RequestID != requestID {
		t.Fatalf("execution claim release fact = %#v, claim = %#v", releaseFact, claimFact)
	}

	lease := persistence.WorkerLease{ExecutionID: fixture.ExecutionID, Generation: claim.Value.Lease.Generation}
	exact := executionClaimReleaseInput(
		lease, completedAt, completedAt, workerClaimReleaseExecutionCompleted,
		workerClaimReleaseAuthorityWorker, worker.ID.String(), requestID,
	)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return recordWorkerClaimReleaseFact(ctx, tx, exact)
	}); err != nil {
		t.Fatalf("exact release replay: %v", err)
	}
	invalidTimeline := exact
	invalidTimeline.RecordedAt = invalidTimeline.ReleasedAt.Add(-time.Second)
	err = db.Transaction(func(tx *gorm.DB) error {
		return recordWorkerClaimReleaseFact(ctx, tx, invalidTimeline)
	})
	assertProblemCode(t, err, "worker_claim_release_time_invalid")
	conflict := exact
	conflict.ReleaseReason = workerClaimReleaseExecutionFailed
	err = db.Transaction(func(tx *gorm.DB) error {
		return recordWorkerClaimReleaseFact(ctx, tx, conflict)
	})
	assertProblemCode(t, err, "worker_claim_release_conflict")
}

func TestWorkspaceCleanupClaimReleaseFactRecordsAcknowledgement(t *testing.T) {
	ctx := context.Background()
	db, service, _ := setupSQLiteRecoveryService(t)
	fixture, _, _ := seedWorkspaceCleanupFixture(t, db, false)
	base := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return base }
	if created, err := service.ReconcileWorkspaceCleanup(ctx, base, 10); err != nil || created != 1 {
		t.Fatalf("reconcile Workspace cleanup = %d, %v", created, err)
	}
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "claim-release-cleanup")
	cleanupWorkers(t, db, worker.ID)

	claimAt := base.Add(time.Second)
	service.now = func() time.Time { return claimAt }
	claimed, err := service.ClaimWorkspaceCleanup(ctx, worker, WorkspaceCleanupClaimInput{}, "claim-release-cleanup-claim")
	if err != nil || claimed.Value.Cleanup == nil {
		t.Fatalf("claim Workspace cleanup: %#v, %v", claimed.Value, err)
	}
	cleanup := *claimed.Value.Cleanup
	leaseInput := WorkspaceCleanupLeaseInput{
		DispatchGeneration: cleanup.DispatchGeneration, LeaseToken: cleanup.Lease.LeaseToken,
	}
	service.now = func() time.Time { return claimAt.Add(time.Second) }
	if _, err := service.StartWorkspaceCleanup(ctx, worker, cleanup.CleanupID, leaseInput, "claim-release-cleanup-start"); err != nil {
		t.Fatal(err)
	}
	acknowledgedAt := claimAt.Add(3 * time.Second)
	service.now = func() time.Time { return acknowledgedAt }
	requestID := "claim-release-cleanup-ack"
	if _, err := service.AcknowledgeWorkspaceCleanup(ctx, worker, cleanup.CleanupID, leaseInput, requestID); err != nil {
		t.Fatal(err)
	}

	claimFact, releaseFact := loadWorkspaceCleanupClaimReleaseFactForTest(
		t, db, cleanup.CleanupID, cleanup.DispatchGeneration,
	)
	if claimFact.ClaimKind != workerClaimKindWorkspaceCleanup || releaseFact.ClaimFactID != claimFact.ID ||
		releaseFact.ReleaseReason != workerClaimReleaseCleanupAcknowledged ||
		!releaseFact.ReleasedAt.Equal(acknowledgedAt) || !releaseFact.RecordedAt.Equal(acknowledgedAt) ||
		releaseFact.AuthorityKind != workerClaimReleaseAuthorityWorker || releaseFact.RequestID == nil ||
		*releaseFact.RequestID != requestID {
		t.Fatalf("Workspace cleanup claim release fact = %#v, claim = %#v", releaseFact, claimFact)
	}
}

func TestExpiredExecutionClaimReleaseUsesOriginalLeaseDeadline(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "claim-release-expired")
	cleanupWorkers(t, db, worker.ID)
	claimAt := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return claimAt }
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "claim-release-expired-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim Execution: %#v, %v", claim.Value, err)
	}
	recordedAt := claim.Value.Lease.ExpiresAt.Add(7 * time.Second)
	service.now = func() time.Time { return recordedAt }
	if err := service.RecoverExpired(ctx, 10); err != nil {
		t.Fatal(err)
	}
	_, release := loadExecutionClaimReleaseFactForTest(
		t, db, fixture.ExecutionID, claim.Value.Lease.Generation,
	)
	if release.ReleaseReason != workerClaimReleaseLeaseExpired ||
		!release.ReleasedAt.Equal(claim.Value.Lease.ExpiresAt) || !release.RecordedAt.Equal(recordedAt) {
		t.Fatalf("expired execution release fact = %#v", release)
	}
}

func TestSyntheticAbsoluteExpiryLeaseDoesNotRecordReleaseFact(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "claim-release-synthetic")
	cleanupWorkers(t, db, worker.ID)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "claim-release-synthetic-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim Execution: %#v, %v", claim.Value, err)
	}
	// Preserve this direct-delete legacy fixture: the first rollout deliberately
	// has no deletion guard while migration-era writers and rows coexist.
	if err := db.Delete(&persistence.WorkerLease{}, "tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).Error; err != nil {
		t.Fatal(err)
	}
	absoluteExpiry := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	if changed, err := service.EnforceResourceLifecycle(ctx, absoluteExpiry.Add(time.Second), 10); err != nil || changed != 1 {
		t.Fatalf("enforce synthetic absolute expiry = %d, %v", changed, err)
	}
	var releases int64
	if err := db.Model(&persistence.WorkerClaimReleaseFact{}).
		Joins("JOIN worker_claim_facts AS claim ON claim.id = worker_claim_release_facts.claim_fact_id").
		Where("claim.execution_id = ? AND claim.execution_generation = ?", fixture.ExecutionID, claim.Value.Lease.Generation).
		Count(&releases).Error; err != nil {
		t.Fatal(err)
	}
	if releases != 0 {
		t.Fatalf("synthetic nonexistent lease recorded %d release facts, want 0", releases)
	}
}

func loadExecutionClaimReleaseFactForTest(
	t *testing.T,
	db *gorm.DB,
	executionID uuid.UUID,
	generation int64,
) (persistence.WorkerClaimFact, persistence.WorkerClaimReleaseFact) {
	t.Helper()
	var claim persistence.WorkerClaimFact
	if err := db.Where("claim_kind = ? AND execution_id = ? AND execution_generation = ?",
		workerClaimKindExecution, executionID, generation).Take(&claim).Error; err != nil {
		t.Fatal(err)
	}
	var release persistence.WorkerClaimReleaseFact
	if err := db.Where("claim_fact_id = ?", claim.ID).Take(&release).Error; err != nil {
		t.Fatal(err)
	}
	return claim, release
}

func loadWorkspaceCleanupClaimReleaseFactForTest(
	t *testing.T,
	db *gorm.DB,
	cleanupID uuid.UUID,
	dispatchGeneration int64,
) (persistence.WorkerClaimFact, persistence.WorkerClaimReleaseFact) {
	t.Helper()
	var claim persistence.WorkerClaimFact
	if err := db.Where("claim_kind = ? AND cleanup_command_id = ? AND cleanup_dispatch_generation = ?",
		workerClaimKindWorkspaceCleanup, cleanupID, dispatchGeneration).Take(&claim).Error; err != nil {
		t.Fatal(err)
	}
	var release persistence.WorkerClaimReleaseFact
	if err := db.Where("claim_fact_id = ?", claim.ID).Take(&release).Error; err != nil {
		t.Fatal(err)
	}
	return claim, release
}
