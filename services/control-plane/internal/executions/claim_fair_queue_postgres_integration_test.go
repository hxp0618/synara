package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestClaimFairShareOrderPostgresPrefersTenantWithFewerActiveServiceUnits(t *testing.T) {
	db := integrationDB(t)
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer func() { _ = tx.Rollback().Error }()

	now := time.Now().UTC().Truncate(time.Microsecond)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "docker", Name: "fair-share-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	tenantA := seedClaimFairShareTenant(t, tx, target, now, true)
	tenantB := seedClaimFairShareTenant(t, tx, target, now.Add(time.Second), false)
	if err := tx.Exec("SET CONSTRAINTS ALL IMMEDIATE").Error; err != nil {
		t.Fatalf("fair-share PostgreSQL fixture violates production constraints: %v", err)
	}

	var selected persistence.AgentExecution
	query := persistence.WithLocking(tx.WithContext(context.Background()), "UPDATE", "SKIP LOCKED").
		Where("agent_executions.execution_target_id = ?", target.ID).
		Where("agent_executions.target_kind = ?", target.Kind).
		Where("agent_executions.status IN ?", []string{"queued", "recovering"})
	if err := applyClaimFairShareOrder(tx, query, false, time.Now().UTC()).Take(&selected).Error; err != nil {
		t.Fatal(err)
	}
	if selected.ID != tenantB.QueuedExecutionID {
		t.Fatalf(
			"PostgreSQL fair-share selected %s, want idle Tenant B %s (Tenant A queued %s)",
			selected.ID,
			tenantB.QueuedExecutionID,
			tenantA.QueuedExecutionID,
		)
	}
}

type claimFairShareTenantFixture struct {
	QueuedExecutionID uuid.UUID
}

func seedClaimFairShareTenant(
	t *testing.T,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	now time.Time,
	withActiveServiceUnit bool,
) claimFairShareTenantFixture {
	t.Helper()
	userID := uuid.New()
	tenantID := uuid.New()
	organizationID := uuid.New()
	projectID := uuid.New()
	slugSuffix := uuid.NewString()[:8]
	models := []any{
		&persistence.User{
			ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Fair share",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Tenant{
			ID: tenantID, Slug: "fair-share-" + slugSuffix, Name: "Fair share",
			Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{},
			CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Organization{
			ID: organizationID, TenantID: tenantID, Slug: "root-" + slugSuffix,
			Name: "Fair share", Kind: "root", Status: "active", Settings: map[string]any{},
			CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Project{
			ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Fair share",
			DefaultBranch: "main", Visibility: "organization", CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, model := range models {
		if err := tx.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	queuedExecution := createClaimFairShareExecution(
		t,
		tx,
		tenantID,
		organizationID,
		projectID,
		userID,
		target,
		now,
		"queued",
		nil,
	)
	if withActiveServiceUnit {
		worker := persistence.WorkerInstance{
			ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(),
			ExecutionTargetID: target.ID, TargetKind: target.Kind, WorkerMode: WorkerModeGeneralPool,
			RegistrationTrustMode: "shared-token", ClusterID: "fair-share", Namespace: "default",
			PodName: "fair-share-" + uuid.NewString(), Version: "test", ProtocolVersion: WorkerProtocolVersion,
			Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
			AuthTokenHash: []byte(uuid.NewString()), Status: "online", AdministrativeStatus: "active",
			RegisteredAt: now, LastHeartbeatAt: now,
		}
		if err := tx.Create(&worker).Error; err != nil {
			t.Fatal(err)
		}
		activeExecution := createClaimFairShareExecution(
			t,
			tx,
			tenantID,
			organizationID,
			projectID,
			userID,
			target,
			now.Add(-time.Minute),
			"leased",
			&worker.ID,
		)
		lease := persistence.WorkerLease{
			ExecutionID: activeExecution.ID, TenantID: tenantID, WorkerID: worker.ID,
			WorkerIncarnation: worker.Incarnation, WorkerInstanceUID: worker.InstanceUID,
			Generation: 1, LeaseTokenHash: []byte(uuid.NewString()),
			AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := tx.Create(&lease).Error; err != nil {
			t.Fatal(err)
		}
	}
	return claimFairShareTenantFixture{QueuedExecutionID: queuedExecution.ID}
}

func createClaimFairShareExecution(
	t *testing.T,
	tx *gorm.DB,
	tenantID uuid.UUID,
	organizationID uuid.UUID,
	projectID uuid.UUID,
	userID uuid.UUID,
	target persistence.ExecutionTarget,
	queuedAt time.Time,
	status string,
	workerID *uuid.UUID,
) persistence.AgentExecution {
	t.Helper()
	session := persistence.AgentSession{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID,
		CreatedBy: userID, Title: "Fair share", Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: target.ID, RequestedExecutionTargetID: target.ID,
		ResourceState: "idle", MeaningfulActivityAt: queuedAt, CreatedAt: queuedAt, UpdatedAt: queuedAt,
	}
	turnStatus := "queued"
	if status == "leased" {
		turnStatus = "running"
	}
	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: tenantID, SessionID: session.ID, CreatedBy: userID,
		Status: turnStatus, InputText: "Fair share", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: queuedAt,
	}
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: tenantID, SessionID: session.ID, TurnID: turn.ID,
		Attempt: 1, Status: status, ExecutionTargetID: target.ID, TargetKind: target.Kind,
		WorkerID: workerID, Generation: 0, RequestedBy: userID, QueuedAt: queuedAt,
	}
	if status == "leased" {
		execution.Generation = 1
	}
	for _, model := range []any{&session, &turn, &execution} {
		if err := tx.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	return execution
}
