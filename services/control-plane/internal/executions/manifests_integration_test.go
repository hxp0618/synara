package executions

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestWorkerManifestPersistsAndBindsClaimedExecution(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		InstanceUID: uuid.NewString(),
		ClusterID:   "manifest-test", Namespace: "default", PodName: "manifest-worker",
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
	claimInput := ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}
	claim, err := service.Claim(context.Background(), worker, claimInput, "manifest-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Execution == nil || claim.Value.Execution.WorkerManifestID == nil ||
		*claim.Value.Execution.WorkerManifestID != *worker.CurrentManifestID ||
		claim.Value.Workload == nil || claim.Value.Workload.WorkerManifestID == nil ||
		*claim.Value.Workload.WorkerManifestID != *worker.CurrentManifestID ||
		claim.Value.Workload.ProviderCredentialID == nil ||
		*claim.Value.Workload.ProviderCredentialID != fixture.ProviderCredentialID {
		t.Fatalf("Claim did not bind the Worker manifest: %#v", claim.Value)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.ProviderCredentialIDSnapshot == nil ||
		*execution.ProviderCredentialIDSnapshot != fixture.ProviderCredentialID ||
		execution.ProviderCredentialVersionSnapshot == nil || *execution.ProviderCredentialVersionSnapshot != 1 ||
		execution.ProviderResumeStrategySnapshot != "native-cursor" ||
		execution.ProviderCursorBindingVersion == nil || *execution.ProviderCursorBindingVersion != providerCursorBindingVersion ||
		len(execution.ProviderCursorBindingDigest) != 32 {
		t.Fatalf("Claim did not persist a complete Provider generation snapshot: %#v", execution)
	}
	var reboundCredential persistence.ProviderCredential
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ProviderCredentialID).
		Take(&reboundCredential).Error; err != nil {
		t.Fatal(err)
	}
	reboundCredential.ID = uuid.New()
	reboundCredential.Name = "Rebound Execution Provider"
	if err := db.Create(&reboundCredential).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Update("provider_credential_id", reboundCredential.ID).Error; err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Claim(context.Background(), worker, claimInput, "manifest-claim")
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Value.Workload == nil || replayed.Value.Workload.ProviderCredentialID == nil ||
		*replayed.Value.Workload.ProviderCredentialID != fixture.ProviderCredentialID {
		t.Fatalf("Session rebind changed the active generation Workload Credential: %#v", replayed)
	}
	var providerCount int64
	if err := db.Model(&persistence.WorkerProviderManifest{}).
		Where("worker_manifest_id = ?", *worker.CurrentManifestID).Count(&providerCount).Error; err != nil {
		t.Fatal(err)
	}
	if providerCount != int64(len(stage3ProviderNames)) {
		t.Fatalf("stored %d Provider manifests", providerCount)
	}
	if claim.Value.Execution.ProviderRuntimeBindingID == nil {
		t.Fatal("Claim did not bind a Provider runtime snapshot")
	}
	var binding persistence.ProviderRuntimeBinding
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, *claim.Value.Execution.ProviderRuntimeBindingID).
		Take(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.RuntimeKind == nil || *binding.RuntimeKind != "cli" || binding.RuntimeVersion == nil ||
		*binding.RuntimeVersion != "0.144.1" || binding.RuntimeCompatible == nil || !*binding.RuntimeCompatible ||
		binding.ReleaseEnabled == nil || !*binding.ReleaseEnabled {
		t.Fatalf("Provider runtime binding omitted runtime/release evidence: %#v", binding)
	}
}

func TestWorkerManifestRejectsAssignedUnsupportedProvider(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	capabilities := workerManifestTestCapabilities()
	setTestProviderRuntime(capabilities, "codex", nil, false, false)
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		InstanceUID: uuid.NewString(),
		ClusterID:   "manifest-test", Namespace: "default", PodName: "incompatible-worker",
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: capabilities, LeaseSupported: true, FencingSupported: true,
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

func TestWorkerHeartbeatRejectsProviderManifestDriftUntilReregistration(t *testing.T) {
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "manifest-drift-worker")
	cleanupWorkers(t, db, worker.ID)
	drifted := workerManifestTestCapabilities()
	setTestProviderCapability(drifted, "codex", "steer-turn", "unsupported")
	draining := true
	_, err := service.Heartbeat(context.Background(), worker, HeartbeatInput{
		Version: worker.Version, ProtocolVersion: WorkerProtocolVersion, Capabilities: drifted, Draining: &draining,
	})
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "worker_manifest_reregistration_required" {
		t.Fatalf("expected worker_manifest_reregistration_required, got %v", err)
	}
	var stored persistence.WorkerInstance
	if err := db.Where("id = ?", worker.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "online" || stored.DrainingAt != nil ||
		testProviderCapabilityMap(stored.Capabilities, "codex")["steer-turn"] != "native" {
		t.Fatalf("manifest drift partially updated the Worker heartbeat: %#v", stored)
	}
}
