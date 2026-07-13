package agentd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

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
