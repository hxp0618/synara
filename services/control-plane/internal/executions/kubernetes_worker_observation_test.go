package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestObserveKubernetesWorkerPodDrainsDeleteRequestAndTerminalizesKubeletPhase(t *testing.T) {
	db, service, fixture := setupSQLiteRecoveryService(t)
	ctx := context.Background()
	registeredAt := time.Date(2026, time.July, 25, 6, 0, 0, 0, time.UTC)
	targetID := uuid.New()
	if err := db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &fixture.TenantID, Kind: "kubernetes",
		Name: "worker-observation-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: registeredAt, UpdatedAt: registeredAt,
	}).Error; err != nil {
		t.Fatal(err)
	}

	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(),
		ExecutionTargetID: targetID, TargetKind: "kubernetes", WorkerMode: WorkerModeGeneralPool,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
		ClusterID:             "kubernetes", Namespace: "default", PodName: "observed-worker",
		Version: "test", ProtocolVersion: WorkerProtocolVersion, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("hash"),
		Status: "online", AdministrativeStatus: "active",
		RegisteredAt: registeredAt, LastHeartbeatAt: registeredAt,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	fact := persistence.WorkerIncarnationFact{
		WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, TenantID: &fixture.TenantID,
		ExecutionTargetID: targetID, TargetKind: worker.TargetKind, WorkerMode: worker.WorkerMode,
		ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
		InstanceUID: worker.InstanceUID, RegisteredAt: registeredAt,
		CurrentState: workerFactStateIdle, StateChangedAt: registeredAt,
		CreatedAt: registeredAt, UpdatedAt: registeredAt,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}

	deleteRequestedAt := registeredAt.Add(5 * time.Second)
	if err := service.ObserveKubernetesWorkerPod(ctx, executiontargets.KubernetesWorkerPodObservation{
		ExecutionTargetID: targetID, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Running", Reason: "delete-requested:scale-down",
		ObservedAt: deleteRequestedAt,
	}); err != nil {
		t.Fatal(err)
	}
	var drainingWorker persistence.WorkerInstance
	if err := db.Where("id = ?", worker.ID).Take(&drainingWorker).Error; err != nil {
		t.Fatal(err)
	}
	drainingFact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if drainingWorker.Status != "draining" || drainingWorker.DrainingAt == nil ||
		!drainingWorker.DrainingAt.Equal(deleteRequestedAt) ||
		drainingFact.CurrentState != workerFactStateDraining || drainingFact.TerminatedAt != nil {
		t.Fatalf("delete-requested Worker lifecycle = worker %#v, fact %#v", drainingWorker, drainingFact)
	}

	terminatedAt := registeredAt.Add(8 * time.Second)
	terminalObservation := executiontargets.KubernetesWorkerPodObservation{
		ExecutionTargetID: targetID, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Succeeded", Reason: "terminal-observation",
		ObservedAt: terminatedAt,
	}
	if err := service.ObserveKubernetesWorkerPod(ctx, terminalObservation); err != nil {
		t.Fatal(err)
	}
	var terminatedWorker persistence.WorkerInstance
	if err := db.Where("id = ?", worker.ID).Take(&terminatedWorker).Error; err != nil {
		t.Fatal(err)
	}
	terminatedFact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if terminatedWorker.Status != "terminated" || terminatedFact.CurrentState != workerFactStateTerminated ||
		terminatedFact.TerminatedAt == nil || !terminatedFact.TerminatedAt.Equal(terminatedAt) ||
		terminatedFact.TerminalReason == nil || *terminatedFact.TerminalReason != "kubernetes-pod-succeeded" {
		t.Fatalf("terminal Worker lifecycle = worker %#v, fact %#v", terminatedWorker, terminatedFact)
	}
	// A repeated terminal observation is idempotent even if Kubernetes later
	// reports a different terminal phase for an already closed incarnation.
	terminalObservation.Phase = "Failed"
	terminalObservation.ObservedAt = terminatedAt.Add(time.Second)
	if err := service.ObserveKubernetesWorkerPod(ctx, terminalObservation); err != nil {
		t.Fatal(err)
	}
	if err := persistence.InTransaction(ctx, db, func(tx *gorm.DB) error {
		return terminalizeWorkerIncarnationFromLeaseLocked(ctx, tx, persistence.WorkerLease{
			WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, WorkerInstanceUID: worker.InstanceUID,
		}, terminatedAt.Add(2*time.Second), "kubernetes-suspend-terminal")
	}); err != nil {
		t.Fatal(err)
	}
	terminalizedAgain := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if terminalizedAgain.CurrentState != workerFactStateTerminated ||
		terminalizedAgain.TerminatedAt == nil || !terminalizedAgain.TerminatedAt.Equal(terminatedAt) ||
		terminalizedAgain.TerminalReason == nil || *terminalizedAgain.TerminalReason != "kubernetes-pod-succeeded" {
		t.Fatalf("reterminalized fact = %#v", terminalizedAgain)
	}
}

func TestObserveKubernetesWorkerPodConfirmedMissingClosesExactIncarnationOnly(t *testing.T) {
	db, service, fixture := setupSQLiteRecoveryService(t)
	now := time.Date(2026, time.July, 25, 7, 0, 0, 0, time.UTC)
	targetID := uuid.New()
	if err := db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &fixture.TenantID, Kind: "kubernetes",
		Name: "worker-missing-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(),
		ExecutionTargetID: targetID, TargetKind: "kubernetes", WorkerMode: WorkerModeGeneralPool,
		RegistrationTrustMode: WorkerRegistrationTrustKubernetesPodBoundV1,
		ClusterID:             "kubernetes", Namespace: "default", PodName: "missing-worker",
		Version: "test", ProtocolVersion: WorkerProtocolVersion, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("hash"),
		Status: "draining", AdministrativeStatus: "active", RegisteredAt: now,
		LastHeartbeatAt: now, DrainingAt: &now,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.WorkerIncarnationFact{
		WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, TenantID: &fixture.TenantID,
		ExecutionTargetID: targetID, TargetKind: worker.TargetKind, WorkerMode: worker.WorkerMode,
		ClusterID: worker.ClusterID, Namespace: worker.Namespace, PodName: worker.PodName,
		InstanceUID: worker.InstanceUID, RegisteredAt: now,
		CurrentState: workerFactStateDraining, StateChangedAt: now,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	missingAt := now.Add(4 * time.Second)
	if err := service.ObserveKubernetesWorkerPod(context.Background(), executiontargets.KubernetesWorkerPodObservation{
		ExecutionTargetID: targetID, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: worker.InstanceUID, Phase: "Missing", Reason: "confirmed-missing:delete-completed",
		ObservedAt: missingAt,
	}); err != nil {
		t.Fatal(err)
	}
	fact := loadWorkerIncarnationFactForTest(t, db, worker.ID, worker.Incarnation)
	if fact.CurrentState != workerFactStateTerminated || fact.TerminalReason == nil ||
		*fact.TerminalReason != "kubernetes-pod-confirmed-missing" {
		t.Fatalf("confirmed-missing fact = %#v", fact)
	}

	// A different physical UID must never close this incarnation.
	if err := service.ObserveKubernetesWorkerPod(context.Background(), executiontargets.KubernetesWorkerPodObservation{
		ExecutionTargetID: targetID, Namespace: worker.Namespace, PodName: worker.PodName,
		PodUID: uuid.NewString(), Phase: "Failed", Reason: "terminal-observation",
		ObservedAt: missingAt.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
}
