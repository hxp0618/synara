package executions

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkerRequestReceiptReplayIsFencedAfterGenerationOrTargetChange(t *testing.T) {
	setupStartedExecution := func(t *testing.T, label string) (*gorm.DB, *Service, persistence.ExecutionTarget, workerTenantIsolationExecution, persistence.WorkerInstance, LeaseInput, string) {
		t.Helper()
		db, service, target := setupWorkerTenantIsolationTest(t, placement.TenantIsolationPinned)
		execution := seedWorkerTenantIsolationExecution(t, db, target, label, time.Now().UTC())
		_, worker := registerWorkerTenantIsolationTestWorker(t, service, target, label+"-worker", uuid.NewString())
		claim := claimWorkerTenantIsolationExecution(t, service, worker, target, label+"-claim")
		lease := LeaseInput{
			TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		}
		requestID := label + "-start"
		started, err := service.Start(context.Background(), worker, execution.ExecutionID, lease, requestID)
		if err != nil || started.Replayed {
			t.Fatalf("start = %#v, %v", started, err)
		}
		replayed, err := service.Start(context.Background(), worker, execution.ExecutionID, lease, requestID)
		if err != nil || !replayed.Replayed {
			t.Fatalf("current-authority replay = %#v, %v", replayed, err)
		}
		var receipt persistence.WorkerRequestReceipt
		if err := db.Where("worker_id = ? AND request_id = ?", worker.ID, requestID).Take(&receipt).Error; err != nil {
			t.Fatal(err)
		}
		if receipt.TenantID == nil || *receipt.TenantID != execution.TenantID ||
			receipt.ExecutionID == nil || *receipt.ExecutionID != execution.ExecutionID ||
			receipt.ExecutionTargetID == nil || *receipt.ExecutionTargetID != target.ID ||
			receipt.ExecutionGeneration == nil || *receipt.ExecutionGeneration != lease.Generation {
			t.Fatalf("receipt authority = %#v", receipt)
		}
		return db, service, target, execution, worker, lease, requestID
	}

	t.Run("generation", func(t *testing.T) {
		_, service, target, execution, worker, lease, requestID := setupStartedExecution(t, "receipt-generation-fence")
		if _, err := service.Release(context.Background(), worker, execution.ExecutionID, ReleaseLeaseInput{
			LeaseInput: lease, Reason: "exercise a successor generation",
		}, "receipt-generation-release"); err != nil {
			t.Fatal(err)
		}
		successor, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
			ExecutionTargetID: target.ID, TargetKind: target.Kind, ExecutionID: &execution.ExecutionID,
		}, "receipt-generation-successor")
		if err != nil || successor.Value.Lease == nil || successor.Value.Lease.Generation <= lease.Generation {
			t.Fatalf("successor claim = %#v, %v", successor, err)
		}
		_, err = service.Start(context.Background(), worker, execution.ExecutionID, lease, requestID)
		assertProblemCode(t, err, "generation_fenced")
	})

	t.Run("legacy-unbound", func(t *testing.T) {
		db, service, _, execution, worker, lease, requestID := setupStartedExecution(t, "receipt-legacy-unbound")
		if err := db.Model(&persistence.WorkerRequestReceipt{}).
			Where("worker_id = ? AND request_id = ?", worker.ID, requestID).
			Updates(map[string]any{
				"tenant_id": nil, "execution_id": nil, "execution_target_id": nil,
				"execution_generation": nil,
			}).Error; err != nil {
			t.Fatal(err)
		}
		_, err := service.Start(context.Background(), worker, execution.ExecutionID, lease, requestID)
		assertProblemCode(t, err, "worker_request_authority_unbound")
	})

	t.Run("target", func(t *testing.T) {
		db, service, _, execution, worker, lease, requestID := setupStartedExecution(t, "receipt-target-fence")
		if _, err := service.Release(context.Background(), worker, execution.ExecutionID, ReleaseLeaseInput{
			LeaseInput: lease, Reason: "move the recovering execution to another target",
		}, "receipt-target-release"); err != nil {
			t.Fatal(err)
		}
		destination := seedWorkerTenantIsolationTarget(t, db, placement.TenantIsolationPinned)
		if err := db.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
			Update("execution_target_id", destination.ID).Error; err != nil {
			t.Fatal(err)
		}
		_, err := service.Start(context.Background(), worker, execution.ExecutionID, lease, requestID)
		assertProblemCode(t, err, "worker_execution_target_mismatch")
	})
}

func TestReregisteredWorkerFencesCredentialHeartbeatAndLeaseReplay(t *testing.T) {
	db, service, target := setupWorkerTenantIsolationTest(t, placement.TenantIsolationPinned)
	execution := seedWorkerTenantIsolationExecution(t, db, target, "worker-incarnation-fence", time.Now().UTC())
	registered, staleWorker := registerWorkerTenantIsolationTestWorker(t, service, target, "worker-incarnation-fence", uuid.NewString())
	claim := claimWorkerTenantIsolationExecution(t, service, staleWorker, target, "worker-incarnation-fence-claim")
	lease := LeaseInput{
		TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}

	replacementUID := uuid.NewString()
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
		ExecutionTargetID: target.ID, TargetKind: platform.TargetKubernetes, InstanceUID: replacementUID,
		ClusterID: staleWorker.ClusterID, Namespace: staleWorker.Namespace, PodName: staleWorker.PodName,
	})
	replacement, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: target.ID, TargetKind: target.Kind, InstanceUID: replacementUID,
		ClusterID: staleWorker.ClusterID, Namespace: staleWorker.Namespace, PodName: staleWorker.PodName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion, Capabilities: capabilities,
		LeaseSupported: true, FencingSupported: true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	currentWorker, err := service.Authenticate(context.Background(), replacement.Token)
	if err != nil {
		t.Fatal(err)
	}
	if currentWorker.ID != staleWorker.ID || currentWorker.Incarnation != staleWorker.Incarnation+1 {
		t.Fatalf("replacement Worker = %#v", currentWorker)
	}
	_, err = service.Authenticate(context.Background(), registered.Token)
	assertProblemCode(t, err, "invalid_worker_token")

	assertIncarnationFenced := func(operation string, err error) {
		t.Helper()
		var apiError *problem.Error
		if !errors.As(err, &apiError) || apiError.Code != "worker_incarnation_fenced" {
			t.Fatalf("%s returned %v", operation, err)
		}
	}
	_, err = service.Heartbeat(context.Background(), staleWorker, HeartbeatInput{ProtocolVersion: WorkerProtocolVersion})
	assertIncarnationFenced("heartbeat", err)
	_, err = service.PullControlCommands(context.Background(), staleWorker, execution.ExecutionID, PullControlCommandsInput{LeaseInput: lease})
	assertIncarnationFenced("control-command pull", err)
	_, err = service.PullInteractionResolutions(context.Background(), staleWorker, execution.ExecutionID, PullInteractionResolutionsInput{LeaseInput: lease})
	assertIncarnationFenced("interaction-resolution pull", err)
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := service.AuthorizeLease(context.Background(), tx, staleWorker, execution.ExecutionID, lease)
		return err
	}); err != nil {
		assertIncarnationFenced("lease authorization", err)
	} else {
		t.Fatal("stale lease authorization succeeded")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := service.AuthorizeArtifactWrite(context.Background(), tx, staleWorker, execution.ExecutionID, lease)
		return err
	}); err != nil {
		assertIncarnationFenced("Artifact authorization", err)
	} else {
		t.Fatal("stale Artifact authorization succeeded")
	}
	_, err = service.Renew(context.Background(), staleWorker, execution.ExecutionID, RenewLeaseInput{LeaseInput: lease}, "worker-incarnation-stale-renew")
	assertIncarnationFenced("stale renewal", err)
	_, err = service.Renew(context.Background(), currentWorker, execution.ExecutionID, RenewLeaseInput{LeaseInput: lease}, "worker-incarnation-inherited-renew")
	assertIncarnationFenced("inherited renewal", err)
}

func TestPinnedGeneralWorkerBindsFirstTenantAcrossClaimsAndReregistration(t *testing.T) {
	db, service, target := setupWorkerTenantIsolationTest(t, placement.TenantIsolationPinned)
	base := time.Now().UTC().Truncate(time.Microsecond)
	tenantAFirst := seedWorkerTenantIsolationExecution(t, db, target, "tenant-a-first", base)
	tenantB := seedWorkerTenantIsolationExecution(t, db, target, "tenant-b", base.Add(time.Second))
	tenantASecond := seedWorkerTenantIsolationExecutionForTenant(
		t, db, target, tenantAFirst, "tenant-a-second", base.Add(2*time.Second),
	)
	_, worker := registerWorkerTenantIsolationTestWorker(t, service, target, "pinned-worker", uuid.NewString())

	first := claimWorkerTenantIsolationExecution(t, service, worker, target, "pinned-first")
	if first.Value.Execution == nil || first.Value.Execution.ID != tenantAFirst.ExecutionID {
		t.Fatalf("first pinned claim = %#v, want Tenant A first Execution", first.Value.Execution)
	}
	assertWorkerTenantBinding(t, db, worker.ID, tenantAFirst.TenantID)
	completeWorkerTenantIsolationExecution(t, service, worker, first, "pinned-first-complete")

	second := claimWorkerTenantIsolationExecution(t, service, worker, target, "pinned-second")
	if second.Value.Execution == nil || second.Value.Execution.ID != tenantASecond.ExecutionID {
		t.Fatalf("second pinned claim = %#v, want later Tenant A Execution instead of earlier Tenant B %s", second.Value.Execution, tenantB.ExecutionID)
	}
	completeWorkerTenantIsolationExecution(t, service, worker, second, "pinned-second-complete")

	if _, err := service.Heartbeat(context.Background(), worker, HeartbeatInput{ProtocolVersion: WorkerProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	assertWorkerTenantBinding(t, db, worker.ID, tenantAFirst.TenantID)

	replacementUID := uuid.NewString()
	replacementCapabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(replacementCapabilities)
	signWorkerManifestTestContainment(t, replacementCapabilities, workerManifestRegistrationContext{
		ExecutionTargetID: target.ID, TargetKind: platform.TargetKubernetes, InstanceUID: replacementUID,
		ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
	})
	replacement, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: target.ID, TargetKind: target.Kind,
		InstanceUID: replacementUID, ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion, Capabilities: replacementCapabilities,
		LeaseSupported: true, FencingSupported: true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Worker.Incarnation != worker.Incarnation+1 || replacement.Worker.TenantBindingID == nil ||
		*replacement.Worker.TenantBindingID != tenantAFirst.TenantID {
		t.Fatalf("replacement Worker lost Tenant binding: %#v", replacement.Worker)
	}

	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("tenant_binding_id", nil).Error; err == nil {
		t.Fatal("SQLite accepted clearing an immutable Worker Tenant binding")
	}
	var stillQueued persistence.AgentExecution
	if err := db.Where("id = ?", tenantB.ExecutionID).Take(&stillQueued).Error; err != nil {
		t.Fatal(err)
	}
	if stillQueued.Status != "queued" || stillQueued.WorkerID != nil {
		t.Fatalf("other Tenant Execution changed under pinned Worker: %#v", stillQueued)
	}
}

func TestSharedGeneralWorkerMayRotateTenantsWithoutBinding(t *testing.T) {
	db, service, target := setupWorkerTenantIsolationTest(t, placement.TenantIsolationShared)
	base := time.Now().UTC().Truncate(time.Microsecond)
	tenantA := seedWorkerTenantIsolationExecution(t, db, target, "shared-tenant-a", base)
	tenantB := seedWorkerTenantIsolationExecution(t, db, target, "shared-tenant-b", base.Add(time.Second))
	_, worker := registerWorkerTenantIsolationTestWorker(t, service, target, "shared-worker", uuid.NewString())

	first := claimWorkerTenantIsolationExecution(t, service, worker, target, "shared-first")
	if first.Value.Execution == nil || first.Value.Execution.ID != tenantA.ExecutionID {
		t.Fatalf("first shared claim = %#v", first.Value.Execution)
	}
	completeWorkerTenantIsolationExecution(t, service, worker, first, "shared-first-complete")
	assertWorkerTenantUnbound(t, db, worker.ID)
	_, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: target.ID, TargetKind: target.Kind,
	}, "shared-second-before-scrub")
	assertProblemCode(t, err, "worker_storage_scrub_required")
	acknowledgeWorkerTenantStorageScrub(t, service, worker, "shared-first-scrub")

	second := claimWorkerTenantIsolationExecution(t, service, worker, target, "shared-second")
	if second.Value.Execution == nil || second.Value.Execution.ID != tenantB.ExecutionID {
		t.Fatalf("second shared claim = %#v, want Tenant B", second.Value.Execution)
	}
	completeWorkerTenantIsolationExecution(t, service, worker, second, "shared-second-complete")
	acknowledgeWorkerTenantStorageScrub(t, service, worker, "shared-second-scrub")
	assertWorkerTenantUnbound(t, db, worker.ID)
}

func TestSharedWorkerPendingScrubOnlyResumesOnSamePhysicalInstance(t *testing.T) {
	db, service, target := setupWorkerTenantIsolationTest(t, placement.TenantIsolationShared)
	execution := seedWorkerTenantIsolationExecution(t, db, target, "shared-reregister", time.Now().UTC())
	_, worker := registerWorkerTenantIsolationTestWorker(t, service, target, "shared-reregister-worker", uuid.NewString())
	claim := claimWorkerTenantIsolationExecution(t, service, worker, target, "shared-reregister-claim")
	if claim.Value.Execution == nil || claim.Value.Execution.ID != execution.ExecutionID {
		t.Fatalf("shared re-registration claim = %#v", claim.Value.Execution)
	}
	completeWorkerTenantIsolationExecution(t, service, worker, claim, "shared-reregister-complete")

	register := func(instanceUID string) (RegisteredWorker, error) {
		capabilities := workerManifestTestCapabilities()
		addWorkerManifestTestContainmentEvidence(capabilities)
		signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
			ExecutionTargetID: target.ID, TargetKind: platform.TargetKubernetes, InstanceUID: instanceUID,
			ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
		})
		return service.Register(context.Background(), RegisterWorkerInput{
			ExecutionTargetID: target.ID, TargetKind: target.Kind, InstanceUID: instanceUID,
			ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
			Version: "worker-test", ProtocolVersion: WorkerProtocolVersion, Capabilities: capabilities,
			LeaseSupported: true, FencingSupported: true,
			RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
		})
	}
	_, err := register(uuid.NewString())
	assertProblemCode(t, err, "worker_storage_scrub_physical_instance_mismatch")

	_, err = register(worker.InstanceUID)
	assertProblemCode(t, err, "kubernetes_worker_instance_already_registered")
	var pending persistence.WorkerStorageScrub
	if err := db.Where("worker_id = ? AND status = ?", worker.ID, workerStorageScrubStatusPending).
		Take(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.TenantID != execution.TenantID {
		t.Fatalf("pending scrub lost its Tenant fence: %#v", pending)
	}
}

func TestSharedWorkerStorageScrubFailureDrainsPhysicalWorker(t *testing.T) {
	db, service, target := setupWorkerTenantIsolationTest(t, placement.TenantIsolationShared)
	seedWorkerTenantIsolationExecution(t, db, target, "shared-scrub-failure", time.Now().UTC())
	_, worker := registerWorkerTenantIsolationTestWorker(t, service, target, "shared-scrub-failure-worker", uuid.NewString())
	claim := claimWorkerTenantIsolationExecution(t, service, worker, target, "shared-scrub-failure-claim")
	completeWorkerTenantIsolationExecution(t, service, worker, claim, "shared-scrub-failure-complete")
	pending, err := service.ClaimWorkerStorageScrub(context.Background(), worker)
	if err != nil || pending.Scrub == nil {
		t.Fatalf("pending failed storage scrub = %#v, err=%v", pending.Scrub, err)
	}
	failed, err := service.FailWorkerStorageScrub(
		context.Background(), worker, pending.Scrub.ID,
		WorkerStorageScrubFailureInput{
			ScrubGeneration: pending.Scrub.ScrubGeneration,
			FailureCode:     "workspace_scrub_failed", FailureMessage: "injected deletion failure",
		},
		"shared-scrub-failure-report",
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Value.Status != workerStorageScrubStatusFailed || failed.Value.FailedAt == nil {
		t.Fatalf("failed storage scrub = %#v", failed.Value)
	}
	var storedWorker persistence.WorkerInstance
	if err := db.Where("id = ?", worker.ID).Take(&storedWorker).Error; err != nil {
		t.Fatal(err)
	}
	if storedWorker.Status != "draining" || storedWorker.DrainingAt == nil {
		t.Fatalf("Worker after storage scrub failure = %#v", storedWorker)
	}
	_, err = service.AcknowledgeWorkerStorageScrub(
		context.Background(), worker, pending.Scrub.ID,
		WorkerStorageScrubReceiptInput{ScrubGeneration: pending.Scrub.ScrubGeneration},
		"shared-scrub-failure-late-ack",
	)
	assertProblemCode(t, err, "worker_storage_scrub_failed")
}

func TestSharedWorkerUnresolvedScrubBlocksNewLogicalWorkerRow(t *testing.T) {
	db, service, target := setupWorkerTenantIsolationTest(t, placement.TenantIsolationShared)
	seedWorkerTenantIsolationExecution(t, db, target, "shared-scrub-replacement", time.Now().UTC())
	_, worker := registerWorkerTenantIsolationTestWorker(t, service, target, "shared-scrub-replacement-worker", uuid.NewString())
	claim := claimWorkerTenantIsolationExecution(t, service, worker, target, "shared-scrub-replacement-claim")
	completeWorkerTenantIsolationExecution(t, service, worker, claim, "shared-scrub-replacement-complete")
	terminatedAt := time.Now().UTC()
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Updates(map[string]any{"status": "terminated", "terminated_at": terminatedAt}).Error; err != nil {
		t.Fatal(err)
	}
	replacementUID := uuid.NewString()
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
		ExecutionTargetID: target.ID, TargetKind: platform.TargetKubernetes, InstanceUID: replacementUID,
		ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
	})
	_, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: target.ID, TargetKind: target.Kind, InstanceUID: replacementUID,
		ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion, Capabilities: capabilities,
		LeaseSupported: true, FencingSupported: true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	})
	assertProblemCode(t, err, "worker_storage_scrub_replacement_blocked")
}

func TestSQLiteWorkerTenantBindingIndexCoversClaimFilter(t *testing.T) {
	db, _, _ := setupWorkerTenantIsolationTest(t, placement.TenantIsolationPinned)
	var columns []struct {
		Name string `gorm:"column:name"`
	}
	if err := db.Raw("PRAGMA index_info('idx_worker_instances_tenant_binding')").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	want := []string{"execution_target_id", "tenant_binding_id", "status", "id"}
	if len(columns) != len(want) {
		t.Fatalf("Tenant binding index columns = %#v, want %#v", columns, want)
	}
	for index, column := range columns {
		if column.Name != want[index] {
			t.Fatalf("Tenant binding index columns = %#v, want %#v", columns, want)
		}
	}
	var definition struct {
		SQL string `gorm:"column:sql"`
	}
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", "idx_worker_instances_tenant_binding").Scan(&definition).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(definition.SQL, "WHERE tenant_binding_id IS NOT NULL") {
		t.Fatalf("Tenant binding index is not partial: %q", definition.SQL)
	}
}

func TestPinnedGeneralWorkerKeepsWorkspaceCleanupWithinBoundTenant(t *testing.T) {
	db, service, target := setupWorkerTenantIsolationTest(t, placement.TenantIsolationPinned)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	tenantAFirst := seedWorkerTenantIsolationExecution(t, db, target, "cleanup-tenant-a-first", base)
	tenantB := seedWorkerTenantIsolationExecution(t, db, target, "cleanup-tenant-b", base.Add(time.Second))
	tenantASecond := seedWorkerTenantIsolationExecutionForTenant(
		t, db, target, tenantAFirst, "cleanup-tenant-a-second", base.Add(2*time.Second),
	)
	cleanupAFirst := seedWorkerTenantIsolationCleanup(t, db, target, tenantAFirst, base)
	cleanupB := seedWorkerTenantIsolationCleanup(t, db, target, tenantB, base.Add(time.Second))
	cleanupASecond := seedWorkerTenantIsolationCleanup(t, db, target, tenantASecond, base.Add(2*time.Second))
	_, worker := registerWorkerTenantIsolationTestWorker(t, service, target, "pinned-cleanup-worker", uuid.NewString())

	first, err := service.ClaimWorkspaceCleanup(
		context.Background(), worker, WorkspaceCleanupClaimInput{}, "pinned-cleanup-first",
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Value.Cleanup == nil || first.Value.Cleanup.CleanupID != cleanupAFirst {
		t.Fatalf("first pinned cleanup claim = %#v, want Tenant A cleanup %s", first.Value.Cleanup, cleanupAFirst)
	}
	assertWorkerTenantBinding(t, db, worker.ID, tenantAFirst.TenantID)
	leaseInput := WorkspaceCleanupLeaseInput{
		DispatchGeneration: first.Value.Cleanup.DispatchGeneration,
		LeaseToken:         first.Value.Cleanup.Lease.LeaseToken,
	}
	if _, err := service.StartWorkspaceCleanup(
		context.Background(), worker, cleanupAFirst, leaseInput, "pinned-cleanup-first-start",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcknowledgeWorkspaceCleanup(
		context.Background(), worker, cleanupAFirst, leaseInput,
		"pinned-cleanup-first-ack",
	); err != nil {
		t.Fatal(err)
	}

	second, err := service.ClaimWorkspaceCleanup(
		context.Background(), worker, WorkspaceCleanupClaimInput{}, "pinned-cleanup-second",
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.Value.Cleanup == nil || second.Value.Cleanup.CleanupID != cleanupASecond {
		t.Fatalf(
			"second pinned cleanup claim = %#v, want later Tenant A cleanup %s instead of earlier Tenant B cleanup %s",
			second.Value.Cleanup, cleanupASecond, cleanupB,
		)
	}
	var retained persistence.WorkspaceCleanupCommand
	if err := db.Where("id = ?", cleanupB).Take(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if retained.Status != "pending" || retained.DeliveryWorkerID != nil {
		t.Fatalf("other Tenant cleanup changed under pinned Worker: %#v", retained)
	}
}

type workerTenantIsolationExecution struct {
	TenantID       uuid.UUID
	UserID         uuid.UUID
	OrganizationID uuid.UUID
	ProjectID      uuid.UUID
	SessionID      uuid.UUID
	ExecutionID    uuid.UUID
}

func setupWorkerTenantIsolationTest(
	t *testing.T,
	tenantIsolation string,
) (*gorm.DB, *Service, persistence.ExecutionTarget) {
	t.Helper()
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	target := seedWorkerTenantIsolationTarget(t, db, tenantIsolation)
	return db, integrationService(t, db), target
}

func seedWorkerTenantIsolationTarget(
	t *testing.T,
	db *gorm.DB,
	tenantIsolation string,
) persistence.ExecutionTarget {
	return seedWorkerTenantIsolationTargetKind(t, db, tenantIsolation, "kubernetes")
}

func seedWorkerTenantIsolationTargetKind(
	t *testing.T,
	db *gorm.DB,
	tenantIsolation, targetKind string,
) persistence.ExecutionTarget {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: targetKind, Name: "tenant-isolation-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: workerManifestTestTargetCapabilities(),
		CreatedAt: now, UpdatedAt: now,
	}
	pool := persistence.WorkerPool{
		ID: uuid.New(), ExecutionTargetID: target.ID, Name: "default", Mode: placement.PoolModeResident,
		CapacityClass: placement.CapacityClassStandard, TenantIsolation: tenantIsolation,
		DesiredIdleUnits: 0, MinIdleUnits: 0, MaxActiveUnits: 2, SchedulingTemplate: map[string]any{},
		Status: placement.PoolStatusActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	policy := persistence.ExecutionPlacementPolicy{
		ExecutionTargetID: target.ID, Version: 1, DefaultPoolID: pool.ID, UpdatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range []any{&target, &pool, &policy} {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return target
}

func seedWorkerTenantIsolationExecution(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	label string,
	queuedAt time.Time,
) workerTenantIsolationExecution {
	t.Helper()
	identity := workerTenantIsolationExecution{
		TenantID: uuid.New(), UserID: uuid.New(), OrganizationID: uuid.New(), ProjectID: uuid.New(),
	}
	return seedWorkerTenantIsolationExecutionForTenant(t, db, target, identity, label, queuedAt)
}

func seedWorkerTenantIsolationExecutionForTenant(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	identity workerTenantIsolationExecution,
	label string,
	queuedAt time.Time,
) workerTenantIsolationExecution {
	t.Helper()
	sessionID, runtimeBindingID, turnID := uuid.New(), uuid.New(), uuid.New()
	providerCredentialID := uuid.New()
	provider := "codex"
	identity.ExecutionID = uuid.New()
	identity.SessionID = sessionID
	models := make([]any, 0, 12)
	if identity.ExecutionID == uuid.Nil {
		t.Fatal("empty Execution identity")
	}
	var tenantCount int64
	if err := db.Model(&persistence.Tenant{}).Where("id = ?", identity.TenantID).Count(&tenantCount).Error; err != nil {
		t.Fatal(err)
	}
	if tenantCount == 0 {
		models = append(models,
			&persistence.User{
				ID: identity.UserID, Email: uuid.NewString() + "@example.com", DisplayName: label,
				Status: "active", EmailVerifiedAt: &queuedAt, CreatedAt: queuedAt, UpdatedAt: queuedAt,
			},
			&persistence.Tenant{
				ID: identity.TenantID, Slug: label + "-" + uuid.NewString()[:8], Name: label,
				Status: "active", PlanCode: "test", Region: "local", Settings: map[string]any{},
				CreatedBy: identity.UserID, CreatedAt: queuedAt, UpdatedAt: queuedAt,
			},
			&persistence.TenantMembership{
				TenantID: identity.TenantID, UserID: identity.UserID, Role: "owner", Status: "active",
				JoinedAt: &queuedAt, CreatedAt: queuedAt, UpdatedAt: queuedAt,
			},
			&persistence.Organization{
				ID: identity.OrganizationID, TenantID: identity.TenantID, Slug: "root", Name: label,
				Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: identity.UserID,
				CreatedAt: queuedAt, UpdatedAt: queuedAt,
			},
			&persistence.OrganizationMembership{
				TenantID: identity.TenantID, OrganizationID: identity.OrganizationID, UserID: identity.UserID,
				Role: "owner", Status: "active", CreatedAt: queuedAt, UpdatedAt: queuedAt,
			},
			&persistence.Project{
				ID: identity.ProjectID, TenantID: identity.TenantID, OrganizationID: identity.OrganizationID,
				Name: label, DefaultBranch: "main", Visibility: "organization", CreatedBy: identity.UserID,
				CreatedAt: queuedAt, UpdatedAt: queuedAt,
			},
		)
	}
	models = append(models,
		&persistence.ProviderCredential{
			ID: providerCredentialID, TenantID: identity.TenantID, OrganizationID: &identity.OrganizationID,
			Name: label, Purpose: "provider", Provider: provider, CredentialType: "api_key",
			EncryptedPayload: []byte("encrypted-provider-payload"), EncryptedDataKey: []byte("encrypted-provider-data-key"),
			KMSProvider: "local", KMSKeyID: "test", Version: 1,
			CreatedBy: identity.UserID, UpdatedBy: identity.UserID, CreatedAt: queuedAt, UpdatedAt: queuedAt,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: identity.TenantID, OrganizationID: identity.OrganizationID, ProjectID: identity.ProjectID,
			CreatedBy: identity.UserID, Title: label, Status: "active", Visibility: "private", Provider: provider,
			ProviderCredentialID: &providerCredentialID, ExecutionTargetID: target.ID,
			CurrentRuntimeBindingID: &runtimeBindingID,
		},
		&persistence.ProviderRuntimeBinding{
			ID: runtimeBindingID, TenantID: identity.TenantID, SessionID: sessionID, Provider: provider,
			Revision: 1, Status: "active", ResumeStrategy: "authoritative-history", CreatedAt: queuedAt, UpdatedAt: queuedAt,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: identity.TenantID, SessionID: sessionID, CreatedBy: identity.UserID,
			Status: "queued", InputText: label, TurnKind: "message", RuntimeMode: "full-access",
			InteractionMode: "default", CreatedAt: queuedAt,
		},
		&persistence.AgentExecution{
			ID: identity.ExecutionID, TenantID: identity.TenantID, SessionID: sessionID, TurnID: turnID,
			Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: target.Kind,
			Provider: &provider, ProviderRuntimeBindingID: &runtimeBindingID,
			RequestedBy: identity.UserID, QueuedAt: queuedAt,
		},
	)
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return identity
}

func seedWorkerTenantIsolationCleanup(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	identity workerTenantIsolationExecution,
	requestedAt time.Time,
) uuid.UUID {
	t.Helper()
	workspace := persistence.RemoteWorkspace{
		ID: uuid.New(), TenantID: identity.TenantID, OrganizationID: identity.OrganizationID,
		ProjectID: identity.ProjectID, SessionID: identity.SessionID, ExecutionTargetID: target.ID,
		WorkspaceMode: "clone", State: "cleanup-pending", DefaultBranch: "main",
		RetentionUntil: &requestedAt, LastUsedAt: &requestedAt, CreatedAt: requestedAt, UpdatedAt: requestedAt,
	}
	materialization := persistence.WorkspaceMaterialization{
		ID: uuid.New(), TenantID: identity.TenantID, WorkspaceID: workspace.ID,
		OrganizationID: identity.OrganizationID, ProjectID: identity.ProjectID, SessionID: identity.SessionID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, StorageScope: "target", LayoutVersion: 3,
		IncarnationID: uuid.New(), State: "cleanup-pending", CleanupRequestedAt: &requestedAt,
		CreatedAt: requestedAt, UpdatedAt: requestedAt,
	}
	command := persistence.WorkspaceCleanupCommand{
		ID: uuid.New(), TenantID: identity.TenantID, MaterializationID: materialization.ID,
		MaterializationIncarnationID: materialization.IncarnationID, WorkspaceID: workspace.ID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, StorageScope: materialization.StorageScope,
		LayoutVersion: materialization.LayoutVersion, Reason: "tenant-isolation-test", Status: "pending",
		DeliveryAvailableAt: requestedAt, RequestedAt: requestedAt, CreatedAt: requestedAt, UpdatedAt: requestedAt,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&workspace).Error; err != nil {
			return err
		}
		if err := tx.Create(&materialization).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", workspace.TenantID, workspace.ID).
			Update("current_materialization_id", materialization.ID).Error; err != nil {
			return err
		}
		return tx.Create(&command).Error
	}); err != nil {
		t.Fatal(err)
	}
	return command.ID
}

func registerWorkerTenantIsolationTestWorker(
	t *testing.T,
	service *Service,
	target persistence.ExecutionTarget,
	podName, instanceUID string,
) (RegisteredWorker, persistence.WorkerInstance) {
	t.Helper()
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	fullPodName := podName + "-" + uuid.NewString()
	signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
		ExecutionTargetID: target.ID, TargetKind: platform.TargetKubernetes, InstanceUID: instanceUID,
		ClusterID: "kubernetes", Namespace: "default", PodName: fullPodName,
	})
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: target.ID, TargetKind: target.Kind, InstanceUID: instanceUID,
		ClusterID: "kubernetes", Namespace: "default", PodName: fullPodName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion, Capabilities: capabilities,
		LeaseSupported: true, FencingSupported: true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(context.Background(), registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	return registered, worker
}

func claimWorkerTenantIsolationExecution(
	t *testing.T,
	service *Service,
	worker persistence.WorkerInstance,
	target persistence.ExecutionTarget,
	requestID string,
) OperationResult[ClaimResult] {
	t.Helper()
	claimed, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: target.ID, TargetKind: target.Kind,
	}, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Value.Execution == nil || claimed.Value.Lease == nil {
		t.Fatalf("claim returned no work: %#v", claimed)
	}
	return claimed
}

func completeWorkerTenantIsolationExecution(
	t *testing.T,
	service *Service,
	worker persistence.WorkerInstance,
	claim OperationResult[ClaimResult],
	requestID string,
) {
	t.Helper()
	if _, err := service.Complete(context.Background(), worker, claim.Value.Execution.ID, CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: claim.Value.Execution.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		},
	}, requestID); err != nil {
		t.Fatal(err)
	}
}

func acknowledgeWorkerTenantStorageScrub(
	t *testing.T,
	service *Service,
	worker persistence.WorkerInstance,
	requestID string,
) {
	t.Helper()
	claimed, err := service.ClaimWorkerStorageScrub(context.Background(), worker)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Scrub == nil || claimed.Scrub.Status != workerStorageScrubStatusPending {
		t.Fatalf("pending Worker storage scrub = %#v", claimed.Scrub)
	}
	_, err = service.AcknowledgeWorkerStorageScrub(
		context.Background(), worker, claimed.Scrub.ID,
		WorkerStorageScrubReceiptInput{ScrubGeneration: claimed.Scrub.ScrubGeneration + 1},
		requestID+"-wrong-generation",
	)
	assertProblemCode(t, err, "worker_storage_scrub_fenced")
	acknowledged, err := service.AcknowledgeWorkerStorageScrub(
		context.Background(), worker, claimed.Scrub.ID,
		WorkerStorageScrubReceiptInput{ScrubGeneration: claimed.Scrub.ScrubGeneration}, requestID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.Value.Status != workerStorageScrubStatusAcknowledged || acknowledged.Value.AcknowledgedAt == nil {
		t.Fatalf("acknowledged Worker storage scrub = %#v", acknowledged.Value)
	}
}

func assertWorkerTenantBinding(t *testing.T, db *gorm.DB, workerID, tenantID uuid.UUID) {
	t.Helper()
	var worker persistence.WorkerInstance
	if err := db.Where("id = ?", workerID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if worker.TenantBindingID == nil || *worker.TenantBindingID != tenantID {
		t.Fatalf("Worker Tenant binding = %#v, want %s", worker.TenantBindingID, tenantID)
	}
}

func assertWorkerTenantUnbound(t *testing.T, db *gorm.DB, workerID uuid.UUID) {
	t.Helper()
	var worker persistence.WorkerInstance
	if err := db.Where("id = ?", workerID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if worker.TenantBindingID != nil {
		t.Fatalf("shared Worker unexpectedly bound to Tenant %s", *worker.TenantBindingID)
	}
}
