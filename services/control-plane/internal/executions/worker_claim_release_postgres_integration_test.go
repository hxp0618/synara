package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestPostgresExecutionCompletionAtomicallyRecordsClaimRelease(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerManifestTestWorker(
		t,
		service,
		fixture.TargetID,
		fixture.TargetKind,
		"postgres-claim-release-"+uuid.NewString(),
	)
	cleanupWorkers(t, db, worker.ID)

	claimAt := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return claimAt }
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "postgres-claim-release-claim-"+uuid.NewString())
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("PostgreSQL claim = %#v, %v", claim.Value, err)
	}

	completedAt := claimAt.Add(5 * time.Second)
	service.now = func() time.Time { return completedAt }
	requestID := "postgres-claim-release-complete-" + uuid.NewString()
	completeInput := CompleteExecutionInput{LeaseInput: LeaseInput{
		TenantID:   fixture.TenantID,
		Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}}
	if _, err := service.Complete(
		context.Background(), worker, fixture.ExecutionID, completeInput, requestID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(
		context.Background(), worker, fixture.ExecutionID, completeInput, requestID,
	); err != nil {
		t.Fatalf("PostgreSQL completion replay: %v", err)
	}

	claimFact, releaseFact := loadExecutionClaimReleaseFactForTest(
		t,
		db,
		fixture.ExecutionID,
		claim.Value.Lease.Generation,
	)
	if releaseFact.ClaimFactID != claimFact.ID ||
		releaseFact.ReleaseReason != workerClaimReleaseExecutionCompleted ||
		!releaseFact.ReleasedAt.Equal(completedAt) ||
		!releaseFact.RecordedAt.Equal(completedAt) ||
		releaseFact.AuthorityKind != workerClaimReleaseAuthorityWorker ||
		releaseFact.RequestID == nil || *releaseFact.RequestID != requestID {
		t.Fatalf("PostgreSQL release fact = %#v, claim = %#v", releaseFact, claimFact)
	}
	var releaseCount int64
	if err := db.Model(&persistence.WorkerClaimReleaseFact{}).
		Where("claim_fact_id = ?", claimFact.ID).
		Count(&releaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if releaseCount != 1 {
		t.Fatalf("PostgreSQL completion replay retained %d release facts", releaseCount)
	}
	var leaseCount int64
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&leaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if leaseCount != 0 {
		t.Fatalf("PostgreSQL completion retained %d active leases", leaseCount)
	}
}
