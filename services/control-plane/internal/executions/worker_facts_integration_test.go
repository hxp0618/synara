package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestWorkerIncarnationFactTracksClaimReplayAndGeneralPoolIdleReturn(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	base := time.Date(2026, time.July, 25, 2, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return base }

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "worker-fact-general", WorkerModeGeneralPool,
	)
	fact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.CurrentState != workerFactStateIdle || fact.ClaimCount != 0 {
		t.Fatalf("registered Worker fact = %#v", fact)
	}

	claimAt := base.Add(5 * time.Second)
	service.now = func() time.Time { return claimAt }
	requestID := "worker-fact-claim-" + uuid.NewString()
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("claim lease is nil")
	}
	fact = loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.CurrentState != workerFactStateActive || fact.ClaimCount != 1 || fact.AccumulatedIdleSeconds != 5 {
		t.Fatalf("claimed Worker fact = %#v", fact)
	}
	claims := loadWorkerClaimFactsForTest(t, db, worker.ID, worker.Incarnation)
	if len(claims) != 1 || claims[0].ClaimKind != workerClaimKindExecution || claims[0].RequestID != requestID ||
		claims[0].ExecutionID == nil || *claims[0].ExecutionID != fixture.ExecutionID ||
		claims[0].ExecutionGeneration == nil || *claims[0].ExecutionGeneration != claim.Value.Lease.Generation ||
		claims[0].CleanupCommandID != nil || claims[0].CleanupDispatchGeneration != nil {
		t.Fatalf("execution claim facts = %#v", claims)
	}

	replayedClaim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if replayedClaim.Value.Lease == nil {
		t.Fatal("replayed claim lease is nil")
	}
	fact = loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.ClaimCount != 1 {
		t.Fatalf("claim replay incremented count: %#v", fact)
	}
	claims = loadWorkerClaimFactsForTest(t, db, worker.ID, worker.Incarnation)
	if len(claims) != 1 {
		t.Fatalf("execution claim replay duplicated claim facts: %#v", claims)
	}
	_, err = service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
	}, "worker-fact-second-claim-"+uuid.NewString())
	assertProblemCode(t, err, "worker_busy")

	completeAt := claimAt.Add(4 * time.Second)
	service.now = func() time.Time { return completeAt }
	if _, err := service.Complete(ctx, worker, fixture.ExecutionID, CompleteExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: replayedClaim.Value.Lease.LeaseToken,
		},
	}, "worker-fact-complete-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	fact = loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.CurrentState != workerFactStateIdle || fact.AccumulatedActiveSeconds != 4 {
		t.Fatalf("completed general-pool Worker fact = %#v", fact)
	}
}

func TestWorkerIncarnationFactMovesOneShotWorkerToDrainingAfterAttempt(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	base := time.Date(2026, time.July, 25, 3, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return base }

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "worker-fact-pinned", WorkerModeExecutionPinned,
	)
	claimAt := base.Add(time.Second)
	service.now = func() time.Time { return claimAt }
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ExecutionID: &fixture.ExecutionID,
	}, "worker-fact-pinned-claim-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("claim lease is nil")
	}

	failAt := claimAt.Add(3 * time.Second)
	service.now = func() time.Time { return failAt }
	if _, err := service.Fail(ctx, worker, fixture.ExecutionID, FailExecutionInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		},
		FailureCode: "provider_failed", FailureMessage: "provider exited",
	}, "worker-fact-pinned-fail-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	fact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.CurrentState != workerFactStateDraining || fact.TerminatedAt != nil || fact.AccumulatedActiveSeconds != 3 {
		t.Fatalf("failed one-shot Worker fact = %#v", fact)
	}
	var stored persistence.WorkerInstance
	if err := db.Where("id = ?", worker.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "draining" || stored.DrainingAt == nil || !stored.DrainingAt.Equal(failAt) {
		t.Fatalf("one-shot Worker row = %#v", stored)
	}
}

func TestWorkerReregistrationTerminalizesOldFactAndResetsRegistrationTime(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	firstAt := time.Date(2026, time.July, 25, 4, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(30 * time.Second)
	service.now = func() time.Time { return firstAt }

	podName := "worker-fact-reregister"
	firstInput := workerFactRegistrationInput(t, fixture.TargetID, fixture.TargetKind, podName, uuid.NewString())
	first, err := service.Register(ctx, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	firstWorker, err := service.Authenticate(ctx, first.Token)
	if err != nil {
		t.Fatal(err)
	}

	service.now = func() time.Time { return secondAt }
	secondInput := workerFactRegistrationInput(t, fixture.TargetID, fixture.TargetKind, podName, uuid.NewString())
	second, err := service.Register(ctx, secondInput)
	if err != nil {
		t.Fatal(err)
	}
	secondWorker, err := service.Authenticate(ctx, second.Token)
	if err != nil {
		t.Fatal(err)
	}
	if secondWorker.ID != firstWorker.ID || secondWorker.Incarnation != firstWorker.Incarnation+1 {
		t.Fatalf("re-registered Worker identity = %#v, first = %#v", secondWorker, firstWorker)
	}
	if !secondWorker.RegisteredAt.Equal(secondAt) {
		t.Fatalf("second registeredAt = %s, want %s", secondWorker.RegisteredAt, secondAt)
	}

	oldFact := loadWorkerIncarnationFactForTest(t, db, firstWorker.ID, firstWorker.Incarnation)
	if oldFact.CurrentState != workerFactStateTerminated || oldFact.TerminatedAt == nil ||
		!oldFact.TerminatedAt.Equal(secondAt) || oldFact.TerminalReason == nil ||
		*oldFact.TerminalReason != "worker-reregistered" {
		t.Fatalf("old Worker fact = %#v", oldFact)
	}
	newFact := loadWorkerIncarnationFactForTest(t, db, secondWorker.ID, secondWorker.Incarnation)
	if newFact.CurrentState != workerFactStateIdle || !newFact.RegisteredAt.Equal(secondAt) {
		t.Fatalf("new Worker fact = %#v", newFact)
	}
}

func TestWorkerRegistrationFreezesAuthenticatedResourceRequestsIntoFact(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	now := time.Date(2026, time.July, 25, 5, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	cpu := int64(750)
	memory := int64(512 << 20)
	ephemeral := int64(2 << 30)
	input := workerFactRegistrationInput(
		t, fixture.TargetID, fixture.TargetKind, "worker-fact-resources", uuid.NewString(),
	)
	input.RequestedCPUMillicores = &cpu
	input.RequestedMemoryBytes = &memory
	input.RequestedEphemeralStorageBytes = &ephemeral
	registered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	fact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.RequestedCPUMillicores == nil || *fact.RequestedCPUMillicores != cpu ||
		fact.RequestedMemoryBytes == nil || *fact.RequestedMemoryBytes != memory ||
		fact.RequestedEphemeralStorageBytes == nil || *fact.RequestedEphemeralStorageBytes != ephemeral {
		t.Fatalf("Worker resource snapshot = %#v", fact)
	}
}

func TestWorkspaceCleanupClaimReplayAndHeartbeatKeepWorkerFactActive(t *testing.T) {
	ctx := context.Background()
	db, service, _ := setupSQLiteRecoveryService(t)
	fixture, _, _ := seedWorkspaceCleanupFixture(t, db, false)
	base := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return base }

	created, err := service.ReconcileWorkspaceCleanup(ctx, base, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands created = %d, want 1", created)
	}

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "worker-fact-cleanup-active", WorkerModeGeneralPool,
	)

	claimAt := base.Add(5 * time.Second)
	service.now = func() time.Time { return claimAt }
	requestID := "worker-fact-cleanup-claim-" + uuid.NewString()
	claimed, err := service.ClaimWorkspaceCleanup(ctx, worker, WorkspaceCleanupClaimInput{}, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Value.Cleanup == nil {
		t.Fatal("cleanup claim is nil")
	}
	fact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.CurrentState != workerFactStateActive || fact.ClaimCount != 1 || fact.AccumulatedIdleSeconds != 5 {
		t.Fatalf("claimed cleanup Worker fact = %#v", fact)
	}
	claims := loadWorkerClaimFactsForTest(t, db, worker.ID, worker.Incarnation)
	if len(claims) != 1 || claims[0].ClaimKind != workerClaimKindWorkspaceCleanup || claims[0].RequestID != requestID ||
		claims[0].CleanupCommandID == nil || *claims[0].CleanupCommandID != claimed.Value.Cleanup.CleanupID ||
		claims[0].CleanupDispatchGeneration == nil || *claims[0].CleanupDispatchGeneration != claimed.Value.Cleanup.DispatchGeneration ||
		claims[0].ExecutionID != nil || claims[0].ExecutionGeneration != nil {
		t.Fatalf("cleanup claim facts = %#v", claims)
	}

	replayed, err := service.ClaimWorkspaceCleanup(ctx, worker, WorkspaceCleanupClaimInput{}, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Value.Cleanup == nil {
		t.Fatalf("cleanup replay = %#v", replayed)
	}
	fact = loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.ClaimCount != 1 {
		t.Fatalf("cleanup replay incremented count: %#v", fact)
	}
	claims = loadWorkerClaimFactsForTest(t, db, worker.ID, worker.Incarnation)
	if len(claims) != 1 {
		t.Fatalf("cleanup claim replay duplicated claim facts: %#v", claims)
	}

	heartbeatAt := claimAt.Add(2 * time.Second)
	service.now = func() time.Time { return heartbeatAt }
	if _, err := service.Heartbeat(ctx, worker, HeartbeatInput{ProtocolVersion: WorkerProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	fact = loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.CurrentState != workerFactStateActive || fact.ClaimCount != 1 {
		t.Fatalf("heartbeat cleared active cleanup fact: %#v", fact)
	}
}

func TestWorkspaceCleanupAcknowledgementReturnsWorkerFactToIdle(t *testing.T) {
	ctx := context.Background()
	db, service, _ := setupSQLiteRecoveryService(t)
	fixture, _, _ := seedWorkspaceCleanupFixture(t, db, false)
	base := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return base }

	created, err := service.ReconcileWorkspaceCleanup(ctx, base, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands created = %d, want 1", created)
	}

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "worker-fact-cleanup-ack", WorkerModeGeneralPool,
	)

	claimAt := base.Add(5 * time.Second)
	service.now = func() time.Time { return claimAt }
	claimed, err := service.ClaimWorkspaceCleanup(ctx, worker, WorkspaceCleanupClaimInput{}, "worker-fact-cleanup-ack-claim-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Value.Cleanup == nil {
		t.Fatal("cleanup claim is nil")
	}

	leaseInput := WorkspaceCleanupLeaseInput{
		DispatchGeneration: claimed.Value.Cleanup.DispatchGeneration,
		LeaseToken:         claimed.Value.Cleanup.Lease.LeaseToken,
	}
	startAt := claimAt.Add(time.Second)
	service.now = func() time.Time { return startAt }
	if _, err := service.StartWorkspaceCleanup(ctx, worker, claimed.Value.Cleanup.CleanupID, leaseInput, "worker-fact-cleanup-start-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	ackAt := claimAt.Add(3 * time.Second)
	service.now = func() time.Time { return ackAt }
	if _, err := service.AcknowledgeWorkspaceCleanup(ctx, worker, claimed.Value.Cleanup.CleanupID, leaseInput, "worker-fact-cleanup-ack-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	fact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.CurrentState != workerFactStateIdle || fact.ClaimCount != 1 || fact.AccumulatedActiveSeconds != 3 {
		t.Fatalf("acknowledged cleanup Worker fact = %#v", fact)
	}
}

func TestWorkspaceCleanupReleaseFailAndExpiryReturnWorkerFactToIdle(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		action func(t *testing.T, service *Service, worker persistence.WorkerInstance, claim WorkspaceCleanupClaim)
	}{
		{
			name: "release",
			action: func(t *testing.T, service *Service, worker persistence.WorkerInstance, claim WorkspaceCleanupClaim) {
				leaseInput := WorkspaceCleanupLeaseInput{
					DispatchGeneration: claim.DispatchGeneration,
					LeaseToken:         claim.Lease.LeaseToken,
				}
				if _, err := service.ReleaseWorkspaceCleanup(context.Background(), worker, claim.CleanupID, leaseInput, "worker-fact-cleanup-release-"+uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fail",
			action: func(t *testing.T, service *Service, worker persistence.WorkerInstance, claim WorkspaceCleanupClaim) {
				input := WorkspaceCleanupFailedInput{
					WorkspaceCleanupLeaseInput: WorkspaceCleanupLeaseInput{
						DispatchGeneration: claim.DispatchGeneration,
						LeaseToken:         claim.Lease.LeaseToken,
					},
					ErrorCode:    "cleanup_failed",
					ErrorMessage: "cleanup failed",
					Retryable:    false,
				}
				if _, err := service.FailWorkspaceCleanup(context.Background(), worker, claim.CleanupID, input, "worker-fact-cleanup-fail-"+uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "expiry",
			action: func(t *testing.T, service *Service, _ persistence.WorkerInstance, _ WorkspaceCleanupClaim) {
				recovered, err := service.RecoverExpiredWorkspaceCleanupLeases(context.Background(), service.now(), 10)
				if err != nil {
					t.Fatal(err)
				}
				if recovered != 1 {
					t.Fatalf("recovered cleanup leases = %d, want 1", recovered)
				}
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			db, service, _ := setupSQLiteRecoveryService(t)
			fixture, _, _ := seedWorkspaceCleanupFixture(t, db, false)
			base := time.Now().UTC().Truncate(time.Microsecond)
			service.now = func() time.Time { return base }
			if scenario.name == "expiry" {
				service.leaseTTL = 2 * time.Second
			}

			created, err := service.ReconcileWorkspaceCleanup(ctx, base, 10)
			if err != nil {
				t.Fatal(err)
			}
			if created != 1 {
				t.Fatalf("cleanup commands created = %d, want 1", created)
			}

			_, worker := registerWorkerModeTestWorker(
				t, service, fixture.TargetID, fixture.TargetKind, "worker-fact-cleanup-"+scenario.name, WorkerModeGeneralPool,
			)

			claimAt := base.Add(5 * time.Second)
			service.now = func() time.Time { return claimAt }
			claimed, err := service.ClaimWorkspaceCleanup(ctx, worker, WorkspaceCleanupClaimInput{}, "worker-fact-cleanup-"+scenario.name+"-claim-"+uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			if claimed.Value.Cleanup == nil {
				t.Fatal("cleanup claim is nil")
			}

			actionAt := claimAt.Add(time.Second)
			expectedActiveSeconds := int64(1)
			if scenario.name == "expiry" {
				actionAt = claimAt.Add(3 * time.Second)
				expectedActiveSeconds = 3
			}
			service.now = func() time.Time { return actionAt }
			scenario.action(t, service, worker, *claimed.Value.Cleanup)

			fact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
			if fact.CurrentState != workerFactStateIdle || fact.ClaimCount != 1 || fact.AccumulatedActiveSeconds != expectedActiveSeconds {
				t.Fatalf("%s cleanup Worker fact = %#v", scenario.name, fact)
			}
		})
	}
}

func workerFactRegistrationInput(
	t *testing.T,
	targetID uuid.UUID,
	targetKind, podName, instanceUID string,
) RegisterWorkerInput {
	t.Helper()
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	kind, err := platform.ParseExecutionTargetKind(targetKind)
	if err != nil {
		t.Fatal(err)
	}
	signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
		ExecutionTargetID: targetID, TargetKind: kind, InstanceUID: instanceUID,
		ClusterID: "test-cluster", Namespace: "default", PodName: podName,
	})
	return RegisterWorkerInput{
		ExecutionTargetID: targetID, TargetKind: targetKind, WorkerMode: WorkerModeGeneralPool,
		InstanceUID: instanceUID, ClusterID: "test-cluster", Namespace: "default", PodName: podName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion, Capabilities: capabilities,
		LeaseSupported: true, FencingSupported: true,
	}
}

func loadWorkerIncarnationFactForTest(
	t *testing.T,
	db *gorm.DB,
	workerID uuid.UUID,
	incarnation int64,
) persistence.WorkerIncarnationFact {
	t.Helper()
	var fact persistence.WorkerIncarnationFact
	if err := db.Where("worker_id = ? AND worker_incarnation = ?", workerID, incarnation).Take(&fact).Error; err != nil {
		t.Fatal(err)
	}
	return fact
}

func loadWorkerClaimFactsForTest(
	t *testing.T,
	db *gorm.DB,
	workerID uuid.UUID,
	incarnation int64,
) []persistence.WorkerClaimFact {
	t.Helper()
	claims := make([]persistence.WorkerClaimFact, 0)
	if err := db.
		Where("worker_id = ? AND worker_incarnation = ?", workerID, incarnation).
		Order("claimed_at ASC, id ASC").
		Find(&claims).Error; err != nil {
		t.Fatal(err)
	}
	return claims
}
