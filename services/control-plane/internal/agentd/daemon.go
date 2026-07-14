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
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

const (
	workspaceCleanupProbeExecutionInterval = 4
	workspaceCleanupProbeMaximumInterval   = 30 * time.Second
)

type workspaceCleanupClaimSchedule struct {
	executionsSinceProbe int
	lastProbe            time.Time
}

func newWorkspaceCleanupClaimSchedule(now time.Time) workspaceCleanupClaimSchedule {
	return workspaceCleanupClaimSchedule{lastProbe: now}
}

func (s workspaceCleanupClaimSchedule) due(now time.Time) bool {
	return s.executionsSinceProbe >= workspaceCleanupProbeExecutionInterval ||
		(!s.lastProbe.IsZero() && now.Sub(s.lastProbe) >= workspaceCleanupProbeMaximumInterval)
}

func (s *workspaceCleanupClaimSchedule) recordExecution() {
	s.executionsSinceProbe++
}

func (s *workspaceCleanupClaimSchedule) recordProbe(now time.Time) {
	s.executionsSinceProbe = 0
	s.lastProbe = now
}

func workspaceCleanupClaimsEnabled(config Config) bool {
	return config.AssignedExecutionID == nil
}

type Daemon struct {
	config    Config
	client    *Client
	runner    *Runner
	workspace workspaceMaterializer
	logger    *slog.Logger
	draining  atomic.Bool
}

func NewDaemon(cfg Config, logger *slog.Logger) *Daemon {
	return &Daemon{
		config: cfg, client: NewClient(cfg), runner: NewRunner(cfg),
		workspace: NewWorkspaceMaterializerWithCache(cfg.WorkspaceRoot, cfg.GitCacheRoot, cfg.ExecutionTargetID), logger: logger,
	}
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
	runContext, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runDone := make(chan struct{})
	defer close(runDone)
	drainMarked := make(chan struct{})
	go d.waitForDrain(ctx, cancelRun, runDone, drainMarked)
	heartbeatContext, stopHeartbeat := context.WithCancel(runContext)
	defer stopHeartbeat()
	go d.heartbeatLoop(heartbeatContext)
	cleanupSchedule := newWorkspaceCleanupClaimSchedule(time.Now())
	canClaimWorkspaceCleanup := workspaceCleanupClaimsEnabled(d.config)

	for {
		if d.draining.Load() || runContext.Err() != nil {
			if d.draining.Load() {
				<-drainMarked
			}
			return nil
		}
		probedWorkspaceCleanup := false
		if canClaimWorkspaceCleanup && cleanupSchedule.due(time.Now()) {
			claimedCleanup, enteredDrain, cleanupErr := d.claimAndRunWorkspaceCleanup(runContext)
			cleanupSchedule.recordProbe(time.Now())
			probedWorkspaceCleanup = true
			if enteredDrain {
				<-drainMarked
				return nil
			}
			if cleanupErr != nil {
				d.logger.Warn("Workspace cleanup fairness probe failed", "error", cleanupErr)
			} else if claimedCleanup {
				continue
			}
		}
		claim, err := d.client.Claim(runContext, d.config)
		if err != nil {
			if d.draining.Load() || runContext.Err() != nil {
				if d.draining.Load() {
					<-drainMarked
				}
				return nil
			}
			d.logger.Warn("execution claim failed", "error", err)
			if !waitContext(runContext, d.config.PollInterval) {
				return nil
			}
			continue
		}
		if claim.Execution == nil || claim.Lease == nil {
			if canClaimWorkspaceCleanup && !probedWorkspaceCleanup {
				claimedCleanup, enteredDrain, cleanupErr := d.claimAndRunWorkspaceCleanup(runContext)
				cleanupSchedule.recordProbe(time.Now())
				if enteredDrain {
					<-drainMarked
					return nil
				}
				if cleanupErr != nil {
					if d.draining.Load() || runContext.Err() != nil {
						if d.draining.Load() {
							<-drainMarked
						}
						return nil
					}
					d.logger.Warn("Workspace cleanup claim failed", "error", cleanupErr)
					if !waitContext(runContext, d.config.PollInterval) {
						return nil
					}
					continue
				}
				if claimedCleanup {
					continue
				}
			}
			if !waitContext(runContext, d.config.PollInterval) {
				return nil
			}
			continue
		}
		if d.draining.Load() {
			d.releaseDuringShutdown(claim.Execution.ID, *claim.Lease, "Worker entered Drain after claiming the Execution")
			<-drainMarked
			return nil
		}
		if claim.Workload == nil {
			_ = d.client.Release(runContext, claim.Execution.ID, *claim.Lease, "claim omitted workload")
			d.logger.Error("claimed execution omitted workload", "executionId", claim.Execution.ID)
			cleanupSchedule.recordExecution()
			continue
		}
		if err := d.runExecution(runContext, *claim.Execution, *claim.Lease, *claim.Workload, claim.ProviderResumeCursor); err != nil {
			d.logger.Error("execution runner failed", "executionId", claim.Execution.ID, "generation", claim.Lease.Generation, "error", err)
		}
		cleanupSchedule.recordExecution()
		if d.draining.Load() {
			<-drainMarked
			return nil
		}
	}
}

func (d *Daemon) claimAndRunWorkspaceCleanup(ctx context.Context) (bool, bool, error) {
	result, err := d.client.ClaimWorkspaceCleanup(ctx, d.config)
	if err != nil {
		return false, false, err
	}
	if result.Cleanup == nil {
		return false, false, nil
	}
	claim := *result.Cleanup
	if d.draining.Load() {
		d.releaseWorkspaceCleanupDuringShutdown(claim, "Worker entered Drain after claiming Workspace cleanup")
		return false, true, nil
	}
	if err := d.runWorkspaceCleanup(ctx, claim); err != nil {
		d.logger.Error(
			"Workspace cleanup failed",
			"cleanupId", claim.CleanupID,
			"materializationId", claim.MaterializationID,
			"dispatchGeneration", claim.DispatchGeneration,
			"error", err,
		)
	}
	return true, false, nil
}

func (d *Daemon) runWorkspaceCleanup(
	ctx context.Context,
	claim executions.WorkspaceCleanupClaim,
) error {
	cleaner, ok := d.workspace.(workspaceCleaner)
	if !ok {
		reportErr := d.client.FailWorkspaceCleanup(
			ctx,
			claim,
			"workspace_cleanup_unsupported",
			"This Worker does not provide the managed Workspace cleanup engine.",
			false,
		)
		return errors.Join(errors.New("Workspace cleanup engine is unavailable"), reportErr)
	}

	cleanupContext, cancelCleanup := context.WithCancel(ctx)
	renewErrors := make(chan error, 1)
	go d.renewWorkspaceCleanupLoop(cleanupContext, claim, cancelCleanup, renewErrors)
	renewStopped := false
	stopRenewal := func() error {
		if renewStopped {
			return nil
		}
		renewStopped = true
		cancelCleanup()
		return drainRenewError(renewErrors)
	}
	defer func() { _ = stopRenewal() }()

	if err := d.client.StartWorkspaceCleanup(cleanupContext, claim); err != nil {
		renewErr := stopRenewal()
		reportErr := d.reportWorkspaceCleanupFailure(
			claim,
			"workspace_cleanup_start_failed",
			"The Workspace cleanup could not be started after its safety fence was revalidated.",
			true,
		)
		return errors.Join(fmt.Errorf("start Workspace cleanup: %w", err), renewErr, reportErr)
	}

	result, cleanupErr := cleaner.CleanupWorkspace(cleanupContext, workspaceCleanupRequestFromClaim(claim))
	renewErr := stopRenewal()
	if cleanupErr != nil {
		code := "workspace_cleanup_failed"
		message := "The Workspace cleanup failed before the physical materialization was confirmed absent."
		retryable := true
		var typed *WorkspaceCleanupError
		if errors.As(cleanupErr, &typed) {
			code = typed.Code
			message = typed.Message
			retryable = typed.Retryable
		}
		reportErr := d.reportWorkspaceCleanupFailure(claim, code, message, retryable)
		return errors.Join(cleanupErr, renewErr, reportErr)
	}
	if renewErr != nil {
		return renewErr
	}
	if err := d.client.AcknowledgeWorkspaceCleanup(ctx, claim); err != nil {
		return fmt.Errorf("acknowledge Workspace cleanup: %w", err)
	}
	d.logger.Info(
		"Workspace cleanup acknowledged",
		"cleanupId", claim.CleanupID,
		"materializationId", claim.MaterializationID,
		"dispatchGeneration", claim.DispatchGeneration,
		"outcome", result.Status,
	)
	return nil
}

func workspaceCleanupRequestFromClaim(claim executions.WorkspaceCleanupClaim) WorkspaceCleanupRequest {
	return WorkspaceCleanupRequest{
		CleanupID:          claim.CleanupID,
		TenantID:           claim.TenantID,
		OrganizationID:     claim.OrganizationID,
		ProjectID:          claim.ProjectID,
		SessionID:          claim.SessionID,
		LogicalWorkspaceID: claim.LogicalWorkspaceID,
		MaterializationID:  claim.MaterializationID,
		IncarnationID:      claim.IncarnationID,
		ExecutionTargetID:  claim.ExecutionTargetID,
		TargetKind:         claim.TargetKind,
		StorageScope:       claim.StorageScope,
		LayoutVersion:      claim.LayoutVersion,
		DispatchGeneration: claim.DispatchGeneration,
	}
}

func (d *Daemon) renewWorkspaceCleanupLoop(
	ctx context.Context,
	claim executions.WorkspaceCleanupClaim,
	cancel context.CancelFunc,
	result chan<- error,
) {
	ticker := time.NewTicker(d.config.LeaseRenewInterval)
	defer ticker.Stop()
	defer close(result)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requestContext, requestCancel := context.WithTimeout(ctx, d.config.RequestTimeout)
			err := d.client.RenewWorkspaceCleanup(requestContext, claim)
			requestCancel()
			if err != nil {
				select {
				case result <- fmt.Errorf("renew Workspace cleanup lease: %w", err):
				default:
				}
				cancel()
				return
			}
		}
	}
}

func (d *Daemon) reportWorkspaceCleanupFailure(
	claim executions.WorkspaceCleanupClaim,
	code, message string,
	retryable bool,
) error {
	if len(message) > 10_000 {
		message = message[:10_000]
	}
	reportContext, cancel := context.WithTimeout(context.Background(), d.shutdownRequestTimeout())
	defer cancel()
	if err := d.client.FailWorkspaceCleanup(reportContext, claim, code, message, retryable); err != nil {
		return fmt.Errorf("report Workspace cleanup failure: %w", err)
	}
	return nil
}

func (d *Daemon) releaseWorkspaceCleanupDuringShutdown(
	claim executions.WorkspaceCleanupClaim,
	reason string,
) {
	releaseContext, cancel := context.WithTimeout(context.Background(), d.shutdownRequestTimeout())
	defer cancel()
	if err := d.client.ReleaseWorkspaceCleanup(releaseContext, claim); err != nil {
		d.logger.Warn(
			"Workspace cleanup release during Drain failed",
			"cleanupId", claim.CleanupID,
			"dispatchGeneration", claim.DispatchGeneration,
			"reason", reason,
			"error", err,
		)
	}
}

func (d *Daemon) waitForDrain(
	parent context.Context,
	cancelRun context.CancelFunc,
	runDone <-chan struct{},
	drainMarked chan<- struct{},
) {
	defer close(drainMarked)
	select {
	case <-parent.Done():
	case <-runDone:
		return
	}
	if !d.draining.CompareAndSwap(false, true) {
		return
	}
	d.logger.Info("agentd draining", "deadline", d.config.DrainTimeout)
	deadline := time.Now().Add(d.config.DrainTimeout)
	heartbeatTimeout := min(d.shutdownRequestTimeout(), time.Until(deadline))
	if heartbeatTimeout > 0 {
		heartbeatContext, cancelHeartbeat := context.WithTimeout(context.Background(), heartbeatTimeout)
		if err := d.client.Heartbeat(heartbeatContext, d.config, true); err != nil {
			d.logger.Warn("worker Drain heartbeat failed", "error", err)
		}
		cancelHeartbeat()
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		cancelRun()
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
		d.logger.Warn("agentd Drain deadline reached; cancelling the active Runner")
		cancelRun()
	case <-runDone:
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
			err := d.client.Heartbeat(requestContext, d.config, d.draining.Load())
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
	executionContext, cancelExecution := context.WithCancel(ctx)
	renewErrors := make(chan error, 1)
	go d.renewLeaseLoop(executionContext, execution.ID, lease, cancelExecution, renewErrors)
	renewStopped := false
	stopRenewal := func() error {
		if renewStopped {
			return nil
		}
		renewStopped = true
		cancelExecution()
		return drainRenewError(renewErrors)
	}
	defer func() { _ = stopRenewal() }()
	materializer := d.workspace
	if materializer == nil {
		materializer = NewWorkspaceMaterializerWithCache(d.config.WorkspaceRoot, d.config.GitCacheRoot, d.config.ExecutionTargetID)
	}
	var err error
	var gitCredential *GitHTTPSCredential
	if workload.GitCredentialID != nil {
		resolved, resolveErr := d.client.ResolveGitCredential(
			executionContext, execution.ID, *workload.GitCredentialID, lease,
		)
		if resolveErr != nil {
			err = workspaceFailure(
				"credential_invalid", "The Project Git Credential could not be resolved.", true, false,
			)
		} else {
			gitCredential = &resolved.Payload
		}
	}
	materialized := WorkspaceMaterialization{}
	if err == nil {
		materialized, err = materializer.Materialize(executionContext, execution, workload, gitCredential)
	}
	if err == nil {
		defer func() {
			if releaseErr := materialized.Release(); releaseErr != nil {
				d.logger.Error("Workspace lock release failed", "executionId", execution.ID, "error", releaseErr)
			}
		}()
	}
	clearGitHTTPSCredential(gitCredential)
	gitCredential = nil
	if err == nil && workload.RestoreCheckpoint != nil {
		restorer, ok := materializer.(workspaceRestorer)
		if !ok {
			err = workspaceFailure(
				"workspace_invalid", "This Worker cannot restore the persisted Workspace Checkpoint.", true, true,
			)
		} else {
			artifactPath := ""
			var cleanup func()
			if workload.RestoreCheckpoint.ArtifactID != nil {
				artifactPath, cleanup, err = d.client.DownloadWorkspaceCheckpointArtifact(
					executionContext, execution.ID, lease, *workload.RestoreCheckpoint,
				)
				if err != nil {
					err = workspaceFailure(
						"workspace_invalid", "The Workspace Checkpoint Artifact could not be downloaded or verified.", true, true,
					)
				}
			}
			if err == nil {
				materialized, err = restorer.Restore(
					executionContext, materialized, *workload.RestoreCheckpoint, artifactPath,
				)
			}
			if cleanup != nil {
				cleanup()
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			renewErr := stopRenewal()
			d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached during Workspace preparation")
			return errors.Join(ctx.Err(), renewErr)
		}
		if executionContext.Err() != nil {
			if renewErr := stopRenewal(); renewErr != nil {
				return renewErr
			}
		}
		failureMessage := err.Error()
		if len(failureMessage) > 10_000 {
			failureMessage = failureMessage[:10_000]
		}
		var reportErr error
		if materialized.Managed || workload.RemoteWorkspaceID != nil {
			reportErr = d.client.MarkWorkspaceFailed(
				executionContext, execution.ID, lease, runnerFailureCode(err), failureMessage,
			)
		}
		failErr := d.failExecution(executionContext, execution.ID, lease, err)
		renewErr := stopRenewal()
		return errors.Join(failErr, reportErr, renewErr)
	}
	if materialized.Managed {
		if err := d.client.MarkWorkspaceReady(executionContext, execution.ID, lease, materialized); err != nil {
			if ctx.Err() != nil {
				renewErr := stopRenewal()
				d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached while reporting Workspace readiness")
				return errors.Join(ctx.Err(), renewErr)
			}
			failErr := d.failExecution(executionContext, execution.ID, lease, fmt.Errorf("persist prepared Workspace: %w", err))
			return errors.Join(failErr, stopRenewal())
		}
	}
	var credential *RunnerCredential
	if workload.ProviderCredentialID != nil {
		resolved, err := d.client.ResolveCredential(executionContext, execution.ID, *workload.ProviderCredentialID, lease)
		if err != nil {
			if ctx.Err() != nil {
				renewErr := stopRenewal()
				d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached while resolving Provider Credential")
				return errors.Join(ctx.Err(), renewErr)
			}
			failErr := d.failExecution(executionContext, execution.ID, lease, fmt.Errorf("resolve provider credential: %w", err))
			return errors.Join(failErr, stopRenewal())
		}
		credential = &resolved
	}
	if err := d.client.Start(executionContext, execution.ID, lease); err != nil {
		if ctx.Err() != nil {
			renewErr := stopRenewal()
			d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached before Provider start")
			return errors.Join(ctx.Err(), renewErr)
		}
		if renewErr := stopRenewal(); renewErr != nil {
			return renewErr
		}
		return fmt.Errorf("start execution: %w", err)
	}
	var controls <-chan RunnerControl
	if d.runner.protocol == RunnerProtocolV2 {
		controlChannel := make(chan RunnerControl)
		controls = controlChannel
		go d.runnerControlLoop(executionContext, execution.ID, lease, controlChannel)
	}
	result, runErr := d.runner.RunControlled(executionContext, RunnerInput{
		Execution: execution, Workload: workload, ProviderResumeCursor: resumeCursor, WorkspaceDirectory: materialized.Directory,
	}, credential, controls, func(messageContext context.Context, message RunnerMessage) error {
		switch message.Type {
		case "event":
			return d.client.AppendEvent(messageContext, execution.ID, lease, message)
		case "artifact":
			artifactPath, err := resolveWorkspaceArtifact(materialized.Directory, message.Artifact.Path)
			if err != nil {
				return err
			}
			if strings.TrimSpace(message.Artifact.OriginalName) == "" {
				message.Artifact.OriginalName = filepath.Base(artifactPath)
			}
			_, err = d.client.UploadArtifact(messageContext, execution.ID, lease, *message.Artifact, artifactPath)
			return err
		case "progress":
			return nil
		case "interaction":
			message, err := canonicalInteractionRuntimeEvent(message)
			if err != nil {
				return err
			}
			return d.client.AppendEvent(messageContext, execution.ID, lease, message)
		case "checkpoint":
			return validateCheckpointRequest(message.Payload)
		default:
			return protocolFailure("Provider Host emitted an unsupported Worker message")
		}
	})
	if runErr == nil && materialized.Managed {
		if inspector, ok := materializer.(workspaceInspector); ok {
			inspection, inspectErr := inspector.Inspect(executionContext, materialized)
			if inspectErr != nil {
				runErr = workspaceFailure(
					"workspace_invalid", "The Workspace state could not be inspected after Provider execution.", true, true,
				)
			} else {
				candidate, checkpointErr := captureWorkspaceCheckpoint(executionContext, execution, materialized, inspection)
				if checkpointErr != nil {
					runErr = workspaceFailure(
						"workspace_invalid", "The Workspace Checkpoint could not be captured.", true, true,
					)
				} else if checkpointMatchesRestored(candidate, workload.RestoreCheckpoint) {
					if candidate.Cleanup != nil {
						candidate.Cleanup()
					}
				} else {
					if inspection.Dirty {
						if dirtyErr := d.client.MarkWorkspaceDirty(executionContext, execution.ID, lease, inspection); dirtyErr != nil {
							runErr = fmt.Errorf("persist dirty Workspace state: %w", dirtyErr)
						}
					}
					if runErr == nil {
						if checkpointErr := d.persistWorkspaceCheckpoint(
							executionContext, execution, lease, candidate,
						); checkpointErr != nil {
							runErr = checkpointErr
						}
					} else if candidate.Cleanup != nil {
						candidate.Cleanup()
					}
				}
			}
		}
	}
	renewErr := stopRenewal()
	if runErr != nil && runnerFailurePersisted(runErr) {
		d.logger.Info("execution interrupted", "executionId", execution.ID, "generation", lease.Generation)
		return nil
	}
	if renewErr != nil {
		return renewErr
	}
	if runErr != nil {
		if ctx.Err() != nil {
			d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached")
			return ctx.Err()
		}
		return d.failExecution(ctx, execution.ID, lease, runErr)
	}
	if err := d.client.Complete(ctx, execution.ID, lease, result); err != nil {
		if ctx.Err() != nil {
			d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached before completion was acknowledged")
			return ctx.Err()
		}
		return fmt.Errorf("complete execution: %w", err)
	}
	d.logger.Info("execution completed", "executionId", execution.ID, "generation", lease.Generation)
	return nil
}

func clearGitHTTPSCredential(credential *GitHTTPSCredential) {
	if credential == nil {
		return
	}
	credential.Host = ""
	credential.Username = ""
	credential.Token = ""
}

func (d *Daemon) releaseDuringShutdown(executionID uuid.UUID, lease executions.Lease, reason string) {
	releaseContext, cancel := context.WithTimeout(context.Background(), d.shutdownRequestTimeout())
	defer cancel()
	if err := d.client.Release(releaseContext, executionID, lease, reason); err != nil {
		d.logger.Warn("execution release during Drain failed", "executionId", executionID, "generation", lease.Generation, "error", err)
	}
}

func (d *Daemon) shutdownRequestTimeout() time.Duration {
	timeout := d.config.RequestTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		return 5 * time.Second
	}
	return timeout
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

func canonicalInteractionRuntimeEvent(message RunnerMessage) (RunnerMessage, error) {
	if message.EventVersion != executions.RuntimeEventVersionV2 {
		return RunnerMessage{}, protocolFailure("Provider Host InteractionRequest did not use the negotiated Runtime Event version")
	}
	requestID, _ := message.Payload["requestId"].(string)
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 200 || strings.ContainsAny(requestID, "\r\n\t") {
		return RunnerMessage{}, protocolFailure("Provider Host InteractionRequest omitted a valid requestId")
	}
	payload := make(map[string]any, len(message.Payload)+2)
	for key, value := range message.Payload {
		payload[key] = value
	}
	payload["requestId"] = requestID

	interactionType, _ := payload["interactionType"].(string)
	switch strings.ToLower(strings.TrimSpace(interactionType)) {
	case "approval":
		requestType := canonicalApprovalRequestType(payload)
		payload["requestType"] = requestType
		if summary, ok := payload["summary"].(string); ok && strings.TrimSpace(summary) != "" {
			payload["detail"] = summary
		}
		message.EventType = "request.opened"
	case "user-input":
		questions, err := canonicalUserInputQuestions(payload["questions"])
		if err != nil {
			return RunnerMessage{}, err
		}
		payload["questions"] = questions
		message.EventType = "user-input.requested"
	default:
		return RunnerMessage{}, protocolFailure("Provider Host InteractionRequest omitted a supported interactionType")
	}
	message.Payload = payload
	return message, nil
}

func canonicalApprovalRequestType(payload map[string]any) string {
	if requestType, ok := payload["requestType"].(string); ok {
		switch requestType {
		case "command_execution_approval", "file_read_approval", "file_change_approval",
			"apply_patch_approval", "exec_command_approval", "dynamic_tool_call", "auth_tokens_refresh", "unknown":
			return requestType
		}
	}
	requestKind, _ := payload["requestKind"].(string)
	switch strings.ToLower(strings.TrimSpace(requestKind)) {
	case "command":
		return "command_execution_approval"
	case "file-read":
		return "file_read_approval"
	case "file-change":
		return "file_change_approval"
	case "apply-patch":
		return "apply_patch_approval"
	case "exec-command":
		return "exec_command_approval"
	case "dynamic-tool-call":
		return "dynamic_tool_call"
	default:
		return "unknown"
	}
}

func canonicalUserInputQuestions(value any) ([]any, error) {
	questions, ok := value.([]any)
	if !ok {
		return nil, protocolFailure("Provider Host user-input InteractionRequest questions are not an array")
	}
	result := make([]any, 0, len(questions))
	for _, value := range questions {
		question, ok := value.(map[string]any)
		if !ok || question == nil {
			return nil, protocolFailure("Provider Host user-input InteractionRequest contains an invalid question")
		}
		for _, key := range []string{"id", "header", "question"} {
			text, ok := question[key].(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, protocolFailure("Provider Host user-input InteractionRequest question omitted required text")
			}
		}
		optionsValue, found := question["options"]
		if !found || optionsValue == nil {
			optionsValue = []any{}
		}
		options, ok := optionsValue.([]any)
		if !ok {
			return nil, protocolFailure("Provider Host user-input InteractionRequest options are not an array")
		}
		normalizedOptions := make([]any, 0, len(options))
		for _, optionValue := range options {
			option, ok := optionValue.(map[string]any)
			if !ok || option == nil {
				return nil, protocolFailure("Provider Host user-input InteractionRequest contains an invalid option")
			}
			label, ok := option["label"].(string)
			if !ok || strings.TrimSpace(label) == "" {
				return nil, protocolFailure("Provider Host user-input InteractionRequest option omitted a label")
			}
			description, _ := option["description"].(string)
			if strings.TrimSpace(description) == "" {
				description = label
			}
			normalizedOption := make(map[string]any, len(option)+1)
			for key, value := range option {
				normalizedOption[key] = value
			}
			normalizedOption["description"] = description
			normalizedOptions = append(normalizedOptions, normalizedOption)
		}
		copy := make(map[string]any, len(question)+1)
		for key, value := range question {
			copy[key] = value
		}
		copy["options"] = normalizedOptions
		result = append(result, copy)
	}
	return result, nil
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
	result := make(map[string]any, len(base)+3)
	for key, value := range base {
		result[key] = value
	}
	featureFlags := map[string]any{}
	if raw, ok := base["featureFlags"].(map[string]any); ok {
		featureFlags = make(map[string]any, len(raw)+1)
		for key, value := range raw {
			featureFlags[key] = value
		}
	}
	delete(featureFlags, workerImageBuildFeatureFlag)
	if config.WorkerImageManifest != nil {
		featureFlags[workerImageBuildFeatureFlag] = config.WorkerImageManifest.featureFlagValue()
	}
	if len(featureFlags) == 0 {
		delete(result, "featureFlags")
	} else {
		result["featureFlags"] = featureFlags
	}
	result["providerHost"] = providerHost
	runtimeEventVersion := executions.RuntimeEventVersionV1
	if config.RunnerProtocol == RunnerProtocolV2 {
		runtimeEventVersion = executions.RuntimeEventVersionV2
	}
	workerRuntime := map[string]any{
		"workerBuildVersion":    config.Version,
		"workerProtocolMinimum": executions.WorkerProtocolVersion,
		"workerProtocolMaximum": executions.WorkerProtocolVersion,
		"runtimeEventMinimum":   runtimeEventVersion,
		"runtimeEventMaximum":   runtimeEventVersion,
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
