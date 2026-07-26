package executions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestKubernetesPodTerminalProofFinalizesSuspend(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	worker := registerPodBoundKubernetesTestWorker(t, service, fixture.TargetID, "pod-terminal")
	cleanupWorkers(t, db, worker.ID)

	leaseInput, directive, requestID := preparePodTerminalSuspend(
		t, service, worker, fixture, &current, "pod-terminal",
	)
	markResourceSuspendQuiescedForTest(
		t, service, worker, fixture.ExecutionID, leaseInput, directive.SuspendAttemptID,
		"pod-terminal-quiesced",
	)
	ready, err := service.MarkResourceSuspendCheckpointReady(
		ctx, worker, fixture.ExecutionID,
		MarkResourceSuspendCheckpointReadyInput{
			LeaseInput: leaseInput, SuspendAttemptID: directive.SuspendAttemptID,
			CheckpointStatus: "unchanged",
		},
		"pod-terminal-checkpoint-ready",
	)
	if err != nil || ready.Value.CheckpointReadyAt.IsZero() {
		t.Fatalf("checkpoint ready = %#v, %v", ready, err)
	}
	assertExecutionStatus(t, db, fixture, "waiting-for-approval")

	_, err = service.FinalizeKubernetesResourceSuspend(ctx, KubernetesPodTerminalProof{
		ExecutionTargetID: fixture.TargetID, ExecutionID: fixture.ExecutionID,
		Generation: leaseInput.Generation, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Failed", ObservedAt: current.Add(time.Second),
	})
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "invalid_kubernetes_pod_terminal_proof" {
		t.Fatalf("Failed Pod proof error = %v", err)
	}
	wrongUIDFinalized, err := service.FinalizeKubernetesResourceSuspend(ctx, KubernetesPodTerminalProof{
		ExecutionTargetID: fixture.TargetID, ExecutionID: fixture.ExecutionID,
		Generation: leaseInput.Generation, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: uuid.NewString(), Phase: "Succeeded", ObservedAt: current.Add(time.Second),
	})
	if err != nil || wrongUIDFinalized {
		t.Fatalf("wrong Pod UID proof finalized = %v, %v", wrongUIDFinalized, err)
	}
	assertExecutionStatus(t, db, fixture, "waiting-for-approval")

	observedAt := current.Add(time.Second)
	current = observedAt
	proof := KubernetesPodTerminalProof{
		ExecutionTargetID: fixture.TargetID, ExecutionID: fixture.ExecutionID,
		Generation: leaseInput.Generation, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Succeeded", ObservedAt: observedAt,
	}
	finalized, err := service.FinalizeKubernetesResourceSuspend(ctx, proof)
	if err != nil || !finalized {
		t.Fatalf("terminal proof finalized = %v, %v", finalized, err)
	}
	assertExecutionStatus(t, db, fixture, "suspended")
	var leaseCount int64
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&leaseCount).Error; err != nil || leaseCount != 0 {
		t.Fatalf("lease count = %d, %v", leaseCount, err)
	}
	_, release := loadExecutionClaimReleaseFactForTest(t, db, fixture.ExecutionID, leaseInput.Generation)
	if release.ReleaseReason != workerClaimReleaseResourceSuspendedPodTerminal ||
		!release.ReleasedAt.Equal(observedAt) || release.AuthorityKind != workerClaimReleaseAuthorityKubernetes ||
		release.AuthorityID == nil || *release.AuthorityID != worker.InstanceUID {
		t.Fatalf("Pod-terminal release fact = %#v", release)
	}
	var attempt persistence.ExecutionSuspendAttempt
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, directive.SuspendAttemptID).
		Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "completed" || attempt.PodTerminalObservedAt == nil ||
		attempt.PodTerminalPhase == nil || *attempt.PodTerminalPhase != "Succeeded" ||
		attempt.WorkerInstanceUID != worker.InstanceUID {
		t.Fatalf("terminal proof was not durably bound: %#v", attempt)
	}
	idempotent, err := service.FinalizeKubernetesResourceSuspend(ctx, proof)
	if err != nil || !idempotent {
		t.Fatalf("idempotent terminal proof = %v, %v", idempotent, err)
	}

	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	resolved, err := service.ResolveApproval(
		ctx, principal, fixture.ExecutionID, requestID, ResolveApprovalInput{Decision: "accept"},
		"pod-terminal-resolve", "pod-terminal-resolve-audit", "127.0.0.1",
	)
	if err != nil || resolved.Value.DeliveryStatus != "resume-recorded" {
		t.Fatalf("resolve suspended interaction = %#v, %v", resolved, err)
	}
	assertExecutionStatus(t, db, fixture, "recovering")
}

func TestKubernetesPodTerminalProofRecoversWhenInteractionResolvedDuringExit(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	worker := registerPodBoundKubernetesTestWorker(t, service, fixture.TargetID, "pod-terminal-race")
	cleanupWorkers(t, db, worker.ID)

	leaseInput, directive, requestID := preparePodTerminalSuspend(
		t, service, worker, fixture, &current, "pod-terminal-race",
	)
	markResourceSuspendQuiescedForTest(
		t, service, worker, fixture.ExecutionID, leaseInput, directive.SuspendAttemptID,
		"pod-terminal-race-quiesced",
	)
	if _, err := service.MarkResourceSuspendCheckpointReady(
		ctx, worker, fixture.ExecutionID,
		MarkResourceSuspendCheckpointReadyInput{
			LeaseInput: leaseInput, SuspendAttemptID: directive.SuspendAttemptID,
			CheckpointStatus: "unchanged",
		},
		"pod-terminal-race-checkpoint-ready",
	); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	resolved, err := service.ResolveApproval(
		ctx, principal, fixture.ExecutionID, requestID, ResolveApprovalInput{Decision: "accept"},
		"pod-terminal-race-resolve", "pod-terminal-race-audit", "127.0.0.1",
	)
	if err != nil || resolved.Value.DeliveryStatus != "resume-recorded" {
		t.Fatalf("resolution during Pod exit = %#v, %v", resolved, err)
	}
	assertExecutionStatus(t, db, fixture, "waiting-for-approval")

	observedAt := current.Add(time.Second)
	current = observedAt
	finalized, err := service.FinalizeKubernetesResourceSuspend(ctx, KubernetesPodTerminalProof{
		ExecutionTargetID: fixture.TargetID, ExecutionID: fixture.ExecutionID,
		Generation: leaseInput.Generation, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Succeeded", ObservedAt: observedAt,
	})
	if err != nil || !finalized {
		t.Fatalf("race terminal proof = %v, %v", finalized, err)
	}
	// No pending interaction remains, so the atomic transaction emits the
	// suspend proof and immediately queues a new Recovery generation.
	assertExecutionStatus(t, db, fixture, "recovering")
}

func TestKubernetesPodTerminalProofSurvivesLogicalWorkerReregistration(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	current := time.Now().UTC().Truncate(time.Millisecond)
	service.now = func() time.Time { return current }
	worker := registerPodBoundKubernetesTestWorker(t, service, fixture.TargetID, "pod-terminal-replaced")
	cleanupWorkers(t, db, worker.ID)

	leaseInput, directive, _ := preparePodTerminalSuspend(
		t, service, worker, fixture, &current, "pod-terminal-replaced",
	)
	markResourceSuspendQuiescedForTest(
		t, service, worker, fixture.ExecutionID, leaseInput, directive.SuspendAttemptID,
		"pod-terminal-replaced-quiesced",
	)
	if _, err := service.MarkResourceSuspendCheckpointReady(
		ctx, worker, fixture.ExecutionID,
		MarkResourceSuspendCheckpointReadyInput{
			LeaseInput: leaseInput, SuspendAttemptID: directive.SuspendAttemptID,
			CheckpointStatus: "unchanged",
		},
		"pod-terminal-replaced-checkpoint-ready",
	); err != nil {
		t.Fatal(err)
	}

	replacementUID := uuid.NewString()
	replacement, err := service.Register(ctx, RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: "kubernetes", InstanceUID: replacementUID,
		ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: workerManifestTestCapabilities(), LeaseSupported: true, FencingSupported: true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Worker.ID != worker.ID || replacement.Worker.Incarnation != worker.Incarnation+1 ||
		replacement.Worker.InstanceUID != replacementUID {
		t.Fatalf("logical Worker replacement = %#v", replacement.Worker)
	}

	observedAt := current.Add(time.Second)
	current = observedAt
	finalized, err := service.FinalizeKubernetesResourceSuspend(ctx, KubernetesPodTerminalProof{
		ExecutionTargetID: fixture.TargetID, ExecutionID: fixture.ExecutionID,
		Generation: leaseInput.Generation, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Succeeded", ObservedAt: observedAt,
	})
	if err != nil || !finalized {
		t.Fatalf("frozen old Pod proof after replacement = %v, %v", finalized, err)
	}
	assertExecutionStatus(t, db, fixture, "suspended")
}

func TestPodBoundKubernetesRegistrationIsOneShotPerPhysicalPodUID(t *testing.T) {
	_, service, fixture := setupSQLiteRecoveryService(t)
	input := RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: "kubernetes", InstanceUID: uuid.NewString(),
		ClusterID: "kubernetes", Namespace: "default", PodName: "one-shot-" + uuid.NewString(),
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: workerManifestTestCapabilities(), LeaseSupported: true, FencingSupported: true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	}
	if _, err := service.Register(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	_, err := service.Register(context.Background(), input)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "kubernetes_worker_instance_already_registered" {
		t.Fatalf("Pod-bound registration replay error = %v", err)
	}
}

func TestPodBoundKubernetesRegistrationRejectsDeletedPodUIDButAllowsReplacementUID(t *testing.T) {
	db, service, fixture := setupSQLiteRecoveryService(t)
	podName := "deletion-fenced-" + uuid.NewString()
	deletedPodUID := uuid.NewString()
	if err := db.Create(&persistence.KubernetesPodDeletionFence{
		ExecutionTargetID: fixture.TargetID,
		Namespace:         "default",
		PodName:           podName,
		PodUID:            deletedPodUID,
		RequestedAt:       time.Now().UTC(),
		Reason:            "registration-race-test",
	}).Error; err != nil {
		t.Fatal(err)
	}
	input := RegisterWorkerInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: "kubernetes", InstanceUID: deletedPodUID,
		ClusterID: "kubernetes", Namespace: "default", PodName: podName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: workerManifestTestCapabilities(), LeaseSupported: true, FencingSupported: true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	}
	_, err := service.Register(context.Background(), input)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "kubernetes_pod_deletion_fenced" {
		t.Fatalf("deletion-fenced Pod registration error = %v", err)
	}

	input.InstanceUID = uuid.NewString()
	registered, err := service.Register(context.Background(), input)
	if err != nil {
		t.Fatalf("replacement Pod UID registration failed: %v", err)
	}
	if registered.Worker.InstanceUID != input.InstanceUID {
		t.Fatalf("replacement registration = %#v", registered.Worker)
	}
}

func TestDeletionFencedKubernetesWorkerCannotHeartbeatOnlineOrClaim(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerPodBoundKubernetesTestWorker(t, service, fixture.TargetID, "deletion-fenced-runtime")
	fencedAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.KubernetesPodDeletionFence{
			ExecutionTargetID: worker.ExecutionTargetID,
			Namespace:         worker.Namespace,
			PodName:           worker.PodName,
			PodUID:            worker.InstanceUID,
			RequestedAt:       fencedAt,
			Reason:            "runtime-reactivation-test",
		}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).Updates(map[string]any{
			"status": "draining", "draining_at": fencedAt,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	draining := false
	_, err := service.Heartbeat(ctx, worker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion,
		Draining:        &draining,
	})
	assertKubernetesPodDeletionFenced(t, err, "heartbeat")
	_, err = service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        "kubernetes",
	}, "deletion-fenced-claim-"+uuid.NewString())
	assertKubernetesPodDeletionFenced(t, err, "claim")

	var stored persistence.WorkerInstance
	if err := db.Where("id = ?", worker.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "draining" || stored.AdministrativeStatus != "active" || stored.DrainingAt == nil {
		t.Fatalf("fenced Worker escaped draining state: %#v", stored)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Updates(map[string]any{"status": "online", "draining_at": nil}).Error; err == nil {
		t.Fatal("SQLite allowed a direct fenced Worker reactivation")
	}
}

func TestDeletedPodUIDFenceAllowsSameNameReplacementToRegisterAndClaim(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	oldWorker := registerPodBoundKubernetesTestWorker(t, service, fixture.TargetID, "replacement-after-delete")
	fencedAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.KubernetesPodDeletionFence{
			ExecutionTargetID: oldWorker.ExecutionTargetID,
			Namespace:         oldWorker.Namespace,
			PodName:           oldWorker.PodName,
			PodUID:            oldWorker.InstanceUID,
			RequestedAt:       fencedAt,
			Reason:            "replacement-after-delete-test",
		}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkerInstance{}).Where("id = ?", oldWorker.ID).Updates(map[string]any{
			"status": "draining", "draining_at": fencedAt,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	replacementUID := uuid.NewString()
	registered, err := service.Register(ctx, RegisterWorkerInput{
		ExecutionTargetID:     oldWorker.ExecutionTargetID,
		TargetKind:            "kubernetes",
		WorkerMode:            oldWorker.WorkerMode,
		InstanceUID:           replacementUID,
		ClusterID:             oldWorker.ClusterID,
		Namespace:             oldWorker.Namespace,
		PodName:               oldWorker.PodName,
		Version:               oldWorker.Version,
		ProtocolVersion:       WorkerProtocolVersion,
		Capabilities:          workerManifestTestCapabilities(),
		LeaseSupported:        true,
		FencingSupported:      true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	})
	if err != nil {
		t.Fatalf("replacement Pod registration failed: %v", err)
	}
	replacement, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.InstanceUID != replacementUID || replacement.Status != "online" || replacement.AdministrativeStatus != "active" {
		t.Fatalf("replacement Worker retained old physical lifecycle state: %#v", replacement)
	}
	claim, err := service.Claim(ctx, replacement, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        "kubernetes",
	}, "replacement-after-delete-claim-"+uuid.NewString())
	if err != nil {
		t.Fatalf("replacement Pod could not claim: %v", err)
	}
	if claim.Value.Lease == nil || claim.Value.Execution == nil {
		t.Fatalf("replacement Pod claim = %#v", claim.Value)
	}
}

func TestPodReplacementDoesNotClearOperatorAdministrativeDrain(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerPodBoundKubernetesTestWorker(t, service, fixture.TargetID, "operator-drained-replacement")
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("administrative_status", "draining").Error; err != nil {
		t.Fatal(err)
	}
	registered, err := service.Register(ctx, RegisterWorkerInput{
		ExecutionTargetID:     worker.ExecutionTargetID,
		TargetKind:            "kubernetes",
		WorkerMode:            worker.WorkerMode,
		InstanceUID:           uuid.NewString(),
		ClusterID:             worker.ClusterID,
		Namespace:             worker.Namespace,
		PodName:               worker.PodName,
		Version:               worker.Version,
		ProtocolVersion:       WorkerProtocolVersion,
		Capabilities:          workerManifestTestCapabilities(),
		LeaseSupported:        true,
		FencingSupported:      true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.AdministrativeStatus != "draining" {
		t.Fatalf("replacement cleared operator administrative drain: %#v", replacement)
	}
}

func assertKubernetesPodDeletionFenced(t *testing.T, err error, operation string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "kubernetes_pod_deletion_fenced" {
		t.Fatalf("%s error = %v, want kubernetes_pod_deletion_fenced", operation, err)
	}
}

func registerPodBoundKubernetesTestWorker(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	podPrefix string,
) persistence.WorkerInstance {
	t.Helper()
	instanceUID := uuid.NewString()
	podName := podPrefix + "-" + uuid.NewString()
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: targetID, TargetKind: "kubernetes", InstanceUID: instanceUID,
		ClusterID: "kubernetes", Namespace: "default", PodName: podName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: workerManifestTestCapabilities(), LeaseSupported: true, FencingSupported: true,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(context.Background(), registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func preparePodTerminalSuspend(
	t *testing.T,
	service *Service,
	worker persistence.WorkerInstance,
	fixture executionFixture,
	current *time.Time,
	requestPrefix string,
) (LeaseInput, *ResourceDirective, string) {
	t.Helper()
	ctx := context.Background()
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, requestPrefix+"-claim")
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	leaseInput := LeaseInput{
		TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := service.Start(ctx, worker, fixture.ExecutionID, leaseInput, requestPrefix+"-start"); err != nil {
		t.Fatal(err)
	}
	requestID := requestPrefix + "-approval-" + uuid.NewString()
	if _, err := service.AppendRuntimeEvent(ctx, worker, fixture.ExecutionID, RuntimeEventInput{
		LeaseInput: leaseInput, EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "request.opened", OccurredAt: *current,
		Payload: map[string]any{
			"requestId": requestID, "requestType": "exec_command_approval", "detail": requestPrefix,
		},
	}, requestPrefix+"-opened"); err != nil {
		t.Fatal(err)
	}
	*current = (*current).Add(16 * time.Minute)
	service.now = func() time.Time { return *current }
	if err := service.db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"heartbeat_at": *current, "expires_at": (*current).Add(service.leaseTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	directive, err := service.PullResourceDirective(ctx, worker, fixture.ExecutionID, PullResourceDirectiveInput{LeaseInput: leaseInput})
	if err != nil || directive == nil || directive.CompletionMode != ResourceSuspendCompletionKubernetesPodTerminalV1 {
		t.Fatalf("Pod-terminal directive = %#v, %v", directive, err)
	}
	return leaseInput, directive, requestID
}
