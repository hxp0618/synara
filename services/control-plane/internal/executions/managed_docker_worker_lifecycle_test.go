package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestManagedDockerDrainBlocksNewClaimsAndFollowsExactReplacement(t *testing.T) {
	db, service, fixture := setupSQLiteManagedDockerLifecycle(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "managed-rollout")
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	request := executiontargets.ManagedDockerWorkerDrainRequest{
		ExecutionTargetID: fixture.TargetID,
		ContainerName:     worker.PodName,
		Reason:            executiontargets.ManagedDockerDrainReasonStaleSpec,
		ObservedAt:        observedAt,
	}

	decision, err := service.PrepareManagedDockerDrain(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.WorkerFound || !decision.DeletionAllowed || decision.ExecutionLeaseCount != 0 || decision.WorkspaceCleanupCount != 0 {
		t.Fatalf("initial managed drain decision = %#v", decision)
	}
	stored := loadManagedDockerLifecycleWorker(t, db, worker.ID)
	if stored.Status != "draining" || stored.DrainingAt == nil ||
		stored.ReconciliationDrainIncarnation == nil || *stored.ReconciliationDrainIncarnation != worker.Incarnation ||
		stored.ReconciliationDrainInstanceUID == nil || *stored.ReconciliationDrainInstanceUID != worker.InstanceUID ||
		stored.ReconciliationDrainRequestedAt == nil || !stored.ReconciliationDrainRequestedAt.Equal(observedAt) ||
		stored.ReconciliationDrainReason == nil || *stored.ReconciliationDrainReason != executiontargets.ManagedDockerDrainReasonStaleSpec {
		t.Fatalf("persisted managed drain = %#v", stored)
	}

	draining := false
	heartbeat, err := service.Heartbeat(context.Background(), stored, HeartbeatInput{
		Version: stored.Version, ProtocolVersion: WorkerProtocolVersion, Draining: &draining,
	})
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.Status != "draining" || heartbeat.ReconciliationDrainRequestedAt == nil {
		t.Fatalf("Worker heartbeat cleared server-authored drain: %#v", heartbeat)
	}
	_, err = service.Claim(context.Background(), stored, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
	}, "managed-drain-claim-rejected")
	assertProblemCode(t, err, "worker_reconciliation_draining")

	replacement := reregisterManagedDockerLifecycleWorker(t, service, stored)
	if replacement.InstanceUID == worker.InstanceUID || replacement.Incarnation != worker.Incarnation+1 ||
		replacement.Status != "draining" || replacement.ReconciliationDrainInstanceUID == nil ||
		*replacement.ReconciliationDrainInstanceUID != worker.InstanceUID {
		t.Fatalf("replacement did not retain the old drain fence: %#v", replacement)
	}
	completed, err := service.CompleteManagedDockerReplacement(
		context.Background(), fixture.TargetID, worker.PodName, worker.InstanceUID, time.Now().UTC(),
	)
	if err != nil || !completed {
		t.Fatalf("complete replacement: completed=%t err=%v", completed, err)
	}
	replacement = loadManagedDockerLifecycleWorker(t, db, worker.ID)
	if replacement.Status != "online" || replacement.DrainingAt != nil ||
		replacement.ReconciliationDrainIncarnation != nil || replacement.ReconciliationDrainInstanceUID != nil ||
		replacement.ReconciliationDrainRequestedAt != nil || replacement.ReconciliationDrainReason != nil {
		t.Fatalf("completed replacement retained drain state: %#v", replacement)
	}
	claim, err := service.Claim(context.Background(), replacement, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
	}, "managed-drain-replacement-claim")
	if err != nil || claim.Value.Lease == nil || claim.Value.Execution == nil {
		t.Fatalf("replacement could not claim after exact completion: %#v, %v", claim, err)
	}
}

func TestManagedDockerDrainWaitsForExecutionLeaseThenTerminalizesFact(t *testing.T) {
	db, service, fixture := setupSQLiteManagedDockerLifecycle(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "managed-busy-rollout")
	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
	}, "managed-busy-rollout-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim before drain: %#v, %v", claim, err)
	}
	lease := *claim.Value.Lease
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	request := executiontargets.ManagedDockerWorkerDrainRequest{
		ExecutionTargetID: fixture.TargetID,
		ContainerName:     worker.PodName,
		Reason:            executiontargets.ManagedDockerDrainReasonScaleDown,
		ObservedAt:        observedAt,
	}
	decision, err := service.PrepareManagedDockerDrain(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if decision.DeletionAllowed || decision.ExecutionLeaseCount != 1 || decision.WorkspaceCleanupCount != 0 {
		t.Fatalf("busy managed drain decision = %#v", decision)
	}
	if err := service.FinalizeManagedDockerDrain(context.Background(), request); err == nil {
		t.Fatal("busy managed Docker Worker was terminalized")
	} else {
		assertProblemCode(t, err, "docker_worker_drain_busy")
	}

	if _, err := service.Release(
		context.Background(), worker, fixture.ExecutionID,
		ReleaseLeaseInput{
			LeaseInput: LeaseInput{
				TenantID: fixture.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			Reason: "managed Docker rolling drain",
		},
		"managed-busy-rollout-release",
	); err != nil {
		t.Fatal(err)
	}
	request.ObservedAt = time.Now().UTC()
	decision, err = service.PrepareManagedDockerDrain(context.Background(), request)
	if err != nil || !decision.DeletionAllowed || decision.ExecutionLeaseCount != 0 {
		t.Fatalf("post-release managed drain decision = %#v, %v", decision, err)
	}
	if err := service.FinalizeManagedDockerDrain(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	stored := loadManagedDockerLifecycleWorker(t, db, worker.ID)
	if stored.Status != "terminated" || stored.TerminatedAt == nil {
		t.Fatalf("managed Docker Worker was not terminalized: %#v", stored)
	}
	var fact persistence.WorkerIncarnationFact
	if err := db.Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).Take(&fact).Error; err != nil {
		t.Fatal(err)
	}
	if fact.CurrentState != workerFactStateTerminated || fact.TerminatedAt == nil ||
		fact.TerminalReason == nil || *fact.TerminalReason != executiontargets.ManagedDockerDrainReasonScaleDown {
		t.Fatalf("managed Docker terminal fact = %#v", fact)
	}
}

func TestManagedDockerDrainWaitsForWorkspaceCleanupLease(t *testing.T) {
	db, service, _ := setupSQLiteRecoveryService(t)
	fixture, _, _ := seedWorkspaceCleanupFixtureForTargetKind(t, db, false, false, "docker")
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil || created != 1 {
		t.Fatalf("reconcile Workspace cleanup: created=%d err=%v", created, err)
	}
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "managed-cleanup-rollout")
	claim, err := service.ClaimWorkspaceCleanup(
		context.Background(), worker, WorkspaceCleanupClaimInput{}, "managed-cleanup-rollout-claim",
	)
	if err != nil || claim.Value.Cleanup == nil {
		t.Fatalf("claim Workspace cleanup: %#v, %v", claim, err)
	}
	cleanup := *claim.Value.Cleanup
	request := executiontargets.ManagedDockerWorkerDrainRequest{
		ExecutionTargetID: fixture.TargetID,
		ContainerName:     worker.PodName,
		Reason:            executiontargets.ManagedDockerDrainReasonStaleSpec,
		ObservedAt:        now,
	}
	decision, err := service.PrepareManagedDockerDrain(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if decision.DeletionAllowed || decision.ExecutionLeaseCount != 0 || decision.WorkspaceCleanupCount != 1 {
		t.Fatalf("cleanup-busy managed drain decision = %#v", decision)
	}
	_, err = service.ClaimWorkspaceCleanup(
		context.Background(), worker, WorkspaceCleanupClaimInput{}, "managed-cleanup-rollout-second-claim",
	)
	assertProblemCode(t, err, "worker_reconciliation_draining")
	if _, err := service.ReleaseWorkspaceCleanup(
		context.Background(),
		worker,
		cleanup.CleanupID,
		WorkspaceCleanupLeaseInput{
			DispatchGeneration: cleanup.DispatchGeneration,
			LeaseToken:         cleanup.Lease.LeaseToken,
		},
		"managed-cleanup-rollout-release",
	); err != nil {
		t.Fatal(err)
	}
	decision, err = service.PrepareManagedDockerDrain(context.Background(), request)
	if err != nil || !decision.DeletionAllowed || decision.WorkspaceCleanupCount != 0 {
		t.Fatalf("post-cleanup-release managed drain decision = %#v, %v", decision, err)
	}
}

func TestManagedDockerDrainShapeIsDatabaseEnforcedOnSQLite(t *testing.T) {
	db, service, fixture := setupSQLiteManagedDockerLifecycle(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "managed-drain-shape")
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("reconciliation_drain_reason", executiontargets.ManagedDockerDrainReasonStaleSpec).Error; err == nil {
		t.Fatal("SQLite accepted a partial managed Worker drain")
	}
}

func TestManagedDockerMissingContainerRecoversPersistedDrainAfterRestart(t *testing.T) {
	db, service, fixture := setupSQLiteManagedDockerLifecycle(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "managed-missing-rollout")
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := executiontargets.ManagedDockerWorkerDrainRequest{
		ExecutionTargetID: fixture.TargetID,
		ContainerName:     worker.PodName,
		Reason:            executiontargets.ManagedDockerDrainReasonStaleSpec,
		ObservedAt:        now,
	}
	if _, err := service.PrepareManagedDockerDrain(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	advanced, err := service.RecoverMissingManagedDockerDrains(
		context.Background(), fixture.TargetID, nil, now.Add(time.Millisecond),
	)
	if err != nil || !advanced {
		t.Fatalf("recover missing managed drain: advanced=%t err=%v", advanced, err)
	}
	stored := loadManagedDockerLifecycleWorker(t, db, worker.ID)
	if stored.Status != "terminated" || stored.TerminatedAt == nil {
		t.Fatalf("missing managed Docker Worker remained active: %#v", stored)
	}
	advanced, err = service.RecoverMissingManagedDockerDrains(
		context.Background(), fixture.TargetID, nil, now.Add(2*time.Millisecond),
	)
	if err != nil || advanced {
		t.Fatalf("stable missing-drain replay: advanced=%t err=%v", advanced, err)
	}
}

func setupSQLiteManagedDockerLifecycle(t *testing.T) (*gorm.DB, *Service, executionFixture) {
	t.Helper()
	db, service, _ := setupSQLiteRecoveryService(t)
	fixture := seedExecutionFixtureForTargetKind(t, db, false, "docker")
	return db, service, fixture
}

func loadManagedDockerLifecycleWorker(t *testing.T, db *gorm.DB, workerID uuid.UUID) persistence.WorkerInstance {
	t.Helper()
	var worker persistence.WorkerInstance
	if err := db.Where("id = ?", workerID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	return worker
}

func reregisterManagedDockerLifecycleWorker(
	t *testing.T,
	service *Service,
	worker persistence.WorkerInstance,
) persistence.WorkerInstance {
	t.Helper()
	instanceUID := uuid.NewString()
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	parsedKind, err := platform.ParseExecutionTargetKind(worker.TargetKind)
	if err != nil {
		t.Fatal(err)
	}
	signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
		ExecutionTargetID: worker.ExecutionTargetID,
		TargetKind:        parsedKind,
		InstanceUID:       instanceUID,
		ClusterID:         worker.ClusterID,
		Namespace:         worker.Namespace,
		PodName:           worker.PodName,
	})
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: worker.ExecutionTargetID,
		TargetKind:        worker.TargetKind,
		WorkerMode:        worker.WorkerMode,
		InstanceUID:       instanceUID,
		ClusterID:         worker.ClusterID,
		Namespace:         worker.Namespace,
		PodName:           worker.PodName,
		Version:           worker.Version,
		ProtocolVersion:   WorkerProtocolVersion,
		Capabilities:      capabilities,
		LeaseSupported:    true,
		FencingSupported:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.Authenticate(context.Background(), registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	return current
}
