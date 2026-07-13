package agentd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
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
	supervisor.newDaemon = func(Config, *slog.Logger) daemonRunner {
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
