package executions

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestRemoteWorkerRequiresLeaseAndFencingAndCannotSwitchTargets(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "worker-target-test")
	if err != nil {
		t.Fatal(err)
	}
	sshTarget := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "ssh", Name: "ssh-test", Status: "active", ConfigurationEncrypted: []byte{},
		Capabilities: map[string]any{},
	}
	if err := store.DB().Create(&sshTarget).Error; err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), config, nil)
	service := NewService(store.DB(), nil, 30*time.Second, 90*time.Second, time.Hour, nil, targetService)
	input := RegisterWorkerInput{
		ExecutionTargetID: sshTarget.ID, TargetKind: "ssh", ClusterID: "local",
		InstanceUID: uuid.NewString(),
		Namespace:   "default", PodName: "agentd-test", Version: "test", ProtocolVersion: WorkerProtocolVersion,
	}
	assertUnsupportedProtocol := func(label string, err error, received int) {
		t.Helper()
		var apiError *problem.Error
		if !errors.As(err, &apiError) || apiError.Status != 426 || apiError.Code != "worker_protocol_version_unsupported" ||
			apiError.Details["received"] != received || apiError.Details["minimumSupported"] != WorkerProtocolVersion ||
			apiError.Details["maximumSupported"] != WorkerProtocolVersion {
			t.Fatalf("%s returned unexpected Worker Protocol rejection: %#v", label, apiError)
		}
	}
	for _, legacyProtocolVersion := range []int{0, 1} {
		legacyInput := input
		legacyInput.ProtocolVersion = legacyProtocolVersion
		legacyInput.InstanceUID = ""
		_, err := service.Register(ctx, legacyInput)
		assertUnsupportedProtocol("legacy registration", err, legacyProtocolVersion)
	}
	if _, err := service.Register(ctx, input); err == nil {
		t.Fatal("remote worker without lease/fencing support was accepted")
	}
	input.LeaseSupported = true
	input.FencingSupported = true
	input.Capabilities = map[string]any{"workspaceModes": []string{"local"}}
	input.ProtocolVersion = WorkerProtocolVersion + 1
	_, err = service.Register(ctx, input)
	assertUnsupportedProtocol("future registration", err, WorkerProtocolVersion+1)
	input.ProtocolVersion = WorkerProtocolVersion
	registered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	firstWorkerID := worker.ID
	if worker.Incarnation != 1 || worker.InstanceUID != input.InstanceUID {
		t.Fatalf("initial Worker identity fence is invalid: %#v", worker)
	}
	_, err = service.Heartbeat(ctx, worker, HeartbeatInput{})
	assertUnsupportedProtocol("missing heartbeat protocol", err, 0)
	input.Version = "test-v2"
	input.InstanceUID = uuid.NewString()
	input.Capabilities = map[string]any{"workspaceModes": []string{"local", "worktree"}}
	reregistered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatalf("re-register SQLite worker with JSON capabilities: %v", err)
	}
	if _, err := service.Authenticate(ctx, registered.Token); err == nil {
		t.Fatal("re-registration did not revoke the previous Worker token")
	}
	worker, err = service.Authenticate(ctx, reregistered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if worker.ID != firstWorkerID || worker.Incarnation != 2 || worker.InstanceUID != input.InstanceUID {
		t.Fatalf("Worker re-registration did not atomically rotate its identity fence: %#v", worker)
	}
	draining := true
	heartbeat, err := service.Heartbeat(ctx, worker, HeartbeatInput{
		Version: "test-v3", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: map[string]any{"workspaceModes": []string{"worktree"}}, Draining: &draining,
	})
	if err != nil {
		t.Fatalf("heartbeat SQLite worker with JSON capabilities: %v", err)
	}
	workspaceModes, ok := heartbeat.Capabilities["workspaceModes"].([]any)
	if !ok || len(workspaceModes) != 1 || workspaceModes[0] != "worktree" {
		t.Fatalf("worker heartbeat did not persist JSON capabilities: %#v", heartbeat.Capabilities)
	}
	if heartbeat.Status != "draining" || heartbeat.DrainingAt == nil || heartbeat.ProtocolVersion != WorkerProtocolVersion {
		t.Fatalf("worker heartbeat did not persist drain/protocol state: %#v", heartbeat)
	}
	worker, err = service.Authenticate(ctx, reregistered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: sshTarget.ID, TargetKind: "ssh",
	}, "claim-draining"); err == nil {
		t.Fatal("draining Worker was allowed to claim")
	}
	staleHeartbeat := time.Now().UTC().Add(-2 * time.Minute)
	if err := store.DB().Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("last_heartbeat_at", staleHeartbeat).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.markStaleWorkers(ctx); err != nil {
		t.Fatal(err)
	}
	var staleWorker persistence.WorkerInstance
	if err := store.DB().Where("id = ?", worker.ID).Take(&staleWorker).Error; err != nil {
		t.Fatal(err)
	}
	if staleWorker.Status != "offline" {
		t.Fatalf("stale Draining Worker was not marked offline: %#v", staleWorker)
	}
	draining = false
	if _, err := service.Heartbeat(ctx, worker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion, Draining: &draining,
	}); err != nil {
		t.Fatalf("worker could not leave drain mode: %v", err)
	}
	worker, err = service.Authenticate(ctx, reregistered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
	}, "claim-wrong-target"); err == nil {
		t.Fatal("worker claimed against an incompatible execution target")
	}
}
