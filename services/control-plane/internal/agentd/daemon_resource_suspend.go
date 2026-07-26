package agentd

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

const (
	resourceSuspendReasonActiveIdleTimeout = "active-idle-timeout"
	resourceSuspendRunnerActive            = int32(0)
	resourceSuspendRunnerQuiescing         = int32(1)
	resourceSuspendRunnerFinished          = int32(2)
)

type resourceSuspendResult struct {
	fenced            bool
	continueExecution bool
	disposition       string
	err               error
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
