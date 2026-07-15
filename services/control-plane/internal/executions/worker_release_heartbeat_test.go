package executions

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestHeartbeatResynchronizesWorkerReleaseState(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "release-heartbeat-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), config, nil)
	service := NewService(store.DB(), nil, 30*time.Second, 90*time.Second, time.Hour, nil, targetService)
	registered, err := service.Register(ctx, RegisterWorkerInput{
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", InstanceUID: uuid.NewString(),
		ClusterID: "release-heartbeat", Namespace: "default", PodName: "release-heartbeat-worker",
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: workerManifestTestCapabilities(), LeaseSupported: true, FencingSupported: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if worker.CurrentManifestID == nil {
		t.Fatal("registered Worker did not persist its immutable manifest")
	}
	revisionID := uuid.New()
	now := time.Now().UTC()
	if err := store.DB().Create(&persistence.WorkerReleaseRevision{
		ID: revisionID, TenantID: domain.TenantID, ExecutionTargetID: domain.ExecutionTargetID,
		Revision: 1, WorkerManifestID: *worker.CurrentManifestID, Description: "heartbeat baseline",
		CreatedBy: domain.UserID, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.WorkerReleasePolicy{
		TenantID: domain.TenantID, ExecutionTargetID: domain.ExecutionTargetID,
		PolicyVersion: 1, PromotedRevisionID: revisionID, UpdatedBy: domain.UserID, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	heartbeat, err := service.Heartbeat(ctx, worker, HeartbeatInput{ProtocolVersion: WorkerProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.WorkerReleaseStatus != "active" || heartbeat.WorkerReleaseRevisionID == nil ||
		*heartbeat.WorkerReleaseRevisionID != revisionID || heartbeat.WorkerReleaseChannel == nil ||
		*heartbeat.WorkerReleaseChannel != "promoted" {
		t.Fatalf("heartbeat release synchronization = %#v", heartbeat)
	}
}
