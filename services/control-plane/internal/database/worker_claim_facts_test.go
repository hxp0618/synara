package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

type workerClaimFactFixture struct {
	base             time.Time
	tenantID         uuid.UUID
	organizationID   uuid.UUID
	target           persistence.ExecutionTarget
	worker           persistence.WorkerInstance
	workerFact       persistence.WorkerIncarnationFact
	execution        persistence.AgentExecution
	executionLease   persistence.WorkerLease
	cleanupCommand   persistence.WorkspaceCleanupCommand
	executionClaimAt time.Time
	cleanupClaimAt   time.Time
}

func seedWorkerClaimFactFixture(t *testing.T, db *gorm.DB) workerClaimFactFixture {
	t.Helper()
	ctx := context.Background()
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Truncate(time.Microsecond)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "worker-claim-target-" + uuid.NewString()[:8], Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatalf("create worker claim target: %v", err)
	}

	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(),
		ExecutionTargetID: target.ID, TargetKind: target.Kind, WorkerMode: "general-pool",
		RegistrationTrustMode: "kubernetes-pod-bound-v1",
		ClusterID:             "kubernetes", Namespace: "default", PodName: "worker-claim-pod",
		Version: "test", ProtocolVersion: 2, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("hash"),
		Status: "online", AdministrativeStatus: "active", RegisteredAt: base, LastHeartbeatAt: base,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatalf("create worker claim worker: %v", err)
	}

	workerFact := persistence.WorkerIncarnationFact{
		WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, TenantID: &domain.TenantID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, WorkerMode: worker.WorkerMode,
		ClusterID: worker.ClusterID, Region: "us-east-1", Namespace: worker.Namespace, PodName: worker.PodName,
		InstanceUID: worker.InstanceUID, RegisteredAt: base, CurrentState: "active", StateChangedAt: base,
		CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&workerFact).Error; err != nil {
		t.Fatalf("create worker claim fact worker snapshot: %v", err)
	}

	project := persistence.Project{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "worker-claim-project", DefaultBranch: "main", Visibility: "private",
		CreatedBy: domain.UserID, CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&project).Error; err != nil {
		t.Fatalf("create worker claim project: %v", err)
	}

	session := persistence.AgentSession{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: project.ID, CreatedBy: domain.UserID, Title: "worker claim session",
		Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: target.ID,
		CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatalf("create worker claim session: %v", err)
	}

	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: session.ID, CreatedBy: domain.UserID,
		Status: "running", InputText: "Continue", StartedAt: &base, CreatedAt: base,
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("create worker claim turn: %v", err)
	}

	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: session.ID, TurnID: turn.ID,
		Attempt: 1, Status: "leased", ExecutionTargetID: target.ID, TargetKind: target.Kind,
		WorkerID: &worker.ID, ProviderResumeStrategySnapshot: "authoritative-history",
		Generation: 1, RequestedBy: domain.UserID, QueuedAt: base, StartedAt: &base,
	}
	if err := db.Create(&execution).Error; err != nil {
		t.Fatalf("create worker claim execution: %v", err)
	}
	leaseAcquiredAt := base.Add(15 * time.Second)
	executionLease := persistence.WorkerLease{
		ExecutionID:       execution.ID,
		TenantID:          domain.TenantID,
		WorkerID:          worker.ID,
		WorkerIncarnation: worker.Incarnation,
		WorkerInstanceUID: worker.InstanceUID,
		Generation:        execution.Generation,
		LeaseTokenHash:    []byte("worker-claim-lease-hash"),
		AcquiredAt:        leaseAcquiredAt,
		HeartbeatAt:       leaseAcquiredAt,
		ExpiresAt:         leaseAcquiredAt.Add(10 * time.Minute),
	}
	if err := db.Create(&executionLease).Error; err != nil {
		t.Fatalf("create worker claim execution lease: %v", err)
	}

	workspace := persistence.RemoteWorkspace{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: project.ID, SessionID: session.ID, ExecutionTargetID: target.ID,
		WorkspaceMode: "clone", State: "cleanup-pending", DefaultBranch: "main",
		CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&workspace).Error; err != nil {
		t.Fatalf("create worker claim workspace: %v", err)
	}

	cleanupRequestedAt := base.Add(2 * time.Minute)
	materialization := persistence.WorkspaceMaterialization{
		ID: uuid.New(), TenantID: domain.TenantID, WorkspaceID: workspace.ID,
		OrganizationID: domain.OrganizationID, ProjectID: project.ID, SessionID: session.ID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, StorageScope: "target", LayoutVersion: 3,
		IncarnationID: uuid.New(), State: "cleanup-pending", CleanupReason: stringPointer("retention-expired"),
		CleanupRequestedAt: &cleanupRequestedAt, CreatedAt: base, UpdatedAt: cleanupRequestedAt,
	}
	if err := db.Create(&materialization).Error; err != nil {
		t.Fatalf("create worker claim materialization: %v", err)
	}
	if err := db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", domain.TenantID, workspace.ID).
		Update("current_materialization_id", materialization.ID).Error; err != nil {
		t.Fatalf("bind worker claim materialization: %v", err)
	}

	leasedAt := base.Add(3 * time.Minute)
	leaseExpiresAt := leasedAt.Add(2 * time.Minute)
	cleanupCommand := persistence.WorkspaceCleanupCommand{
		ID: uuid.New(), TenantID: domain.TenantID, MaterializationID: materialization.ID,
		MaterializationIncarnationID: materialization.IncarnationID, WorkspaceID: workspace.ID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, StorageScope: materialization.StorageScope,
		LayoutVersion: materialization.LayoutVersion, Reason: "retention-expired", Status: "leased",
		LeaseTokenHash: []byte("cleanup-hash"), DispatchGeneration: 1,
		DeliveryWorkerID: &worker.ID, DeliveryWorkerIncarnation: &worker.Incarnation,
		DeliveryAttempts: 1, DeliveryAvailableAt: cleanupRequestedAt, LeaseExpiresAt: &leaseExpiresAt,
		RequestedAt: cleanupRequestedAt, LeasedAt: &leasedAt, CreatedAt: cleanupRequestedAt, UpdatedAt: leasedAt,
	}
	if err := db.Create(&cleanupCommand).Error; err != nil {
		t.Fatalf("create worker claim cleanup command: %v", err)
	}

	return workerClaimFactFixture{
		base:             base,
		tenantID:         domain.TenantID,
		organizationID:   domain.OrganizationID,
		target:           target,
		worker:           worker,
		workerFact:       workerFact,
		execution:        execution,
		executionLease:   executionLease,
		cleanupCommand:   cleanupCommand,
		executionClaimAt: base.Add(30 * time.Second),
		cleanupClaimAt:   leasedAt,
	}
}

func validExecutionWorkerClaimFact(fixture workerClaimFactFixture, requestID string) persistence.WorkerClaimFact {
	generation := fixture.execution.Generation
	return persistence.WorkerClaimFact{
		ID:                  uuid.New(),
		WorkerID:            fixture.worker.ID,
		WorkerIncarnation:   fixture.worker.Incarnation,
		TenantID:            fixture.tenantID,
		ExecutionTargetID:   fixture.target.ID,
		TargetKind:          fixture.target.Kind,
		ClaimKind:           "execution",
		RequestID:           requestID,
		ClaimedAt:           fixture.executionClaimAt,
		ExecutionID:         &fixture.execution.ID,
		ExecutionGeneration: &generation,
		CreatedAt:           fixture.executionClaimAt,
	}
}

func validCleanupWorkerClaimFact(fixture workerClaimFactFixture, requestID string) persistence.WorkerClaimFact {
	dispatchGeneration := fixture.cleanupCommand.DispatchGeneration
	return persistence.WorkerClaimFact{
		ID:                        uuid.New(),
		WorkerID:                  fixture.worker.ID,
		WorkerIncarnation:         fixture.worker.Incarnation,
		TenantID:                  fixture.tenantID,
		ExecutionTargetID:         fixture.target.ID,
		TargetKind:                fixture.target.Kind,
		ClaimKind:                 "workspace-cleanup",
		RequestID:                 requestID,
		ClaimedAt:                 fixture.cleanupClaimAt,
		CleanupCommandID:          &fixture.cleanupCommand.ID,
		CleanupDispatchGeneration: &dispatchGeneration,
		CreatedAt:                 fixture.cleanupClaimAt,
	}
}

func validWorkerClaimReleaseFact(
	claim persistence.WorkerClaimFact,
	reason string,
	releasedAt time.Time,
) persistence.WorkerClaimReleaseFact {
	return persistence.WorkerClaimReleaseFact{
		ClaimFactID: claim.ID, ReleasedAt: releasedAt, RecordedAt: releasedAt.Add(time.Second),
		ReleaseReason: reason, AuthorityKind: "control-plane", Metadata: map[string]any{},
	}
}

func invalidReincarnatedExecutionWorkerClaimFact(
	t *testing.T,
	db *gorm.DB,
	fixture workerClaimFactFixture,
	requestID string,
) persistence.WorkerClaimFact {
	t.Helper()

	nextIncarnation := fixture.worker.Incarnation + 1
	reRegisteredAt := fixture.cleanupClaimAt.Add(30 * time.Second)
	newInstanceUID := uuid.NewString()
	if err := db.Model(&persistence.WorkerInstance{}).
		Where("id = ?", fixture.worker.ID).
		Updates(map[string]any{
			"incarnation":       nextIncarnation,
			"instance_uid":      newInstanceUID,
			"registered_at":     reRegisteredAt,
			"last_heartbeat_at": reRegisteredAt,
		}).Error; err != nil {
		t.Fatalf("re-register worker claim fixture: %v", err)
	}

	nextWorkerFact := persistence.WorkerIncarnationFact{
		WorkerID:          fixture.worker.ID,
		WorkerIncarnation: nextIncarnation,
		TenantID:          &fixture.tenantID,
		ExecutionTargetID: fixture.target.ID,
		TargetKind:        fixture.target.Kind,
		WorkerMode:        fixture.workerFact.WorkerMode,
		ClusterID:         fixture.workerFact.ClusterID,
		Region:            fixture.workerFact.Region,
		Namespace:         fixture.workerFact.Namespace,
		PodName:           fixture.workerFact.PodName,
		InstanceUID:       newInstanceUID,
		RegisteredAt:      reRegisteredAt,
		CurrentState:      "active",
		StateChangedAt:    reRegisteredAt,
		CreatedAt:         reRegisteredAt,
		UpdatedAt:         reRegisteredAt,
	}
	if err := db.Create(&nextWorkerFact).Error; err != nil {
		t.Fatalf("create reincarnated worker claim fact fixture: %v", err)
	}

	generation := fixture.execution.Generation
	claimedAt := reRegisteredAt.Add(time.Second)
	return persistence.WorkerClaimFact{
		ID:                  uuid.New(),
		WorkerID:            fixture.worker.ID,
		WorkerIncarnation:   nextIncarnation,
		TenantID:            fixture.tenantID,
		ExecutionTargetID:   fixture.target.ID,
		TargetKind:          fixture.target.Kind,
		ClaimKind:           "execution",
		RequestID:           requestID,
		ClaimedAt:           claimedAt,
		ExecutionID:         &fixture.execution.ID,
		ExecutionGeneration: &generation,
		CreatedAt:           claimedAt,
	}
}

func assertWorkerClaimFactDuplicate(t *testing.T, err error) {
	t.Helper()
	if err == nil || !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("worker claim ledger accepted duplicate source identity or returned wrong error: %v", err)
	}
}

func assertWorkerClaimFactParentRetained(t *testing.T, err error) {
	t.Helper()
	if err == nil || (!errors.Is(err, gorm.ErrForeignKeyViolated) &&
		!strings.Contains(err.Error(), "Worker claim fact parent is retained")) {
		t.Fatalf("worker claim ledger allowed a referenced parent to be deleted or returned wrong error: %v", err)
	}
}
