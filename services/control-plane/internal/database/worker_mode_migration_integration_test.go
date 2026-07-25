package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestWorkerModeMigrationBackfillsHistoricalWorkersAndFencesEnum(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_WORKER_MODE_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	}
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_WORKER_MODE_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000056_active_turn_suspend_checkpoint.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "worker-mode-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	kubernetesTargetID := uuid.New()
	if err := db.Create(&persistence.ExecutionTarget{
		ID: kubernetesTargetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "worker-mode-kubernetes", Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create kubernetes target: %v", err)
	}

	localWorker := postgresCurrentRevocationWorker(domain.ExecutionTargetID, "legacy-general", now)
	localWorker.RegistrationTrustMode = "shared-token"
	kubernetesWorker := postgresCurrentRevocationWorker(kubernetesTargetID, "legacy-pinned", now)
	kubernetesWorker.TargetKind = "kubernetes"
	kubernetesWorker.ClusterID = "kubernetes"
	kubernetesWorker.RegistrationTrustMode = "kubernetes-pod-bound-v1"

	for _, worker := range []*persistence.WorkerInstance{&localWorker, &kubernetesWorker} {
		if err := insertPreWorkerModeWorker(db, worker); err != nil {
			t.Fatalf("insert pre-000057 worker %s: %v", worker.PodName, err)
		}
	}

	if err := Migrate(ctx, db, migrationsThrough(t, "000057_worker_mode.sql")); err != nil {
		t.Fatal(err)
	}

	for _, expected := range []struct {
		id   uuid.UUID
		mode string
	}{
		{id: localWorker.ID, mode: "general-pool"},
		{id: kubernetesWorker.ID, mode: "execution-pinned"},
	} {
		var worker persistence.WorkerInstance
		if err := db.Where("id = ?", expected.id).Take(&worker).Error; err != nil {
			t.Fatal(err)
		}
		if worker.WorkerMode != expected.mode {
			t.Fatalf("worker %s mode = %q, want %q", worker.ID, worker.WorkerMode, expected.mode)
		}
	}

	assertMigrationIndex(
		t,
		db,
		"idx_worker_instances_claimability",
		"execution_target_id,worker_mode,administrative_status,compatibility_status,status,last_heartbeat_at,id",
		"",
	)
	if err := db.Model(&persistence.WorkerInstance{}).
		Where("id = ?", localWorker.ID).
		Update("worker_mode", "bogus").Error; err == nil {
		t.Fatal("000057 accepted an invalid worker_mode")
	}
}

func insertPreWorkerModeWorker(db *gorm.DB, worker *persistence.WorkerInstance) error {
	return db.Select(
		"id", "incarnation", "instance_uid", "execution_target_id", "target_kind",
		"registration_trust_mode", "cluster_id", "namespace", "pod_name", "version", "protocol_version",
		"capabilities", "compatibility_status", "compatibility_reason", "compatibility_checked_at",
		"worker_release_revision_id", "worker_release_channel", "worker_release_status",
		"worker_release_reason", "worker_release_checked_at",
		"lease_supported", "fencing_supported", "auth_token_hash", "status", "administrative_status",
		"registered_at", "last_heartbeat_at", "draining_at", "terminated_at",
		"revoked_at", "revoked_by", "revocation_reason",
	).Create(worker).Error
}
