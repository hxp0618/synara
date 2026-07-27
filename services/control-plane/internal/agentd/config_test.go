package agentd

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestLoadConfigDefaultsExperimentalProvidersToDisabled(t *testing.T) {
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ExperimentalProviders) != 0 {
		t.Fatalf("experimental Providers defaulted to enabled: %v", cfg.ExperimentalProviders)
	}
}

func TestLoadConfigParsesExperimentalProviderPolicy(t *testing.T) {
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_AGENTD_CAPABILITIES_JSON", `{
		"workspaceModes":["worktree"],
		"providerPolicy":{"experimentalProviders":["pi","claudeAgent","codex"]}
	}`)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claudeAgent", "codex", "pi"}
	if !slices.Equal(cfg.ExperimentalProviders, want) {
		t.Fatalf("experimental Providers = %v, want %v", cfg.ExperimentalProviders, want)
	}
}

func TestLoadConfigStripsReservedContainmentCapability(t *testing.T) {
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_AGENTD_CAPABILITIES_JSON", `{
		"gpu": false,
		"resourceSuspendContainment": "forged"
	}`)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, found := cfg.Capabilities[resourceSuspendContainmentCapabilityKey]; found {
		t.Fatalf("reserved capability %q was preserved: %#v", resourceSuspendContainmentCapabilityKey, cfg.Capabilities)
	}
	if cfg.Capabilities["gpu"] != false {
		t.Fatalf("ordinary capability was lost: %#v", cfg.Capabilities)
	}
}

func TestLoadConfigRejectsCgroupRootOutsideLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-Linux validation")
	}
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_ROOT", "/sys/fs/cgroup/synara")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "only supported on Linux") {
		t.Fatalf("non-Linux cgroup root was accepted: %v", err)
	}
}

func TestLoadConfigRejectsInvalidCgroupRootPath(t *testing.T) {
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	if runtime.GOOS == "linux" {
		t.Setenv("SYNARA_AGENTD_CGROUP_V2_ROOT", ".")
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("relative cgroup root was accepted: %v", err)
		}
		t.Setenv("SYNARA_AGENTD_CGROUP_V2_ROOT", "/")
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "delegated subtree") {
			t.Fatalf("root cgroup path was accepted: %v", err)
		}
		return
	}
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_ROOT", ".")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "only supported on Linux") {
		t.Fatalf("non-Linux relative cgroup root returned %v", err)
	}
}

func TestLoadConfigParsesProtectedCgroupProviderIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only protected cgroup identity parsing")
	}
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_ROOT", "/sys/fs/cgroup/synara")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID", "1234")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID", "2345")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CgroupV2ProviderIdentity == nil ||
		cfg.CgroupV2ProviderIdentity.UID != 1234 ||
		cfg.CgroupV2ProviderIdentity.GID != 2345 {
		t.Fatalf("protected cgroup provider identity = %#v", cfg.CgroupV2ProviderIdentity)
	}
}

func TestLoadConfigRejectsIncompleteProtectedCgroupProviderIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only protected cgroup identity validation")
	}
	tests := []struct {
		name    string
		root    string
		uid     string
		gid     string
		message string
	}{
		{
			name:    "uid without gid",
			root:    "/sys/fs/cgroup/synara",
			uid:     "1234",
			message: "configure both",
		},
		{
			name:    "gid without uid",
			root:    "/sys/fs/cgroup/synara",
			gid:     "2345",
			message: "configure both",
		},
		{
			name:    "pair without root",
			uid:     "1234",
			gid:     "2345",
			message: "require SYNARA_AGENTD_CGROUP_V2_ROOT",
		},
		{
			name:    "invalid uid",
			root:    "/sys/fs/cgroup/synara",
			uid:     "not-a-number",
			gid:     "2345",
			message: "PROVIDER_UID",
		},
		{
			name:    "invalid gid",
			root:    "/sys/fs/cgroup/synara",
			uid:     "1234",
			gid:     "not-a-number",
			message: "PROVIDER_GID",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
			t.Setenv("SYNARA_AGENTD_CGROUP_V2_ROOT", test.root)
			t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID", test.uid)
			t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID", test.gid)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("invalid protected cgroup provider identity was accepted: %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsProtectedCgroupProviderIdentityOutsideLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-Linux validation")
	}
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID", "1234")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID", "2345")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "only supported on Linux") {
		t.Fatalf("non-Linux protected cgroup provider identity was accepted: %v", err)
	}
}

func TestLoadConfigParsesProtectedCgroupAttestationConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only protected cgroup attestation parsing")
	}
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_ROOT", "/sys/fs/cgroup/synara")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID", "1234")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID", "2345")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID", "test-key")
	t.Setenv("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE", "/etc/synara/keys/process-containment.ed25519")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CgroupV2Attestation == nil ||
		cfg.CgroupV2Attestation.KeyID != "test-key" ||
		cfg.CgroupV2Attestation.PrivateKeyPath != "/etc/synara/keys/process-containment.ed25519" {
		t.Fatalf("protected cgroup attestation = %#v", cfg.CgroupV2Attestation)
	}
}

func TestLoadConfigRejectsIncompleteProtectedCgroupAttestationConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only protected cgroup attestation validation")
	}
	tests := []struct {
		name    string
		root    string
		uid     string
		gid     string
		keyID   string
		keyPath string
		message string
	}{
		{
			name:    "key id without key path",
			root:    "/sys/fs/cgroup/synara",
			uid:     "1234",
			gid:     "2345",
			keyID:   "test-key",
			message: "configure both",
		},
		{
			name:    "key path without key id",
			root:    "/sys/fs/cgroup/synara",
			uid:     "1234",
			gid:     "2345",
			keyPath: "/etc/synara/keys/process-containment.ed25519",
			message: "configure both",
		},
		{
			name:    "missing protected provider identity",
			root:    "/sys/fs/cgroup/synara",
			keyID:   "test-key",
			keyPath: "/etc/synara/keys/process-containment.ed25519",
			message: "require protected cgroup Provider identity",
		},
		{
			name:    "relative key path",
			root:    "/sys/fs/cgroup/synara",
			uid:     "1234",
			gid:     "2345",
			keyID:   "test-key",
			keyPath: "relative-key",
			message: "absolute path",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
			t.Setenv("SYNARA_AGENTD_CGROUP_V2_ROOT", test.root)
			t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID", test.uid)
			t.Setenv("SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID", test.gid)
			t.Setenv("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID", test.keyID)
			t.Setenv("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE", test.keyPath)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("invalid protected cgroup attestation config was accepted: %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidExperimentalProviderPolicy(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "policy is not object", value: `{"providerPolicy":[]}`},
		{name: "allowlist is not array", value: `{"providerPolicy":{"experimentalProviders":"codex"}}`},
		{name: "unknown Provider", value: `{"providerPolicy":{"experimentalProviders":["unknown"]}}`},
		{name: "duplicate Provider", value: `{"providerPolicy":{"experimentalProviders":["codex","codex"]}}`},
		{name: "non canonical Provider", value: `{"providerPolicy":{"experimentalProviders":[" claudeAgent "]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
			t.Setenv("SYNARA_AGENTD_CAPABILITIES_JSON", test.value)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "providerPolicy") {
				t.Fatalf("invalid Provider policy was accepted: %v", err)
			}
		})
	}
}

func TestLoadConfigDefaultsGitCacheRootBesideWorkspaceRoot(t *testing.T) {
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	setAgentdConfigEnvironment(t, workspaceRoot, "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(filepath.Dir(workspaceRoot), "git-cache")
	if cfg.WorkspaceRoot != workspaceRoot || cfg.GitCacheRoot != expected {
		t.Fatalf("unexpected workspace storage roots: workspace=%q gitCache=%q", cfg.WorkspaceRoot, cfg.GitCacheRoot)
	}
}

func TestLoadConfigUsesExplicitGitCacheRoot(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspaces")
	gitCacheRoot := filepath.Join(root, "shared-git-cache")
	setAgentdConfigEnvironment(t, workspaceRoot, gitCacheRoot)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitCacheRoot != gitCacheRoot {
		t.Fatalf("unexpected explicit Git cache root %q", cfg.GitCacheRoot)
	}
}

func TestLoadConfigBoundsWorkspaceFetchFreshnessWindow(t *testing.T) {
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceFetchWindow != 0 {
		t.Fatalf("Workspace Fetch freshness window default = %s, want 0", cfg.WorkspaceFetchWindow)
	}

	t.Setenv("SYNARA_AGENTD_WORKSPACE_FETCH_FRESHNESS_WINDOW", "15m")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceFetchWindow != 15*time.Minute {
		t.Fatalf("Workspace Fetch freshness window = %s, want 15m", cfg.WorkspaceFetchWindow)
	}

	for _, value := range []string{"-1s", "1h1ns", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("SYNARA_AGENTD_WORKSPACE_FETCH_FRESHNESS_WINDOW", value)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "SYNARA_AGENTD_WORKSPACE_FETCH_FRESHNESS_WINDOW") {
				t.Fatalf("invalid Workspace Fetch freshness window %q was accepted: %v", value, err)
			}
		})
	}
}

func TestLoadConfigGeneratesInstanceUIDOutsideKubernetes(t *testing.T) {
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	setAgentdConfigEnvironment(t, workspaceRoot, "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(cfg.InstanceUID); err != nil {
		t.Fatalf("generated instance UID is invalid: %q", cfg.InstanceUID)
	}
}

func TestLoadConfigRequiresKubernetesInstanceUID(t *testing.T) {
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	setAgentdConfigEnvironment(t, workspaceRoot, "")
	t.Setenv("SYNARA_EXECUTION_TARGET_KIND", "kubernetes")

	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "SYNARA_AGENTD_INSTANCE_UID is required") {
		t.Fatalf("expected Kubernetes instance UID requirement, got %v", err)
	}

	instanceUID := uuid.NewString()
	t.Setenv("SYNARA_AGENTD_INSTANCE_UID", strings.ToUpper(instanceUID))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceUID != instanceUID {
		t.Fatalf("unexpected Kubernetes instance UID %q", cfg.InstanceUID)
	}
}

func TestLoadConfigRequiresSSHBootstrapGeneration(t *testing.T) {
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_EXECUTION_TARGET_KIND", "ssh")
	t.Setenv("SYNARA_AGENTD_INSTANCE_UID", uuid.NewString())

	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "SSH_BOOTSTRAP_GENERATION") {
		t.Fatalf("missing SSH bootstrap generation was accepted: %v", err)
	}
	t.Setenv("SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION", "0")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Fatalf("invalid SSH bootstrap generation was accepted: %v", err)
	}
	t.Setenv("SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION", "17")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSHBootstrapGeneration == nil || *cfg.SSHBootstrapGeneration != 17 {
		t.Fatalf("SSH bootstrap generation = %#v", cfg.SSHBootstrapGeneration)
	}
}

func TestLoadConfigDerivesWorkerModeFromAssignment(t *testing.T) {
	tests := []struct {
		name                string
		assignedExecutionID string
		explicitWorkerMode  string
		wantWorkerMode      string
	}{
		{name: "default general pool", wantWorkerMode: executions.WorkerModeGeneralPool},
		{
			name:                "assigned execution defaults to execution pinned",
			assignedExecutionID: uuid.NewString(),
			wantWorkerMode:      executions.WorkerModeExecutionPinned,
		},
		{
			name:               "explicit warm pool without assignment",
			explicitWorkerMode: executions.WorkerModeWarmPool,
			wantWorkerMode:     executions.WorkerModeWarmPool,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
			t.Setenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", test.assignedExecutionID)
			t.Setenv("SYNARA_AGENTD_WORKER_MODE", test.explicitWorkerMode)

			cfg, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkerMode != test.wantWorkerMode {
				t.Fatalf("worker mode = %q, want %q", cfg.WorkerMode, test.wantWorkerMode)
			}
		})
	}
}

func TestLoadConfigRejectsWorkerModeAssignmentMismatch(t *testing.T) {
	tests := []struct {
		name                string
		assignedExecutionID string
		explicitWorkerMode  string
		wantMessage         string
	}{
		{
			name:               "execution pinned requires assigned execution",
			explicitWorkerMode: executions.WorkerModeExecutionPinned,
			wantMessage:        "requires SYNARA_AGENTD_ASSIGNED_EXECUTION_ID",
		},
		{
			name:                "warm pool rejects assigned execution",
			assignedExecutionID: uuid.NewString(),
			explicitWorkerMode:  executions.WorkerModeWarmPool,
			wantMessage:         "cannot be combined with SYNARA_AGENTD_ASSIGNED_EXECUTION_ID",
		},
		{
			name:                "general pool rejects assigned execution",
			assignedExecutionID: uuid.NewString(),
			explicitWorkerMode:  executions.WorkerModeGeneralPool,
			wantMessage:         "cannot be combined with SYNARA_AGENTD_ASSIGNED_EXECUTION_ID",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
			t.Setenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", test.assignedExecutionID)
			t.Setenv("SYNARA_AGENTD_WORKER_MODE", test.explicitWorkerMode)

			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("worker mode mismatch was accepted: %v", err)
			}
		})
	}
}

func TestLoadConfigReadsKubernetesPodBoundRegistrationTokenFile(t *testing.T) {
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	setAgentdConfigEnvironment(t, workspaceRoot, "")
	t.Setenv("SYNARA_EXECUTION_TARGET_KIND", "kubernetes")
	t.Setenv("SYNARA_AGENTD_INSTANCE_UID", uuid.NewString())
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN", "")
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("pod-bound-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN_FILE", tokenPath)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistrationToken != "pod-bound-token" || cfg.RegistrationTokenFile != tokenPath {
		t.Fatalf("Pod-bound registration token file was not loaded: %#v", cfg)
	}
}

func TestLoadConfigRejectsOverlappingWorkspaceAndGitCacheRoots(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name          string
		workspaceRoot string
		gitCacheRoot  string
	}{
		{name: "same root", workspaceRoot: filepath.Join(root, "shared"), gitCacheRoot: filepath.Join(root, "shared")},
		{name: "cache inside workspace", workspaceRoot: filepath.Join(root, "workspaces"), gitCacheRoot: filepath.Join(root, "workspaces", "git-cache")},
		{name: "workspace inside cache", workspaceRoot: filepath.Join(root, "git-cache", "workspaces"), gitCacheRoot: filepath.Join(root, "git-cache")},
	} {
		t.Run(test.name, func(t *testing.T) {
			setAgentdConfigEnvironment(t, test.workspaceRoot, test.gitCacheRoot)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "must be separate") {
				t.Fatalf("expected overlapping storage roots to be rejected, got %v", err)
			}
		})
	}
}

func TestLoadConfigUsesAndValidatesWorkerImageManifestBuildIdentity(t *testing.T) {
	fixture := newWorkerImageManifestFixture(t)
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv(workerImageManifestEnvironment, fixture.Path)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != fixture.Manifest.Source.Version || cfg.BuildGitSHA != fixture.Manifest.Source.GitSHA ||
		cfg.WorkerImageManifest == nil {
		t.Fatalf("Worker image build identity was not loaded: %#v", cfg)
	}

	t.Setenv("SYNARA_AGENTD_VERSION", fixture.Manifest.Source.Version)
	t.Setenv("SYNARA_AGENTD_BUILD_GIT_SHA", fixture.Manifest.Source.GitSHA)
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("matching explicit Worker build identity was rejected: %v", err)
	}
}

func TestLoadConfigRejectsWorkerImageManifestBuildIdentityDrift(t *testing.T) {
	for _, test := range []struct {
		name     string
		variable string
		value    string
		message  string
	}{
		{name: "version", variable: "SYNARA_AGENTD_VERSION", value: "9.9.9", message: "VERSION"},
		{name: "Git SHA", variable: "SYNARA_AGENTD_BUILD_GIT_SHA", value: strings.Repeat("e", 40), message: "BUILD_GIT_SHA"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWorkerImageManifestFixture(t)
			setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
			t.Setenv(workerImageManifestEnvironment, fixture.Path)
			t.Setenv(test.variable, test.value)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Worker image build identity drift was accepted: %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsReservedWorkerImageBuildFeatureFlag(t *testing.T) {
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_AGENTD_CAPABILITIES_JSON", `{
		"featureFlags":{"workerImageBuild":{"forged":true}}
	}`)
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved Worker image build Feature Flag was accepted: %v", err)
	}
}

func TestLoadConfigRequiresCanonicalSHA256ImageDigest(t *testing.T) {
	setAgentdConfigEnvironment(t, filepath.Join(t.TempDir(), "workspaces"), "")
	t.Setenv("SYNARA_AGENTD_IMAGE_DIGEST", "sha256:test")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "image digest") {
		t.Fatalf("invalid Worker image digest was accepted: %v", err)
	}
	t.Setenv("SYNARA_AGENTD_IMAGE_DIGEST", "sha256:"+strings.Repeat("a", 64))
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("canonical Worker image digest was rejected: %v", err)
	}
}

func setAgentdConfigEnvironment(t *testing.T, workspaceRoot, gitCacheRoot string) {
	t.Helper()
	for _, name := range []string{
		"SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", "SYNARA_AGENTD_BUILD_GIT_SHA",
		"SYNARA_AGENTD_CAPABILITIES_JSON", "SYNARA_AGENTD_CLUSTER_ID",
		"SYNARA_AGENTD_CGROUP_V2_ROOT",
		"SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID", "SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID",
		"SYNARA_AGENTD_DRAIN_TIMEOUT", "SYNARA_AGENTD_HEARTBEAT_INTERVAL",
		"SYNARA_AGENTD_IMAGE_DIGEST", "SYNARA_AGENTD_INSTANCE_ID",
		"SYNARA_AGENTD_INSTANCE_UID",
		"SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION",
		"SYNARA_AGENTD_LEASE_RENEW_INTERVAL", "SYNARA_AGENTD_NAMESPACE",
		"SYNARA_AGENTD_POLL_INTERVAL", "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL",
		"SYNARA_AGENTD_REQUEST_TIMEOUT", "SYNARA_AGENTD_ARTIFACT_TIMEOUT",
		"SYNARA_AGENTD_WORKSPACE_FETCH_FRESHNESS_WINDOW",
		"SYNARA_AGENTD_WORKER_MODE",
		"SYNARA_AGENTD_RUNNER_MESSAGE_BYTES", "SYNARA_AGENTD_VERSION",
		"SYNARA_WORKER_REGISTRATION_TOKEN_FILE",
		workerImageManifestEnvironment,
	} {
		t.Setenv(name, "")
	}
	t.Setenv("SYNARA_CONTROL_PLANE_URL", "http://127.0.0.1:3780")
	t.Setenv("SYNARA_EXECUTION_TARGET_ID", uuid.NewString())
	t.Setenv("SYNARA_EXECUTION_TARGET_KIND", "local")
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN", "registration-token")
	t.Setenv("SYNARA_AGENTD_RUNNER_COMMAND_JSON", `["runner"]`)
	t.Setenv("SYNARA_AGENTD_WORKSPACE_ROOT", workspaceRoot)
	t.Setenv("SYNARA_AGENTD_GIT_CACHE_ROOT", gitCacheRoot)
}
