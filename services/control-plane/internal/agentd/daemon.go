package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

const (
	workspaceCleanupProbeExecutionInterval            = 4
	workspaceCleanupProbeMaximumInterval              = 30 * time.Second
	drainCheckpointTimeout                            = 2 * time.Second
	drainPreservationTimeout                          = 8 * time.Second
	drainCheckpointRenewMaximumAttempts               = 3
	drainCheckpointRenewRequestIDPrefix               = "drain-checkpoint-renew-"
	resourceSuspendReasonActiveIdleTimeout            = "active-idle-timeout"
	resourceSuspendRunnerActive                       = int32(0)
	resourceSuspendRunnerQuiescing                    = int32(1)
	resourceSuspendRunnerFinished                     = int32(2)
	providerCredentialAccessStatusActive              = "active"
	providerCredentialAccessStatusRefreshWindowClosed = "refresh-window-closed"
	providerCredentialAccessStatusUnavailable         = "credential-unavailable"
	providerCredentialAccessStatusExpired             = "expired"
)

var turnDiffArtifactEventNamespace = uuid.MustParse("c77821ff-66e7-5bd8-9618-e453ff4625f0")

type workspaceCleanupClaimSchedule struct {
	executionsSinceProbe int
	lastProbe            time.Time
}

type resourceSuspendResult struct {
	fenced            bool
	continueExecution bool
	disposition       string
	err               error
}

type providerCredentialAccessUpdate struct {
	access *ProviderCredentialAccess
	err    error
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
	return effectiveWorkerMode(config) == executions.WorkerModeGeneralPool
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
	containmentProbeContext, cancelContainmentProbe := protectedCgroupPreflightContext(ctx, d.config)
	processContainmentReport, err := protectedCgroupProbeHook(containmentProbeContext, d.config)
	if err != nil {
		cancelContainmentProbe()
		return fmt.Errorf("probe protected cgroup containment: %w", err)
	}
	cancelContainmentProbe()
	d.config.ProcessContainmentCapability = processContainmentReport.workerRuntimeProcessContainmentCapability()
	providerHostProbeContext, cancelProviderHostProbe := context.WithTimeout(ctx, d.config.RequestTimeout)
	providerHostCapabilities, err := d.runner.CapabilitySummary(providerHostProbeContext)
	cancelProviderHostProbe()
	if err != nil {
		return fmt.Errorf("probe Provider Host compatibility: %w", err)
	}
	d.config.Capabilities = withProviderHostCapabilities(
		d.config.Capabilities,
		providerHostCapabilities,
		d.config,
	)
	registered, err := d.client.Register(ctx, d.config)
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	workerMode := effectiveWorkerMode(d.config)
	d.logger.Info("agentd registered", "workerId", registered.Worker.ID, "executionTargetId", registered.Worker.ExecutionTargetID, "targetKind", registered.Worker.TargetKind, "workerMode", workerMode)
	runContext, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runDone := make(chan struct{})
	defer close(runDone)
	drainMarked := make(chan struct{})
	go d.waitForDrain(ctx, cancelRun, runDone, drainMarked)
	heartbeatContext, stopHeartbeat := context.WithCancel(runContext)
	defer stopHeartbeat()
	workerFatalErrors := make(chan error, 1)
	go d.heartbeatLoop(heartbeatContext, cancelRun, workerFatalErrors)
	cleanupSchedule := newWorkspaceCleanupClaimSchedule(time.Now())
	canClaimWorkspaceCleanup := workspaceCleanupClaimsEnabled(d.config)

	for {
		if d.draining.Load() || runContext.Err() != nil {
			if fatalErr := receiveWorkerFatalError(workerFatalErrors); fatalErr != nil {
				return fmt.Errorf("Worker authorization was revoked: %w", fatalErr)
			}
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
				if isWorkerRevocationError(cleanupErr) {
					cancelRun()
					return fmt.Errorf("claim Workspace cleanup: %w", cleanupErr)
				}
				d.logger.Warn("Workspace cleanup fairness probe failed", "error", cleanupErr)
			} else if claimedCleanup {
				continue
			}
		}
		claim, err := d.client.Claim(runContext, d.config)
		if err != nil {
			if isWorkerRevocationError(err) {
				cancelRun()
				return fmt.Errorf("claim execution: %w", err)
			}
			if d.draining.Load() || runContext.Err() != nil {
				if fatalErr := receiveWorkerFatalError(workerFatalErrors); fatalErr != nil {
					return fmt.Errorf("Worker authorization was revoked: %w", fatalErr)
				}
				if d.draining.Load() {
					<-drainMarked
				}
				return nil
			}
			d.logger.Warn("execution claim failed", "error", err)
			if !waitContext(runContext, d.config.PollInterval) {
				if fatalErr := receiveWorkerFatalError(workerFatalErrors); fatalErr != nil {
					return fmt.Errorf("Worker authorization was revoked: %w", fatalErr)
				}
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
					if isWorkerRevocationError(cleanupErr) {
						cancelRun()
						return fmt.Errorf("claim Workspace cleanup: %w", cleanupErr)
					}
					if d.draining.Load() || runContext.Err() != nil {
						if d.draining.Load() {
							<-drainMarked
						}
						return nil
					}
					d.logger.Warn("Workspace cleanup claim failed", "error", cleanupErr)
					if !waitContext(runContext, d.config.PollInterval) {
						if fatalErr := receiveWorkerFatalError(workerFatalErrors); fatalErr != nil {
							return fmt.Errorf("Worker authorization was revoked: %w", fatalErr)
						}
						return nil
					}
					continue
				}
				if claimedCleanup {
					continue
				}
			}
			if !waitContext(runContext, d.config.PollInterval) {
				if fatalErr := receiveWorkerFatalError(workerFatalErrors); fatalErr != nil {
					return fmt.Errorf("Worker authorization was revoked: %w", fatalErr)
				}
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
			if workerMode != executions.WorkerModeGeneralPool {
				return nil
			}
			continue
		}
		runErr := d.runExecution(runContext, *claim.Execution, *claim.Lease, *claim.Workload, claim.ProviderResumeCursor)
		if runErr != nil {
			if isWorkerRevocationError(runErr) {
				cancelRun()
				return fmt.Errorf("run execution after Worker revocation: %w", runErr)
			}
			if isContainmentError(runErr) {
				cancelRun()
				return fmt.Errorf("Worker process containment failed: %w", runErr)
			}
			d.logger.Error("execution runner failed", "executionId", claim.Execution.ID, "generation", claim.Lease.Generation, "error", runErr)
		}
		cleanupSchedule.recordExecution()
		// A Kubernetes Pod is physically bound to one Execution generation. It
		// must terminate after that attempt so the kubelet can publish an exact
		// terminal status; re-claiming a later generation from the old Pod would
		// bypass the Pod UID/name generation fence.
		if workerMode != executions.WorkerModeGeneralPool {
			if runErr != nil {
				return fmt.Errorf("%s execution attempt failed: %w", workerMode, runErr)
			}
			return nil
		}
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
		if isWorkerRevocationError(err) {
			return true, false, err
		}
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
			requestContext, requestCancel := context.WithTimeout(
				ctx,
				executionLeaseRenewalRequestTimeout(d.config),
			)
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

func (d *Daemon) heartbeatLoop(
	ctx context.Context,
	cancelRun context.CancelFunc,
	fatalErrors chan<- error,
) {
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
				if isWorkerRevocationError(err) {
					select {
					case fatalErrors <- err:
					default:
					}
					cancelRun()
					return
				}
				d.logger.Warn("worker heartbeat failed", "error", err)
			}
		}
	}
}

func receiveWorkerFatalError(fatalErrors <-chan error) error {
	select {
	case err := <-fatalErrors:
		return err
	default:
		return nil
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
	if err := executions.ValidateRecoveryBundle(execution, workload); err != nil {
		cancelExecution()
		return d.failExecution(ctx, execution.ID, lease, err)
	}
	renewErrors := make(chan error, 1)
	renewLeaseUpdates := make(chan executions.Lease, 1)
	var providerCredentialAccessRenewalsReady atomic.Bool
	go d.renewLeaseLoop(
		executionContext,
		execution.ID,
		lease,
		cancelExecution,
		renewErrors,
		renewLeaseUpdates,
		&providerCredentialAccessRenewalsReady,
	)
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
	executionGuard, guardErr := newExecutionSecretGuard(resumeCursor)
	if guardErr != nil {
		return d.failExecution(executionContext, execution.ID, lease, guardErr)
	}
	defer executionGuard.Close()
	executionContext = withExecutionSecretGuard(executionContext, executionGuard)
	materializer := d.workspace
	if materializer == nil {
		materializer = NewWorkspaceMaterializerWithCache(d.config.WorkspaceRoot, d.config.GitCacheRoot, d.config.ExecutionTargetID)
	}
	var err error
	var gitCredential *WorkspaceGitCredential
	gitCredential, err = resolveWorkspaceGitCredentialStage(
		executionContext, d.client, execution.ID, lease, workload.CredentialGrants, "git_fetch", executionGuard,
	)
	if err != nil && !secretguard.IsExposure(err) {
		err = workspaceFailure(
			"credential_invalid", "The Project Git Credential could not be resolved or validated.", true, false,
		)
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
	clearWorkspaceGitCredential(gitCredential)
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
	var artifactRoot *workspaceArtifactRoot
	if err == nil {
		artifactRoot, err = openWorkspaceArtifactRoot(materialized.Directory)
		if err != nil {
			err = workspaceFailure(
				"workspace_invalid", "The Workspace Artifact root could not be bound safely.", true, false,
			)
		}
	}
	if artifactRoot != nil {
		defer artifactRoot.Close()
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
		err = executionGuard.SanitizeError(err)
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
		failErr := d.failExecutionGuarded(executionContext, execution.ID, lease, executionGuard, err)
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
			failErr := d.failExecutionGuarded(executionContext, execution.ID, lease, executionGuard, fmt.Errorf("persist prepared Workspace: %w", err))
			return errors.Join(failErr, stopRenewal())
		}
	}
	memoryDocuments, err := d.resolveMemoryDocuments(executionContext, execution, lease, workload.MemoryReferences)
	if err != nil {
		if ctx.Err() != nil {
			renewErr := stopRenewal()
			d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached while restoring Agent Memory")
			return errors.Join(ctx.Err(), renewErr)
		}
		failErr := d.failExecutionGuarded(
			executionContext, execution.ID, lease, executionGuard,
			&runnerFailure{
				code: "memory_invalid", message: "The frozen Agent Memory could not be downloaded and verified.",
				requiresNewExecution: true, requiresUserAction: true,
				canReconstructFromHistory: true, canMoveWorker: true,
			},
		)
		return errors.Join(failErr, stopRenewal())
	}
	var credential *RunnerCredential
	var providerCredentialAccessDone <-chan error
	if workload.ProviderCredentialGrantID != nil || workload.ProviderCredentialID != nil {
		var (
			resolved RunnerCredential
			err      error
		)
		if workload.ProviderCredentialGrantID != nil {
			resolved, err = d.client.ResolveProviderCredentialGrant(
				executionContext, execution.ID, *workload.ProviderCredentialGrantID, lease,
			)
		} else {
			resolved, err = d.client.ResolveCredential(
				executionContext, execution.ID, *workload.ProviderCredentialID, lease,
			)
		}
		if err != nil {
			if ctx.Err() != nil {
				renewErr := stopRenewal()
				d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached while resolving Provider Credential")
				return errors.Join(ctx.Err(), renewErr)
			}
			failErr := d.failExecutionGuarded(executionContext, execution.ID, lease, executionGuard, fmt.Errorf("resolve provider credential: %w", err))
			return errors.Join(failErr, stopRenewal())
		}
		credential = &resolved
		if guardErr := executionGuard.AddProviderCredential(credential); guardErr != nil {
			clearRunnerCredential(credential)
			return d.failExecutionGuarded(executionContext, execution.ID, lease, executionGuard, guardErr)
		}
		if workload.ProviderCredentialGrantID != nil {
			initialAccess := credential.Access
			if err := validateInitialProviderCredentialAccess(*workload.ProviderCredentialGrantID, initialAccess, time.Now().UTC()); err != nil {
				clearRunnerCredential(credential)
				return d.failExecutionGuarded(executionContext, execution.ID, lease, executionGuard, err)
			}
			// Renewals begin before Workspace materialization so the Execution
			// Lease cannot expire. Publish access metadata only after the explicit
			// Grant resolve has attached serial 1; otherwise a delayed pre-resolve
			// renewal could replay an intentionally absent access block.
			providerCredentialAccessRenewalsReady.Store(true)
		}
		defer clearRunnerCredential(credential)
	}
	providerStateDirectory := ""
	if credential != nil && strings.EqualFold(strings.TrimSpace(workload.Provider), "codex") {
		providerStateDirectory, err = ensureWorkspaceProviderStateDirectory(materialized.LogicalRoot)
		if err != nil {
			failErr := d.failExecutionGuarded(
				executionContext,
				execution.ID,
				lease,
				executionGuard,
				workspaceFailure(
					"workspace_invalid",
					"The Workspace provider state directory is unavailable.",
					true,
					true,
				),
			)
			return errors.Join(failErr, stopRenewal())
		}
	}
	runtimeOutputRoot, err := newRuntimeOutputArtifactRoot()
	if err != nil {
		if ctx.Err() != nil {
			renewErr := stopRenewal()
			d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached while preparing Runtime Output")
			return errors.Join(ctx.Err(), renewErr)
		}
		failErr := d.failExecutionGuarded(
			executionContext,
			execution.ID,
			lease,
			executionGuard,
			fmt.Errorf("prepare Provider Runtime Output: %w", err),
		)
		return errors.Join(failErr, stopRenewal())
	}
	defer func() {
		if closeErr := runtimeOutputRoot.Close(); closeErr != nil {
			d.logger.Warn(
				"Runtime Output root cleanup failed",
				"executionId", execution.ID,
				"generation", lease.Generation,
				"error", closeErr,
			)
		}
	}()
	if err := prepareProtectedProviderExecutionFilesystem(
		d.config,
		materialized,
		providerStateDirectory,
		runtimeOutputRoot.directory,
	); err != nil {
		if ctx.Err() != nil {
			renewErr := stopRenewal()
			d.releaseDuringShutdown(execution.ID, lease, "agentd Drain deadline reached while preparing protected Provider filesystem access")
			return errors.Join(ctx.Err(), renewErr)
		}
		failErr := d.failExecutionGuarded(
			executionContext,
			execution.ID,
			lease,
			executionGuard,
			workspaceFailure(
				"workspace_invalid",
				"The protected Provider filesystem handoff is unavailable.",
				true,
				true,
			),
		)
		return errors.Join(failErr, stopRenewal())
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
	var primaryDelivery *executions.ControlCommandDelivery
	var primaryControl *RunnerPrimaryOperationControl
	if operation := workload.PrimaryOperation; operation != nil {
		delivery := executions.ControlCommandDelivery{
			ControlCommandID: operation.ControlCommandID,
			Provider:         operation.Provider, CommandType: operation.CommandType,
			CommandID: operation.CommandID, Payload: operation.Payload,
		}
		primaryDelivery = &delivery
		primaryControl = &RunnerPrimaryOperationControl{
			MarkDelivered: func(controlContext context.Context) error {
				return d.retryInteractionDeliveryUpdate(controlContext, func(requestContext context.Context) error {
					return d.client.MarkControlCommandDelivered(requestContext, execution.ID, lease, delivery)
				})
			},
		}
	}
	runnerContext, cancelRunnerCause := context.WithCancelCause(executionContext)
	cancelRunner := func() { cancelRunnerCause(nil) }
	defer cancelRunner()
	if workload.ProviderCredentialGrantID != nil && credential != nil && credential.Access != nil {
		providerCredentialAccessUpdates := startProviderCredentialAccessLeaseBridge(runnerContext, renewLeaseUpdates)
		providerCredentialAccessDone = startProviderCredentialAccessMonitor(
			runnerContext,
			*workload.ProviderCredentialGrantID,
			*credential.Access,
			providerCredentialAccessUpdates,
			cancelRunnerCause,
			func() time.Time { return time.Now().UTC() },
		)
	}
	var controls <-chan RunnerControl
	if d.runner.protocol == RunnerProtocolV2 {
		controlChannel := make(chan RunnerControl)
		controls = controlChannel
		go d.runnerControlLoop(runnerContext, execution.ID, lease, executionGuard, controlChannel)
	}
	terminalLogs := newTerminalLogCollector(d.client, execution.ID, lease, executionGuard)
	defer func() {
		if closeErr := terminalLogs.Close(); closeErr != nil {
			d.logger.Warn(
				"terminal log temp cleanup failed",
				"executionId", execution.ID,
				"generation", lease.Generation,
				"error", closeErr,
			)
		}
	}()
	resourceSuspendContext, cancelResourceSuspend := context.WithCancel(executionContext)
	defer cancelResourceSuspend()
	runnerStopped := make(chan error, 1)
	resourceSuspendResults := make(chan resourceSuspendResult, 1)
	var resourceSuspendRunnerState atomic.Int32
	go d.resourceSuspendLoop(
		resourceSuspendContext, execution, lease, workload, materializer, materialized,
		terminalLogs, cancelRunner, runnerStopped, &resourceSuspendRunnerState, resourceSuspendResults,
	)
	result, runErr := d.runner.RunControlled(runnerContext, RunnerInput{
		Execution: execution, Workload: workload, MemoryDocuments: memoryDocuments,
		ProviderResumeCursor: resumeCursor,
		WorkspaceDirectory:   materialized.Directory, ProviderStateDirectory: providerStateDirectory,
		RuntimeOutputDirectory: runtimeOutputRoot.directory,
	}, credential, primaryControl, controls, func(messageContext context.Context, message RunnerMessage) error {
		switch message.Type {
		case "event":
			return terminalLogs.Handle(messageContext, message)
		case "artifact":
			var sanitizeErr error
			message, sanitizeErr = executionGuard.SanitizeRunnerMessage(message)
			if sanitizeErr != nil {
				return sanitizeErr
			}
			if message.Artifact.SourceRoot == "runtime-output" {
				source, relativePath, err := runtimeOutputRoot.open(message.Artifact.Path)
				if err != nil {
					return err
				}
				defer source.Close()
				if message.Artifact.Kind == "terminal_log" {
					return terminalLogs.HandleArtifactSource(
						messageContext,
						*message.Artifact,
						source,
						message.OccurredAt,
					)
				}
				if message.Artifact.Kind != "diff" {
					return protocolFailure("Runner Runtime Output Artifact kind is unsupported")
				}
				info, err := source.rewind()
				if err != nil {
					return fmt.Errorf("rewind Diff Artifact source: %w", err)
				}
				if message.Artifact.ReportedSize == nil || *message.Artifact.ReportedSize != info.Size() {
					return protocolFailure("Provider Host Diff reportedSize does not match the bound file")
				}
				if strings.TrimSpace(message.Artifact.OriginalName) == "" {
					message.Artifact.OriginalName = filepath.Base(relativePath)
				}
				if err := executionGuard.RequireSafeStructuralString(message.Artifact.OriginalName); err != nil {
					return err
				}
				normalizedContentType, err := normalizeRunnerArtifactContentType(message.Artifact.ContentType)
				if err != nil {
					return err
				}
				if normalizedContentType != "text/x-diff; charset=utf-8" {
					return protocolFailure("Provider Host Diff Artifact Content-Type is invalid")
				}
				message.Artifact.ContentType = normalizedContentType
				safeSource, cleanupSafeSource, err := guardedArtifactUploadSource(
					messageContext, executionGuard, normalizedContentType, source,
				)
				if err != nil {
					return err
				}
				defer cleanupSafeSource()
				completed, err := d.client.uploadArtifactSource(
					messageContext,
					execution.ID,
					lease,
					*message.Artifact,
					safeSource,
				)
				if err != nil {
					return err
				}
				if completed.Status != "ready" || completed.ID == uuid.Nil || completed.Kind != "diff" ||
					completed.ContentType == nil || completed.SizeBytes == nil || completed.SHA256 == nil ||
					message.Artifact.FileCount == nil || message.Artifact.Additions == nil || message.Artifact.Deletions == nil {
					return errors.New("ready Diff Artifact is missing reference metadata")
				}
				if err := verifyReadyArtifactSource(messageContext, safeSource, normalizedContentType, completed); err != nil {
					return fmt.Errorf("verify ready Diff Artifact: %w", err)
				}
				eventID := uuid.NewSHA1(turnDiffArtifactEventNamespace, completed.ID[:])
				return d.client.AppendEvent(messageContext, execution.ID, lease, RunnerMessage{
					Type: "event", EventID: &eventID, EventVersion: executions.RuntimeEventVersionV2,
					EventType: "turn.diff.updated", OccurredAt: message.OccurredAt,
					Payload: map[string]any{"artifact": map[string]any{
						"artifactId": completed.ID.String(), "contentType": *completed.ContentType,
						"sizeBytes": *completed.SizeBytes, "sha256": *completed.SHA256,
						"fileCount": *message.Artifact.FileCount, "additions": *message.Artifact.Additions,
						"deletions": *message.Artifact.Deletions,
					}},
				})
			}
			if message.Artifact.SourceRoot != "" && message.Artifact.SourceRoot != "workspace" {
				return protocolFailure("Runner Artifact sourceRoot is unsupported")
			}
			source, relativePath, err := artifactRoot.open(message.Artifact.Path)
			if err != nil {
				return err
			}
			defer source.Close()
			if strings.TrimSpace(message.Artifact.OriginalName) == "" {
				message.Artifact.OriginalName = filepath.Base(relativePath)
			}
			if err := executionGuard.RequireSafeStructuralString(message.Artifact.OriginalName); err != nil {
				return err
			}
			normalizedContentType, err := normalizeRunnerArtifactContentType(message.Artifact.ContentType)
			if err != nil {
				return err
			}
			message.Artifact.ContentType = normalizedContentType
			safeSource, cleanupSafeSource, err := guardedArtifactUploadSource(
				messageContext, executionGuard, normalizedContentType, source,
			)
			if err != nil {
				return err
			}
			defer cleanupSafeSource()
			_, err = d.client.uploadArtifactSource(
				messageContext,
				execution.ID,
				lease,
				*message.Artifact,
				safeSource,
			)
			return err
		case "progress":
			return nil
		case "interaction":
			message, err := canonicalInteractionRuntimeEvent(message)
			if err != nil {
				return err
			}
			message, err = executionGuard.SanitizeRunnerMessage(message)
			if err != nil {
				return err
			}
			return d.client.AppendEvent(messageContext, execution.ID, lease, message)
		case "checkpoint":
			payload, err := executionGuard.SanitizeMap(message.Payload)
			if err != nil {
				return err
			}
			return validateCheckpointRequest(payload)
		default:
			return protocolFailure("Provider Host emitted an unsupported Worker message")
		}
	})
	if errors.Is(runErr, context.Canceled) {
		var accessFailure *runnerFailure
		if errors.As(context.Cause(runnerContext), &accessFailure) {
			runErr = accessFailure
		}
	}
	cancelRunner()
	if providerCredentialAccessDone != nil {
		// Cancellation bounds this wait: the monitor either already selected a
		// credential failure or observes runnerContext.Done and exits. Draining
		// it prevents a nil natural-completion cancel from hiding an in-flight
		// access-expiry decision.
		if monitorErr := <-providerCredentialAccessDone; runErr == nil && monitorErr != nil {
			runErr = monitorErr
		}
	}
	resourceSuspendOwnsRunner := finishRunnerForResourceSuspend(&resourceSuspendRunnerState, runnerStopped, runErr)
	if !resourceSuspendOwnsRunner {
		_, resourceSuspendOwnsRunner = runnerSuspendedTerminal(runErr)
	}
	if !resourceSuspendOwnsRunner {
		cancelResourceSuspend()
	} else {
		suspendResult := <-resourceSuspendResults
		cancelResourceSuspend()
		if suspendResult.continueExecution {
			goto resourceSuspendHandled
		}
		if suspendResult.err != nil {
			suspendResult.err = executionGuard.SanitizeError(suspendResult.err)
		}
		renewErr := stopRenewal()
		if suspendResult.fenced {
			if suspendResult.err != nil {
				d.logger.Warn(
					"execution resource suspension required a fenced recovery path",
					"executionId", execution.ID,
					"generation", lease.Generation,
					"disposition", suspendResult.disposition,
					"error", suspendResult.err,
				)
			}
			if renewErr != nil {
				d.logger.Warn(
					"execution Lease renewal ended after resource suspension",
					"executionId", execution.ID,
					"generation", lease.Generation,
					"error", renewErr,
				)
			}
			d.logger.Info(
				"execution resource suspension stopped the local Provider generation",
				"executionId", execution.ID,
				"generation", lease.Generation,
				"disposition", suspendResult.disposition,
			)
			return nil
		}
		if ctx.Err() != nil {
			preservationStatus := d.persistExecutionStateDuringDrain(
				execution, lease, workload, materializer, materialized, terminalLogs,
			)
			d.releaseDuringShutdown(
				execution.ID,
				lease,
				appendDrainCheckpointStatus(
					"agentd Drain interrupted resource suspension after Provider quiesce",
					preservationStatus,
				),
			)
			return errors.Join(ctx.Err(), suspendResult.err, renewErr)
		}
		d.logger.Warn(
			"execution resource suspension could not confirm an authoritative transition; Lease renewal remains fenced",
			"executionId", execution.ID,
			"generation", lease.Generation,
			"error", suspendResult.err,
		)
		return errors.Join(suspendResult.err, renewErr)
	}
resourceSuspendHandled:
	if ctx.Err() == nil {
		hadOpenTerminals, terminalErr := terminalLogs.FinalizeOpen(executionContext, "provider_error")
		if terminalErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("flush terminal logs: %w", terminalErr))
		}
		if hadOpenTerminals && runErr == nil {
			runErr = protocolFailure("Provider Host completed with an open terminal")
		}
	}
	if runErr != nil {
		runErr = executionGuard.SanitizeError(runErr)
	} else {
		result, runErr = executionGuard.SanitizeResult(result)
	}
	drainPreservationStatus := ""
	drainPreservationKnown := false
	if runErr == nil && materialized.Managed && primaryDelivery == nil {
		checkpointCreated, checkpointErr := d.persistManagedWorkspaceState(
			executionContext, execution, lease, workload, materializer, materialized,
		)
		if checkpointErr != nil {
			runErr = checkpointErr
		} else {
			drainPreservationKnown = true
			drainPreservationStatus = appendDrainCheckpointStatus(
				"terminal-log=flushed",
				drainCheckpointReleaseStatus(checkpointCreated),
			)
		}
	}
	preserveForDrain := func() string {
		if drainPreservationKnown {
			return drainPreservationStatus
		}
		drainPreservationKnown = true
		drainPreservationStatus = d.persistExecutionStateDuringDrain(
			execution, lease, workload, materializer, materialized, terminalLogs,
		)
		return drainPreservationStatus
	}
	renewErr := stopRenewal()
	if ctx.Err() != nil {
		preserveForDrain()
	}
	if runErr != nil && runnerFailurePersisted(runErr) {
		d.logger.Info("execution interrupted", "executionId", execution.ID, "generation", lease.Generation)
		return nil
	}
	if ctx.Err() != nil {
		reason := "agentd Drain deadline reached"
		if runErr == nil {
			reason = "agentd Drain deadline reached before completion was acknowledged"
		}
		d.releaseDuringShutdown(
			execution.ID,
			lease,
			appendDrainCheckpointStatus(reason, preserveForDrain()),
		)
		return errors.Join(ctx.Err(), runErr, renewErr)
	}
	if renewErr != nil {
		return renewErr
	}
	if runErr != nil {
		return d.failExecutionGuarded(ctx, execution.ID, lease, executionGuard, runErr)
	}
	if primaryDelivery != nil {
		if result.PrimaryOperationResult == nil {
			return d.failExecutionGuarded(ctx, execution.ID, lease, executionGuard, protocolFailure("Primary Provider operation omitted its terminal Result payload"))
		}
		if err := d.retryInteractionDeliveryUpdate(ctx, func(requestContext context.Context) error {
			return d.client.AcknowledgeControlCommand(
				requestContext, execution.ID, lease, *primaryDelivery, result.PrimaryOperationResult,
			)
		}); err != nil {
			if ctx.Err() != nil {
				d.releaseDuringShutdown(
					execution.ID,
					lease,
					"agentd Drain deadline reached before the primary Provider operation was acknowledged",
				)
				return ctx.Err()
			}
			return fmt.Errorf("acknowledge primary Provider operation: %w", err)
		}
		d.logger.Info(
			"primary Provider operation completed",
			"executionId", execution.ID,
			"generation", lease.Generation,
			"commandType", primaryDelivery.CommandType,
		)
		return nil
	}
	if err := d.client.Complete(ctx, execution.ID, lease, result); err != nil {
		if ctx.Err() != nil {
			d.releaseDuringShutdown(
				execution.ID,
				lease,
				appendDrainCheckpointStatus(
					"agentd Drain deadline reached before completion was acknowledged",
					preserveForDrain(),
				),
			)
			return ctx.Err()
		}
		return fmt.Errorf("complete execution: %w", err)
	}
	d.logger.Info("execution completed", "executionId", execution.ID, "generation", lease.Generation)
	return nil
}

func finishRunnerForResourceSuspend(state *atomic.Int32, stopped chan<- error, runnerErr error) bool {
	resourceSuspendOwnsRunner := !state.CompareAndSwap(
		resourceSuspendRunnerActive,
		resourceSuspendRunnerFinished,
	)
	// Publish the terminal owner before waking the suspend coordinator. Closing
	// first would let a late directive steal a naturally completed generation.
	stopped <- runnerErr
	close(stopped)
	return resourceSuspendOwnsRunner
}

func (d *Daemon) resourceSuspendLoop(
	ctx context.Context,
	execution executions.Execution,
	lease executions.Lease,
	workload executions.Workload,
	materializer workspaceMaterializer,
	materialized WorkspaceMaterialization,
	terminalLogs *terminalLogCollector,
	cancelRunner context.CancelFunc,
	runnerStopped <-chan error,
	runnerState *atomic.Int32,
	results chan<- resourceSuspendResult,
) {
	interval := d.config.PollInterval
	if interval <= 0 || interval > time.Second {
		interval = time.Second
	}
	for {
		requestContext, cancel := context.WithTimeout(ctx, d.config.RequestTimeout)
		directive, err := d.client.PullResourceDirective(requestContext, execution.ID, lease)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.logger.Warn(
				"resource directive pull failed",
				"executionId", execution.ID,
				"generation", lease.Generation,
				"error", err,
			)
			if !waitContext(ctx, interval) {
				return
			}
			continue
		}
		if directive == nil {
			if !waitContext(ctx, interval) {
				return
			}
			continue
		}
		if directive.Action == "terminate" {
			if !runnerState.CompareAndSwap(resourceSuspendRunnerActive, resourceSuspendRunnerQuiescing) {
				return
			}
			cancelRunner()
			select {
			case runnerErr := <-runnerStopped:
				if isContainmentError(runnerErr) {
					results <- resourceSuspendResult{disposition: "containment-failed", err: runnerErr}
					return
				}
				if runnerErr != nil && !errors.Is(runnerErr, context.Canceled) {
					runnerErr = fmt.Errorf("Provider stopped after authoritative resource termination: %w", runnerErr)
				}
				results <- resourceSuspendResult{
					fenced: true, disposition: "absolute-expired", err: runnerErr,
				}
			case <-ctx.Done():
				results <- resourceSuspendResult{
					err: fmt.Errorf("wait for Provider termination after absolute expiry: %w", ctx.Err()),
				}
			}
			return
		}
		if directive.Action != "suspend" {
			d.logger.Warn(
				"unknown resource directive ignored",
				"executionId", execution.ID,
				"generation", lease.Generation,
				"action", directive.Action,
			)
			if !waitContext(ctx, interval) {
				return
			}
			continue
		}
		if directive.Reason == resourceSuspendReasonActiveIdleTimeout {
			state := runnerState.Load()
			switch state {
			case resourceSuspendRunnerActive:
				// Arbitration prevents a directive pulled concurrently with
				// natural completion from starting a second terminal lifecycle.
				if !runnerState.CompareAndSwap(resourceSuspendRunnerActive, resourceSuspendRunnerQuiescing) {
					return
				}
				waitUntil := directive.CheckpointDeadlineAt
				waitContext, cancelWait := context.WithDeadline(ctx, waitUntil)
				runnerErr, waitErr := waitForRunnerStop(waitContext, runnerStopped)
				cancelWait()
				if waitErr == nil {
					results <- d.finishActiveTurnResourceSuspend(
						ctx,
						execution,
						lease,
						workload,
						materializer,
						materialized,
						terminalLogs,
						*directive,
						runnerErr,
						true,
					)
					return
				}
				if !errors.Is(waitErr, context.DeadlineExceeded) {
					results <- resourceSuspendResult{
						err: fmt.Errorf("wait for Provider stop before active-turn resource suspension: %w", waitErr),
					}
					return
				}
				cancelRunner()
				runnerErr, waitErr = waitForRunnerStop(ctx, runnerStopped)
				if waitErr != nil {
					results <- resourceSuspendResult{
						err: fmt.Errorf("wait for Provider stop after active-turn suspension timeout: %w", waitErr),
					}
					return
				}
				results <- d.finishActiveTurnResourceSuspend(
					ctx,
					execution,
					lease,
					workload,
					materializer,
					materialized,
					terminalLogs,
					*directive,
					errors.Join(
						runnerErr,
						errors.New("Provider did not stop before the active-turn suspend checkpoint deadline"),
					),
					false,
				)
				return
			case resourceSuspendRunnerFinished:
				runnerErr, waitErr := waitForRunnerStop(ctx, runnerStopped)
				if waitErr != nil {
					results <- resourceSuspendResult{
						err: fmt.Errorf("load stopped Provider outcome for active-turn resource suspension: %w", waitErr),
					}
					return
				}
				results <- d.finishActiveTurnResourceSuspend(
					ctx,
					execution,
					lease,
					workload,
					materializer,
					materialized,
					terminalLogs,
					*directive,
					runnerErr,
					true,
				)
				return
			default:
				return
			}
		}

		// Arbitration prevents a directive pulled concurrently with natural
		// Provider completion from starting a second terminal lifecycle. Once
		// quiescing wins, the old generation must never be resumed locally.
		if !runnerState.CompareAndSwap(resourceSuspendRunnerActive, resourceSuspendRunnerQuiescing) {
			return
		}
		cancelRunner()
		select {
		case runnerErr := <-runnerStopped:
			// RunControlled does not return until the Provider process tree has
			// terminated, so Workspace and terminal state are stable from here.
			if isContainmentError(runnerErr) {
				d.abortResourceSuspend(
					ctx, execution.ID, lease, *directive,
					"provider_containment_failed",
					"The Provider process containment boundary could not prove an empty process tree.",
				)
				results <- resourceSuspendResult{
					disposition: "containment-failed",
					err:         runnerErr,
				}
				return
			}
			if runnerErr != nil && !errors.Is(runnerErr, context.Canceled) {
				results <- d.recoverAfterQuiescedResourceSuspendFailure(
					ctx, execution.ID, lease, *directive,
					"provider_quiesce_failed",
					"The Provider stopped with an unexpected error while preparing resource suspension.",
					runnerErr,
				)
				return
			}
		case <-ctx.Done():
			results <- resourceSuspendResult{
				err: fmt.Errorf("wait for Provider quiesce before resource suspension: %w", ctx.Err()),
			}
			return
		}
		quiesceContext, cancelQuiesce := context.WithDeadline(ctx, directive.CheckpointDeadlineAt)
		_, quiesceErr := d.client.MarkResourceSuspendQuiesced(
			quiesceContext, execution.ID, lease, *directive,
		)
		cancelQuiesce()
		if quiesceErr != nil {
			results <- d.recoverAfterQuiescedResourceSuspendFailure(
				ctx, execution.ID, lease, *directive,
				"provider_quiesce_record_failed",
				"The Provider stopped, but its durable quiesce proof could not be recorded.",
				fmt.Errorf("record durable Provider quiesce proof: %w", quiesceErr),
			)
			return
		}
		results <- d.completeResourceSuspendAfterProviderQuiesce(
			ctx, execution, lease, workload, materializer, materialized, terminalLogs, *directive,
		)
		return
	}
}

func waitForRunnerStop(ctx context.Context, runnerStopped <-chan error) (error, error) {
	select {
	case runnerErr, open := <-runnerStopped:
		if !open {
			return nil, nil
		}
		return runnerErr, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *Daemon) finishActiveTurnResourceSuspend(
	ctx context.Context,
	execution executions.Execution,
	lease executions.Lease,
	workload executions.Workload,
	materializer workspaceMaterializer,
	materialized WorkspaceMaterialization,
	terminalLogs *terminalLogCollector,
	directive executions.ResourceDirective,
	runnerErr error,
	allowNaturalCompletion bool,
) resourceSuspendResult {
	if runnerErr == nil {
		if allowNaturalCompletion {
			return resourceSuspendResult{
				continueExecution: true,
				disposition:       "completed-before-active-suspend",
			}
		}
		runnerErr = errors.New("active-turn suspension abandoned local execution ownership before the Provider stopped")
	}
	if _, suspended := runnerSuspendedTerminal(runnerErr); suspended {
		quiesceContext, cancelQuiesce := context.WithTimeout(ctx, d.config.RequestTimeout)
		_, quiesceErr := d.client.MarkResourceSuspendQuiesced(
			quiesceContext, execution.ID, lease, directive,
		)
		cancelQuiesce()
		if quiesceErr == nil {
			return d.completeResourceSuspendAfterProviderQuiesce(
				ctx,
				execution,
				lease,
				workload,
				materializer,
				materialized,
				terminalLogs,
				directive,
			)
		}
		return d.recoverAfterQuiescedResourceSuspendFailure(
			ctx,
			execution.ID,
			lease,
			directive,
			"provider_quiesce_record_failed",
			"The Provider stopped, but its durable active-turn checkpoint receipt could not be confirmed.",
			errors.Join(runnerErr, fmt.Errorf("record durable Provider quiesce proof: %w", quiesceErr)),
		)
	}
	return d.recoverAfterActiveTurnSuspendFailure(ctx, execution.ID, lease, directive, runnerErr)
}

func (d *Daemon) recoverAfterActiveTurnSuspendFailure(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	directive executions.ResourceDirective,
	cause error,
) resourceSuspendResult {
	quiesceContext, cancelQuiesce := context.WithTimeout(ctx, d.config.RequestTimeout)
	_, quiesceErr := d.client.MarkResourceSuspendQuiesced(
		quiesceContext, executionID, lease, directive,
	)
	cancelQuiesce()
	if quiesceErr == nil {
		return d.recoverAfterQuiescedResourceSuspendFailure(
			ctx,
			executionID,
			lease,
			directive,
			"provider_quiesce_failed",
			"The Provider stopped before the active-turn suspend checkpoint could be committed.",
			cause,
		)
	}
	return d.recoverAfterQuiescedResourceSuspendFailure(
		ctx,
		executionID,
		lease,
		directive,
		"provider_quiesce_record_failed",
		"The Provider stopped, but its durable active-turn checkpoint receipt could not be confirmed.",
		errors.Join(cause, fmt.Errorf("record durable Provider quiesce proof: %w", quiesceErr)),
	)
}

func (d *Daemon) completeResourceSuspendAfterProviderQuiesce(
	ctx context.Context,
	execution executions.Execution,
	lease executions.Lease,
	workload executions.Workload,
	materializer workspaceMaterializer,
	materialized WorkspaceMaterialization,
	terminalLogs *terminalLogCollector,
	directive executions.ResourceDirective,
) resourceSuspendResult {
	checkpointContext, cancelCheckpoint := context.WithDeadline(ctx, directive.CheckpointDeadlineAt)
	hadOpenTerminals, terminalErr := terminalLogs.FinalizeOpen(checkpointContext, "timeout")

	checkpointStatus := "unchanged"
	var checkpointErr error
	if materialized.Managed {
		var checkpointCreated bool
		checkpointCreated, checkpointErr = d.persistManagedWorkspaceState(
			checkpointContext, execution, lease, workload, materializer, materialized,
		)
		if checkpointCreated {
			checkpointStatus = "ready"
		}
	}
	if terminalErr != nil {
		cancelCheckpoint()
		return d.recoverAfterQuiescedResourceSuspendFailure(
			ctx, execution.ID, lease, directive,
			"terminal_checkpoint_failed",
			"Terminal state could not be finalized after the Provider was quiesced.",
			fmt.Errorf("finalize terminal state before resource suspension: %w", terminalErr),
		)
	}
	if checkpointErr != nil {
		cancelCheckpoint()
		return d.recoverAfterQuiescedResourceSuspendFailure(
			ctx, execution.ID, lease, directive,
			"workspace_checkpoint_failed",
			"The managed Workspace could not be checkpointed after the Provider was quiesced.",
			fmt.Errorf("checkpoint Workspace before resource suspension: %w", checkpointErr),
		)
	}
	if hadOpenTerminals {
		cancelCheckpoint()
		return d.recoverAfterQuiescedResourceSuspendFailure(
			ctx, execution.ID, lease, directive,
			"terminal_activity_in_progress",
			"A live terminal was finalized while the Provider was being quiesced; the Execution will recover in a new generation.",
			errors.New("resource suspension encountered terminal activity during Provider quiesce"),
		)
	}

	completionMode := directive.CompletionMode
	if completionMode == "" {
		completionMode = executions.ResourceSuspendCompletionWorkerAttestedV1
	}
	if completionMode == executions.ResourceSuspendCompletionKubernetesPodTerminalV1 {
		_, readyErr := d.client.MarkResourceSuspendCheckpointReady(
			checkpointContext, execution.ID, lease, directive, checkpointStatus,
		)
		cancelCheckpoint()
		if readyErr == nil {
			return resourceSuspendResult{
				fenced: true, disposition: "checkpoint-ready-awaiting-pod-terminal",
			}
		}
		return d.recoverAfterQuiescedResourceSuspendFailure(
			ctx, execution.ID, lease, directive,
			"suspend_checkpoint_handoff_failed",
			"The suspend Checkpoint could not be handed off for Kubernetes Pod-terminal verification; the Execution will recover in a new generation.",
			fmt.Errorf("record Kubernetes suspend Checkpoint handoff: %w", readyErr),
		)
	}
	if completionMode != executions.ResourceSuspendCompletionWorkerAttestedV1 {
		cancelCheckpoint()
		return d.recoverAfterQuiescedResourceSuspendFailure(
			ctx, execution.ID, lease, directive,
			"suspend_completion_mode_unsupported",
			"The resource suspension completion mode is unsupported by this Worker; the Execution will recover in a new generation.",
			fmt.Errorf("unsupported resource suspension completion mode %q", completionMode),
		)
	}

	stopRunner, completeErr := d.commitResourceSuspend(
		checkpointContext, execution.ID, lease, directive, checkpointStatus,
	)
	cancelCheckpoint()
	if stopRunner {
		disposition := "suspended"
		if completeErr != nil {
			disposition = "suspend-commit-ambiguous"
		}
		return resourceSuspendResult{fenced: true, disposition: disposition, err: completeErr}
	}
	return d.recoverAfterQuiescedResourceSuspendFailure(
		ctx, execution.ID, lease, directive,
		"suspend_commit_failed",
		"The resource suspension commit was rejected after the Provider was quiesced; the Execution will recover in a new generation.",
		completeErr,
	)
}

func (d *Daemon) recoverAfterQuiescedResourceSuspendFailure(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	directive executions.ResourceDirective,
	failureCode, failureMessage string,
	cause error,
) resourceSuspendResult {
	releaseContext, cancelRelease := context.WithTimeout(ctx, d.config.RequestTimeout)
	releaseErr := d.client.ReleaseAfterQuiescedResourceSuspend(
		releaseContext,
		executionID,
		lease,
		"Provider quiesced before resource suspension could be committed; recover in a new generation.",
	)
	cancelRelease()
	if releaseErr == nil {
		return resourceSuspendResult{fenced: true, disposition: "recovering", err: cause}
	}
	// Release must see the still-active durable quiesce proof so it can retain
	// pending callbacks while moving to a new Generation. Abort only after a
	// failed/ambiguous release; aborting first would erase that provenance and
	// turn a safe recovery into callback loss.
	d.abortResourceSuspend(ctx, executionID, lease, directive, failureCode, failureMessage)
	return resourceSuspendResult{
		disposition: "lease-expiry-recovery",
		err:         errors.Join(cause, fmt.Errorf("release quiesced Execution for recovery: %w", releaseErr)),
	}
}

// commitResourceSuspend distinguishes a definitive semantic rejection from an
// acknowledgement that may have been lost after the Control Plane committed.
// The completion request ID is deterministic, so a fresh-context replay reads
// the atomic idempotency receipt. The Provider is already quiesced before this
// function runs; if transport ambiguity remains, stopping Lease renewal lets
// recovery reconcile the server side if the original commit did not land.
func (d *Daemon) commitResourceSuspend(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	directive executions.ResourceDirective,
	checkpointStatus string,
) (bool, error) {
	requestContext, cancel := context.WithDeadline(ctx, directive.CheckpointDeadlineAt)
	err := d.client.CompleteResourceSuspend(
		requestContext, executionID, lease, directive, checkpointStatus,
	)
	cancel()
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, err
	}
	if resourceSuspendCompletionRejected(err) {
		return false, err
	}

	confirmationContext, cancelConfirmation := context.WithTimeout(ctx, d.config.RequestTimeout)
	confirmationErr := d.client.CompleteResourceSuspend(
		confirmationContext, executionID, lease, directive, checkpointStatus,
	)
	cancelConfirmation()
	if confirmationErr == nil {
		return true, nil
	}
	combined := errors.Join(err, fmt.Errorf("confirm resource suspension commit: %w", confirmationErr))
	if ctx.Err() != nil {
		return false, combined
	}
	if resourceSuspendCompletionRejected(confirmationErr) {
		return false, combined
	}
	return true, combined
}

func resourceSuspendCompletionRejected(err error) bool {
	var controlPlaneErr *controlPlaneProblem
	if !errors.As(err, &controlPlaneErr) {
		return false
	}
	switch controlPlaneErr.Code {
	case "invalid_resource_suspend_completion",
		"session_absolute_expired",
		"session_not_found",
		"lease_not_current",
		"generation_fenced",
		"resource_suspend_state_conflict",
		"resource_suspend_attempt_not_found",
		"resource_suspend_attempt_finished",
		"resource_suspend_checkpoint_expired",
		"resource_suspend_no_longer_safe",
		"resource_suspend_workspace_unavailable",
		"resource_suspend_checkpoint_required",
		"resource_suspend_quiesce_required",
		"active_suspend_activity_boundary_missing",
		"active_suspend_receipt_required",
		"active_suspend_receipt_invalid",
		"active_suspend_receipt_integrity_failed",
		"active_suspend_control_unacknowledged",
		"active_suspend_cursor_drift",
		"active_suspend_primary_outcome_unknown",
		"active_suspend_session_unavailable":
		return true
	default:
		return false
	}
}

func (d *Daemon) abortResourceSuspend(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	directive executions.ResourceDirective,
	failureCode, failureMessage string,
) {
	if len(failureMessage) > 2000 {
		failureMessage = failureMessage[:2000]
	}
	requestContext, cancel := context.WithTimeout(ctx, d.config.RequestTimeout)
	defer cancel()
	if err := d.client.AbortResourceSuspend(
		requestContext, executionID, lease, directive, failureCode, failureMessage,
	); err != nil && ctx.Err() == nil {
		d.logger.Warn(
			"resource suspension abort could not be persisted",
			"executionId", executionID,
			"generation", lease.Generation,
			"suspendAttemptId", directive.SuspendAttemptID,
			"error", err,
		)
	}
}

func (d *Daemon) resolveMemoryDocuments(
	ctx context.Context,
	execution executions.Execution,
	lease executions.Lease,
	references []executions.RecoveryMemoryReference,
) ([]MemoryDocument, error) {
	if len(references) > executions.MaximumRecoveryMemoryReferences {
		return nil, errors.New("Recovery Bundle contains too many Agent Memory references")
	}
	documents := make([]MemoryDocument, 0, len(references))
	totalBytes := 0
	for _, reference := range references {
		document, err := d.client.DownloadMemoryArtifact(ctx, execution.ID, lease, reference)
		if err != nil {
			return nil, err
		}
		totalBytes += len(document.Content)
		if totalBytes > executions.MaximumRecoveryMemoryTotalBytes {
			return nil, errors.New("Recovery Bundle Agent Memory exceeds the total size limit")
		}
		documents = append(documents, document)
	}
	return documents, nil
}

func (d *Daemon) persistManagedWorkspaceState(
	ctx context.Context,
	execution executions.Execution,
	lease executions.Lease,
	workload executions.Workload,
	materializer workspaceMaterializer,
	materialized WorkspaceMaterialization,
) (bool, error) {
	inspector, ok := materializer.(workspaceInspector)
	if !ok {
		return false, workspaceFailure(
			"workspace_invalid", "This Worker cannot inspect a managed Workspace before Checkpoint.", true, true,
		)
	}
	inspection, err := inspector.Inspect(ctx, materialized)
	if err != nil {
		return false, workspaceFailure(
			"workspace_invalid", "The Workspace state could not be inspected after Provider execution.", true, true,
		)
	}
	candidate, err := captureWorkspaceCheckpoint(ctx, execution, materialized, inspection)
	if err != nil {
		return false, workspaceFailure(
			"workspace_invalid", "The Workspace Checkpoint could not be captured.", true, true,
		)
	}
	if checkpointMatchesRestored(candidate, workload.RestoreCheckpoint) {
		if candidate.Cleanup != nil {
			candidate.Cleanup()
		}
		return false, nil
	}
	if candidate.Strategy != "git-reference" {
		if err := d.client.MarkWorkspaceDirty(ctx, execution.ID, lease, inspection); err != nil {
			if candidate.Cleanup != nil {
				candidate.Cleanup()
			}
			return false, fmt.Errorf("persist dirty Workspace state: %w", err)
		}
	}
	if err := d.persistWorkspaceCheckpoint(ctx, execution, lease, candidate); err != nil {
		return false, err
	}
	return true, nil
}

func (d *Daemon) persistExecutionStateDuringDrain(
	execution executions.Execution,
	lease executions.Lease,
	workload executions.Workload,
	materializer workspaceMaterializer,
	materialized WorkspaceMaterialization,
	terminalLogs *terminalLogCollector,
) string {
	terminalPending := terminalLogs.HasOpen()
	workspacePending := materialized.Managed
	if !terminalPending && !workspacePending {
		return "terminal-log=flushed"
	}
	preservationContext, cancel := context.WithTimeout(context.Background(), drainPreservationTimeout)
	defer cancel()
	preservationContext = withExecutionSecretGuard(preservationContext, terminalLogs.guard)
	renewContext, cancelRenew := context.WithTimeout(preservationContext, drainCheckpointTimeout)
	err := d.renewLeaseForDrainCheckpoint(renewContext, execution.ID, lease)
	cancelRenew()
	if err != nil {
		d.logger.Warn(
			"execution persistence Lease probe during Drain failed",
			"executionId", execution.ID,
			"generation", lease.Generation,
			"error", err,
		)
		status := "terminal-log=flushed"
		if terminalPending {
			status = "data-loss-risk=terminal-log-flush-failed"
		}
		if workspacePending {
			status = appendDrainCheckpointStatus(status, "data-loss-risk=workspace-checkpoint-failed")
		}
		return status
	}
	status := "terminal-log=flushed"
	if terminalPending {
		_, terminalErr := terminalLogs.FinalizeOpen(preservationContext, "timeout")
		if terminalErr != nil {
			d.logger.Warn(
				"terminal log flush during Drain failed",
				"executionId", execution.ID,
				"generation", lease.Generation,
				"error", terminalErr,
			)
			status = "data-loss-risk=terminal-log-flush-failed"
		}
	}
	if workspacePending {
		checkpointCreated, checkpointErr := d.persistManagedWorkspaceState(
			preservationContext, execution, lease, workload, materializer, materialized,
		)
		if checkpointErr != nil {
			d.logger.Warn(
				"managed Workspace Checkpoint during Drain failed",
				"executionId", execution.ID,
				"generation", lease.Generation,
				"error", checkpointErr,
			)
			status = appendDrainCheckpointStatus(status, "data-loss-risk=workspace-checkpoint-failed")
		} else {
			checkpointStatus := drainCheckpointReleaseStatus(checkpointCreated)
			status = appendDrainCheckpointStatus(status, checkpointStatus)
			d.logger.Info(
				"managed Workspace state preserved during Drain",
				"executionId", execution.ID,
				"generation", lease.Generation,
				"checkpointStatus", checkpointStatus,
			)
		}
	}
	return status
}

func (d *Daemon) renewLeaseForDrainCheckpoint(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
) error {
	requestID := drainCheckpointRenewRequestIDPrefix + uuid.NewString()
	input := executions.RenewLeaseInput{LeaseInput: executions.LeaseInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}}
	var lastErr error
	for attempt := 0; attempt < drainCheckpointRenewMaximumAttempts; attempt++ {
		err := d.client.doJSON(
			ctx,
			http.MethodPost,
			executionPath(executionID, "renew"),
			d.client.workerToken,
			requestID,
			input,
			nil,
		)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil || !ambiguousDrainCheckpointRenewError(err) {
			return err
		}
	}
	return lastErr
}

func ambiguousDrainCheckpointRenewError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func drainCheckpointReleaseStatus(checkpointCreated bool) string {
	if checkpointCreated {
		return "workspace-checkpoint=ready"
	}
	return "workspace-checkpoint=unchanged"
}

func appendDrainCheckpointStatus(reason, status string) string {
	if strings.TrimSpace(status) == "" {
		return reason
	}
	return reason + "; " + status
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
	guard *executionSecretGuard,
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
			d.logger.Warn("control command pull failed", "executionId", executionID, "generation", lease.Generation, "error", guard.SanitizeError(err))
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
					sanitized, sanitizeErr := guard.SanitizeControlResult(result)
					if sanitizeErr != nil {
						return sanitizeErr
					}
					return d.retryInteractionDeliveryUpdate(controlContext, func(requestContext context.Context) error {
						return d.client.AcknowledgeControlCommand(requestContext, executionID, lease, delivery, sanitized)
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
				d.logger.Warn("interaction resolution pull failed", "executionId", executionID, "generation", lease.Generation, "error", guard.SanitizeError(interactionErr))
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

func (d *Daemon) renewLeaseLoop(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	cancel context.CancelFunc,
	result chan<- error,
	updates chan executions.Lease,
	updatesReady *atomic.Bool,
) {
	ticker := time.NewTicker(d.config.LeaseRenewInterval)
	defer ticker.Stop()
	defer close(result)
	if updates != nil {
		defer close(updates)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publishRenewal := updates != nil && (updatesReady == nil || updatesReady.Load())
			requestContext, requestCancel := context.WithTimeout(
				ctx,
				executionLeaseRenewalRequestTimeout(d.config),
			)
			renewed, err := d.client.Renew(requestContext, executionID, lease)
			requestCancel()
			if err != nil {
				renewErr := executionLeaseRenewalError(ctx, err)
				if ctx.Err() != nil {
					return
				}
				if renewErr == nil {
					d.logger.Warn(
						"execution Lease renewal failed; retrying",
						"executionId", executionID,
						"generation", lease.Generation,
						"error", err,
					)
					continue
				}
				select {
				case result <- renewErr:
				default:
				}
				cancel()
				return
			}
			// A pre-access request can serialize behind the explicit Grant
			// resolve and therefore return the first valid access projection even
			// though readiness was false when the HTTP request began. Publish that
			// non-nil projection once readiness is visible, while still discarding
			// genuinely pre-attach nil responses.
			if !publishRenewal && updates != nil && updatesReady != nil &&
				updatesReady.Load() && renewed.ProviderCredentialAccess != nil {
				publishRenewal = true
			}
			if publishRenewal {
				publishLatestLeaseUpdate(updates, renewed)
			}
		}
	}
}

func publishLatestLeaseUpdate(updates chan executions.Lease, lease executions.Lease) {
	if updates == nil {
		return
	}
	select {
	case updates <- lease:
	default:
		select {
		case <-updates:
		default:
		}
		select {
		case updates <- lease:
		default:
		}
	}
}

func startProviderCredentialAccessMonitor(
	ctx context.Context,
	expectedGrantID uuid.UUID,
	initial ProviderCredentialAccess,
	updates <-chan providerCredentialAccessUpdate,
	cancelRunner context.CancelCauseFunc,
	now func() time.Time,
) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		current := initial
		timer := time.NewTimer(providerCredentialAccessDurationUntil(now, current.ExpiresAt))
		defer timer.Stop()
		report := func(err error) {
			select {
			case done <- err:
			default:
			}
		}
		for {
			select {
			case <-ctx.Done():
				report(nil)
				return
			case <-timer.C:
				err := providerCredentialAccessExpiredFailure()
				cancelRunner(err)
				report(err)
				return
			case update, ok := <-updates:
				if !ok {
					if ctx.Err() != nil {
						report(nil)
						return
					}
					err := providerCredentialAccessUnavailableFailureWithMessage(
						"The Provider Credential access lease renewal stream closed during execution.",
					)
					cancelRunner(err)
					report(err)
					return
				}
				if update.err != nil {
					cancelRunner(update.err)
					report(update.err)
					return
				}
				if update.access == nil {
					err := providerCredentialAccessInvalidFailure(
						"The Provider Credential access lease metadata disappeared during execution.",
					)
					cancelRunner(err)
					report(err)
					return
				}
				if err := validateUpdatedProviderCredentialAccess(expectedGrantID, current, *update.access, now()); err != nil {
					cancelRunner(err)
					report(err)
					return
				}
				current = *update.access
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(providerCredentialAccessDurationUntil(now, current.ExpiresAt))
			}
		}
	}()
	return done
}

func startProviderCredentialAccessLeaseBridge(
	ctx context.Context,
	leaseUpdates <-chan executions.Lease,
) <-chan providerCredentialAccessUpdate {
	updates := make(chan providerCredentialAccessUpdate, 1)
	go func() {
		defer close(updates)
		for {
			select {
			case <-ctx.Done():
				return
			case renewed, ok := <-leaseUpdates:
				if !ok {
					return
				}
				access, err := providerCredentialAccessFromLease(renewed)
				if err != nil {
					publishLatestProviderCredentialAccessUpdate(updates, providerCredentialAccessUpdate{
						err: providerCredentialAccessInvalidFailure(
							"The Provider Credential access lease metadata in the renewal response is invalid.",
						),
					})
					return
				}
				publishLatestProviderCredentialAccessUpdate(updates, providerCredentialAccessUpdate{access: access})
			}
		}
	}()
	return updates
}

func providerCredentialAccessDurationUntil(now func() time.Time, deadline time.Time) time.Duration {
	duration := deadline.Sub(now())
	if duration <= 0 {
		return time.Nanosecond
	}
	return duration
}

func publishLatestProviderCredentialAccessUpdate(
	updates chan providerCredentialAccessUpdate,
	update providerCredentialAccessUpdate,
) {
	select {
	case updates <- update:
	default:
		select {
		case <-updates:
		default:
		}
		select {
		case updates <- update:
		default:
		}
	}
}

func validateInitialProviderCredentialAccess(
	expectedGrantID uuid.UUID,
	access *ProviderCredentialAccess,
	now time.Time,
) error {
	if access == nil {
		return providerCredentialAccessInvalidFailure("The Provider Credential access lease metadata is missing.")
	}
	if err := validateProviderCredentialAccessShape(expectedGrantID, *access, now); err != nil {
		return err
	}
	if strings.TrimSpace(access.Status) != providerCredentialAccessStatusActive {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease must start in an active state.",
		)
	}
	return nil
}

func validateUpdatedProviderCredentialAccess(
	expectedGrantID uuid.UUID,
	current ProviderCredentialAccess,
	next ProviderCredentialAccess,
	now time.Time,
) error {
	if err := validateProviderCredentialAccessShape(expectedGrantID, next, now); err != nil {
		return err
	}
	switch strings.TrimSpace(next.Status) {
	case providerCredentialAccessStatusUnavailable:
		return providerCredentialAccessUnavailableFailure()
	case providerCredentialAccessStatusExpired:
		return providerCredentialAccessExpiredFailure()
	}
	if next.Serial < current.Serial {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease serial rolled back during execution.",
		)
	}
	if !next.IssuedAt.Equal(current.IssuedAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease issuance timestamp changed during execution.",
		)
	}
	if next.ActivitySequence < current.ActivitySequence {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease activity sequence rolled back during execution.",
		)
	}
	if next.ActivitySequence == current.ActivitySequence && !next.ActivityAt.Equal(current.ActivityAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease activity timestamp changed without a newer semantic event.",
		)
	}
	if next.ActivitySequence > current.ActivitySequence && next.ActivityAt.Before(current.ActivityAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease activity timestamp rolled back during execution.",
		)
	}
	if next.RenewedAt.Before(current.RenewedAt) || next.ExpiresAt.Before(current.ExpiresAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease renewal window rolled back during execution.",
		)
	}
	if providerCredentialHardExpiryExtended(current.HardExpiresAt, next.HardExpiresAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease hard expiry was extended during execution.",
		)
	}
	if next.Serial == current.Serial &&
		(next.ActivitySequence != current.ActivitySequence ||
			!next.ActivityAt.Equal(current.ActivityAt) ||
			!next.RenewedAt.Equal(current.RenewedAt) ||
			!next.ExpiresAt.Equal(current.ExpiresAt) ||
			!next.RefreshDeadlineAt.Equal(current.RefreshDeadlineAt) ||
			!optionalTimesEqual(next.HardExpiresAt, current.HardExpiresAt)) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease changed durable state without advancing its serial.",
		)
	}
	return nil
}

func providerCredentialHardExpiryExtended(current, next *time.Time) bool {
	if current == nil {
		return false
	}
	return next == nil || next.After(*current)
}

func optionalTimesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func validateProviderCredentialAccessShape(
	expectedGrantID uuid.UUID,
	access ProviderCredentialAccess,
	now time.Time,
) error {
	status := strings.TrimSpace(access.Status)
	if expectedGrantID == uuid.Nil || access.GrantID == uuid.Nil || access.GrantID != expectedGrantID {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease no longer matches the expected grant.",
		)
	}
	switch status {
	case providerCredentialAccessStatusActive, providerCredentialAccessStatusRefreshWindowClosed:
	case providerCredentialAccessStatusUnavailable:
		return providerCredentialAccessUnavailableFailure()
	case providerCredentialAccessStatusExpired:
		return providerCredentialAccessExpiredFailure()
	default:
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease status is invalid.",
		)
	}
	if access.Serial <= 0 || access.ActivitySequence < 0 {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease sequence metadata is invalid.",
		)
	}
	if access.IssuedAt.IsZero() || access.RenewedAt.IsZero() || access.ExpiresAt.IsZero() ||
		access.ActivityAt.IsZero() || access.RefreshDeadlineAt.IsZero() {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease deadlines are incomplete.",
		)
	}
	if access.RefreshDeadlineAt.Before(access.ActivityAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease refresh deadline is invalid.",
		)
	}
	if access.RenewedAt.Before(access.IssuedAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease renewal timestamp is invalid.",
		)
	}
	if !access.ExpiresAt.After(access.RenewedAt) || !access.ExpiresAt.After(now) {
		return providerCredentialAccessExpiredFailure()
	}
	if access.RenewAfterAt == nil || access.RenewAfterAt.IsZero() {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease renewAfter deadline is invalid.",
		)
	}
	if access.RenewAfterAt.Before(access.RenewedAt) || !access.RenewAfterAt.Before(access.ExpiresAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease renewAfter deadline is invalid.",
		)
	}
	if access.HardExpiresAt != nil && access.HardExpiresAt.Before(access.ExpiresAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease hard expiry is invalid.",
		)
	}
	return nil
}

func providerCredentialAccessInvalidFailure(message string) error {
	return &runnerFailure{
		code: "credential_invalid", message: message,
		requiresNewExecution: true, requiresUserAction: true,
		canReconstructFromHistory: true, canMoveWorker: true,
	}
}

func providerCredentialAccessUnavailableFailure() error {
	return providerCredentialAccessUnavailableFailureWithMessage(
		"The Provider Credential access lease became unavailable during execution.",
	)
}

func providerCredentialAccessUnavailableFailureWithMessage(message string) error {
	return &runnerFailure{
		code:                 "credential_unavailable",
		message:              message,
		requiresNewExecution: true, requiresUserAction: true,
		canReconstructFromHistory: true, canMoveWorker: true,
	}
}

func providerCredentialAccessExpiredFailure() error {
	return &runnerFailure{
		code:                 "credential_expired",
		message:              "The Provider Credential access lease expired during execution.",
		requiresNewExecution: true, requiresUserAction: true,
		canReconstructFromHistory: true, canMoveWorker: true,
	}
}

func providerCredentialAccessFromLease(lease executions.Lease) (*ProviderCredentialAccess, error) {
	if lease.ProviderCredentialAccess == nil {
		return nil, errors.New("Provider Credential access metadata is missing from the renewed Lease")
	}
	return lease.ProviderCredentialAccess, nil
}

func executionLeaseRenewalRequestTimeout(config Config) time.Duration {
	if config.RequestTimeout < config.LeaseRenewInterval {
		return config.RequestTimeout
	}
	return config.LeaseRenewInterval
}

func executionLeaseRenewalError(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return nil
	}
	var problem *controlPlaneProblem
	if !errors.As(err, &problem) || problem.Status == http.StatusTooManyRequests || problem.Status >= http.StatusInternalServerError {
		return nil
	}
	return fmt.Errorf("renew execution lease: %w", err)
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

func (d *Daemon) failExecutionGuarded(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	guard *executionSecretGuard,
	cause error,
) error {
	if guard != nil {
		cause = guard.SanitizeError(cause)
	}
	return d.failExecution(ctx, executionID, lease, cause)
}

func withProviderHostCapabilities(
	base map[string]any,
	providerHost map[string]any,
	config Config,
) map[string]any {
	result := make(map[string]any, len(base)+3)
	for key, value := range base {
		result[key] = value
	}
	delete(result, resourceSuspendContainmentCapabilityKey)
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
	if capability := copyProcessContainmentCapability(config.ProcessContainmentCapability); capability != nil {
		workerRuntime["processContainment"] = capability
	}
	result["workerRuntime"] = workerRuntime
	return result
}

func resolveWorkspaceArtifact(workspaceDirectory, candidate string) (string, error) {
	source, relative, err := openWorkspaceArtifactSource(workspaceDirectory, candidate)
	if err != nil {
		return "", err
	}
	defer source.Close()
	rootPath, _, err := workspaceArtifactPath(workspaceDirectory, relative)
	if err != nil {
		return "", err
	}
	return filepath.Join(rootPath, relative), nil
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
