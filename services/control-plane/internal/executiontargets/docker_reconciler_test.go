package executiontargets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestDockerPoolReconcilerCreatesStableWorkersAndDefersBusyRemoval(t *testing.T) {
	fixture := newDockerReconcileFixture(t, 2)
	engine := newFakeDockerEngine()
	fixture.reconciler.factory = &fakeDockerFactory{engine: engine}
	busy := map[string]bool{}
	fixture.reconciler.busyWorkers = func(context.Context, uuid.UUID) (map[string]bool, error) {
		copy := make(map[string]bool, len(busy))
		for name, value := range busy {
			copy[name] = value
		}
		return copy, nil
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.containers) != 2 || engine.ensureImageCalls != 1 || len(engine.createdSpecs) != 2 {
		t.Fatalf("unexpected initial Docker reconciliation: containers=%d pulls=%d creates=%d", len(engine.containers), engine.ensureImageCalls, len(engine.createdSpecs))
	}
	for _, spec := range engine.createdSpecs {
		if spec.MemoryBytes != 256<<20 || spec.NanoCPUs != 500_000_000 || spec.User != "10001:10001" {
			t.Fatalf("Docker resource limits were not applied: %#v", spec)
		}
		if spec.WorkingDir != "/data" || len(spec.Binds) != 1 || spec.Binds[0] != "synara-docker-test:/data" {
			t.Fatalf("Docker workspace volume was not mounted consistently: %#v", spec)
		}
		environment := strings.Join(spec.Environment, "\n")
		if !strings.Contains(environment, "SYNARA_WORKER_REGISTRATION_TOKEN=docker-registration-secret") ||
			!strings.Contains(environment, "SYNARA_EXECUTION_TARGET_KIND=docker") ||
			!strings.Contains(environment, "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL=v2") ||
			!strings.Contains(environment, "SYNARA_AGENTD_DRAIN_TIMEOUT=20s") ||
			!strings.Contains(environment, "SYNARA_AGENTD_WORKSPACE_ROOT=/data/workspaces") ||
			!strings.Contains(environment, "SYNARA_AGENTD_GIT_CACHE_ROOT=/data/git-cache") {
			t.Fatalf("Docker Worker environment is incomplete: %s", environment)
		}
		encodedLabels, _ := json.Marshal(spec.Labels)
		if bytes.Contains(encodedLabels, []byte("docker-registration-secret")) {
			t.Fatalf("Docker Worker secret leaked into labels: %s", encodedLabels)
		}
	}
	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != "active" {
		t.Fatalf("reconciled Docker target status is %q", target.Status)
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("resource_id = ? AND action = ?", fixture.targetID, "execution_target.docker_reconciled").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("initial Docker reconciliation wrote %d audits", auditCount)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.createdSpecs) != 2 {
		t.Fatalf("stable reconciliation created redundant containers: %d", len(engine.createdSpecs))
	}
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("resource_id = ? AND action = ?", fixture.targetID, "execution_target.docker_reconciled").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("stable reconciliation wrote redundant audits: %d", auditCount)
	}

	busyName := fmt.Sprintf("synara-agentd-%s-1", fixture.targetID)
	busy[busyName] = true
	fixture.updateDesiredWorkers(t, 1)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := engine.containers[busyName]; !exists {
		t.Fatal("Docker reconciler removed a Worker with a current Lease")
	}
	delete(busy, busyName)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := engine.containers[busyName]; exists {
		t.Fatal("Docker reconciler did not remove the obsolete Worker after its Lease cleared")
	}
	if len(engine.containers) != 1 {
		t.Fatalf("Docker scale-down left %d containers", len(engine.containers))
	}

	var audits []persistence.AuditLog
	if err := fixture.db.Where("resource_id = ?", fixture.targetID).Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	encodedAudits, err := json.Marshal(audits)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedAudits, []byte("docker-registration-secret")) {
		t.Fatalf("Docker registration secret leaked into audit logs: %s", encodedAudits)
	}
}

func TestDockerPoolReconcilerRejectsOverlappingWorkspaceAndGitCacheRoots(t *testing.T) {
	reconciler := &DockerPoolReconciler{config: DockerPoolReconcilerConfig{RegistrationToken: "registration-token"}}
	target := persistence.ExecutionTarget{ID: uuid.New()}
	for _, test := range []struct {
		name          string
		workspaceRoot string
		gitCacheRoot  string
	}{
		{name: "same root", workspaceRoot: "/data/shared", gitCacheRoot: "/data/shared"},
		{name: "cache inside workspace", workspaceRoot: "/data/workspaces", gitCacheRoot: "/data/workspaces/git-cache"},
		{name: "workspace inside cache", workspaceRoot: "/data/git-cache/workspaces", gitCacheRoot: "/data/git-cache"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := reconciler.normalize(target, dockerTargetConfiguration{
				Image: "synara-agentd:test", ControlPlaneURL: "https://control-plane.example.com",
				RunnerCommand: []string{"runner"}, WorkspaceMount: "/data",
				WorkspaceRoot: test.workspaceRoot, GitCacheRoot: test.gitCacheRoot,
			})
			assertExecutionTargetProblemCode(t, err, "invalid_docker_configuration")
		})
	}
}

type dockerReconcileFixture struct {
	db         *gorm.DB
	targets    *Service
	reconciler *DockerPoolReconciler
	targetID   uuid.UUID
}

func newDockerReconcileFixture(t *testing.T, desiredWorkers int) dockerReconcileFixture {
	t.Helper()
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "docker-reconcile-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := NewService(store.DB(), platformConfig, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	target, err := targetService.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "docker", Name: "managed-docker",
		Configuration: dockerTestConfiguration(desiredWorkers),
		Capabilities:  map[string]any{"workspaceModes": []string{"local", "worktree"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := NewDockerPoolReconciler(targetService, DockerPoolReconcilerConfig{
		RegistrationToken: "docker-registration-secret", PublicControlPlaneURL: "http://control-plane:3780",
	}, slog.Default())
	return dockerReconcileFixture{db: store.DB(), targets: targetService, reconciler: reconciler, targetID: target.ID}
}

func (f dockerReconcileFixture) updateDesiredWorkers(t *testing.T, desiredWorkers int) {
	t.Helper()
	encrypted, err := encryptConfiguration(f.targets.cipher, dockerTestConfiguration(desiredWorkers))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", f.targetID).
		Update("configuration_encrypted", encrypted).Error; err != nil {
		t.Fatal(err)
	}
}

func dockerTestConfiguration(desiredWorkers int) map[string]any {
	return map[string]any{
		"socketPath": "/var/run/docker.sock", "image": "synara-agentd:test", "pullPolicy": "if-not-present",
		"controlPlaneUrl": "http://control-plane:3780", "allowInsecureControlPlane": true,
		"runnerCommand": []string{"provider-host", "run", "--jsonl"}, "desiredWorkers": desiredWorkers,
		"workspaceVolume": "synara-docker-test", "workspaceMount": "/data", "workspaceRoot": "/data/workspaces",
		"networkMode": "synara-test", "user": "10001:10001", "memoryBytes": 256 << 20, "nanoCpus": 500_000_000,
	}
}

type fakeDockerFactory struct {
	engine dockerEngine
}

func (f *fakeDockerFactory) Open(string) (dockerEngine, error) { return f.engine, nil }

type fakeDockerEngine struct {
	containers       map[string]dockerContainer
	createdSpecs     []dockerContainerSpec
	removedNames     []string
	ensureImageCalls int
	nextID           int
}

func newFakeDockerEngine() *fakeDockerEngine {
	return &fakeDockerEngine{containers: map[string]dockerContainer{}}
}

func (e *fakeDockerEngine) ListManaged(_ context.Context, targetID uuid.UUID) ([]dockerContainer, error) {
	items := make([]dockerContainer, 0, len(e.containers))
	for _, container := range e.containers {
		if container.Labels[dockerTargetLabel] == targetID.String() {
			items = append(items, container)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (e *fakeDockerEngine) EnsureImage(context.Context, string, string) error {
	e.ensureImageCalls++
	return nil
}

func (e *fakeDockerEngine) CreateAndStart(_ context.Context, spec dockerContainerSpec) (dockerContainer, error) {
	e.nextID++
	container := dockerContainer{ID: fmt.Sprintf("container-%d", e.nextID), Name: spec.Name, State: "running", Labels: spec.Labels}
	e.createdSpecs = append(e.createdSpecs, spec)
	e.containers[spec.Name] = container
	return container, nil
}

func (e *fakeDockerEngine) Start(_ context.Context, id string) error {
	for name, container := range e.containers {
		if container.ID == id {
			container.State = "running"
			e.containers[name] = container
			return nil
		}
	}
	return fmt.Errorf("container %s not found", id)
}

func (e *fakeDockerEngine) Remove(_ context.Context, id string) error {
	for name, container := range e.containers {
		if container.ID == id {
			e.removedNames = append(e.removedNames, name)
			delete(e.containers, name)
			return nil
		}
	}
	return nil
}
