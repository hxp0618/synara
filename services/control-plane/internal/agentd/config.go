package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Config struct {
	ControlPlaneURL              *url.URL
	RegistrationToken            string
	RegistrationTokenFile        string
	ExecutionTargetID            uuid.UUID
	AssignedExecutionID          *uuid.UUID
	WorkerMode                   string
	TargetKind                   platform.ExecutionTargetKind
	ClusterID                    string
	Namespace                    string
	PodName                      string
	InstanceUID                  string
	SSHBootstrapGeneration       *int64
	Version                      string
	BuildGitSHA                  string
	ImageDigest                  string
	WorkerImageManifest          *workerImageManifest
	Capabilities                 map[string]any
	ExperimentalProviders        []string
	RunnerCommand                []string
	RunnerProtocol               RunnerProtocol
	CgroupV2Root                 string
	CgroupV2ProviderIdentity     *ProtectedCgroupIdentity
	CgroupV2Attestation          *ProtectedCgroupAttestationConfig
	ProcessContainmentCapability map[string]any
	WorkspaceRoot                string
	GitCacheRoot                 string
	PollInterval                 time.Duration
	HeartbeatInterval            time.Duration
	LeaseRenewInterval           time.Duration
	DrainTimeout                 time.Duration
	RequestTimeout               time.Duration
	ArtifactTimeout              time.Duration
	RunnerMessageBytes           int
}

var stage3ProviderNames = providercatalog.ProviderNames()

func LoadConfig() (Config, error) {
	rawURL := strings.TrimSpace(os.Getenv("SYNARA_CONTROL_PLANE_URL"))
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return Config{}, errors.New("SYNARA_CONTROL_PLANE_URL must be an HTTP(S) origin")
	}
	targetID, err := uuid.Parse(strings.TrimSpace(os.Getenv("SYNARA_EXECUTION_TARGET_ID")))
	if err != nil {
		return Config{}, errors.New("SYNARA_EXECUTION_TARGET_ID must be a UUID")
	}
	targetKind, err := platform.ParseExecutionTargetKind(os.Getenv("SYNARA_EXECUTION_TARGET_KIND"))
	if err != nil {
		return Config{}, fmt.Errorf("SYNARA_EXECUTION_TARGET_KIND: %w", err)
	}
	var sshBootstrapGeneration *int64
	rawSSHBootstrapGeneration := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION"))
	if targetKind == platform.TargetSSH {
		value, parseErr := strconv.ParseInt(rawSSHBootstrapGeneration, 10, 64)
		if parseErr != nil || value <= 0 {
			return Config{}, errors.New("SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION must be a positive integer for SSH workers")
		}
		sshBootstrapGeneration = &value
	} else if rawSSHBootstrapGeneration != "" {
		return Config{}, errors.New("SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION is only valid for SSH workers")
	}
	runnerCommand, err := validation.CommandJSON(os.Getenv("SYNARA_AGENTD_RUNNER_COMMAND_JSON"))
	if err != nil {
		return Config{}, fmt.Errorf("SYNARA_AGENTD_RUNNER_COMMAND_JSON: %w", err)
	}
	runnerProtocol, err := parseRunnerProtocol(os.Getenv("SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL"))
	if err != nil {
		return Config{}, err
	}
	cgroupV2Root, err := parseCgroupV2Root(os.Getenv("SYNARA_AGENTD_CGROUP_V2_ROOT"))
	if err != nil {
		return Config{}, err
	}
	cgroupV2ProviderIdentity, err := parseProtectedCgroupProviderIdentity(
		cgroupV2Root,
		os.Getenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID"),
		os.Getenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID"),
	)
	if err != nil {
		return Config{}, err
	}
	cgroupV2Attestation, err := parseProtectedCgroupAttestationConfig(
		cgroupV2Root,
		cgroupV2ProviderIdentity,
		os.Getenv("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID"),
		os.Getenv("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE"),
	)
	if err != nil {
		return Config{}, err
	}
	capabilities := map[string]any{}
	if raw := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_CAPABILITIES_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &capabilities); err != nil || capabilities == nil {
			return Config{}, errors.New("SYNARA_AGENTD_CAPABILITIES_JSON must be a JSON object")
		}
	}
	delete(capabilities, resourceSuspendContainmentCapabilityKey)
	if err := validateWorkerImageBuildFeatureFlagReservation(capabilities); err != nil {
		return Config{}, err
	}
	experimentalProviders, err := parseExperimentalProviders(capabilities)
	if err != nil {
		return Config{}, err
	}
	workerImageManifest, err := loadConfiguredWorkerImageManifest()
	if err != nil {
		return Config{}, err
	}
	version, buildGitSHA, err := resolveWorkerBuildIdentity(
		os.Getenv("SYNARA_AGENTD_VERSION"),
		os.Getenv("SYNARA_AGENTD_BUILD_GIT_SHA"),
		workerImageManifest,
	)
	if err != nil {
		return Config{}, err
	}
	workspaceRoot := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_WORKSPACE_ROOT"))
	if workspaceRoot == "" {
		workspaceRoot = "./data/workspaces"
	}
	workspaceRoot, err = filepath.Abs(workspaceRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve SYNARA_AGENTD_WORKSPACE_ROOT: %w", err)
	}
	gitCacheRoot := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_GIT_CACHE_ROOT"))
	if gitCacheRoot == "" {
		gitCacheRoot = filepath.Join(filepath.Dir(workspaceRoot), "git-cache")
	}
	gitCacheRoot, err = filepath.Abs(gitCacheRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve SYNARA_AGENTD_GIT_CACHE_ROOT: %w", err)
	}
	workspaceRoot, gitCacheRoot, err = validateWorkspaceRoots(workspaceRoot, gitCacheRoot)
	if err != nil {
		return Config{}, err
	}
	instanceUID := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_INSTANCE_UID"))
	if instanceUID == "" {
		if targetKind == platform.TargetKubernetes {
			return Config{}, errors.New("SYNARA_AGENTD_INSTANCE_UID is required for Kubernetes workers")
		}
		instanceUID = uuid.NewString()
	}
	parsedInstanceUID, err := uuid.Parse(instanceUID)
	if err != nil || parsedInstanceUID == uuid.Nil {
		return Config{}, errors.New("SYNARA_AGENTD_INSTANCE_UID is invalid")
	}
	instanceUID = parsedInstanceUID.String()
	registrationToken := strings.TrimSpace(os.Getenv("SYNARA_WORKER_REGISTRATION_TOKEN"))
	registrationTokenFile := strings.TrimSpace(os.Getenv("SYNARA_WORKER_REGISTRATION_TOKEN_FILE"))
	if registrationToken != "" && registrationTokenFile != "" {
		return Config{}, errors.New("configure only one of SYNARA_WORKER_REGISTRATION_TOKEN and SYNARA_WORKER_REGISTRATION_TOKEN_FILE")
	}
	if registrationTokenFile != "" {
		if targetKind != platform.TargetKubernetes || !filepath.IsAbs(registrationTokenFile) {
			return Config{}, errors.New("SYNARA_WORKER_REGISTRATION_TOKEN_FILE must be an absolute path for a Kubernetes worker")
		}
		contents, readErr := os.ReadFile(filepath.Clean(registrationTokenFile))
		if readErr != nil {
			return Config{}, fmt.Errorf("read SYNARA_WORKER_REGISTRATION_TOKEN_FILE: %w", readErr)
		}
		registrationToken = strings.TrimSpace(string(contents))
	}
	cfg := Config{
		ControlPlaneURL: parsedURL, RegistrationToken: registrationToken,
		RegistrationTokenFile: registrationTokenFile,
		ExecutionTargetID:     targetID, TargetKind: targetKind,
		SSHBootstrapGeneration: sshBootstrapGeneration,
		ClusterID:              envDefault("SYNARA_AGENTD_CLUSTER_ID", "local"), Namespace: envDefault("SYNARA_AGENTD_NAMESPACE", "default"),
		PodName: envDefault("SYNARA_AGENTD_INSTANCE_ID", hostname()), InstanceUID: instanceUID,
		Version: version, BuildGitSHA: buildGitSHA,
		ImageDigest:         strings.TrimSpace(os.Getenv("SYNARA_AGENTD_IMAGE_DIGEST")),
		WorkerImageManifest: workerImageManifest,
		Capabilities:        capabilities, ExperimentalProviders: experimentalProviders,
		RunnerCommand: runnerCommand, RunnerProtocol: runnerProtocol,
		CgroupV2Root:             cgroupV2Root,
		CgroupV2ProviderIdentity: cgroupV2ProviderIdentity,
		CgroupV2Attestation:      cgroupV2Attestation,
		WorkspaceRoot:            workspaceRoot, GitCacheRoot: gitCacheRoot,
	}
	if raw := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID")); raw != "" {
		assignedExecutionID, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			return Config{}, errors.New("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID must be a UUID")
		}
		cfg.AssignedExecutionID = &assignedExecutionID
	}
	cfg.WorkerMode, err = resolveConfiguredWorkerMode(
		os.Getenv("SYNARA_AGENTD_WORKER_MODE"),
		cfg.AssignedExecutionID,
	)
	if err != nil {
		return Config{}, err
	}
	if cfg.RegistrationToken == "" {
		return Config{}, errors.New("a Worker registration token or token file is required")
	}
	if cfg.PollInterval, err = durationEnv("SYNARA_AGENTD_POLL_INTERVAL", time.Second); err != nil {
		return Config{}, err
	}
	if cfg.HeartbeatInterval, err = durationEnv("SYNARA_AGENTD_HEARTBEAT_INTERVAL", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.LeaseRenewInterval, err = durationEnv("SYNARA_AGENTD_LEASE_RENEW_INTERVAL", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.DrainTimeout, err = durationEnv("SYNARA_AGENTD_DRAIN_TIMEOUT", 20*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = durationEnv("SYNARA_AGENTD_REQUEST_TIMEOUT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ArtifactTimeout, err = durationEnv("SYNARA_AGENTD_ARTIFACT_TIMEOUT", 30*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.RunnerMessageBytes, err = intEnv("SYNARA_AGENTD_RUNNER_MESSAGE_BYTES", 1<<20); err != nil {
		return Config{}, err
	}
	if cfg.PollInterval <= 0 || cfg.HeartbeatInterval <= 0 || cfg.LeaseRenewInterval <= 0 || cfg.DrainTimeout <= 0 || cfg.RequestTimeout <= 0 || cfg.ArtifactTimeout <= 0 || cfg.RunnerMessageBytes < 1024 {
		return Config{}, errors.New("agentd intervals, request timeout, and runner message limit must be positive")
	}
	if cfg.ImageDigest != "" && !validImageDigest(cfg.ImageDigest) {
		return Config{}, errors.New("agentd image digest is invalid")
	}
	return cfg, nil
}

func effectiveWorkerMode(cfg Config) string {
	mode, err := resolveConfiguredWorkerMode(cfg.WorkerMode, cfg.AssignedExecutionID)
	if err != nil {
		if cfg.AssignedExecutionID != nil {
			return executions.WorkerModeExecutionPinned
		}
		return executions.WorkerModeGeneralPool
	}
	return mode
}

func resolveConfiguredWorkerMode(raw string, assignedExecutionID *uuid.UUID) (string, error) {
	mode := strings.TrimSpace(raw)
	if mode == "" {
		if assignedExecutionID != nil {
			return executions.WorkerModeExecutionPinned, nil
		}
		return executions.WorkerModeGeneralPool, nil
	}
	switch mode {
	case executions.WorkerModeExecutionPinned:
		if assignedExecutionID == nil {
			return "", errors.New("SYNARA_AGENTD_WORKER_MODE=execution-pinned requires SYNARA_AGENTD_ASSIGNED_EXECUTION_ID")
		}
	case executions.WorkerModeWarmPool, executions.WorkerModeGeneralPool:
		if assignedExecutionID != nil {
			return "", fmt.Errorf("SYNARA_AGENTD_WORKER_MODE=%s cannot be combined with SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", mode)
		}
	default:
		return "", errors.New("SYNARA_AGENTD_WORKER_MODE must be execution-pinned, warm-pool, or general-pool")
	}
	return mode, nil
}

func parseCgroupV2Root(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if runtime.GOOS != "linux" {
		return "", errors.New("SYNARA_AGENTD_CGROUP_V2_ROOT is only supported on Linux workers")
	}
	if !filepath.IsAbs(trimmed) {
		return "", errors.New("SYNARA_AGENTD_CGROUP_V2_ROOT must be an absolute path")
	}
	normalized := filepath.Clean(trimmed)
	if normalized == string(filepath.Separator) {
		return "", errors.New("SYNARA_AGENTD_CGROUP_V2_ROOT must point at a delegated subtree, not /")
	}
	return normalized, nil
}

func parseProtectedCgroupProviderIdentity(
	cgroupV2Root, uidValue, gidValue string,
) (*ProtectedCgroupIdentity, error) {
	uidValue = strings.TrimSpace(uidValue)
	gidValue = strings.TrimSpace(gidValue)
	if uidValue == "" && gidValue == "" {
		return nil, nil
	}
	if runtime.GOOS != "linux" {
		return nil, errors.New(
			"SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID and SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID are only supported on Linux workers",
		)
	}
	if uidValue == "" || gidValue == "" {
		return nil, errors.New(
			"configure both SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID and SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID together",
		)
	}
	if strings.TrimSpace(cgroupV2Root) == "" {
		return nil, errors.New(
			"SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID and SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID require SYNARA_AGENTD_CGROUP_V2_ROOT",
		)
	}
	uid, err := parseProtectedCgroupIdentityComponent("SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID", uidValue)
	if err != nil {
		return nil, err
	}
	gid, err := parseProtectedCgroupIdentityComponent("SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID", gidValue)
	if err != nil {
		return nil, err
	}
	return &ProtectedCgroupIdentity{UID: uid, GID: gid}, nil
}

func parseProtectedCgroupAttestationConfig(
	cgroupV2Root string,
	providerIdentity *ProtectedCgroupIdentity,
	keyIDValue, keyPathValue string,
) (*ProtectedCgroupAttestationConfig, error) {
	keyIDValue = strings.TrimSpace(keyIDValue)
	keyPathValue = strings.TrimSpace(keyPathValue)
	if keyIDValue == "" && keyPathValue == "" {
		return nil, nil
	}
	if runtime.GOOS != "linux" {
		return nil, errors.New(
			"SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID and SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE are only supported on Linux workers",
		)
	}
	if keyIDValue == "" || keyPathValue == "" {
		return nil, errors.New(
			"configure both SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID and SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE together",
		)
	}
	if strings.TrimSpace(cgroupV2Root) == "" || providerIdentity == nil {
		return nil, errors.New(
			"SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID and SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE require protected cgroup Provider identity configuration",
		)
	}
	if !filepath.IsAbs(keyPathValue) {
		return nil, errors.New("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE must be an absolute path")
	}
	keyIDValue = strings.TrimSpace(keyIDValue)
	if len(keyIDValue) == 0 || len(keyIDValue) > 160 {
		return nil, errors.New("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID length is invalid")
	}
	for index := 0; index < len(keyIDValue); index++ {
		if keyIDValue[index] < 0x21 || keyIDValue[index] > 0x7e {
			return nil, errors.New("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID must use safe ASCII without whitespace")
		}
	}
	return &ProtectedCgroupAttestationConfig{
		KeyID:          keyIDValue,
		PrivateKeyPath: filepath.Clean(keyPathValue),
	}, nil
}

func parseProtectedCgroupIdentityComponent(name, value string) (uint32, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be a uint32: %w", name, err)
	}
	return uint32(parsed), nil
}

func parseExperimentalProviders(capabilities map[string]any) ([]string, error) {
	rawPolicy, found := capabilities["providerPolicy"]
	if !found {
		return nil, nil
	}
	policy, ok := rawPolicy.(map[string]any)
	if !ok || policy == nil {
		return nil, errors.New("SYNARA_AGENTD_CAPABILITIES_JSON providerPolicy must be a JSON object")
	}
	rawProviders, found := policy["experimentalProviders"]
	if !found {
		return nil, nil
	}
	values, ok := rawProviders.([]any)
	if !ok {
		if typed, typedOK := rawProviders.([]string); typedOK {
			values = make([]any, len(typed))
			for index := range typed {
				values[index] = typed[index]
			}
		} else {
			return nil, errors.New("SYNARA_AGENTD_CAPABILITIES_JSON providerPolicy.experimentalProviders must be an array")
		}
	}
	allowed := make(map[string]struct{}, len(stage3ProviderNames))
	for _, provider := range stage3ProviderNames {
		allowed[provider] = struct{}{}
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		provider, ok := value.(string)
		if !ok || provider != strings.TrimSpace(provider) || provider == "" {
			return nil, errors.New("SYNARA_AGENTD_CAPABILITIES_JSON providerPolicy.experimentalProviders contains an invalid Provider ID")
		}
		if _, ok := allowed[provider]; !ok {
			return nil, fmt.Errorf("SYNARA_AGENTD_CAPABILITIES_JSON providerPolicy.experimentalProviders contains unknown Provider %q", provider)
		}
		if _, duplicate := seen[provider]; duplicate {
			return nil, fmt.Errorf("SYNARA_AGENTD_CAPABILITIES_JSON providerPolicy.experimentalProviders contains duplicate Provider %q", provider)
		}
		seen[provider] = struct{}{}
		result = append(result, provider)
	}
	sort.Strings(result)
	return result, nil
}

func validBuildGitSHA(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func parseRunnerProtocol(value string) (RunnerProtocol, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(RunnerProtocolV2):
		return RunnerProtocolV2, nil
	case string(RunnerProtocolV1):
		return RunnerProtocolV1, nil
	default:
		return "", errors.New("SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL must be v2 or v1")
	}
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func hostname() string {
	value, err := os.Hostname()
	if err != nil || strings.TrimSpace(value) == "" {
		return uuid.NewString()
	}
	return value
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration", name)
	}
	return parsed, nil
}

func intEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}
