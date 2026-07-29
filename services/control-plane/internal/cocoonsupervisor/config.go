package cocoonsupervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

const (
	defaultHealthSocketPath = "/run/synara-cocoon-supervisor/health.sock"
	defaultStateRoot        = "/var/lib/synara-cocoon-supervisor"
)

var kubernetesNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

type Config struct {
	VirtualNodeName   string
	Namespace         string
	ExecutionTargetID uuid.UUID
	ControlPlaneURL   *url.URL
	ClusterID         string
	PIDsMax           uint64
	CapabilitiesJSON  string
	WorkerVersion     string
	WorkerBuildGitSHA string
	PollInterval      time.Duration
	HealthSocketPath  string
	StateRoot         string
	KubectlCommand    string
	CocoonCommand     string
	VirtiofsdCommand  string
	AgentdCommand     string
	TransportCommand  string
}

func LoadConfig() (Config, error) {
	targetID, err := uuid.Parse(strings.TrimSpace(os.Getenv("SYNARA_COCOON_SUPERVISOR_EXECUTION_TARGET_ID")))
	if err != nil || targetID == uuid.Nil {
		return Config{}, errors.New("SYNARA_COCOON_SUPERVISOR_EXECUTION_TARGET_ID must be a UUID")
	}
	controlPlaneURL, err := url.Parse(strings.TrimSpace(os.Getenv("SYNARA_CONTROL_PLANE_URL")))
	if err != nil || controlPlaneURL.Host == "" ||
		(controlPlaneURL.Scheme != "http" && controlPlaneURL.Scheme != "https") {
		return Config{}, errors.New("SYNARA_CONTROL_PLANE_URL must be an HTTP(S) origin")
	}
	virtualNode := strings.TrimSpace(os.Getenv("SYNARA_COCOON_SUPERVISOR_VIRTUAL_NODE"))
	namespace := strings.TrimSpace(os.Getenv("SYNARA_COCOON_SUPERVISOR_NAMESPACE"))
	if !validKubernetesName(virtualNode) || !validKubernetesName(namespace) {
		return Config{}, errors.New("Cocoon supervisor virtual Node and namespace must be Kubernetes names")
	}
	pidsMax, err := strconv.ParseUint(strings.TrimSpace(os.Getenv(platform.KubernetesPIDsLimitEnvironment)), 10, 64)
	if err != nil || pidsMax == 0 || pidsMax > platform.MaximumKubernetesPIDsLimit {
		return Config{}, fmt.Errorf("%s must be between 1 and %d", platform.KubernetesPIDsLimitEnvironment, platform.MaximumKubernetesPIDsLimit)
	}
	capabilities := strings.TrimSpace(os.Getenv("SYNARA_AGENTD_CAPABILITIES_JSON"))
	if capabilities == "" {
		capabilities = "{}"
	}
	var parsedCapabilities map[string]any
	if json.Unmarshal([]byte(capabilities), &parsedCapabilities) != nil || parsedCapabilities == nil {
		return Config{}, errors.New("SYNARA_AGENTD_CAPABILITIES_JSON must be a JSON object")
	}
	pollInterval, err := time.ParseDuration(envDefault("SYNARA_COCOON_SUPERVISOR_POLL_INTERVAL", "2s"))
	if err != nil || pollInterval < 500*time.Millisecond || pollInterval > time.Minute {
		return Config{}, errors.New("SYNARA_COCOON_SUPERVISOR_POLL_INTERVAL must be between 500ms and 1m")
	}
	cfg := Config{
		VirtualNodeName: virtualNode, Namespace: namespace, ExecutionTargetID: targetID,
		ControlPlaneURL: controlPlaneURL, ClusterID: envDefault("SYNARA_AGENTD_CLUSTER_ID", "local"),
		PIDsMax: pidsMax, CapabilitiesJSON: capabilities,
		WorkerVersion:     strings.TrimSpace(os.Getenv("SYNARA_AGENTD_VERSION")),
		WorkerBuildGitSHA: strings.TrimSpace(os.Getenv("SYNARA_AGENTD_BUILD_GIT_SHA")),
		PollInterval:      pollInterval,
		HealthSocketPath:  envDefault("SYNARA_COCOON_SUPERVISOR_HEALTH_SOCKET", defaultHealthSocketPath),
		StateRoot:         envDefault("SYNARA_COCOON_SUPERVISOR_STATE_ROOT", defaultStateRoot),
		KubectlCommand: envDefault(
			"SYNARA_COCOON_SUPERVISOR_KUBECTL",
			firstExecutable("/usr/local/bin/kubectl", "/usr/bin/kubectl"),
		),
		CocoonCommand:    envDefault("SYNARA_COCOON_SUPERVISOR_COCOON", "/usr/local/bin/cocoon"),
		VirtiofsdCommand: envDefault("SYNARA_COCOON_SUPERVISOR_VIRTIOFSD", "/usr/lib/qemu/virtiofsd"),
		AgentdCommand:    envDefault("SYNARA_COCOON_SUPERVISOR_AGENTD", "/usr/local/bin/synara-agentd"),
		TransportCommand: envDefault("SYNARA_COCOON_SUPERVISOR_TRANSPORT", "/usr/local/bin/synara-cocoon-provider-transport"),
	}
	for name, path := range map[string]*string{
		"health socket": &cfg.HealthSocketPath, "state root": &cfg.StateRoot,
		"kubectl": &cfg.KubectlCommand, "cocoon": &cfg.CocoonCommand,
		"virtiofsd": &cfg.VirtiofsdCommand, "agentd": &cfg.AgentdCommand,
		"transport": &cfg.TransportCommand,
	} {
		value := filepath.Clean(strings.TrimSpace(*path))
		if !filepath.IsAbs(value) || value == string(filepath.Separator) || strings.ContainsAny(value, "\r\n\x00") {
			return Config{}, fmt.Errorf("Cocoon supervisor %s path must be a safe absolute path", name)
		}
		*path = value
	}
	if cfg.StateRoot == filepath.Dir(cfg.HealthSocketPath) || pathContains(cfg.StateRoot, cfg.HealthSocketPath) {
		return Config{}, errors.New("Cocoon supervisor health socket must be outside the durable state root")
	}
	return cfg, nil
}

func validKubernetesName(value string) bool {
	return len(value) > 0 && len(value) <= 63 && kubernetesNamePattern.MatchString(value)
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func firstExecutable(candidates ...string) string {
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}
