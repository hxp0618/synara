package executions

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestWorkerManifestPersistsAndBindsClaimedExecution(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ClusterID: "manifest-test", Namespace: "default", PodName: "manifest-worker",
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: workerManifestTestCapabilities(), LeaseSupported: true, FencingSupported: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(context.Background(), registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupWorkers(t, db, worker.ID) })
	if worker.CurrentManifestID == nil || worker.CompatibilityStatus != "compatible" {
		t.Fatalf("Worker manifest was not attached: %#v", worker)
	}
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "manifest-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Execution == nil || claim.Value.Execution.WorkerManifestID == nil ||
		*claim.Value.Execution.WorkerManifestID != *worker.CurrentManifestID ||
		claim.Value.Workload == nil || claim.Value.Workload.WorkerManifestID == nil ||
		*claim.Value.Workload.WorkerManifestID != *worker.CurrentManifestID {
		t.Fatalf("Claim did not bind the Worker manifest: %#v", claim.Value)
	}
	var providerCount int64
	if err := db.Model(&persistence.WorkerProviderManifest{}).
		Where("worker_manifest_id = ?", *worker.CurrentManifestID).Count(&providerCount).Error; err != nil {
		t.Fatal(err)
	}
	if providerCount != 2 {
		t.Fatalf("stored %d Provider manifests", providerCount)
	}
}

func TestWorkerManifestRejectsAssignedUnsupportedProvider(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
			Update("provider", "claudeAgent").Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Update("provider", "claudeAgent").Error
	}); err != nil {
		t.Fatal(err)
	}
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ClusterID: "manifest-test", Namespace: "default", PodName: "incompatible-worker",
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: workerManifestTestCapabilities(), LeaseSupported: true, FencingSupported: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(context.Background(), registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupWorkers(t, db, worker.ID) })
	_, err = service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "manifest-incompatible-claim")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "worker_provider_incompatible" {
		t.Fatalf("expected worker_provider_incompatible, got %v", err)
	}
}
