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
		Namespace: "default", PodName: "agentd-test", Version: "test",
	}
	if _, err := service.Register(ctx, input); err == nil {
		t.Fatal("remote worker without lease/fencing support was accepted")
	}
	input.LeaseSupported = true
	input.FencingSupported = true
	input.Capabilities = map[string]any{"workspaceModes": []string{"local"}}
	registered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	input.Version = "test-v2"
	input.Capabilities = map[string]any{"workspaceModes": []string{"local", "worktree"}}
	reregistered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatalf("re-register SQLite worker with JSON capabilities: %v", err)
	}
	worker, err = service.Authenticate(ctx, reregistered.Token)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat, err := service.Heartbeat(ctx, worker, HeartbeatInput{
		Version: "test-v3", Capabilities: map[string]any{"workspaceModes": []string{"worktree"}},
	})
	if err != nil {
		t.Fatalf("heartbeat SQLite worker with JSON capabilities: %v", err)
	}
	workspaceModes, ok := heartbeat.Capabilities["workspaceModes"].([]any)
	if !ok || len(workspaceModes) != 1 || workspaceModes[0] != "worktree" {
		t.Fatalf("worker heartbeat did not persist JSON capabilities: %#v", heartbeat.Capabilities)
	}
	if _, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
	}, "claim-wrong-target"); err == nil {
		t.Fatal("worker claimed against an incompatible execution target")
	}
}
