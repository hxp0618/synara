package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteWorkerModeSafetyBackfillsLegacyWorkersAndRejectsInvalidModes(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.DB().WithContext(ctx).AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-worker-mode-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	kubernetesTargetID := uuid.New()
	if err := store.DB().Create(&persistence.ExecutionTarget{
		ID: kubernetesTargetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "sqlite-worker-mode-kubernetes", Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create kubernetes target: %v", err)
	}

	localWorker := sqliteRevocationWorker(domain.ExecutionTargetID, "legacy-general", now)
	kubernetesWorker := sqliteRevocationWorker(kubernetesTargetID, "legacy-pinned", now)
	kubernetesWorker.TargetKind = "kubernetes"
	kubernetesWorker.ClusterID = "kubernetes"
	kubernetesWorker.RegistrationTrustMode = "kubernetes-pod-bound-v1"
	for _, worker := range []*persistence.WorkerInstance{&localWorker, &kubernetesWorker} {
		if err := store.DB().Create(worker).Error; err != nil {
			t.Fatalf("create sqlite worker %s: %v", worker.PodName, err)
		}
	}
	if err := store.DB().Exec(
		`UPDATE worker_instances SET worker_mode = '' WHERE id IN (?, ?)`,
		localWorker.ID, kubernetesWorker.ID,
	).Error; err != nil {
		t.Fatalf("clear sqlite worker modes before safety migration: %v", err)
	}

	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"trg_worker_instances_worker_mode_insert",
		"trg_worker_instances_worker_mode_update",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite Worker mode safety object %s count = %d, want 1", name, count)
		}
	}

	for _, expected := range []struct {
		id   uuid.UUID
		mode string
	}{
		{id: localWorker.ID, mode: "general-pool"},
		{id: kubernetesWorker.ID, mode: "execution-pinned"},
	} {
		var worker persistence.WorkerInstance
		if err := store.DB().Where("id = ?", expected.id).Take(&worker).Error; err != nil {
			t.Fatal(err)
		}
		if worker.WorkerMode != expected.mode {
			t.Fatalf("sqlite worker %s mode = %q, want %q", worker.ID, worker.WorkerMode, expected.mode)
		}
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", localWorker.ID).
		Update("worker_mode", "bogus").Error; err == nil {
		t.Fatal("SQLite accepted an invalid worker_mode")
	}
}
