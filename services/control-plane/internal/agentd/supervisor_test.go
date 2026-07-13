package agentd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLocalControlPlaneURLUsesLoopbackForWildcardListener(t *testing.T) {
	resolved, err := localControlPlaneURL("[::]:3780")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.String() != "http://127.0.0.1:3780" {
		t.Fatalf("unexpected local control-plane URL %q", resolved)
	}
}

func TestLocalSupervisorDefaultsGitCacheRootBesideWorkspaceRoot(t *testing.T) {
	input := validLocalSupervisorInput(t.TempDir())
	supervisor, err := NewLocalSupervisor(input, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(filepath.Dir(input.WorkspaceRoot), "git-cache")
	if supervisor.config.GitCacheRoot != expected {
		t.Fatalf("unexpected default Git cache root %q", supervisor.config.GitCacheRoot)
	}
}

func TestLocalSupervisorPassesGitCacheRootToDaemon(t *testing.T) {
	input := validLocalSupervisorInput(t.TempDir())
	input.GitCacheRoot = filepath.Join(filepath.Dir(input.WorkspaceRoot), "shared-git-cache")
	supervisor, err := NewLocalSupervisor(input, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var captured Config
	supervisor.newDaemon = func(cfg Config, _ *slog.Logger) daemonRunner {
		captured = cfg
		return daemonRunnerFunc(func(context.Context) error {
			cancel()
			return nil
		})
	}
	supervisor.Run(ctx)
	if captured.GitCacheRoot != input.GitCacheRoot {
		t.Fatalf("daemon received Git cache root %q, want %q", captured.GitCacheRoot, input.GitCacheRoot)
	}
}

func TestLocalSupervisorGeneratesDistinctCanonicalInstanceUIDs(t *testing.T) {
	input := validLocalSupervisorInput(t.TempDir())
	first, err := NewLocalSupervisor(input, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewLocalSupervisor(input, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, instanceUID := range []string{first.config.InstanceUID, second.config.InstanceUID} {
		parsed, err := uuid.Parse(instanceUID)
		if err != nil || parsed == uuid.Nil || parsed.String() != instanceUID {
			t.Fatalf("local supervisor generated a non-canonical Instance UID %q: %v", instanceUID, err)
		}
	}
	if first.config.InstanceUID == second.config.InstanceUID {
		t.Fatalf("separate local supervisors reused Instance UID %q", first.config.InstanceUID)
	}
}

func TestLocalSupervisorPassesTargetProviderPolicyToDaemon(t *testing.T) {
	input := validLocalSupervisorInput(t.TempDir())
	input.Capabilities = map[string]any{
		"workspaceModes": []string{"local", "worktree"},
		"providerPolicy": map[string]any{
			"experimentalProviders": []string{"codex", "claudeAgent"},
		},
	}
	supervisor, err := NewLocalSupervisor(input, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(supervisor.config.ExperimentalProviders, []string{"claudeAgent", "codex"}) {
		t.Fatalf("local agentd Provider policy = %v", supervisor.config.ExperimentalProviders)
	}
	policy := supervisor.config.Capabilities["providerPolicy"].(map[string]any)
	if !slices.Equal(policy["experimentalProviders"].([]string), []string{"codex", "claudeAgent"}) {
		t.Fatalf("local agentd capabilities lost the target Provider policy: %#v", supervisor.config.Capabilities)
	}
}

func TestLocalSupervisorProviderPolicyEnablesProviderHostDescriptors(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "success")
	input := validLocalSupervisorInput(t.TempDir())
	input.RunnerCommand = providerHostV2TestCommand()
	input.Capabilities = map[string]any{
		"providerPolicy": map[string]any{
			"experimentalProviders": []string{"codex", "claudeAgent"},
		},
	}
	supervisor, err := NewLocalSupervisor(input, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := NewRunner(supervisor.config).CapabilitySummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	providers := summary["providers"].(map[string]any)
	for _, provider := range []string{"codex", "claudeAgent"} {
		descriptor := providers[provider].(providerHostDescriptor)
		if descriptor.CapabilityDescriptor.ReleasePolicy == nil ||
			!descriptor.CapabilityDescriptor.ReleasePolicy.Enabled {
			t.Fatalf("local Provider %s was not enabled by the target policy: %#v", provider, descriptor)
		}
	}
}

func TestLocalSupervisorRejectsOverlappingWorkspaceAndGitCacheRoots(t *testing.T) {
	for _, test := range []struct {
		name              string
		workspaceRelative string
		cacheRelative     string
	}{
		{name: "same root", workspaceRelative: "shared", cacheRelative: "shared"},
		{name: "cache inside workspace", workspaceRelative: "workspaces", cacheRelative: "workspaces/git-cache"},
		{name: "workspace inside cache", workspaceRelative: "git-cache/workspaces", cacheRelative: "git-cache"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			input := validLocalSupervisorInput(root)
			input.WorkspaceRoot = filepath.Join(root, filepath.FromSlash(test.workspaceRelative))
			input.GitCacheRoot = filepath.Join(root, filepath.FromSlash(test.cacheRelative))
			if _, err := NewLocalSupervisor(input, slog.Default()); err == nil || !strings.Contains(err.Error(), "must be separate") {
				t.Fatalf("expected overlapping storage roots to be rejected, got %v", err)
			}
		})
	}
}

func TestLocalSupervisorRestartsDaemonUntilCancelled(t *testing.T) {
	input := validLocalSupervisorInput(t.TempDir())
	input.RestartBackoff = time.Millisecond
	supervisor, err := NewLocalSupervisor(input, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var starts atomic.Int32
	instanceUIDs := make([]string, 0, 2)
	supervisor.newDaemon = func(cfg Config, _ *slog.Logger) daemonRunner {
		instanceUIDs = append(instanceUIDs, cfg.InstanceUID)
		return daemonRunnerFunc(func(context.Context) error {
			if starts.Add(1) == 2 {
				cancel()
			}
			return errors.New("stopped")
		})
	}
	supervisor.Run(ctx)
	if starts.Load() != 2 {
		t.Fatalf("expected two daemon starts, got %d", starts.Load())
	}
	if len(instanceUIDs) != 2 || instanceUIDs[0] == "" || instanceUIDs[0] != instanceUIDs[1] {
		t.Fatalf("daemon restarts did not reuse one supervisor Instance UID: %v", instanceUIDs)
	}
}

type daemonRunnerFunc func(context.Context) error

func (f daemonRunnerFunc) Run(ctx context.Context) error { return f(ctx) }

func validLocalSupervisorInput(root string) LocalSupervisorInput {
	return LocalSupervisorInput{
		ListenAddress: ":3780", RegistrationToken: "registration-token",
		ExecutionTargetID: uuid.New(), RunnerCommand: []string{"runner"},
		WorkspaceRoot: filepath.Join(root, "workspaces"), WorkerLeaseTTL: 30 * time.Second,
		HeartbeatTimeout: 90 * time.Second,
	}
}
