package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresWorkerTenantIsolationRejectsPolicyAndBindingMutation(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantA, tenantB := uuid.New(), uuid.New(), uuid.New()
	if err := db.Create(&persistence.User{
		ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Tenant isolation PG",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for index, tenantID := range []uuid.UUID{tenantA, tenantB} {
			createdAt := now.Add(time.Duration(index) * time.Second)
			if err := tx.Create(&persistence.Tenant{
				ID: tenantID, Slug: "tenant-isolation-pg-" + uuid.NewString(), Name: "Tenant isolation PG",
				Status: "active", PlanCode: "test", Region: "local", Settings: map[string]any{},
				CreatedBy: userID, CreatedAt: createdAt, UpdatedAt: createdAt,
			}).Error; err != nil {
				return err
			}
			if err := tx.Create(&persistence.TenantMembership{
				TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
				JoinedAt: &createdAt, CreatedAt: createdAt, UpdatedAt: createdAt,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sharedTarget := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "docker", Name: "tenant-isolation-shared-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	tenantTarget := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &tenantA, Kind: "docker", Name: "tenant-isolation-tenant-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&[]persistence.ExecutionTarget{sharedTarget, tenantTarget}).Error; err != nil {
		t.Fatal(err)
	}
	pinned := persistence.WorkerPool{
		ID: uuid.New(), ExecutionTargetID: sharedTarget.ID, Name: "pinned", Mode: placement.PoolModeResident,
		CapacityClass: placement.CapacityClassStandard, TenantIsolation: placement.TenantIsolationPinned,
		DesiredIdleUnits: 0, MinIdleUnits: 0, MaxActiveUnits: 2, SchedulingTemplate: map[string]any{},
		Status: placement.PoolStatusActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pinned).Error; err != nil {
		t.Fatal(err)
	}
	assertWorkerTenantIsolationPostgresConstraint(t, db.Model(&persistence.WorkerPool{}).Where("id = ?", pinned.ID).
		Updates(map[string]any{"tenant_isolation": placement.TenantIsolationShared, "version": 2, "updated_at": now.Add(time.Second)}).Error,
		"mutation of immutable Worker Pool Tenant isolation")
	invalid := pinned
	invalid.ID = uuid.New()
	invalid.Name = "invalid"
	invalid.TenantIsolation = "dedicated"
	assertWorkerTenantIsolationPostgresConstraint(t, db.Create(&invalid).Error, "unknown Worker Pool Tenant isolation policy")

	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: sharedTarget.ID,
		TargetKind: "docker", WorkerMode: "general-pool", RegistrationTrustMode: "shared-token",
		ClusterID: "postgres", Namespace: "default", PodName: "tenant-isolation-worker", Version: "test",
		ProtocolVersion: 2, Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: []byte(uuid.NewString()), Status: "online", AdministrativeStatus: "active",
		RegisteredAt: now, LastHeartbeatAt: now,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("tenant_binding_id", tenantA).Error; err != nil {
		t.Fatalf("bind shared-target Worker to Tenant A: %v", err)
	}
	assertWorkerTenantIsolationPostgresConstraint(t, db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("tenant_binding_id", tenantB).Error, "changing an immutable Worker Tenant binding")
	assertWorkerTenantIsolationPostgresConstraint(t, db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("tenant_binding_id", nil).Error, "clearing an immutable Worker Tenant binding")

	wrongTargetWorker := worker
	wrongTargetWorker.ID = uuid.New()
	wrongTargetWorker.ExecutionTargetID = tenantTarget.ID
	wrongTargetWorker.PodName = "wrong-target-binding"
	wrongTargetWorker.AuthTokenHash = []byte(uuid.NewString())
	wrongTargetWorker.TenantBindingID = &tenantB
	assertWorkerTenantIsolationPostgresConstraint(t, db.Create(&wrongTargetWorker).Error, "Worker Tenant binding outside its tenant-owned Target")
	nonGeneral := worker
	nonGeneral.ID = uuid.New()
	nonGeneral.WorkerMode = "execution-pinned"
	nonGeneral.PodName = "non-general-binding"
	nonGeneral.AuthTokenHash = []byte(uuid.NewString())
	nonGeneral.TenantBindingID = &tenantA
	assertWorkerTenantIsolationPostgresConstraint(t, db.Create(&nonGeneral).Error, "Tenant binding on a non-general Worker")
}

func assertWorkerTenantIsolationPostgresConstraint(t *testing.T, err error, description string) {
	t.Helper()
	if err == nil {
		t.Fatalf("PostgreSQL accepted %s", description)
	}
	if !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("PostgreSQL rejected %s through the wrong constraint class: %v", description, err)
	}
}
