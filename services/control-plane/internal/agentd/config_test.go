package agentd

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
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
		"SYNARA_AGENTD_DRAIN_TIMEOUT", "SYNARA_AGENTD_HEARTBEAT_INTERVAL",
		"SYNARA_AGENTD_IMAGE_DIGEST", "SYNARA_AGENTD_INSTANCE_ID",
		"SYNARA_AGENTD_INSTANCE_UID",
		"SYNARA_AGENTD_LEASE_RENEW_INTERVAL", "SYNARA_AGENTD_NAMESPACE",
		"SYNARA_AGENTD_POLL_INTERVAL", "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL",
		"SYNARA_AGENTD_REQUEST_TIMEOUT", "SYNARA_AGENTD_ARTIFACT_TIMEOUT",
		"SYNARA_AGENTD_RUNNER_MESSAGE_BYTES", "SYNARA_AGENTD_VERSION",
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
