package executions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
)

func TestPostgresConcurrentPinnedGeneralWorkerClaimsNeverCrossTenant(t *testing.T) {
	primaryDB, replicaDB := isolatedPostgresTestDBs(t)
	target := seedWorkerTenantIsolationTarget(t, primaryDB, placement.TenantIsolationPinned)
	base := time.Now().UTC().Truncate(time.Microsecond)
	tenantA := seedWorkerTenantIsolationExecution(t, primaryDB, target, "postgres-pinned-tenant-a", base)
	tenantB := seedWorkerTenantIsolationExecution(t, primaryDB, target, "postgres-pinned-tenant-b", base.Add(time.Second))
	primaryService := integrationService(t, primaryDB)
	replicaService := integrationService(t, replicaDB)
	_, worker := registerWorkerTenantIsolationTestWorker(
		t, primaryService, target, "postgres-pinned-worker", uuid.NewString(),
	)

	type outcome struct {
		result OperationResult[ClaimResult]
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for index, service := range []*Service{primaryService, replicaService} {
		index, service := index, service
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := service.Claim(
				context.Background(), worker,
				ClaimExecutionInput{ExecutionTargetID: target.ID, TargetKind: target.Kind},
				"postgres-pinned-concurrent-"+string(rune('a'+index)),
			)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)

	claimed := make([]Execution, 0, 1)
	empty := 0
	busy := 0
	for outcome := range outcomes {
		if outcome.err != nil {
			assertProblemCode(t, outcome.err, "worker_busy")
			busy++
			continue
		}
		if outcome.result.Value.Execution == nil {
			empty++
			continue
		}
		claimed = append(claimed, *outcome.result.Value.Execution)
	}
	if len(claimed) != 1 || busy != 1 || empty != 0 {
		t.Fatalf(
			"concurrent pinned claims = %#v, busy=%d empty=%d; want one claim and one serialized busy result",
			claimed, busy, empty,
		)
	}

	var storedWorker persistence.WorkerInstance
	if err := primaryDB.Where("id = ?", worker.ID).Take(&storedWorker).Error; err != nil {
		t.Fatal(err)
	}
	if storedWorker.TenantBindingID == nil || *storedWorker.TenantBindingID != claimed[0].TenantID {
		t.Fatalf("atomic Worker Tenant binding = %#v, claimed Tenant = %s", storedWorker.TenantBindingID, claimed[0].TenantID)
	}

	otherTenantID := tenantA.TenantID
	otherExecutionID := tenantA.ExecutionID
	if claimed[0].TenantID == tenantA.TenantID {
		otherTenantID = tenantB.TenantID
		otherExecutionID = tenantB.ExecutionID
	}
	var other persistence.AgentExecution
	if err := primaryDB.Where("id = ?", otherExecutionID).Take(&other).Error; err != nil {
		t.Fatal(err)
	}
	if other.TenantID != otherTenantID || other.Status != "queued" || other.WorkerID != nil {
		t.Fatalf("other Tenant Execution crossed the atomic binding fence: %#v", other)
	}
}
