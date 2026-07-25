package executions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

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

	finalized, err := service.FinalizeKubernetesResourceSuspend(ctx, KubernetesPodTerminalProof{
		ExecutionTargetID: fixture.TargetID, ExecutionID: fixture.ExecutionID,
		Generation: leaseInput.Generation, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Succeeded", ObservedAt: current.Add(time.Second),
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

	finalized, err := service.FinalizeKubernetesResourceSuspend(ctx, KubernetesPodTerminalProof{
		ExecutionTargetID: fixture.TargetID, ExecutionID: fixture.ExecutionID,
		Generation: leaseInput.Generation, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Succeeded", ObservedAt: current.Add(time.Second),
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
