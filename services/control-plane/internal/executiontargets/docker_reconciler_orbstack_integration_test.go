package executiontargets_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

const orbStackDockerIntegrationEnv = "SYNARA_ORBSTACK_DOCKER_TEST"

func TestManagedDockerRollingDrainOrbStackIntegration(t *testing.T) {
	if os.Getenv(orbStackDockerIntegrationEnv) != "1" {
		t.Skip(orbStackDockerIntegrationEnv + "=1 is required")
	}
	image := strings.TrimSpace(os.Getenv("SYNARA_ORBSTACK_AGENTD_IMAGE"))
	if image == "" {
		t.Fatal("SYNARA_ORBSTACK_AGENTD_IMAGE must name an existing immutable local acceptance image")
	}
	imageID := dockerRun(t, "image", "inspect", "--format", "{{.Id}}", image)
	engineVersion := dockerRun(t, "version", "--format", "{{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}")

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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "orbstack-docker-drain-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x6d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), platformConfig, cipher)
	sessionService := sessions.NewService(store.DB(), projects.NewService(store.DB()), targetService)
	executionService := executions.NewService(
		store.DB(), sessionService, 12*time.Second, 20*time.Second, time.Hour, cipher, targetService,
	)

	registrationToken := "orbstack-docker-drain-registration"
	workerAPI := newOrbStackWorkerAPI(executionService, registrationToken)
	server := httptest.NewServer(workerAPI)
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	controlPlaneURL := "http://host.docker.internal:" + serverURL.Port()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	networkName := "synara-stage4-drain-" + suffix
	volumeName := "synara-stage4-drain-volume-" + suffix
	dockerRun(t, "network", "create", "--label", "synara.io/acceptance=stage4-docker-drain", networkName)
	t.Cleanup(func() {
		if err := dockerRunOptional("network", "rm", networkName); err != nil {
			t.Errorf("clean exact managed Docker acceptance network: %v", err)
		}
	})
	dockerRun(t, "volume", "create", "--label", "synara.io/acceptance=stage4-docker-drain", volumeName)
	t.Cleanup(func() {
		if err := dockerRunOptional("volume", "rm", volumeName); err != nil {
			t.Errorf("clean exact managed Docker acceptance volume: %v", err)
		}
	})

	configuration := managedDockerOrbStackConfiguration(
		image, controlPlaneURL, networkName, volumeName, 2, 250_000_000,
	)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	target, err := targetService.Create(ctx, principal, domain.TenantID, executiontargets.CreateInput{
		OrganizationID: &domain.OrganizationID,
		Kind:           "docker",
		Name:           "OrbStack rolling Drain " + suffix,
		Configuration:  configuration,
		Capabilities: map[string]any{
			"workspaceModes": []string{"local", "worktree"},
			"providerPolicy": map[string]any{
				"experimentalProviders": []string{"codex", "claudeAgent"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	containerNames := []string{
		"synara-agentd-" + target.ID.String() + "-0",
		"synara-agentd-" + target.ID.String() + "-1",
	}
	t.Cleanup(func() {
		for _, name := range containerNames {
			if err := dockerRunOptional("rm", "-f", name); err != nil {
				t.Errorf("clean exact managed Docker acceptance container %s: %v", name, err)
			}
		}
	})

	reconciler := executiontargets.NewDockerPoolReconciler(
		targetService,
		executiontargets.DockerPoolReconcilerConfig{
			RegistrationToken: registrationToken,
			WorkerLeaseTTL:    12 * time.Second,
		},
		slog.Default(),
	)
	reconciler.SetWorkerLifecycleCoordinator(executionService)
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitForManagedDockerWorkers(t, store.DB(), executionService, target.ID, containerNames, 2, 45*time.Second, workerAPI)
	initial := managedDockerContainerIDs(t, target.ID)
	if len(initial) != 2 {
		t.Fatalf("initial managed Docker containers = %#v", initial)
	}

	configuration["nanoCpus"] = int64(350_000_000)
	updateManagedDockerConfiguration(t, store.DB(), cipher, target.ID, configuration)
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	firstPass := managedDockerContainerIDs(t, target.ID)
	assertOneManagedDockerContainerChanged(t, initial, firstPass)
	waitForManagedDockerWorkers(t, store.DB(), executionService, target.ID, containerNames, 2, 45*time.Second, workerAPI)

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	secondPass := managedDockerContainerIDs(t, target.ID)
	assertOneManagedDockerContainerChanged(t, firstPass, secondPass)
	for name, initialID := range initial {
		if secondPass[name] == initialID {
			t.Fatalf("rolling Drain did not replace %s: initial=%s final=%s", name, initialID, secondPass[name])
		}
	}
	waitForManagedDockerWorkers(t, store.DB(), executionService, target.ID, containerNames, 2, 45*time.Second, workerAPI)
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	stable := managedDockerContainerIDs(t, target.ID)
	if !equalManagedDockerContainerIDs(secondPass, stable) {
		t.Fatalf("converged managed Docker pool changed: before=%#v after=%#v", secondPass, stable)
	}

	var activeDrains int64
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("execution_target_id = ? AND reconciliation_drain_requested_at IS NOT NULL AND status <> ?", target.ID, "terminated").
		Count(&activeDrains).Error; err != nil {
		t.Fatal(err)
	}
	var currentWorkers int64
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where(
			"execution_target_id = ? AND target_kind = ? AND status = ? AND reconciliation_drain_requested_at IS NULL",
			target.ID, "docker", "online",
		).
		Count(&currentWorkers).Error; err != nil {
		t.Fatal(err)
	}
	var terminatedFacts int64
	if err := store.DB().Model(&persistence.WorkerIncarnationFact{}).
		Where(
			"execution_target_id = ? AND current_state = ? AND terminal_reason = ?",
			target.ID, "terminated", executiontargets.ManagedDockerDrainReasonStaleSpec,
		).
		Count(&terminatedFacts).Error; err != nil {
		t.Fatal(err)
	}
	if activeDrains != 0 || currentWorkers != 2 || terminatedFacts != 2 {
		t.Fatalf(
			"final managed Docker lifecycle: activeDrains=%d currentWorkers=%d terminatedFacts=%d errors=%#v",
			activeDrains, currentWorkers, terminatedFacts, workerAPI.Errors(),
		)
	}
	if unexpected := unexpectedOrbStackWorkerAPIErrors(workerAPI.Errors()); len(unexpected) > 0 {
		t.Fatalf("unexpected managed Docker Worker API errors: %#v", unexpected)
	}
	t.Logf(
		"OrbStack=%s image=%s initial=%v firstPass=%v secondPass=%v activeDrains=%d currentWorkers=%d terminatedFacts=%d",
		engineVersion, imageID, initial, firstPass, secondPass, activeDrains, currentWorkers, terminatedFacts,
	)
}

func managedDockerOrbStackConfiguration(
	image, controlPlaneURL, networkName, volumeName string,
	desiredWorkers int,
	nanoCPUs int64,
) map[string]any {
	return map[string]any{
		"socketPath":                "/var/run/docker.sock",
		"image":                     image,
		"pullPolicy":                "never",
		"controlPlaneUrl":           controlPlaneURL,
		"allowInsecureControlPlane": true,
		"runnerCommand": []string{
			"/usr/local/bin/node",
			"/opt/synara/acceptance/provider-host-fixture.mjs",
		},
		"desiredWorkers":  desiredWorkers,
		"workspaceVolume": volumeName,
		"workspaceMount":  "/data",
		"workspaceRoot":   "/data/workspaces",
		"gitCacheRoot":    "/data/git-cache",
		"networkMode":     networkName,
		"user":            "10001:10001",
		"memoryBytes":     int64(256 << 20),
		"nanoCpus":        nanoCPUs,
	}
}

type orbStackWorkerAPI struct {
	executions        *executions.Service
	registrationToken string
	mu                sync.Mutex
	errors            []string
}

func newOrbStackWorkerAPI(
	service *executions.Service,
	registrationToken string,
) *orbStackWorkerAPI {
	return &orbStackWorkerAPI{executions: service, registrationToken: registrationToken}
}

func (a *orbStackWorkerAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/workers/register":
		if bearerToken(r) != a.registrationToken {
			a.writeError(w, problem.New(401, "invalid_worker_registration_token", "The worker registration token is invalid."))
			return
		}
		var input executions.RegisterWorkerInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			a.writeError(w, err)
			return
		}
		input.RegistrationTrustMode = executions.WorkerRegistrationTrustSharedToken
		registered, err := a.executions.Register(r.Context(), input)
		if err != nil {
			a.writeError(w, err)
			return
		}
		a.writeJSON(w, http.StatusCreated, registered)
	case "/v1/workers/heartbeat":
		worker, err := a.authenticate(r)
		if err != nil {
			a.writeError(w, err)
			return
		}
		var input executions.HeartbeatInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			a.writeError(w, err)
			return
		}
		value, err := a.executions.Heartbeat(r.Context(), worker, input)
		if err != nil {
			a.writeError(w, err)
			return
		}
		a.writeJSON(w, http.StatusOK, value)
	case "/v1/workers/executions/claim":
		worker, err := a.authenticate(r)
		if err != nil {
			a.writeError(w, err)
			return
		}
		var input executions.ClaimExecutionInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			a.writeError(w, err)
			return
		}
		result, err := a.executions.Claim(r.Context(), worker, input, requestID(r))
		if err != nil {
			a.writeError(w, err)
			return
		}
		a.writeJSON(w, http.StatusOK, result.Value)
	case "/v1/workers/workspace-cleanups/claim":
		worker, err := a.authenticate(r)
		if err != nil {
			a.writeError(w, err)
			return
		}
		var input executions.WorkspaceCleanupClaimInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			a.writeError(w, err)
			return
		}
		result, err := a.executions.ClaimWorkspaceCleanup(r.Context(), worker, input, requestID(r))
		if err != nil {
			a.writeError(w, err)
			return
		}
		a.writeJSON(w, http.StatusOK, result.Value)
	default:
		http.NotFound(w, r)
	}
}

func (a *orbStackWorkerAPI) authenticate(r *http.Request) (persistence.WorkerInstance, error) {
	return a.executions.Authenticate(r.Context(), bearerToken(r))
}

func (a *orbStackWorkerAPI) writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	message := err.Error()
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		status = apiError.Status
		code = apiError.Code
		message = apiError.Message
	}
	a.mu.Lock()
	a.errors = append(a.errors, code+":"+message)
	a.mu.Unlock()
	a.writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func (a *orbStackWorkerAPI) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *orbStackWorkerAPI) Errors() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.errors...)
}

func unexpectedOrbStackWorkerAPIErrors(values []string) []string {
	result := make([]string, 0)
	for _, value := range values {
		if strings.HasPrefix(value, "worker_reconciliation_draining:") {
			continue
		}
		result = append(result, value)
	}
	return result
}

func bearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
}

func requestID(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Request-ID")); value != "" {
		return value
	}
	return uuid.NewString()
}

func waitForManagedDockerWorkers(
	t *testing.T,
	db *gorm.DB,
	lifecycle *executions.Service,
	targetID uuid.UUID,
	containerNames []string,
	want int64,
	timeout time.Duration,
	api *orbStackWorkerAPI,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var count int64
		err := db.Model(&persistence.WorkerInstance{}).
			Where(
				"execution_target_id = ? AND target_kind = ? AND status = ? AND reconciliation_drain_requested_at IS NULL",
				targetID, "docker", "online",
			).
			Count(&count).Error
		if err == nil && count == want {
			ready, readyErr := lifecycle.ManagedDockerDesiredWorkersReady(
				context.Background(), targetID, containerNames, time.Now().UTC(),
			)
			if readyErr == nil && ready {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	containers := managedDockerContainerIDs(t, targetID)
	diagnostics := make(map[string]string, len(containers))
	for name := range containers {
		state, _ := dockerRunOptionalOutput("inspect", "--format", "{{.State.Status}} {{.State.ExitCode}} {{.State.Error}}", name)
		logs, _ := dockerRunOptionalOutput("logs", "--tail", "80", name)
		diagnostics[name] = strings.TrimSpace(state + "\n" + logs)
	}
	t.Fatalf(
		"managed Docker Workers did not become ready: errors=%#v containers=%#v diagnostics=%#v",
		api.Errors(), containers, diagnostics,
	)
}

func updateManagedDockerConfiguration(
	t *testing.T,
	db *gorm.DB,
	cipher *secret.CursorCipher,
	targetID uuid.UUID,
	configuration map[string]any,
) {
	t.Helper()
	encoded, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", targetID).
		Update("configuration_encrypted", encrypted).Error; err != nil {
		t.Fatal(err)
	}
}

func managedDockerContainerIDs(t *testing.T, targetID uuid.UUID) map[string]string {
	t.Helper()
	output := dockerRun(
		t,
		"ps", "--all",
		"--filter", "label=synara.io/execution-target-id="+targetID.String(),
		"--format", "{{.Names}}\t{{.ID}}",
	)
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			t.Fatalf("unexpected Docker container row %q", line)
		}
		result[parts[0]] = parts[1]
	}
	return result
}

func assertOneManagedDockerContainerChanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("managed Docker pool size changed: before=%#v after=%#v", before, after)
	}
	changed := 0
	for name, beforeID := range before {
		afterID, found := after[name]
		if !found {
			t.Fatalf("managed Docker container %s disappeared: before=%#v after=%#v", name, before, after)
		}
		if afterID != beforeID {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("managed Docker rolling pass changed %d containers: before=%#v after=%#v", changed, before, after)
	}
}

func equalManagedDockerContainerIDs(first, second map[string]string) bool {
	if len(first) != len(second) {
		return false
	}
	keys := make([]string, 0, len(first))
	for key := range first {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if second[key] != first[key] {
			return false
		}
	}
	return true
}

func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("docker", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func dockerRunOptional(args ...string) error {
	_, err := dockerRunOptionalOutput(args...)
	return err
}

func dockerRunOptionalOutput(args ...string) (string, error) {
	command := exec.Command("docker", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(output)), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output)), nil
}
