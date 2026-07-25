package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkerPoolPlacementMigrationBackfillsAndFencesScope(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000056_active_turn_suspend_checkpoint.sql")); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	userID := uuid.New()
	tenantID := uuid.New()
	if err := db.Create(&persistence.User{
		ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Placement owner",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: tenantID, Slug: "placement-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12], Name: "Placement Tenant",
			Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{},
			CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}

	sharedDockerID := uuid.New()
	kubernetesID := uuid.New()
	sshID := uuid.New()
	for _, target := range []persistence.ExecutionTarget{
		{
			ID: sharedDockerID, Kind: "docker", Name: "shared-docker", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: kubernetesID, TenantID: &tenantID, Kind: "kubernetes", Name: "tenant-kubernetes", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: sshID, TenantID: &tenantID, Kind: "ssh", Name: "tenant-ssh", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := db.Create(&target).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	pools := make([]persistence.WorkerPool, 0)
	if err := db.Order("execution_target_id").Find(&pools).Error; err != nil {
		t.Fatal(err)
	}
	if len(pools) != 3 {
		t.Fatalf("worker pool count = %d, want 3", len(pools))
	}
	byTarget := make(map[uuid.UUID]persistence.WorkerPool, len(pools))
	for _, pool := range pools {
		byTarget[pool.ExecutionTargetID] = pool
		if pool.Name != "default" || pool.CapacityClass != "standard" || pool.DesiredIdleUnits != 0 ||
			pool.MaxActiveUnits != 1 || pool.Status != "active" || pool.Version != 1 {
			t.Fatalf("backfilled pool = %#v", pool)
		}
	}
	if byTarget[sharedDockerID].Mode != "resident" || byTarget[sharedDockerID].TenantID != nil {
		t.Fatalf("shared docker pool = %#v", byTarget[sharedDockerID])
	}
	if byTarget[kubernetesID].Mode != "per-execution" || byTarget[kubernetesID].TenantID == nil || *byTarget[kubernetesID].TenantID != tenantID {
		t.Fatalf("kubernetes pool = %#v", byTarget[kubernetesID])
	}
	if byTarget[sshID].Mode != "resident" || byTarget[sshID].TenantID == nil || *byTarget[sshID].TenantID != tenantID {
		t.Fatalf("ssh pool = %#v", byTarget[sshID])
	}

	policies := make([]persistence.ExecutionPlacementPolicy, 0)
	if err := db.Order("execution_target_id").Find(&policies).Error; err != nil {
		t.Fatal(err)
	}
	if len(policies) != 3 {
		t.Fatalf("policy count = %d, want 3", len(policies))
	}
	for _, policy := range policies {
		pool, ok := byTarget[policy.ExecutionTargetID]
		if !ok {
			t.Fatalf("missing pool for policy %#v", policy)
		}
		if policy.Version != 1 || policy.DefaultPoolID != pool.ID || policy.BalancedPoolID != nil ||
			policy.LowLatencyPoolID != nil || policy.UpdatedBy != nil {
			t.Fatalf("backfilled policy = %#v", policy)
		}
		if (policy.TenantID == nil) != (pool.TenantID == nil) {
			t.Fatalf("policy tenant mismatch policy=%#v pool=%#v", policy, pool)
		}
		if policy.TenantID != nil && *policy.TenantID != *pool.TenantID {
			t.Fatalf("policy tenant mismatch policy=%#v pool=%#v", policy, pool)
		}
	}

	err := db.Exec(`
		UPDATE execution_placement_policies
		SET version = version + 1,
		    default_pool_id = ?,
		    updated_at = ?
		WHERE execution_target_id = ?
	`, byTarget[sharedDockerID].ID, now.Add(time.Second), kubernetesID).Error
	if err == nil {
		t.Fatal("migration accepted a policy pointing at another target's pool")
	}

	err = db.Model(&persistence.WorkerPool{}).
		Where("id = ?", byTarget[kubernetesID].ID).
		Update("status", "disabled").Error
	if err == nil {
		t.Fatal("migration accepted disabling a selected default pool")
	}
}
