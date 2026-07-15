package executiontargets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

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
		if len(spec.ExtraHosts) != 0 {
			t.Fatalf("Docker Worker unexpectedly added host aliases for a named Control Plane host: %#v", spec.ExtraHosts)
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
		if strings.Contains(environment, "SYNARA_AGENTD_VERSION=managed") {
			t.Fatalf("Docker Worker overrides the immutable image version: %s", environment)
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

func TestDockerPoolReconcilerAddsHostGatewayForDockerInternalControlPlane(t *testing.T) {
	fixture := newDockerReconcileFixture(t, 1)
	engine := newFakeDockerEngine()
	fixture.reconciler.factory = &fakeDockerFactory{engine: engine}
	configuration := dockerTestConfiguration(1)
	configuration["controlPlaneUrl"] = "http://host.docker.internal:3780"
	fixture.updateConfiguration(t, configuration)

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.createdSpecs) != 1 {
		t.Fatalf("Docker Worker create count = %d, want 1", len(engine.createdSpecs))
	}
	if got := engine.createdSpecs[0].ExtraHosts; len(got) != 1 || got[0] != "host.docker.internal:host-gateway" {
		t.Fatalf("Docker Worker host aliases = %#v", got)
	}
}

func TestDockerPoolReconcilerDefersBusyConfigReplacementWithoutNameConflict(t *testing.T) {
	fixture := newDockerReconcileFixture(t, 1)
	engine := newFakeDockerEngine()
	fixture.reconciler.factory = &fakeDockerFactory{engine: engine}
	busy := map[string]bool{}
	fixture.reconciler.busyWorkers = func(context.Context, uuid.UUID) (map[string]bool, error) {
		return busy, nil
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	workerName := fmt.Sprintf("synara-agentd-%s-0", fixture.targetID)
	first := engine.containers[workerName]
	if first.ID == "" || len(engine.createdSpecs) != 1 {
		t.Fatalf("initial Worker was not created: %#v", engine.containers)
	}

	busy[workerName] = true
	configuration := dockerTestConfiguration(1)
	configuration["image"] = "synara-agentd:next"
	fixture.updateConfiguration(t, configuration)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.createdSpecs) != 1 {
		t.Fatalf("busy stale Worker caused a conflicting replacement create: %d", len(engine.createdSpecs))
	}
	if current := engine.containers[workerName]; current.ID != first.ID {
		t.Fatalf("busy stale Worker was replaced before its Lease cleared: before=%s after=%s", first.ID, current.ID)
	}
	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != "offline" {
		t.Fatalf("target with only a deferred stale Worker remained %q", target.Status)
	}

	delete(busy, workerName)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.createdSpecs) != 2 {
		t.Fatalf("cleared Lease did not create the desired replacement: %d", len(engine.createdSpecs))
	}
	if current := engine.containers[workerName]; current.ID == first.ID {
		t.Fatalf("stale Worker was not replaced after its Lease cleared: %#v", current)
	}
}

func TestDockerPoolReconcilerMaterializesPromotedAndCanaryReleaseImagesWithPullCredential(t *testing.T) {
	fixture := newDockerReconcileFixture(t, 4)
	configuration := dockerTestConfiguration(4)
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	fixture.updateConfiguration(t, configuration)
	promotedDigest := "sha256:" + strings.Repeat("a", 64)
	canaryDigest := "sha256:" + strings.Repeat("b", 64)
	promotedRevision := fixture.seedReleaseRevision(t, 1, promotedDigest)
	canaryRevision := fixture.seedReleaseRevision(t, 2, canaryDigest)
	policy := persistence.WorkerReleasePolicy{
		TenantID: fixture.tenantID, ExecutionTargetID: fixture.targetID, PolicyVersion: 1,
		PromotedRevisionID: promotedRevision, CanaryRevisionID: &canaryRevision, CanaryPercent: 25,
		UpdatedBy: fixture.userID, UpdatedAt: time.Now().UTC(),
	}
	if err := fixture.db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}
	credential := &ImagePullCredential{
		BindingID: uuid.New(), CredentialID: uuid.New(), CredentialVersion: 3,
		Host: "ghcr.io", Username: "synara", Password: "registry-secret",
	}
	fixture.reconciler.config.ResolveImagePull = func(_ context.Context, _, _ uuid.UUID, selector string) (ImagePullCredentialResolution, error) {
		if selector != "ghcr.io" {
			t.Fatalf("Docker image pull selector = %q", selector)
		}
		return ImagePullCredentialResolution{Credential: credential, Authoritative: true}, nil
	}
	engine := newFakeDockerEngine()
	fixture.reconciler.factory = &fakeDockerFactory{engine: engine}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.createdSpecs) != 4 || len(engine.ensuredImages) != 2 {
		t.Fatalf("release reconciliation creates=%d images=%#v", len(engine.createdSpecs), engine.ensuredImages)
	}
	channels := map[string]int{}
	for _, spec := range engine.createdSpecs {
		channel := spec.Labels[dockerReleaseChannelLabel]
		channels[channel]++
		if channel == "promoted" && spec.Image != "ghcr.io/synara/worker@"+promotedDigest {
			t.Fatalf("promoted image = %q", spec.Image)
		}
		if channel == "canary" && spec.Image != "ghcr.io/synara/worker@"+canaryDigest {
			t.Fatalf("canary image = %q", spec.Image)
		}
		imageParts := strings.SplitN(spec.Image, "@", 2)
		if len(imageParts) != 2 || !strings.Contains(strings.Join(spec.Environment, "\n"), "SYNARA_AGENTD_IMAGE_DIGEST="+imageParts[1]) {
			t.Fatalf("release digest was not projected into the Worker environment: %#v", spec.Environment)
		}
	}
	if channels["promoted"] != 3 || channels["canary"] != 1 {
		t.Fatalf("release channel allocation = %#v", channels)
	}
	for _, resolved := range engine.pullCredentials {
		if resolved != credential {
			t.Fatalf("image pull Credential projection changed: %#v", resolved)
		}
	}
}

func TestDockerPoolReconcilerBusyPromotedWorkerDoesNotReserveCanarySlot(t *testing.T) {
	fixture := newDockerReconcileFixture(t, 4)
	configuration := dockerTestConfiguration(4)
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	fixture.updateConfiguration(t, configuration)
	promotedRevision := fixture.seedReleaseRevision(t, 1, "sha256:"+strings.Repeat("a", 64))
	canaryRevision := fixture.seedReleaseRevision(t, 2, "sha256:"+strings.Repeat("b", 64))
	policy := persistence.WorkerReleasePolicy{
		TenantID: fixture.tenantID, ExecutionTargetID: fixture.targetID, PolicyVersion: 1,
		PromotedRevisionID: promotedRevision, UpdatedBy: fixture.userID, UpdatedAt: time.Now().UTC(),
	}
	if err := fixture.db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}
	engine := newFakeDockerEngine()
	fixture.reconciler.factory = &fakeDockerFactory{engine: engine}
	busy := map[string]bool{}
	fixture.reconciler.busyWorkers = func(context.Context, uuid.UUID) (map[string]bool, error) { return busy, nil }
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	busyName := fmt.Sprintf("synara-agentd-%s-promoted-3", fixture.targetID)
	busyContainer := engine.containers[busyName]
	if busyContainer.ID == "" {
		t.Fatalf("baseline promoted Worker %q was not created", busyName)
	}
	busy[busyName] = true
	if err := fixture.db.Model(&persistence.WorkerReleasePolicy{}).
		Where("execution_target_id = ? AND policy_version = ?", fixture.targetID, 1).
		Updates(map[string]any{
			"policy_version": 2, "canary_revision_id": canaryRevision,
			"canary_percent": 25, "updated_at": time.Now().UTC(),
		}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if current := engine.containers[busyName]; current.ID != busyContainer.ID {
		t.Fatalf("busy promoted Worker was replaced: before=%s after=%s", busyContainer.ID, current.ID)
	}
	canaryName := fmt.Sprintf("synara-agentd-%s-canary-0", fixture.targetID)
	if canary := engine.containers[canaryName]; canary.ID == "" {
		t.Fatalf("busy promoted Worker reserved the canary slot: containers=%#v", engine.containers)
	}
}

func TestDockerReleaseSlotsStayWithinDesiredCapacity(t *testing.T) {
	promoted := managedReleaseImage{RevisionID: uuid.New(), Channel: "promoted", Image: "worker@sha256:" + strings.Repeat("a", 64)}
	canary := managedReleaseImage{RevisionID: uuid.New(), Channel: "canary", Image: "worker@sha256:" + strings.Repeat("b", 64)}
	plan := &managedReleasePlan{Promoted: promoted, Canary: &canary, CanaryPercent: 100}

	if _, err := dockerReleaseSlots(1, "worker:latest", plan); err == nil {
		t.Fatal("one-Worker Docker pool accepted a two-channel canary policy")
	} else {
		assertExecutionTargetProblemCode(t, err, "docker_worker_release_canary_capacity_insufficient")
	}
	slots, err := dockerReleaseSlots(4, "worker:latest", plan)
	if err != nil {
		t.Fatal(err)
	}
	channels := map[string]int{}
	for _, slot := range slots {
		channels[slot.Channel]++
	}
	if len(slots) != 4 || channels["promoted"] != 1 || channels["canary"] != 3 {
		t.Fatalf("100%% canary slots = %#v, channels=%#v", slots, channels)
	}
}

func TestDockerRegistryAuthHeaderUsesOpaqueEngineHeader(t *testing.T) {
	header, err := dockerRegistryAuthHeader(&ImagePullCredential{
		Host: "ghcr.io", Username: "synara", Password: "registry-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["serveraddress"] != "ghcr.io" || payload["username"] != "synara" ||
		payload["password"] != "registry-secret" {
		t.Fatalf("Docker Registry auth payload = %#v", payload)
	}
}

func TestDockerRegistryAuthHeaderUsesPaddedEncodingAndDirectRegistryToken(t *testing.T) {
	header, err := dockerRegistryAuthHeader(&ImagePullCredential{
		Host: "docker.io", RegistryToken: "registry-bearer-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(header, "=") {
		t.Fatalf("Docker Registry auth header is not padded base64url: %q", header)
	}
	decoded, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["serveraddress"] != "https://index.docker.io/v1/" ||
		payload["registrytoken"] != "registry-bearer-secret" || payload["identitytoken"] != "" {
		t.Fatalf("Docker Registry bearer auth payload = %#v", payload)
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
	tenantID   uuid.UUID
	userID     uuid.UUID
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
	return dockerReconcileFixture{
		db: store.DB(), targets: targetService, reconciler: reconciler,
		targetID: target.ID, tenantID: domain.TenantID, userID: domain.UserID,
	}
}

func (f dockerReconcileFixture) seedReleaseRevision(t *testing.T, revision int64, imageDigest string) uuid.UUID {
	t.Helper()
	manifestID := uuid.New()
	manifest := persistence.WorkerManifest{
		ID: manifestID, ManifestHash: fmt.Sprintf("%064x", revision),
		WorkerBuildVersion: fmt.Sprintf("release-%d", revision), WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, OperatingSystem: "linux", Architecture: "amd64",
		ImageDigest: &imageDigest, FeatureFlags: map[string]any{}, CreatedAt: time.Now().UTC(),
	}
	if err := f.db.Create(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	revisionID := uuid.New()
	model := persistence.WorkerReleaseRevision{
		ID: revisionID, TenantID: f.tenantID, ExecutionTargetID: f.targetID,
		Revision: revision, WorkerManifestID: manifestID, Description: "test release",
		CreatedBy: f.userID, CreatedAt: time.Now().UTC(),
	}
	if err := f.db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	return revisionID
}

func (f dockerReconcileFixture) updateDesiredWorkers(t *testing.T, desiredWorkers int) {
	t.Helper()
	f.updateConfiguration(t, dockerTestConfiguration(desiredWorkers))
}

func (f dockerReconcileFixture) updateConfiguration(t *testing.T, configuration map[string]any) {
	t.Helper()
	encrypted, err := encryptConfiguration(f.targets.cipher, configuration)
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
	ensuredImages    []string
	pullCredentials  []*ImagePullCredential
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

func (e *fakeDockerEngine) EnsureImage(
	_ context.Context,
	image, _ string,
	credential *ImagePullCredential,
) error {
	e.ensureImageCalls++
	e.ensuredImages = append(e.ensuredImages, image)
	e.pullCredentials = append(e.pullCredentials, credential)
	return nil
}

func (e *fakeDockerEngine) CreateAndStart(_ context.Context, spec dockerContainerSpec) (dockerContainer, error) {
	if current, exists := e.containers[spec.Name]; exists {
		return dockerContainer{}, fmt.Errorf("container name %s is already owned by %s", spec.Name, current.ID)
	}
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
