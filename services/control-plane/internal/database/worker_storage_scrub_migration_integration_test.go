package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresWorkerStorageScrubIdentityAndTerminalReceiptsAreFenced(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "postgres-storage-scrub-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	worker := sqliteRevocationWorker(domain.ExecutionTargetID, "postgres-storage-scrub-"+uuid.NewString(), now)
	worker.InstanceUID = uuid.NewString()
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	first := persistence.WorkerStorageScrub{
		ID: uuid.New(), WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
		WorkerInstanceUID: worker.InstanceUID, ExecutionTargetID: domain.ExecutionTargetID,
		TenantID: domain.TenantID, ScopeKind: "execution", ScopeID: uuid.New(), ScopeGeneration: 1,
		ScrubGeneration: 1, Status: "pending", CreatedAt: now,
	}
	if err := db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	concurrent := first
	concurrent.ID = uuid.New()
	concurrent.ScopeID = uuid.New()
	concurrent.ScrubGeneration = 2
	if err := db.Create(&concurrent).Error; err == nil {
		t.Fatal("PostgreSQL accepted two unresolved storage scrubs for one Worker incarnation")
	}
	if err := db.Model(&persistence.WorkerStorageScrub{}).Where("id = ?", first.ID).
		Update("tenant_id", uuid.New()).Error; err == nil {
		t.Fatal("PostgreSQL accepted Worker storage scrub identity mutation")
	}
	acknowledgedAt := now.Add(time.Second)
	if err := db.Model(&persistence.WorkerStorageScrub{}).Where("id = ?", first.ID).
		Updates(map[string]any{"status": "acknowledged", "acknowledged_at": acknowledgedAt}).Error; err != nil {
		t.Fatalf("acknowledge PostgreSQL Worker storage scrub: %v", err)
	}
	if err := db.Model(&persistence.WorkerStorageScrub{}).Where("id = ?", first.ID).
		Update("status", "pending").Error; err == nil {
		t.Fatal("PostgreSQL revived an acknowledged Worker storage scrub")
	}
	if err := db.Delete(&persistence.WorkerStorageScrub{}, "id = ?", first.ID).Error; err == nil {
		t.Fatal("PostgreSQL deleted append-only Worker storage scrub history")
	}

	second := concurrent
	second.ScrubGeneration = 2
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("create next PostgreSQL Worker storage scrub after acknowledgement: %v", err)
	}
	if err := db.Model(&persistence.WorkerStorageScrub{}).Where("id = ?", second.ID).
		Update("worker_incarnation", 2).Error; err == nil {
		t.Fatal("PostgreSQL transferred pending scrub before the physical Worker re-registered")
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("incarnation", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerStorageScrub{}).Where("id = ?", second.ID).
		Update("worker_incarnation", 2).Error; err != nil {
		t.Fatalf("transfer pending scrub to matching PostgreSQL Worker re-registration: %v", err)
	}
}
