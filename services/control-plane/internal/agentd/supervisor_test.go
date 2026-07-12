package agentd

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

func TestLocalSupervisorRestartsDaemonUntilCancelled(t *testing.T) {
	supervisor, err := NewLocalSupervisor(LocalSupervisorInput{
		ListenAddress: ":3780", RegistrationToken: "registration-token",
		ExecutionTargetID: uuid.New(), RunnerCommand: []string{"runner"},
		WorkspaceRoot: t.TempDir(), WorkerLeaseTTL: 30 * time.Second,
		HeartbeatTimeout: 90 * time.Second, RestartBackoff: time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
