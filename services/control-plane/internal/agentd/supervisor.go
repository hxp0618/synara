package agentd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

type LocalSupervisorInput struct {
	ListenAddress     string
	RegistrationToken string
	ExecutionTargetID uuid.UUID
	RunnerCommand     []string
	WorkspaceRoot     string
	WorkerLeaseTTL    time.Duration
	HeartbeatTimeout  time.Duration
	RestartBackoff    time.Duration
}

type LocalSupervisor struct {
	config         Config
	restartBackoff time.Duration
	logger         *slog.Logger
	newDaemon      func(Config, *slog.Logger) daemonRunner
}

type daemonRunner interface {
	Run(context.Context) error
}

func NewLocalSupervisor(input LocalSupervisorInput, logger *slog.Logger) (*LocalSupervisor, error) {
	controlPlaneURL, err := localControlPlaneURL(input.ListenAddress)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.RegistrationToken) == "" {
		return nil, errors.New("local agentd requires a worker registration token")
	}
	if input.ExecutionTargetID == uuid.Nil {
		return nil, errors.New("local agentd requires an execution target")
	}
	if len(input.RunnerCommand) == 0 {
		return nil, errors.New("local agentd requires a runner command")
	}
	workspaceRoot, err := filepath.Abs(strings.TrimSpace(input.WorkspaceRoot))
	if err != nil || strings.TrimSpace(input.WorkspaceRoot) == "" {
		return nil, errors.New("local agentd workspace root is invalid")
	}
	if input.WorkerLeaseTTL <= 0 || input.HeartbeatTimeout <= input.WorkerLeaseTTL {
		return nil, errors.New("local agentd lease and heartbeat durations are invalid")
	}
	restartBackoff := input.RestartBackoff
	if restartBackoff <= 0 {
		restartBackoff = time.Second
	}
	leaseRenewInterval := input.WorkerLeaseTTL / 3
	if leaseRenewInterval < time.Second {
		leaseRenewInterval = time.Second
	}
	heartbeatInterval := input.HeartbeatTimeout / 3
	if heartbeatInterval < time.Second {
		heartbeatInterval = time.Second
	}
	config := Config{
		ControlPlaneURL: controlPlaneURL, RegistrationToken: input.RegistrationToken,
		ExecutionTargetID: input.ExecutionTargetID, TargetKind: platform.TargetLocal,
		ClusterID: "control-plane", Namespace: "local",
		PodName: "local-agentd-" + input.ExecutionTargetID.String(), Version: "embedded",
		Capabilities:  map[string]any{"workspaceModes": []string{"local", "worktree"}},
		RunnerCommand: append([]string(nil), input.RunnerCommand...), WorkspaceRoot: workspaceRoot,
		PollInterval: time.Second, HeartbeatInterval: heartbeatInterval,
		LeaseRenewInterval: leaseRenewInterval, RequestTimeout: 30 * time.Second,
		ArtifactTimeout: 30 * time.Minute, RunnerMessageBytes: 1 << 20,
	}
	return &LocalSupervisor{
		config: config, restartBackoff: restartBackoff, logger: logger,
		newDaemon: func(cfg Config, logger *slog.Logger) daemonRunner { return NewDaemon(cfg, logger) },
	}, nil
}

func (s *LocalSupervisor) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.newDaemon(s.config, s.logger).Run(ctx)
		if ctx.Err() != nil {
			return
		}
		s.logger.Error("local agentd stopped; restarting", "error", err, "backoff", s.restartBackoff)
		if !waitContext(ctx, s.restartBackoff) {
			return
		}
	}
}

func localControlPlaneURL(listenAddress string) (*url.URL, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddress))
	if err != nil {
		return nil, fmt.Errorf("derive local agentd URL from SYNARA_CONTROL_PLANE_LISTEN: %w", err)
	}
	if strings.TrimSpace(port) == "" {
		return nil, errors.New("derive local agentd URL from SYNARA_CONTROL_PLANE_LISTEN: port is empty")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}, nil
}
