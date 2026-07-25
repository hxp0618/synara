package executions

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestRegisterWorkerPersistsNormalizedWorkerMode(t *testing.T) {
	tests := []struct {
		name            string
		inputWorkerMode string
		wantWorkerMode  string
	}{
		{
			name:            "defaults empty worker mode to general pool",
			inputWorkerMode: "",
			wantWorkerMode:  WorkerModeGeneralPool,
		},
		{
			name:            "persists explicit warm pool worker mode",
			inputWorkerMode: WorkerModeWarmPool,
			wantWorkerMode:  WorkerModeWarmPool,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, service, fixture := setupSQLiteRecoveryService(t)

			registered, worker := registerWorkerModeTestWorker(
				t, service, fixture.TargetID, fixture.TargetKind, test.name, test.inputWorkerMode,
			)
			if registered.Worker.WorkerMode != test.wantWorkerMode {
				t.Fatalf("registered workerMode = %q, want %q", registered.Worker.WorkerMode, test.wantWorkerMode)
			}
			if worker.WorkerMode != test.wantWorkerMode {
				t.Fatalf("persisted workerMode = %q, want %q", worker.WorkerMode, test.wantWorkerMode)
			}
		})
	}
}

func TestExecutionPinnedWorkerRequiresExplicitExecutionClaim(t *testing.T) {
	_, service, fixture := setupSQLiteRecoveryService(t)
	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "execution-pinned-claim", WorkerModeExecutionPinned,
	)

	_, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
	}, "worker-mode-claim-"+uuid.NewString())
	assertProblemCode(t, err, "worker_mode_execution_id_required")
}

func TestWorkspaceCleanupClaimRejectsNonGeneralPoolWorker(t *testing.T) {
	db, service, _ := setupSQLiteRecoveryService(t)
	fixture, _, _ := seedWorkspaceCleanupFixture(t, db, false)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }

	created, err := service.ReconcileWorkspaceCleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("cleanup commands created = %d, want 1", created)
	}

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "cleanup-warm-pool", WorkerModeWarmPool,
	)
	_, err = service.ClaimWorkspaceCleanup(
		context.Background(), worker, WorkspaceCleanupClaimInput{}, "worker-mode-cleanup-"+uuid.NewString(),
	)
	assertProblemCode(t, err, "worker_mode_cleanup_unsupported")
}

func TestWarmPoolWorkerCannotClaimAfterPoolStopsBeingActive(t *testing.T) {
	db, service, fixture := setupSQLiteRecoveryService(t)
	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "inactive-warm-pool", WorkerModeWarmPool,
	)
	if worker.WorkerPoolID == nil || worker.WorkerPoolVersion == nil || worker.CapacityClass == nil {
		t.Fatalf("warm Worker identity = %#v", worker)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{
			"worker_pool_id": *worker.WorkerPoolID, "worker_pool_version": *worker.WorkerPoolVersion,
			"capacity_class": *worker.CapacityClass, "placement_policy_version": int64(1),
		}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Model(&persistence.WorkerPool{}).
		Where("id = ? AND version = ?", *worker.WorkerPoolID, *worker.WorkerPoolVersion).
		Updates(map[string]any{
			"status":     placement.PoolStatusDraining,
			"version":    *worker.WorkerPoolVersion + 1,
			"updated_at": now,
		}).Error; err != nil {
		t.Fatal(err)
	}

	claim, err := service.Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
	}, "inactive-warm-pool-claim-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Execution != nil || claim.Value.Lease != nil {
		t.Fatalf("inactive warm Pool returned a claim: %#v", claim.Value)
	}
	var leases int64
	if err := db.Model(&persistence.WorkerLease{}).Where("worker_id = ?", worker.ID).Count(&leases).Error; err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("inactive warm Pool created %d leases", leases)
	}
}

func registerWorkerModeTestWorker(
	t *testing.T,
	service *Service,
	targetID uuid.UUID,
	targetKind string,
	podName string,
	workerMode string,
) (RegisteredWorker, persistence.WorkerInstance) {
	t.Helper()
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)

	parsedTargetKind, err := platform.ParseExecutionTargetKind(targetKind)
	if err != nil {
		t.Fatal(err)
	}
	instanceUID := uuid.NewString()
	fullPodName := podName + "-" + uuid.NewString()
	var assignedExecutionID *uuid.UUID
	var workerPoolID *uuid.UUID
	var workerPoolVersion *int64
	var capacityClass *string
	if runtimeCapability, ok := capabilities["workerRuntime"].(map[string]any); ok && runtimeCapability["processContainment"] != nil {
		signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
			ExecutionTargetID: targetID,
			TargetKind:        parsedTargetKind,
			InstanceUID:       instanceUID,
			ClusterID:         "test-cluster",
			Namespace:         "default",
			PodName:           fullPodName,
		})
	}
	switch workerMode {
	case WorkerModeExecutionPinned:
		var execution persistence.AgentExecution
		if err := service.db.WithContext(context.Background()).
			Where("execution_target_id = ? AND target_kind = ?", targetID, targetKind).
			Order("queued_at, id").
			Take(&execution).Error; err != nil {
			t.Fatal(err)
		}
		assignedExecutionID = &execution.ID
	case WorkerModeWarmPool:
		now := time.Now().UTC().Truncate(time.Microsecond)
		var target persistence.ExecutionTarget
		if err := service.db.WithContext(context.Background()).
			Where("id = ?", targetID).
			Take(&target).Error; err != nil {
			t.Fatal(err)
		}
		poolID := uuid.New()
		version := int64(1)
		class := placement.CapacityClassInteractive
		pool := persistence.WorkerPool{
			ID: poolID, TenantID: target.TenantID, ExecutionTargetID: targetID,
			Name: "worker-mode-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8],
			Mode: placement.PoolModeWarm, CapacityClass: class,
			DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{},
			Status: placement.PoolStatusActive, Version: version, CreatedAt: now, UpdatedAt: now,
		}
		if err := service.db.WithContext(context.Background()).Create(&pool).Error; err != nil {
			t.Fatal(err)
		}
		workerPoolID = &poolID
		workerPoolVersion = &version
		capacityClass = &class
	}

	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID:   targetID,
		TargetKind:          targetKind,
		WorkerMode:          workerMode,
		AssignedExecutionID: assignedExecutionID,
		WorkerPoolID:        workerPoolID,
		WorkerPoolVersion:   workerPoolVersion,
		CapacityClass:       capacityClass,
		InstanceUID:         instanceUID,
		ClusterID:           "test-cluster",
		Namespace:           "default",
		PodName:             fullPodName,
		Version:             "worker-test",
		ProtocolVersion:     WorkerProtocolVersion,
		Capabilities:        capabilities,
		LeaseSupported:      true,
		FencingSupported:    true,
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
