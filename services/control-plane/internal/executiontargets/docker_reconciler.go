package executiontargets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/workertiming"
)

const (
	dockerReconcilerAdvisoryLock = "synara:docker-worker-pool-reconciler"

	dockerManagedLabel         = "synara.io/managed"
	dockerTargetLabel          = "synara.io/execution-target-id"
	dockerConfigLabel          = "synara.io/config-sha256"
	dockerIndexLabel           = "synara.io/worker-index"
	dockerReleaseRevisionLabel = "synara.io/worker-release-revision-id"
	dockerReleaseChannelLabel  = "synara.io/worker-release-channel"
	dockerContainerSpecVersion = 6

	ManagedDockerDrainReasonStaleSpec = "managed-docker-stale-spec"
	ManagedDockerDrainReasonScaleDown = "managed-docker-scale-down"
)

type ManagedDockerWorkerDrainRequest struct {
	ExecutionTargetID uuid.UUID
	ContainerName     string
	Reason            string
	ObservedAt        time.Time
}

type ManagedDockerWorkerDrainDecision struct {
	WorkerFound           bool
	DeletionAllowed       bool
	ExecutionLeaseCount   int64
	WorkspaceCleanupCount int64
}

type ManagedDockerWorkerDrainState struct {
	ContainerName      string
	DrainInstanceUID   string
	CurrentInstanceUID string
}

// ManagedDockerWorkerLifecycleCoordinator owns the durable database half of
// a managed Docker replacement. The reconciler owns only the external Engine
// snapshot and mutation; both halves are deliberately joined through this
// fail-closed interface so a Control Plane restart cannot forget a Drain.
type ManagedDockerWorkerLifecycleCoordinator interface {
	RecoverMissingManagedDockerDrains(context.Context, uuid.UUID, []string, time.Time) (bool, error)
	ActiveManagedDockerDrain(context.Context, uuid.UUID) (*ManagedDockerWorkerDrainState, error)
	ManagedDockerDesiredWorkersReady(context.Context, uuid.UUID, []string, time.Time) (bool, error)
	PrepareManagedDockerDrain(context.Context, ManagedDockerWorkerDrainRequest) (ManagedDockerWorkerDrainDecision, error)
	FinalizeManagedDockerDrain(context.Context, ManagedDockerWorkerDrainRequest) error
	CompleteManagedDockerReplacement(context.Context, uuid.UUID, string, string, time.Time) (bool, error)
}

type DockerPoolReconcilerConfig struct {
	RegistrationToken     string
	PublicControlPlaneURL string
	WorkerLeaseTTL        time.Duration
	Interval              time.Duration
	Observer              BackgroundObserver
	ResolveImagePull      ImagePullCredentialResolver
	ObservabilityRoot     string
}

type BackgroundObserver interface {
	ObserveBackground(kind string, started time.Time, err error)
}

type dockerTargetConfiguration struct {
	SocketPath                string                         `json:"socketPath"`
	Image                     string                         `json:"image"`
	PullPolicy                string                         `json:"pullPolicy"`
	ControlPlaneURL           string                         `json:"controlPlaneUrl"`
	AllowInsecureControlPlane bool                           `json:"allowInsecureControlPlane"`
	RunnerCommand             []string                       `json:"runnerCommand"`
	DesiredWorkers            int                            `json:"desiredWorkers"`
	WorkspaceVolume           string                         `json:"workspaceVolume"`
	WorkspaceMount            string                         `json:"workspaceMount"`
	WorkspaceRoot             string                         `json:"workspaceRoot"`
	GitCacheRoot              string                         `json:"gitCacheRoot"`
	NetworkMode               string                         `json:"networkMode"`
	User                      string                         `json:"user"`
	MemoryBytes               int64                          `json:"memoryBytes"`
	NanoCPUs                  int64                          `json:"nanoCpus"`
	PIDsLimit                 int64                          `json:"pidsLimit"`
	RuntimeIsolation          *runtimeIsolationConfiguration `json:"runtimeIsolation"`
	RuntimeIsolationDecision  *runtimeIsolationDecision      `json:"-"`
}

type dockerContainer struct {
	ID     string
	Name   string
	State  string
	Labels map[string]string
}

type dockerContainerSpec struct {
	Name           string
	Image          string
	Environment    []string
	Entrypoint     []string
	Labels         map[string]string
	User           string
	WorkingDir     string
	Binds          []string
	ExtraHosts     []string
	NetworkMode    string
	MemoryBytes    int64
	NanoCPUs       int64
	PIDsLimit      int64
	Runtime        string
	CapDrop        []string
	SecurityOpt    []string
	ReadonlyRootfs bool
	Tmpfs          map[string]string
}

type dockerEngine interface {
	RuntimeNames(context.Context) ([]string, error)
	ListManaged(context.Context, uuid.UUID) ([]dockerContainer, error)
	EnsureImage(context.Context, string, string, *ImagePullCredential) error
	CreateAndStart(context.Context, dockerContainerSpec) (dockerContainer, error)
	Start(context.Context, string) error
	Remove(context.Context, string) error
}

type dockerEngineFactory interface {
	Open(string) (dockerEngine, error)
}

type DockerPoolReconciler struct {
	targets         *Service
	config          DockerPoolReconcilerConfig
	factory         dockerEngineFactory
	logger          *slog.Logger
	workerLifecycle ManagedDockerWorkerLifecycleCoordinator
	now             func() time.Time
}

func NewDockerPoolReconciler(
	targets *Service,
	config DockerPoolReconcilerConfig,
	logger *slog.Logger,
) *DockerPoolReconciler {
	reconciler := &DockerPoolReconciler{
		targets: targets, config: config, factory: dockerHTTPFactory{}, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}
	return reconciler
}

func (r *DockerPoolReconciler) SetWorkerLifecycleCoordinator(coordinator ManagedDockerWorkerLifecycleCoordinator) {
	r.workerLifecycle = coordinator
}

func (r *DockerPoolReconciler) Run(ctx context.Context) {
	interval := r.config.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		started := time.Now()
		err := r.ReconcileOnce(ctx)
		if r.config.Observer != nil {
			r.config.Observer.ObserveBackground("docker", started, err)
		}
		if err != nil && ctx.Err() == nil {
			r.logger.Error("docker worker pool reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *DockerPoolReconciler) ReconcileOnce(ctx context.Context) error {
	release, acquired, err := r.targets.tryDockerReconcilerLock(ctx)
	if err != nil {
		return problem.Wrap(500, "docker_reconciler_lock_failed", "Docker reconciler coordination failed.", err)
	}
	if !acquired {
		return nil
	}
	defer release()
	var targets []persistence.ExecutionTarget
	if err := r.targets.db.WithContext(ctx).
		Where("kind = ? AND status <> ?", "docker", "disabled").
		Order("id").Find(&targets).Error; err != nil {
		return problem.Wrap(500, "docker_targets_load_failed", "Docker execution targets could not be loaded.", err)
	}
	var failures []error
	for _, target := range targets {
		if err := r.reconcileTarget(ctx, target); err != nil {
			failures = append(failures, fmt.Errorf("target %s: %w", target.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) tryDockerReconcilerLock(ctx context.Context) (func(), bool, error) {
	if s.db.Dialector.Name() == "postgres" {
		return persistence.TryAdvisoryLock(ctx, s.db, dockerReconcilerAdvisoryLock)
	}
	if !s.dockerReconcilerLocalMu.TryLock() {
		return func() {}, false, nil
	}
	return s.dockerReconcilerLocalMu.Unlock, true, nil
}

func (r *DockerPoolReconciler) reconcileTarget(ctx context.Context, target persistence.ExecutionTarget) error {
	configuration, err := r.loadConfiguration(target)
	if err != nil {
		r.setStatus(ctx, target, "offline", false, "configuration_invalid", 0, 0)
		return err
	}
	engine, err := r.factory.Open(configuration.SocketPath)
	if err != nil {
		r.setStatus(ctx, target, "offline", false, "engine_unavailable", 0, 0)
		return problem.Wrap(503, "docker_engine_unavailable", "Docker Engine is unavailable.", err)
	}
	runtimeNames, err := engine.RuntimeNames(ctx)
	if err != nil {
		r.setStatus(ctx, target, "offline", false, "runtime_unavailable", 0, 0)
		return problem.Wrap(503, "docker_runtime_isolation_unavailable", "Docker runtime inventory is unavailable.", err)
	}
	gvisorCompatibilityErr := gvisorCompatibilityAccepted(
		ctx, r.targets.db, target, configuration.RuntimeIsolation,
	)
	runtimeDecision, err := resolveDockerRuntimeIsolation(
		configuration,
		filterDockerGVisorRuntimeNames(runtimeNames, gvisorCompatibilityErr),
	)
	err = preferGVisorCompatibilityError(&runtimeDecision, err, gvisorCompatibilityErr)
	runtimeObservedAt := r.now()
	if observationErr := persistRuntimeIsolationObservation(
		ctx, r.targets.db, target.ID, detectDockerRuntimeIsolationCapabilities(runtimeNames), &runtimeDecision, err, runtimeObservedAt,
	); observationErr != nil {
		r.setStatus(ctx, target, "offline", false, "runtime_observation_unavailable", 0, 0)
		return observationErr
	}
	if err != nil {
		r.setStatus(ctx, target, "offline", false, "runtime_unavailable", 0, 0)
		return err
	}
	configuration.RuntimeIsolationDecision = &runtimeDecision
	resolution, err := r.resolveImagePullCredential(ctx, target, configuration.Image)
	if err != nil {
		r.setStatus(ctx, target, "offline", false, "image_pull_credential_invalid", 0, 0)
		return err
	}
	credential := resolution.Credential
	specs, configHash, err := r.desiredSpecs(ctx, target, configuration, credential)
	if err != nil {
		r.setStatus(ctx, target, "offline", false, "configuration_invalid", 0, 0)
		return err
	}
	containers, err := engine.ListManaged(ctx, target.ID)
	if err != nil {
		r.setStatus(ctx, target, "offline", false, "list_failed", 0, 0)
		return problem.Wrap(502, "docker_containers_load_failed", "Docker Worker containers could not be listed.", err)
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	presentNames := make([]string, 0, len(containers))
	for _, container := range containers {
		presentNames = append(presentNames, container.Name)
	}
	observedAt := r.now()
	rolloutAdvanced := false
	if r.workerLifecycle != nil {
		recovered, err := r.workerLifecycle.RecoverMissingManagedDockerDrains(ctx, target.ID, presentNames, observedAt)
		if err != nil {
			return err
		}
		rolloutAdvanced = recovered
	}
	current := make(map[int]dockerContainer, len(specs))
	deferred := make(map[int]dockerContainer, len(specs))
	stale := make([]dockerContainer, 0)
	changed := false
	for _, container := range containers {
		index, indexErr := strconv.Atoi(container.Labels[dockerIndexLabel])
		validIndex := indexErr == nil && index >= 0 && index < len(specs)
		valid := validIndex && container.Labels[dockerConfigLabel] == configHash &&
			container.Name == specs[index].Name
		if valid {
			if _, duplicate := current[index]; !duplicate {
				current[index] = container
				continue
			}
		}
		stale = append(stale, container)
	}
	sortStaleForDrain(stale, specs)
	validNames := make([]string, 0, len(current))
	for _, container := range current {
		validNames = append(validNames, container.Name)
	}
	sort.Strings(validNames)

	selectedDrainName := ""
	if len(stale) > 0 && r.workerLifecycle == nil {
		return problem.New(503, "docker_worker_lifecycle_unavailable", "Managed Docker Worker lifecycle coordination is unavailable.")
	}
	if r.workerLifecycle != nil {
		activeDrain, err := r.workerLifecycle.ActiveManagedDockerDrain(ctx, target.ID)
		if err != nil {
			return err
		}
		if activeDrain != nil {
			if _, found := dockerContainerByName(current, activeDrain.ContainerName); found {
				completed, err := r.workerLifecycle.CompleteManagedDockerReplacement(
					ctx,
					target.ID,
					activeDrain.ContainerName,
					activeDrain.DrainInstanceUID,
					observedAt,
				)
				if err != nil {
					return err
				}
				if completed {
					activeDrain = nil
					rolloutAdvanced = true
				}
			}
			if activeDrain != nil {
				selectedDrainName = activeDrain.ContainerName
			}
		}
		if selectedDrainName == "" && len(stale) > 0 && !rolloutAdvanced {
			ready, err := r.workerLifecycle.ManagedDockerDesiredWorkersReady(ctx, target.ID, validNames, observedAt)
			if err != nil {
				return err
			}
			if ready {
				selectedDrainName = stale[0].Name
			}
		}
	}

	reserveDeferred := func(container dockerContainer, allowAny bool) {
		for index := range specs {
			if specs[index].Name != container.Name {
				continue
			}
			if _, occupied := current[index]; occupied {
				continue
			}
			if _, alreadyReserved := deferred[index]; !alreadyReserved {
				deferred[index] = container
				return
			}
		}
		for index := range specs {
			if _, occupied := current[index]; occupied {
				continue
			}
			if _, reserved := deferred[index]; reserved {
				continue
			}
			if dockerReleaseClassMatches(container.Labels, specs[index].Labels) {
				deferred[index] = container
				return
			}
		}
		if !allowAny {
			return
		}
		for index := range specs {
			if _, occupied := current[index]; occupied {
				continue
			}
			if _, alreadyReserved := deferred[index]; alreadyReserved {
				continue
			}
			deferred[index] = container
			return
		}
	}

	for _, container := range stale {
		if container.Name != selectedDrainName {
			// A rollout advances at most one stale logical Worker per cycle.
			// Reserve every other old container against an unoccupied desired
			// slot, even across a full naming/config transition, so the pass
			// cannot accidentally become an all-at-once replacement.
			reserveDeferred(container, !dockerReleaseClassIsDesired(container.Labels, specs))
			continue
		}
		request := ManagedDockerWorkerDrainRequest{
			ExecutionTargetID: target.ID,
			ContainerName:     container.Name,
			Reason:            dockerManagedDrainReason(container, specs),
			ObservedAt:        observedAt,
		}
		decision, err := r.workerLifecycle.PrepareManagedDockerDrain(ctx, request)
		if err != nil {
			return err
		}
		if !decision.DeletionAllowed {
			// A busy old Worker is now durably draining and therefore cannot
			// acquire another Execution or cleanup delivery. Keep its exact
			// container until both existing lease classes clear.
			reserveDeferred(container, false)
			continue
		}
		if err := engine.Remove(ctx, container.ID); err != nil {
			return problem.Wrap(502, "docker_container_remove_failed", "A stale Docker Worker could not be removed.", err)
		}
		if err := r.workerLifecycle.FinalizeManagedDockerDrain(ctx, request); err != nil {
			return err
		}
		changed = true
	}
	missing := make([]int, 0)
	for index := range specs {
		container, found := current[index]
		if !found {
			if _, waitingForLease := deferred[index]; waitingForLease {
				continue
			}
			missing = append(missing, index)
			continue
		}
		if container.State != "running" {
			if err := engine.Start(ctx, container.ID); err != nil {
				return problem.Wrap(502, "docker_container_start_failed", "A Docker Worker could not be started.", err)
			}
			container.State = "running"
			current[index] = container
			changed = true
		}
	}
	if len(missing) > 0 {
		ensured := make(map[string]struct{}, len(missing))
		for _, index := range missing {
			image := specs[index].Image
			if _, found := ensured[image]; !found {
				if err := engine.EnsureImage(ctx, image, configuration.PullPolicy, credential); err != nil {
					return problem.Wrap(502, "docker_image_unavailable", "The Docker Worker image is unavailable.", err)
				}
				ensured[image] = struct{}{}
			}
			created, err := engine.CreateAndStart(ctx, specs[index])
			if err != nil {
				return problem.Wrap(502, "docker_container_create_failed", "A Docker Worker could not be created.", err)
			}
			created.State = "running"
			current[index] = created
			changed = true
		}
	}
	running := 0
	for index := range specs {
		if container, found := current[index]; found && container.State == "running" {
			running++
			continue
		}
		if container, found := deferred[index]; found && container.State == "running" {
			running++
		}
	}
	status := "offline"
	if running == len(specs) {
		status = "active"
	}
	if err := r.setStatus(ctx, target, status, changed, "reconciled", len(specs), running); err != nil {
		return err
	}
	if status == "active" {
		finalObservedAt := r.now()
		var targetUpdatedAt time.Time
		if err := r.targets.db.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
			Where("id = ?", target.ID).Select("updated_at").Scan(&targetUpdatedAt).Error; err != nil {
			return problem.Wrap(500, "runtime_isolation_target_load_failed", "The reconciled Docker Target timestamp could not be loaded.", err)
		}
		if targetUpdatedAt.After(finalObservedAt) {
			finalObservedAt = targetUpdatedAt
		}
		if err := persistRuntimeIsolationObservation(
			ctx, r.targets.db, target.ID, detectDockerRuntimeIsolationCapabilities(runtimeNames),
			&runtimeDecision, nil, finalObservedAt,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *DockerPoolReconciler) loadConfiguration(target persistence.ExecutionTarget) (dockerTargetConfiguration, error) {
	if target.TenantID == nil {
		return dockerTargetConfiguration{}, problem.New(409, "docker_target_tenant_required", "Managed Docker targets must belong to a Tenant.")
	}
	if len(target.ConfigurationEncrypted) == 0 || r.targets.cipher == nil {
		return dockerTargetConfiguration{}, problem.New(409, "docker_configuration_missing", "Docker execution target configuration is missing.")
	}
	decoded, err := r.targets.cipher.Decrypt(target.ConfigurationEncrypted)
	if err != nil {
		return dockerTargetConfiguration{}, problem.Wrap(503, "docker_configuration_unavailable", "Docker execution target configuration could not be decrypted.", err)
	}
	var configuration dockerTargetConfiguration
	decoder := json.NewDecoder(strings.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker execution target configuration is invalid.")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker execution target configuration is invalid.")
	}
	return r.normalize(target, configuration)
}

var dockerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (r *DockerPoolReconciler) normalize(
	target persistence.ExecutionTarget,
	configuration dockerTargetConfiguration,
) (dockerTargetConfiguration, error) {
	configuration.SocketPath = strings.TrimSpace(configuration.SocketPath)
	if configuration.SocketPath == "" {
		configuration.SocketPath = "/var/run/docker.sock"
	}
	configuration.Image = strings.TrimSpace(configuration.Image)
	configuration.PullPolicy = strings.ToLower(strings.TrimSpace(configuration.PullPolicy))
	if configuration.PullPolicy == "" {
		configuration.PullPolicy = "if-not-present"
	}
	configuration.ControlPlaneURL = strings.TrimRight(strings.TrimSpace(configuration.ControlPlaneURL), "/")
	if configuration.ControlPlaneURL == "" {
		configuration.ControlPlaneURL = strings.TrimRight(strings.TrimSpace(r.config.PublicControlPlaneURL), "/")
	}
	if configuration.DesiredWorkers == 0 {
		configuration.DesiredWorkers = 1
	}
	configuration.WorkspaceVolume = strings.TrimSpace(configuration.WorkspaceVolume)
	if configuration.WorkspaceVolume == "" {
		configuration.WorkspaceVolume = "synara-agentd-" + target.ID.String()
	}
	configuration.WorkspaceMount = strings.TrimSpace(configuration.WorkspaceMount)
	if configuration.WorkspaceMount == "" {
		configuration.WorkspaceMount = "/data"
	}
	configuration.WorkspaceRoot = strings.TrimSpace(configuration.WorkspaceRoot)
	if configuration.WorkspaceRoot == "" {
		configuration.WorkspaceRoot = configuration.WorkspaceMount + "/workspaces"
	}
	configuration.GitCacheRoot = strings.TrimSpace(configuration.GitCacheRoot)
	if configuration.GitCacheRoot == "" {
		configuration.GitCacheRoot = configuration.WorkspaceMount + "/git-cache"
	}
	configuration.NetworkMode = strings.TrimSpace(configuration.NetworkMode)
	if configuration.NetworkMode == "" {
		configuration.NetworkMode = "bridge"
	}
	configuration.User = strings.TrimSpace(configuration.User)
	if configuration.User == "" {
		configuration.User = "10001:10001"
	}
	if !remotePathPattern.MatchString(configuration.SocketPath) || !remotePathPattern.MatchString(configuration.WorkspaceMount) ||
		!remotePathPattern.MatchString(configuration.WorkspaceRoot) || !remotePathPattern.MatchString(configuration.GitCacheRoot) ||
		strings.Contains(configuration.SocketPath, "..") || strings.Contains(configuration.WorkspaceMount, "..") ||
		strings.Contains(configuration.WorkspaceRoot, "..") || strings.Contains(configuration.GitCacheRoot, "..") ||
		(configuration.WorkspaceRoot != configuration.WorkspaceMount && !strings.HasPrefix(configuration.WorkspaceRoot, configuration.WorkspaceMount+"/")) ||
		(configuration.GitCacheRoot != configuration.WorkspaceMount && !strings.HasPrefix(configuration.GitCacheRoot, configuration.WorkspaceMount+"/")) ||
		configuration.WorkspaceRoot == configuration.GitCacheRoot || strings.HasPrefix(configuration.WorkspaceRoot, configuration.GitCacheRoot+"/") ||
		strings.HasPrefix(configuration.GitCacheRoot, configuration.WorkspaceRoot+"/") {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker socketPath, workspaceMount, workspaceRoot, and gitCacheRoot must be safe compatible absolute paths.")
	}
	if configuration.Image == "" || len(configuration.Image) > 512 || strings.ContainsAny(configuration.Image, "\r\n\t\x00") {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker image is required and must be valid.")
	}
	if configuration.PullPolicy != "always" && configuration.PullPolicy != "if-not-present" && configuration.PullPolicy != "never" {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker pullPolicy must be always, if-not-present, or never.")
	}
	if configuration.DesiredWorkers < 1 || configuration.DesiredWorkers > 100 {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker desiredWorkers must be between 1 and 100.")
	}
	if !dockerNamePattern.MatchString(configuration.WorkspaceVolume) || !dockerNamePattern.MatchString(configuration.NetworkMode) ||
		strings.ContainsAny(configuration.User, "\r\n\t\x00") {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker volume, networkMode, or user is invalid.")
	}
	if configuration.MemoryBytes != 0 && configuration.MemoryBytes < 64<<20 {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker memoryBytes must be at least 67108864 when configured.")
	}
	if configuration.NanoCPUs < 0 {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker nanoCpus must not be negative.")
	}
	if err := normalizeDockerRuntimeIsolationConfiguration(&configuration); err != nil {
		return dockerTargetConfiguration{}, err
	}
	if slices.Contains(configuration.RuntimeIsolation.Preferred, runtimeIsolationGVisor) ||
		configuration.RuntimeIsolation.Runtime == runtimeIsolationGVisor {
		_, _, numericNonRootUser := dockerNumericUser(configuration.User)
		if configuration.PIDsLimit == 0 {
			configuration.PIDsLimit = 256
		}
		if configuration.PIDsLimit < 1 || configuration.PIDsLimit > platform.MaximumKubernetesPIDsLimit ||
			configuration.MemoryBytes == 0 || configuration.NanoCPUs == 0 || !numericNonRootUser {
			return dockerTargetConfiguration{}, problem.New(
				400,
				"runtime_isolation_configuration_invalid",
				"Docker gVisor requires a numeric non-root user and finite PID, memory, and CPU limits.",
			)
		}
	}
	if len(configuration.RunnerCommand) == 0 || strings.TrimSpace(r.config.RegistrationToken) == "" {
		return dockerTargetConfiguration{}, problem.New(503, "docker_worker_configuration_unavailable", "Docker runnerCommand and Worker registration are required.")
	}
	for _, value := range configuration.RunnerCommand {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker runnerCommand is invalid.")
		}
	}
	parsedURL, err := url.Parse(configuration.ControlPlaneURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" ||
		(parsedURL.Scheme != "https" && !(parsedURL.Scheme == "http" && configuration.AllowInsecureControlPlane)) {
		return dockerTargetConfiguration{}, problem.New(400, "invalid_docker_configuration", "Docker controlPlaneUrl must use HTTPS unless allowInsecureControlPlane is explicitly enabled.")
	}
	return configuration, nil
}

func (r *DockerPoolReconciler) desiredSpecs(
	ctx context.Context,
	target persistence.ExecutionTarget,
	configuration dockerTargetConfiguration,
	credential *ImagePullCredential,
) ([]dockerContainerSpec, string, error) {
	controlPlaneURL, err := url.Parse(configuration.ControlPlaneURL)
	if err != nil {
		return nil, "", err
	}
	extraHosts := []string(nil)
	if strings.EqualFold(controlPlaneURL.Hostname(), "host.docker.internal") {
		extraHosts = []string{"host.docker.internal:host-gateway"}
	}
	runner, err := json.Marshal(configuration.RunnerCommand)
	if err != nil {
		return nil, "", err
	}
	capabilities, err := json.Marshal(target.Capabilities)
	if err != nil {
		return nil, "", err
	}
	baseEnvironment := []string{
		"SYNARA_CONTROL_PLANE_URL=" + configuration.ControlPlaneURL,
		"SYNARA_WORKER_REGISTRATION_TOKEN=" + r.config.RegistrationToken,
		"SYNARA_EXECUTION_TARGET_ID=" + target.ID.String(),
		"SYNARA_EXECUTION_TARGET_KIND=docker",
		"SYNARA_AGENTD_CLUSTER_ID=docker",
		"SYNARA_AGENTD_NAMESPACE=" + target.ID.String(),
		"SYNARA_AGENTD_CAPABILITIES_JSON=" + string(capabilities),
		"SYNARA_AGENTD_RUNNER_COMMAND_JSON=" + string(runner),
		"SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL=v2",
		"SYNARA_AGENTD_LEASE_RENEW_INTERVAL=" + workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL).String(),
		"SYNARA_AGENTD_DRAIN_TIMEOUT=20s",
		"SYNARA_AGENTD_WORKSPACE_ROOT=" + configuration.WorkspaceRoot,
		"SYNARA_AGENTD_GIT_CACHE_ROOT=" + configuration.GitCacheRoot,
	}
	observabilitySource := ""
	observabilityDestination := ""
	if root := strings.TrimSpace(r.config.ObservabilityRoot); root != "" {
		if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
			return nil, "", problem.New(500, "docker_worker_observability_authority_invalid", "Docker Worker observability root must be a non-root absolute directory.")
		}
		observabilitySource = filepath.Join(root, target.ID.String())
		observabilityDestination = "/etc/synara/targets/" + target.ID.String()
		baseEnvironment = append(
			baseEnvironment,
			"SYNARA_AGENTD_OBSERVABILITY_ENV_FILE="+observabilityDestination+"/observability.env",
		)
	}
	releasePlan, err := loadManagedReleasePlan(ctx, r.targets.db, target.ID, configuration.Image)
	if err != nil {
		return nil, "", err
	}
	slots, err := dockerReleaseSlots(configuration.DesiredWorkers, configuration.Image, releasePlan)
	if err != nil {
		return nil, "", err
	}
	for _, slot := range slots {
		if err := validateImagePullCredential(slot.Image, credential); err != nil {
			return nil, "", err
		}
	}
	hashPayload, err := json.Marshal(struct {
		SpecVersion       int
		Configuration     dockerTargetConfiguration
		RuntimeDecision   *runtimeIsolationDecision
		Capabilities      json.RawMessage
		TokenHash         [32]byte
		ReleasePlan       *managedReleasePlan
		LeaseRenew        time.Duration
		ObservabilityRoot string
	}{dockerContainerSpecVersion, configuration, configuration.RuntimeIsolationDecision, capabilities, sha256.Sum256([]byte(r.config.RegistrationToken)), releasePlan, workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL), strings.TrimSpace(r.config.ObservabilityRoot)})
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(hashPayload)
	configHash := hex.EncodeToString(digest[:])
	specs := make([]dockerContainerSpec, len(slots))
	for index, slot := range slots {
		name := fmt.Sprintf("synara-agentd-%s-%d", target.ID, index)
		if slot.Channel != "" {
			name = fmt.Sprintf("synara-agentd-%s-%s-%d", target.ID, slot.Channel, slot.Ordinal)
		}
		environment := append([]string(nil), baseEnvironment...)
		environment = append(environment, "SYNARA_AGENTD_INSTANCE_ID="+name)
		if digest := immutableImageDigest(slot.Image); digest != "" {
			environment = append(environment, "SYNARA_AGENTD_IMAGE_DIGEST="+digest)
		}
		labels := map[string]string{
			dockerManagedLabel: "true", dockerTargetLabel: target.ID.String(), dockerConfigLabel: configHash,
			dockerIndexLabel: strconv.Itoa(index), "synara.io/tenant-id": target.TenantID.String(),
		}
		if target.OrganizationID != nil {
			labels["synara.io/organization-id"] = target.OrganizationID.String()
		}
		if slot.RevisionID != uuid.Nil {
			labels[dockerReleaseRevisionLabel] = slot.RevisionID.String()
			labels[dockerReleaseChannelLabel] = slot.Channel
		}
		specs[index] = dockerContainerSpec{
			Name: name, Image: slot.Image, Environment: environment,
			Entrypoint: []string{"/usr/local/bin/synara-agentd"}, Labels: labels,
			User: configuration.User, WorkingDir: configuration.WorkspaceMount,
			Binds: []string{configuration.WorkspaceVolume + ":" + configuration.WorkspaceMount}, ExtraHosts: extraHosts,
			NetworkMode: configuration.NetworkMode, MemoryBytes: configuration.MemoryBytes, NanoCPUs: configuration.NanoCPUs,
		}
		if observabilitySource != "" {
			specs[index].Binds = append(
				specs[index].Binds,
				observabilitySource+":"+observabilityDestination+":ro",
			)
		}
		if configuration.RuntimeIsolationDecision != nil &&
			configuration.RuntimeIsolationDecision.EffectiveRuntime == runtimeIsolationGVisor {
			uid, gid, _ := dockerNumericUser(configuration.User)
			specs[index].Runtime = "runsc"
			specs[index].PIDsLimit = configuration.PIDsLimit
			specs[index].CapDrop = []string{"ALL"}
			specs[index].SecurityOpt = []string{"no-new-privileges:true"}
			specs[index].ReadonlyRootfs = true
			specs[index].Tmpfs = map[string]string{
				"/tmp":         "rw,nosuid,nodev,noexec,size=64m,mode=1777",
				"/home/synara": "rw,nosuid,nodev,size=64m,mode=0700,uid=" + uid + ",gid=" + gid,
			}
		}
	}
	return specs, configHash, nil
}

func dockerNumericUser(value string) (string, string, bool) {
	parts := strings.Split(value, ":")
	if len(parts) > 2 || len(parts) == 0 {
		return "", "", false
	}
	uid, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || uid == 0 {
		return "", "", false
	}
	gid := uid
	if len(parts) == 2 {
		parsed, parseErr := strconv.ParseUint(parts[1], 10, 32)
		if parseErr != nil {
			return "", "", false
		}
		gid = parsed
	}
	return strconv.FormatUint(uid, 10), strconv.FormatUint(gid, 10), true
}

type dockerReleaseSlot struct {
	Image      string
	RevisionID uuid.UUID
	Channel    string
	Ordinal    int
}

func dockerReleaseSlots(desiredWorkers int, baseImage string, plan *managedReleasePlan) ([]dockerReleaseSlot, error) {
	if plan == nil {
		slots := make([]dockerReleaseSlot, desiredWorkers)
		for index := range slots {
			slots[index] = dockerReleaseSlot{Image: baseImage, Ordinal: index}
		}
		return slots, nil
	}
	promotedCount := desiredWorkers
	canaryCount := 0
	if plan.Canary != nil {
		if desiredWorkers < 2 {
			return nil, problem.New(
				409,
				"docker_worker_release_canary_capacity_insufficient",
				"Managed Docker canary releases require at least two desired Workers.",
			)
		}
		canaryCount = (desiredWorkers*plan.CanaryPercent + 99) / 100
		if canaryCount < 1 {
			canaryCount = 1
		}
		if canaryCount >= desiredWorkers {
			canaryCount = desiredWorkers - 1
		}
		promotedCount = desiredWorkers - canaryCount
	}
	slots := make([]dockerReleaseSlot, 0, promotedCount+canaryCount)
	for index := 0; index < promotedCount; index++ {
		slots = append(slots, dockerReleaseSlot{
			Image: plan.Promoted.Image, RevisionID: plan.Promoted.RevisionID,
			Channel: plan.Promoted.Channel, Ordinal: index,
		})
	}
	if plan.Canary != nil {
		for index := 0; index < canaryCount; index++ {
			slots = append(slots, dockerReleaseSlot{
				Image: plan.Canary.Image, RevisionID: plan.Canary.RevisionID,
				Channel: plan.Canary.Channel, Ordinal: index,
			})
		}
	}
	return slots, nil
}

func dockerReleaseClassMatches(current, desired map[string]string) bool {
	return current[dockerReleaseRevisionLabel] == desired[dockerReleaseRevisionLabel] &&
		current[dockerReleaseChannelLabel] == desired[dockerReleaseChannelLabel]
}

func (r *DockerPoolReconciler) resolveImagePullCredential(
	ctx context.Context,
	target persistence.ExecutionTarget,
	image string,
) (ImagePullCredentialResolution, error) {
	if r.config.ResolveImagePull == nil {
		return ImagePullCredentialResolution{Authoritative: true}, nil
	}
	if target.TenantID == nil {
		return ImagePullCredentialResolution{Authoritative: true}, problem.New(409, "docker_target_tenant_required", "Managed Docker targets must belong to a Tenant.")
	}
	authority, err := registryAuthorityFromImageReference(image)
	if err != nil {
		return ImagePullCredentialResolution{Authoritative: true}, err
	}
	return r.config.ResolveImagePull(ctx, *target.TenantID, target.ID, registryComparisonAuthority(authority))
}

func (r *DockerPoolReconciler) setStatus(
	ctx context.Context,
	target persistence.ExecutionTarget,
	status string,
	changed bool,
	reason string,
	desired, running int,
) error {
	statusChanged := target.Status != status
	if !changed && !statusChanged {
		return nil
	}
	if target.TenantID == nil {
		return problem.New(409, "docker_target_tenant_required", "Managed Docker targets must belong to a Tenant.")
	}
	return persistence.InTransaction(ctx, r.targets.db, func(tx *gorm.DB) error {
		if statusChanged {
			result := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
				Where("id = ? AND kind = ? AND status <> ?", target.ID, "docker", "disabled").
				Update("status", status)
			if result.Error != nil {
				return result.Error
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: *target.TenantID, ActorType: "system",
			Action: "execution_target.docker_reconciled", ResourceType: "execution_target", ResourceID: &target.ID,
			OrganizationID: target.OrganizationID, RequestID: "docker-reconciler:" + uuid.NewString(),
			Metadata: map[string]any{"status": status, "reason": reason, "desiredWorkers": desired, "runningWorkers": running},
		})
	})
}

type dockerHTTPFactory struct{}

func (dockerHTTPFactory) Open(socketPath string) (dockerEngine, error) {
	if !remotePathPattern.MatchString(socketPath) || strings.Contains(socketPath, "..") {
		return nil, errors.New("Docker socket path is invalid")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &dockerHTTPEngine{client: &http.Client{Transport: transport, Timeout: 2 * time.Minute}}, nil
}

type dockerHTTPEngine struct {
	client *http.Client
}

func (e *dockerHTTPEngine) RuntimeNames(ctx context.Context) ([]string, error) {
	var response struct {
		DefaultRuntime string                     `json:"DefaultRuntime"`
		Runtimes       map[string]json.RawMessage `json:"Runtimes"`
	}
	if err := e.do(ctx, http.MethodGet, "/info", nil, &response, http.StatusOK); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(response.Runtimes)+1)
	for name := range response.Runtimes {
		if name = strings.TrimSpace(name); name != "" {
			seen[name] = struct{}{}
		}
	}
	if name := strings.TrimSpace(response.DefaultRuntime); name != "" {
		seen[name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (e *dockerHTTPEngine) ListManaged(ctx context.Context, targetID uuid.UUID) ([]dockerContainer, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {dockerTargetLabel + "=" + targetID.String()}})
	query := url.Values{"all": {"1"}, "filters": {string(filters)}}
	var response []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if err := e.do(ctx, http.MethodGet, "/containers/json?"+query.Encode(), nil, &response, http.StatusOK); err != nil {
		return nil, err
	}
	items := make([]dockerContainer, 0, len(response))
	for _, item := range response {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		items = append(items, dockerContainer{ID: item.ID, Name: name, State: item.State, Labels: item.Labels})
	}
	return items, nil
}

func (e *dockerHTTPEngine) EnsureImage(
	ctx context.Context,
	image, policy string,
	credential *ImagePullCredential,
) error {
	needPull := policy == "always"
	if policy == "if-not-present" {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/images/"+url.PathEscape(image)+"/json", nil)
		if err != nil {
			return err
		}
		response, err := e.client.Do(request)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		needPull = response.StatusCode == http.StatusNotFound
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNotFound {
			return fmt.Errorf("Docker image inspect returned %d", response.StatusCode)
		}
	}
	if policy == "never" || !needPull {
		return nil
	}
	query := url.Values{"fromImage": {image}}
	headers := http.Header{}
	if credential != nil {
		auth, err := dockerRegistryAuthHeader(credential)
		if err != nil {
			return err
		}
		headers.Set("X-Registry-Auth", auth)
	}
	return e.doWithHeaders(ctx, http.MethodPost, "/images/create?"+query.Encode(), nil, nil, headers, http.StatusOK)
}

func dockerRegistryAuthHeader(credential *ImagePullCredential) (string, error) {
	if credential == nil {
		return "", nil
	}
	payload := struct {
		Username      string `json:"username,omitempty"`
		Password      string `json:"password,omitempty"`
		ServerAddress string `json:"serveraddress"`
		RegistryToken string `json:"registrytoken,omitempty"`
	}{
		Username: credential.Username, Password: credential.Password,
		RegistryToken: credential.RegistryToken,
	}
	authority, err := normalizeRegistryAuthority(credential.Host)
	if err != nil {
		return "", err
	}
	payload.ServerAddress = registryAuthServerAddress(authority)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(encoded), nil
}

func (e *dockerHTTPEngine) CreateAndStart(ctx context.Context, spec dockerContainerSpec) (dockerContainer, error) {
	requestBody := struct {
		Image      string            `json:"Image"`
		Env        []string          `json:"Env"`
		Entrypoint []string          `json:"Entrypoint"`
		Labels     map[string]string `json:"Labels"`
		User       string            `json:"User"`
		WorkingDir string            `json:"WorkingDir"`
		HostConfig struct {
			Binds          []string          `json:"Binds"`
			ExtraHosts     []string          `json:"ExtraHosts,omitempty"`
			NetworkMode    string            `json:"NetworkMode"`
			Memory         int64             `json:"Memory"`
			NanoCPUs       int64             `json:"NanoCpus"`
			PIDsLimit      int64             `json:"PidsLimit,omitempty"`
			Runtime        string            `json:"Runtime,omitempty"`
			CapDrop        []string          `json:"CapDrop,omitempty"`
			SecurityOpt    []string          `json:"SecurityOpt,omitempty"`
			ReadonlyRootfs bool              `json:"ReadonlyRootfs,omitempty"`
			Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
			RestartPolicy  struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
	}{Image: spec.Image, Env: spec.Environment, Entrypoint: spec.Entrypoint, Labels: spec.Labels, User: spec.User, WorkingDir: spec.WorkingDir}
	requestBody.HostConfig.Binds = spec.Binds
	requestBody.HostConfig.ExtraHosts = spec.ExtraHosts
	requestBody.HostConfig.NetworkMode = spec.NetworkMode
	requestBody.HostConfig.Memory = spec.MemoryBytes
	requestBody.HostConfig.NanoCPUs = spec.NanoCPUs
	requestBody.HostConfig.PIDsLimit = spec.PIDsLimit
	requestBody.HostConfig.Runtime = spec.Runtime
	requestBody.HostConfig.CapDrop = spec.CapDrop
	requestBody.HostConfig.SecurityOpt = spec.SecurityOpt
	requestBody.HostConfig.ReadonlyRootfs = spec.ReadonlyRootfs
	requestBody.HostConfig.Tmpfs = spec.Tmpfs
	requestBody.HostConfig.RestartPolicy.Name = "unless-stopped"
	var response struct {
		ID string `json:"Id"`
	}
	query := url.Values{"name": {spec.Name}}
	if err := e.do(ctx, http.MethodPost, "/containers/create?"+query.Encode(), requestBody, &response, http.StatusCreated); err != nil {
		return dockerContainer{}, err
	}
	if err := e.Start(ctx, response.ID); err != nil {
		_ = e.Remove(context.WithoutCancel(ctx), response.ID)
		return dockerContainer{}, err
	}
	return dockerContainer{ID: response.ID, Name: spec.Name, State: "running", Labels: spec.Labels}, nil
}

func (e *dockerHTTPEngine) Start(ctx context.Context, id string) error {
	return e.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, http.StatusNoContent, http.StatusNotModified)
}

func (e *dockerHTTPEngine) Remove(ctx context.Context, id string) error {
	_ = e.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop?t=30", nil, nil, http.StatusNoContent, http.StatusNotModified, http.StatusNotFound)
	return e.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=true&v=true", nil, nil, http.StatusNoContent, http.StatusNotFound)
}

func (e *dockerHTTPEngine) do(
	ctx context.Context,
	method, path string,
	input, output any,
	accepted ...int,
) error {
	return e.doWithHeaders(ctx, method, path, input, output, nil, accepted...)
}

func (e *dockerHTTPEngine) doWithHeaders(
	ctx context.Context,
	method, path string,
	input, output any,
	headers http.Header,
	accepted ...int,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := e.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	acceptedStatus := false
	for _, status := range accepted {
		if response.StatusCode == status {
			acceptedStatus = true
			break
		}
	}
	if !acceptedStatus {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("Docker Engine returned status %d", response.StatusCode)
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output)
}

// sortStaleForDrain orders stale containers so overhanging indices (scale-down
// survivors) drain before config-mismatched containers that still occupy a
// valid logical Worker slot. Within each group higher indices drain first
// so the Reconciler converges in the fewest cycles.
func sortStaleForDrain(stale []dockerContainer, specs []dockerContainerSpec) {
	sort.Slice(stale, func(i, j int) bool {
		iIndex, iErr := strconv.Atoi(stale[i].Labels[dockerIndexLabel])
		jIndex, jErr := strconv.Atoi(stale[j].Labels[dockerIndexLabel])
		iOverhanging := iErr != nil || iIndex < 0 || iIndex >= len(specs)
		jOverhanging := jErr != nil || jIndex < 0 || jIndex >= len(specs)
		if iOverhanging != jOverhanging {
			return iOverhanging
		}
		if iIndex != jIndex {
			return iIndex > jIndex
		}
		return stale[i].Name < stale[j].Name
	})
}

func dockerContainerByName(containers map[int]dockerContainer, name string) (dockerContainer, bool) {
	for _, container := range containers {
		if container.Name == name {
			return container, true
		}
	}
	return dockerContainer{}, false
}

func dockerManagedDrainReason(container dockerContainer, specs []dockerContainerSpec) string {
	for _, spec := range specs {
		if spec.Name == container.Name {
			return ManagedDockerDrainReasonStaleSpec
		}
	}
	return ManagedDockerDrainReasonScaleDown
}

func dockerReleaseClassIsDesired(current map[string]string, specs []dockerContainerSpec) bool {
	for _, spec := range specs {
		if dockerReleaseClassMatches(current, spec.Labels) {
			return true
		}
	}
	return false
}
