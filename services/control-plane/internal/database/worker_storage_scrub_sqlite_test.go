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

func TestSQLiteWorkerStorageScrubIdentityAndTerminalReceiptsAreFenced(t *testing.T) {
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
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-storage-scrub-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	worker := sqliteRevocationWorker(domain.ExecutionTargetID, "storage-scrub", now)
	worker.InstanceUID = uuid.NewString()
	if err := store.DB().Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	first := persistence.WorkerStorageScrub{
		ID: uuid.New(), WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
		WorkerInstanceUID: worker.InstanceUID, ExecutionTargetID: domain.ExecutionTargetID,
		TenantID: domain.TenantID, ScopeKind: "execution", ScopeID: uuid.New(), ScopeGeneration: 1,
		ScrubGeneration: 1, Status: "pending", CreatedAt: now,
	}
	if err := store.DB().Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	concurrent := first
	concurrent.ID = uuid.New()
	concurrent.ScopeID = uuid.New()
	concurrent.ScrubGeneration = 2
	if err := store.DB().Create(&concurrent).Error; err == nil {
		t.Fatal("SQLite accepted two unresolved storage scrubs for one Worker incarnation")
	}
	if err := store.DB().Model(&persistence.WorkerStorageScrub{}).Where("id = ?", first.ID).
		Update("tenant_id", uuid.New()).Error; err == nil {
		t.Fatal("SQLite accepted Worker storage scrub identity mutation")
	}
	acknowledgedAt := now.Add(time.Second)
	if err := store.DB().Model(&persistence.WorkerStorageScrub{}).Where("id = ?", first.ID).
		Updates(map[string]any{"status": "acknowledged", "acknowledged_at": acknowledgedAt}).Error; err != nil {
		t.Fatalf("acknowledge SQLite Worker storage scrub: %v", err)
	}
	if err := store.DB().Model(&persistence.WorkerStorageScrub{}).Where("id = ?", first.ID).
		Update("status", "pending").Error; err == nil {
		t.Fatal("SQLite revived an acknowledged Worker storage scrub")
	}
	if err := store.DB().Delete(&persistence.WorkerStorageScrub{}, "id = ?", first.ID).Error; err == nil {
		t.Fatal("SQLite deleted append-only Worker storage scrub history")
	}

	second := concurrent
	second.ScrubGeneration = 2
	if err := store.DB().Create(&second).Error; err != nil {
		t.Fatalf("create next SQLite Worker storage scrub after acknowledgement: %v", err)
	}
	if err := store.DB().Model(&persistence.WorkerStorageScrub{}).Where("id = ?", second.ID).
		Update("worker_incarnation", 2).Error; err == nil {
		t.Fatal("SQLite transferred pending scrub before the physical Worker re-registered")
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("incarnation", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.WorkerStorageScrub{}).Where("id = ?", second.ID).
		Update("worker_incarnation", 2).Error; err != nil {
		t.Fatalf("transfer pending scrub to matching Worker re-registration: %v", err)
	}
}
