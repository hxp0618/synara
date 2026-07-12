package agentd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

type Daemon struct {
	config Config
	client *Client
	runner *Runner
	logger *slog.Logger
}

func NewDaemon(cfg Config, logger *slog.Logger) *Daemon {
	return &Daemon{config: cfg, client: NewClient(cfg), runner: NewRunner(cfg), logger: logger}
}

func (d *Daemon) Run(ctx context.Context) error {
	probeContext, cancelProbe := context.WithTimeout(ctx, d.config.RequestTimeout)
	providerHostCapabilities, err := d.runner.CapabilitySummary(probeContext)
	cancelProbe()
	if err != nil {
		return fmt.Errorf("probe Provider Host compatibility: %w", err)
	}
	d.config.Capabilities = withProviderHostCapabilities(d.config.Capabilities, providerHostCapabilities, d.config)
	registered, err := d.client.Register(ctx, d.config)
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	d.logger.Info("agentd registered", "workerId", registered.Worker.ID, "executionTargetId", registered.Worker.ExecutionTargetID, "targetKind", registered.Worker.TargetKind)
	heartbeatContext, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go d.heartbeatLoop(heartbeatContext)

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		claim, err := d.client.Claim(ctx, d.config)
		if err != nil {
			d.logger.Warn("execution claim failed", "error", err)
			if !waitContext(ctx, d.config.PollInterval) {
				return nil
			}
			continue
		}
		if claim.Execution == nil || claim.Lease == nil {
			if !waitContext(ctx, d.config.PollInterval) {
				return nil
			}
			continue
		}
		if claim.Workload == nil {
			_ = d.client.Release(ctx, claim.Execution.ID, *claim.Lease, "claim omitted workload")
			d.logger.Error("claimed execution omitted workload", "executionId", claim.Execution.ID)
			continue
		}
		if err := d.runExecution(ctx, *claim.Execution, *claim.Lease, *claim.Workload, claim.ProviderResumeCursor); err != nil {
			d.logger.Error("execution runner failed", "executionId", claim.Execution.ID, "generation", claim.Lease.Generation, "error", err)
		}
	}
}

func (d *Daemon) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(d.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requestContext, cancel := context.WithTimeout(ctx, d.config.RequestTimeout)
			err := d.client.Heartbeat(requestContext, d.config)
			cancel()
			if err != nil && ctx.Err() == nil {
				d.logger.Warn("worker heartbeat failed", "error", err)
			}
		}
	}
}

func (d *Daemon) runExecution(
	ctx context.Context,
	execution executions.Execution,
	lease executions.Lease,
	workload executions.Workload,
	resumeCursor *string,
) error {
	if err := d.client.Start(ctx, execution.ID, lease); err != nil {
		return fmt.Errorf("start execution: %w", err)
	}
	workspaceDirectory := filepath.Join(
		d.config.WorkspaceRoot, workload.TenantID.String(), workload.ProjectID.String(), workload.SessionID.String(), execution.ID.String(),
	)
	if err := os.MkdirAll(workspaceDirectory, 0o700); err != nil {
		return d.failExecution(ctx, execution.ID, lease, fmt.Errorf("create execution workspace: %w", err))
	}
	var credential *RunnerCredential
	if workload.ProviderCredentialID != nil {
		resolved, err := d.client.ResolveCredential(ctx, execution.ID, *workload.ProviderCredentialID, lease)
		if err != nil {
			return d.failExecution(ctx, execution.ID, lease, fmt.Errorf("resolve provider credential: %w", err))
		}
		credential = &resolved
	}

	runnerContext, cancelRunner := context.WithCancel(ctx)
	renewErrors := make(chan error, 1)
	go d.renewLeaseLoop(runnerContext, execution.ID, lease, cancelRunner, renewErrors)
	var controls <-chan RunnerControl
	if d.runner.protocol == RunnerProtocolV2 {
		controlChannel := make(chan RunnerControl)
		controls = controlChannel
		go d.runnerControlLoop(runnerContext, execution.ID, lease, controlChannel)
	}
	result, runErr := d.runner.RunControlled(runnerContext, RunnerInput{
		Execution: execution, Workload: workload, ProviderResumeCursor: resumeCursor, WorkspaceDirectory: workspaceDirectory,
	}, credential, controls, func(messageContext context.Context, message RunnerMessage) error {
		switch message.Type {
		case "event":
			return d.client.AppendEvent(messageContext, execution.ID, lease, message)
		case "artifact":
			artifactPath, err := resolveWorkspaceArtifact(workspaceDirectory, message.Artifact.Path)
			if err != nil {
				return err
			}
			if strings.TrimSpace(message.Artifact.OriginalName) == "" {
				message.Artifact.OriginalName = filepath.Base(artifactPath)
			}
			return d.client.UploadArtifact(messageContext, execution.ID, lease, *message.Artifact, artifactPath)
		case "progress":
			return nil
		case "interaction":
			eventType, err := interactionRuntimeEventType(message.Payload)
			if err != nil {
				return err
			}
			message.EventType = eventType
			return d.client.AppendEvent(messageContext, execution.ID, lease, message)
		case "checkpoint":
			return &runnerFailure{
				code:                 "capability_unsupported",
				message:              "Provider Host emitted a lifecycle message that this Worker version cannot persist",
				requiresNewExecution: true, requiresUserAction: true,
				canReconstructFromHistory: true, canMoveWorker: true,
			}
		default:
			return protocolFailure("Provider Host emitted an unsupported Worker message")
		}
	})
	cancelRunner()
	renewErr := drainRenewError(renewErrors)
	if runErr != nil && runnerFailurePersisted(runErr) {
		d.logger.Info("execution interrupted", "executionId", execution.ID, "generation", lease.Generation)
		return nil
	}
	if renewErr != nil {
		return renewErr
	}
	if runErr != nil {
		if ctx.Err() != nil {
			releaseContext, cancel := context.WithTimeout(context.Background(), d.config.RequestTimeout)
			defer cancel()
			_ = d.client.Release(releaseContext, execution.ID, lease, "agentd shutting down")
			return ctx.Err()
		}
		return d.failExecution(ctx, execution.ID, lease, runErr)
	}
	if err := d.client.Complete(ctx, execution.ID, lease, result); err != nil {
		return fmt.Errorf("complete execution: %w", err)
	}
	d.logger.Info("execution completed", "executionId", execution.ID, "generation", lease.Generation)
	return nil
}

func (d *Daemon) runnerControlLoop(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	output chan<- RunnerControl,
) {
	defer close(output)
	interval := d.config.PollInterval
	if interval <= 0 || interval > 500*time.Millisecond {
		interval = 500 * time.Millisecond
	}
	for {
		requestContext, cancel := context.WithTimeout(ctx, d.config.RequestTimeout)
		commands, err := d.client.PullControlCommands(requestContext, executionID, lease)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.logger.Warn("control command pull failed", "executionId", executionID, "generation", lease.Generation, "error", err)
			if !waitContext(ctx, interval) {
				return
			}
			continue
		}
		var control RunnerControl
		if len(commands) > 0 {
			delivery := commands[0]
			control = RunnerControl{
				Command: RunnerControlCommand{
					Provider: delivery.Provider, CommandType: delivery.CommandType,
					CommandID: delivery.CommandID, Payload: delivery.Payload,
				},
				MarkDelivered: func(controlContext context.Context) error {
					return d.retryInteractionDeliveryUpdate(controlContext, func(requestContext context.Context) error {
						return d.client.MarkControlCommandDelivered(requestContext, executionID, lease, delivery)
					})
				},
				Acknowledge: func(controlContext context.Context, result map[string]any) error {
					return d.retryInteractionDeliveryUpdate(controlContext, func(requestContext context.Context) error {
						return d.client.AcknowledgeControlCommand(requestContext, executionID, lease, delivery, result)
					})
				},
			}
		} else {
			requestContext, cancel = context.WithTimeout(ctx, d.config.RequestTimeout)
			items, interactionErr := d.client.PullInteractionResolutions(requestContext, executionID, lease)
			cancel()
			if interactionErr != nil {
				if ctx.Err() != nil {
					return
				}
				d.logger.Warn("interaction resolution pull failed", "executionId", executionID, "generation", lease.Generation, "error", interactionErr)
				if !waitContext(ctx, interval) {
					return
				}
				continue
			}
			if len(items) > 0 {
				delivery := items[0]
				control = RunnerControl{
					Command: RunnerControlCommand{
						Provider: delivery.Provider, CommandType: delivery.CommandType, CommandID: delivery.CommandID,
						Payload: map[string]any{
							"interactionId": delivery.InteractionID.String(), "requestId": delivery.RequestID,
							"resolutionKind": delivery.ResolutionKind, "resolution": delivery.Resolution,
						},
					},
					MarkDelivered: func(controlContext context.Context) error {
						return d.retryInteractionDeliveryUpdate(controlContext, func(requestContext context.Context) error {
							return d.client.MarkInteractionResolutionDelivered(requestContext, executionID, lease, delivery)
						})
					},
					Acknowledge: func(controlContext context.Context, _ map[string]any) error {
						return d.retryInteractionDeliveryUpdate(controlContext, func(requestContext context.Context) error {
							return d.client.AcknowledgeInteractionResolution(requestContext, executionID, lease, delivery)
						})
					},
				}
			}
		}
		if control.Command.CommandType == "" {
			if !waitContext(ctx, interval) {
				return
			}
			continue
		}
		done := make(chan error, 1)
		control.Done = done
		select {
		case output <- control:
		case <-ctx.Done():
			return
		}
		select {
		case err := <-done:
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *Daemon) retryInteractionDeliveryUpdate(
	ctx context.Context,
	update func(context.Context) error,
) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		requestContext, cancel := context.WithTimeout(ctx, d.config.RequestTimeout)
		lastErr = update(requestContext)
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt < 2 && !waitContext(ctx, time.Duration(attempt+1)*100*time.Millisecond) {
			return ctx.Err()
		}
	}
	return lastErr
}

func interactionRuntimeEventType(payload map[string]any) (string, error) {
	interactionType, _ := payload["interactionType"].(string)
	switch strings.ToLower(strings.TrimSpace(interactionType)) {
	case "approval":
		return "approval.requested", nil
	case "user-input":
		return "user-input.requested", nil
	default:
		return "", protocolFailure("Provider Host InteractionRequest omitted a supported interactionType")
	}
}

func (d *Daemon) renewLeaseLoop(ctx context.Context, executionID uuid.UUID, lease executions.Lease, cancel context.CancelFunc, result chan<- error) {
	ticker := time.NewTicker(d.config.LeaseRenewInterval)
	defer ticker.Stop()
	defer close(result)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requestContext, requestCancel := context.WithTimeout(ctx, d.config.RequestTimeout)
			err := d.client.Renew(requestContext, executionID, lease)
			requestCancel()
			if err != nil {
				select {
				case result <- fmt.Errorf("renew execution lease: %w", err):
				default:
				}
				cancel()
				return
			}
		}
	}
}

func (d *Daemon) failExecution(ctx context.Context, executionID uuid.UUID, lease executions.Lease, cause error) error {
	message := cause.Error()
	if len(message) > 10_000 {
		message = message[:10_000]
	}
	if err := d.client.Fail(ctx, executionID, lease, runnerFailureCode(cause), message); err != nil {
		return fmt.Errorf("runner failed (%v) and execution failure could not be reported: %w", cause, err)
	}
	return cause
}

func withProviderHostCapabilities(base map[string]any, providerHost map[string]any, config Config) map[string]any {
	result := make(map[string]any, len(base)+2)
	for key, value := range base {
		result[key] = value
	}
	result["providerHost"] = providerHost
	workerRuntime := map[string]any{
		"workerBuildVersion":    config.Version,
		"workerProtocolMinimum": executions.WorkerProtocolVersion,
		"workerProtocolMaximum": executions.WorkerProtocolVersion,
		"runtimeEventMinimum":   1,
		"runtimeEventMaximum":   1,
		"operatingSystem":       runtime.GOOS,
		"architecture":          runtime.GOARCH,
	}
	if config.BuildGitSHA != "" {
		workerRuntime["workerBuildGitSha"] = config.BuildGitSHA
	}
	if config.ImageDigest != "" {
		workerRuntime["imageDigest"] = config.ImageDigest
	}
	result["workerRuntime"] = workerRuntime
	return result
}

func resolveWorkspaceArtifact(workspaceDirectory, candidate string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", errors.New("runner artifact path is empty")
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workspaceDirectory, candidate)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve runner artifact path: %w", err)
	}
	workspace, err := filepath.EvalSymlinks(workspaceDirectory)
	if err != nil {
		return "", fmt.Errorf("resolve execution workspace: %w", err)
	}
	relative, err := filepath.Rel(workspace, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("runner artifact path escapes the execution workspace")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat runner artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("runner artifact must be a regular file")
	}
	return resolved, nil
}

func drainRenewError(channel <-chan error) error {
	for err := range channel {
		if err != nil {
			return err
		}
	}
	return nil
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
