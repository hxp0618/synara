package executions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestManagedDockerDrainSerializesWithPostgresExecutionClaim(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixtureForTargetKind(t, db, true, "docker")
	service := integrationService(t, db)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "postgres-managed-drain")
	cleanupWorkers(t, db, worker.ID)

	type claimOutcome struct {
		result OperationResult[ClaimResult]
		err    error
	}
	type drainOutcome struct {
		decision executiontargets.ManagedDockerWorkerDrainDecision
		err      error
	}
	start := make(chan struct{})
	claimDone := make(chan claimOutcome, 1)
	drainDone := make(chan drainOutcome, 1)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		result, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
			ExecutionTargetID: fixture.TargetID,
			TargetKind:        fixture.TargetKind,
			ExecutionID:       &fixture.ExecutionID,
		}, "postgres-managed-drain-claim-"+uuid.NewString())
		claimDone <- claimOutcome{result: result, err: err}
	}()
	request := executiontargets.ManagedDockerWorkerDrainRequest{
		ExecutionTargetID: fixture.TargetID,
		ContainerName:     worker.PodName,
		Reason:            executiontargets.ManagedDockerDrainReasonStaleSpec,
		ObservedAt:        time.Now().UTC(),
	}
	go func() {
		defer wait.Done()
		<-start
		decision, err := service.PrepareManagedDockerDrain(context.Background(), request)
		drainDone <- drainOutcome{decision: decision, err: err}
	}()
	close(start)
	wait.Wait()
	claim := <-claimDone
	drain := <-drainDone
	if drain.err != nil {
		t.Fatal(drain.err)
	}

	claimWon := claim.err == nil && claim.result.Value.Lease != nil
	switch {
	case claimWon:
		if drain.decision.DeletionAllowed || drain.decision.ExecutionLeaseCount != 1 {
			t.Fatalf("Claim committed but drain did not observe its Lease: %#v", drain.decision)
		}
		lease := *claim.result.Value.Lease
		if _, err := service.Release(
			context.Background(), worker, fixture.ExecutionID,
			ReleaseLeaseInput{
				LeaseInput: LeaseInput{
					TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
				},
				Reason: "PostgreSQL managed Docker drain serialization",
			},
			"postgres-managed-drain-release-"+uuid.NewString(),
		); err != nil {
			t.Fatal(err)
		}
		request.ObservedAt = time.Now().UTC()
		decision, err := service.PrepareManagedDockerDrain(context.Background(), request)
		if err != nil || !decision.DeletionAllowed || decision.ExecutionLeaseCount != 0 {
			t.Fatalf("drain did not advance after Lease release: %#v, %v", decision, err)
		}
	case claim.err != nil:
		assertProblemCode(t, claim.err, "worker_reconciliation_draining")
		if !drain.decision.DeletionAllowed || drain.decision.ExecutionLeaseCount != 0 {
			t.Fatalf("Drain committed first but did not own the empty boundary: %#v", drain.decision)
		}
	default:
		t.Fatalf("Claim returned neither a Lease nor a fenced error: %#v, %v", claim.result, claim.err)
	}

	if err := service.FinalizeManagedDockerDrain(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var stored persistence.WorkerInstance
	if err := db.Where("id = ?", worker.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "terminated" || stored.TerminatedAt == nil {
		t.Fatalf("serialized managed Docker drain did not terminalize: %#v", stored)
	}
	var leases int64
	if err := db.Model(&persistence.WorkerLease{}).Where("worker_id = ?", worker.ID).Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("serialized managed Docker drain retained %d Worker Leases", leases)
	}
}

func TestManagedDockerDrainPostgresAllowsOneInflightWorkerPerTarget(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixtureForTargetKind(t, db, true, "docker")
	service := integrationService(t, db)
	first := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "postgres-managed-drain-a")
	second := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "postgres-managed-drain-b")
	cleanupWorkers(t, db, first.ID, second.ID)

	type outcome struct {
		request  executiontargets.ManagedDockerWorkerDrainRequest
		decision executiontargets.ManagedDockerWorkerDrainDecision
		err      error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, worker := range []persistence.WorkerInstance{first, second} {
		worker := worker
		request := executiontargets.ManagedDockerWorkerDrainRequest{
			ExecutionTargetID: fixture.TargetID,
			ContainerName:     worker.PodName,
			Reason:            executiontargets.ManagedDockerDrainReasonStaleSpec,
			ObservedAt:        time.Now().UTC(),
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			decision, err := service.PrepareManagedDockerDrain(context.Background(), request)
			results <- outcome{request: request, decision: decision, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	winners := make([]outcome, 0, 1)
	conflicts := 0
	for result := range results {
		if result.err == nil {
			winners = append(winners, result)
			continue
		}
		assertProblemCode(t, result.err, "docker_worker_drain_conflict")
		conflicts++
	}
	if len(winners) != 1 || conflicts != 1 || !winners[0].decision.DeletionAllowed {
		t.Fatalf("concurrent managed drain outcomes: winners=%#v conflicts=%d", winners, conflicts)
	}
	active, err := service.ActiveManagedDockerDrain(context.Background(), fixture.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ContainerName != winners[0].request.ContainerName {
		t.Fatalf("active managed drain = %#v, winner=%#v", active, winners[0])
	}
	if err := service.FinalizeManagedDockerDrain(context.Background(), winners[0].request); err != nil {
		t.Fatal(err)
	}
}
