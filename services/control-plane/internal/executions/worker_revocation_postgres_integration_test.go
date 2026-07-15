package executions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestPostgresWorkerRevocationLinearizesWithLeaseRenewal(t *testing.T) {
	fixture := newPostgresWorkerRevocationRaceFixture(t, "worker-revoke-renew-race")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var revokeErr, renewErr error
	go func() {
		defer wait.Done()
		<-start
		_, revokeErr = fixture.services[0].RevokeWorker(
			ctx, fixture.principal, fixture.execution.TenantID, fixture.worker.ID,
			RevokeWorkerInput{ExpectedIncarnation: fixture.worker.Incarnation, Reason: "concurrent lease renewal fencing"},
			"worker-revoke-renew-"+uuid.NewString(), "worker-revoke-renew", "127.0.0.1",
		)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, renewErr = fixture.services[1].Renew(
			ctx, fixture.worker, fixture.execution.ExecutionID,
			RenewLeaseInput{LeaseInput: fixture.leaseInput}, "worker-renew-race-"+uuid.NewString(),
		)
	}()
	close(start)
	wait.Wait()

	if revokeErr != nil {
		t.Fatalf("concurrent Worker revocation failed: %v", revokeErr)
	}
	assertWorkerRaceError(t, renewErr, "worker_token_revoked")
	assertPostgresRevokedWorkerRaceState(t, fixture, "recovering")
}

func TestPostgresWorkerRevocationLinearizesWithExecutionCompletion(t *testing.T) {
	fixture := newPostgresWorkerRevocationRaceFixture(t, "worker-revoke-complete-race")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var revokeErr, completeErr error
	go func() {
		defer wait.Done()
		<-start
		_, revokeErr = fixture.services[0].RevokeWorker(
			ctx, fixture.principal, fixture.execution.TenantID, fixture.worker.ID,
			RevokeWorkerInput{ExpectedIncarnation: fixture.worker.Incarnation, Reason: "concurrent execution completion fencing"},
			"worker-revoke-complete-"+uuid.NewString(), "worker-revoke-complete", "127.0.0.1",
		)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, completeErr = fixture.services[1].Complete(
			ctx, fixture.worker, fixture.execution.ExecutionID,
			CompleteExecutionInput{LeaseInput: fixture.leaseInput, Output: map[string]any{"race": "complete"}},
			"worker-complete-race-"+uuid.NewString(),
		)
	}()
	close(start)
	wait.Wait()

	if revokeErr != nil {
		t.Fatalf("concurrent Worker revocation failed: %v", revokeErr)
	}
	assertWorkerRaceError(t, completeErr, "worker_token_revoked")
	assertPostgresRevokedWorkerRaceState(t, fixture, "recovering", "completed")
}

type postgresWorkerRevocationRaceFixture struct {
	db         *gorm.DB
	services   [2]*Service
	execution  executionFixture
	worker     persistence.WorkerInstance
	leaseInput LeaseInput
	principal  identity.Principal
}

func newPostgresWorkerRevocationRaceFixture(t *testing.T, podName string) postgresWorkerRevocationRaceFixture {
	t.Helper()
	db := integrationDB(t)
	execution := seedExecutionFixture(t, db)
	services := [2]*Service{integrationService(t, db), integrationService(t, db)}
	worker := registerManifestTestWorker(t, services[0], execution.TargetID, execution.TargetKind, podName)
	cleanupWorkers(t, db, worker.ID)
	claim, err := services[0].Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: execution.TargetID, TargetKind: execution.TargetKind, ExecutionID: &execution.ExecutionID,
	}, "worker-revocation-race-claim-"+uuid.NewString())
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim Worker revocation race Execution: %#v, %v", claim, err)
	}
	return postgresWorkerRevocationRaceFixture{
		db: db, services: services, execution: execution, worker: worker,
		leaseInput: LeaseInput{
			TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		},
		principal: identity.Principal{UserID: execution.UserID, ActiveTenantID: &execution.TenantID},
	}
}

func assertWorkerRaceError(t *testing.T, err error, allowedCodes ...string) {
	t.Helper()
	if err == nil {
		return
	}
	var apiError *problem.Error
	if !errors.As(err, &apiError) {
		t.Fatalf("concurrent Worker request returned non-problem error: %v", err)
	}
	for _, code := range allowedCodes {
		if apiError.Code == code {
			return
		}
	}
	t.Fatalf("concurrent Worker request problem code = %q, want one of %v", apiError.Code, allowedCodes)
}

func assertPostgresRevokedWorkerRaceState(
	t *testing.T,
	fixture postgresWorkerRevocationRaceFixture,
	allowedStatuses ...string,
) {
	t.Helper()
	var worker persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", fixture.worker.ID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if worker.AdministrativeStatus != "revoked" || worker.RevokedAt == nil || worker.RevokedBy == nil {
		t.Fatalf("Worker race did not persist revocation: %#v", worker)
	}
	var leaseCount int64
	if err := fixture.db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID).
		Count(&leaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if leaseCount != 0 {
		t.Fatalf("Worker revocation race retained %d execution Leases", leaseCount)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	allowed := false
	for _, status := range allowedStatuses {
		if execution.Status == status {
			allowed = true
			break
		}
	}
	if !allowed {
		t.Fatalf("Worker revocation race left Execution status %q, want one of %v", execution.Status, allowedStatuses)
	}
	if execution.Status == "recovering" && execution.WorkerID != nil {
		t.Fatalf("recovering Execution retained revoked Worker ownership: %#v", execution)
	}
}
