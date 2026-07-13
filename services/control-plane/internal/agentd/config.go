package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Config struct {
	ControlPlaneURL       *url.URL
	RegistrationToken     string
	ExecutionTargetID     uuid.UUID
	AssignedExecutionID   *uuid.UUID
	TargetKind            platform.ExecutionTargetKind
	ClusterID             string
	Namespace             string
	PodName               string
	InstanceUID           string
	Version               string
	BuildGitSHA           string
	ImageDigest           string
	Capabilities          map[string]any
	ExperimentalProviders []string
	RunnerCommand         []string
	RunnerProtocol        RunnerProtocol
	WorkspaceRoot         string
	GitCacheRoot          string
	PollInterval          time.Duration
	HeartbeatInterval     time.Duration
	LeaseRenewInterval    time.Duration
	DrainTimeout          time.Duration
	RequestTimeout        time.Duration
	ArtifactTimeout       time.Duration
	RunnerMessageBytes    int
}

var stage3ProviderNames = []string{
	"codex", "claudeAgent", "cursor", "gemini", "grok", "kilo", "opencode", "pi",
}

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
	runnerCommand, err := validation.CommandJSON(os.Getenv("SYNARA_AGENTD_RUNNER_COMMAND_JSON"))
	if err != nil {
		return Config{}, fmt.Errorf("SYNARA_AGENTD_RUNNER_COMMAND_JSON: %w", err)
	}
	runnerProtocol, err := parseRunnerProtocol(os.Getenv("SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL"))
	if err != nil {
		return Config{}, err
	}
	capabilities := map[string]any{}
	if raw := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_CAPABILITIES_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &capabilities); err != nil || capabilities == nil {
			return Config{}, errors.New("SYNARA_AGENTD_CAPABILITIES_JSON must be a JSON object")
		}
	}
	experimentalProviders, err := parseExperimentalProviders(capabilities)
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
	cfg := Config{
		ControlPlaneURL: parsedURL, RegistrationToken: strings.TrimSpace(os.Getenv("SYNARA_WORKER_REGISTRATION_TOKEN")),
		ExecutionTargetID: targetID, TargetKind: targetKind,
		ClusterID: envDefault("SYNARA_AGENTD_CLUSTER_ID", "local"), Namespace: envDefault("SYNARA_AGENTD_NAMESPACE", "default"),
		PodName: envDefault("SYNARA_AGENTD_INSTANCE_ID", hostname()), InstanceUID: instanceUID,
		Version:      envDefault("SYNARA_AGENTD_VERSION", "dev"),
		BuildGitSHA:  strings.TrimSpace(os.Getenv("SYNARA_AGENTD_BUILD_GIT_SHA")),
		ImageDigest:  strings.TrimSpace(os.Getenv("SYNARA_AGENTD_IMAGE_DIGEST")),
		Capabilities: capabilities, ExperimentalProviders: experimentalProviders,
		RunnerCommand: runnerCommand, RunnerProtocol: runnerProtocol,
		WorkspaceRoot: workspaceRoot, GitCacheRoot: gitCacheRoot,
	}
	if raw := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID")); raw != "" {
		assignedExecutionID, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			return Config{}, errors.New("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID must be a UUID")
		}
		cfg.AssignedExecutionID = &assignedExecutionID
	}
	if cfg.RegistrationToken == "" {
		return Config{}, errors.New("SYNARA_WORKER_REGISTRATION_TOKEN is required")
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
	if (cfg.BuildGitSHA != "" && !validBuildGitSHA(cfg.BuildGitSHA)) || len(cfg.ImageDigest) > 512 {
		return Config{}, errors.New("agentd build Git SHA or image digest is invalid")
	}
	return cfg, nil
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
