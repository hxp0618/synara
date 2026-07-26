package executions

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const (
	resourceSuspendCheckpointTimeout = 2 * time.Minute
	resourceSuspendRetryBackoff      = time.Minute

	resourceSuspendReasonWaitingKeepAlive = "waiting-keepalive"
)

// PullResourceDirective is a server-authoritative, generation-fenced poll. It
// creates at most one durable checkpoint handshake once the frozen Session
// waiting keep-alive has elapsed. Browser presence and Worker heartbeats are
// deliberately absent from the eligibility calculation.
func (s *Service) PullResourceDirective(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input PullResourceDirectiveInput,
) (*ResourceDirective, error) {
	var directive *ResourceDirective
	var appended persistence.SessionEvent
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		lease, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return err
		}
		now := s.now()
		expired, event, err := s.cancelExecutionIfSessionAbsoluteExpiredLocked(
			ctx, tx, &execution, &lease, now,
		)
		if err != nil {
			return err
		}
		if expired {
			appended = event
			directive = &ResourceDirective{
				Action: "terminate", Reason: sessionAbsoluteExpiryAction, RequestedAt: now,
			}
			return nil
		}
		if execution.TargetKind != "kubernetes" ||
			(execution.Status != "waiting-for-approval" && execution.Status != "running") {
			return nil
		}
		if execution.Status == "running" {
			supported, supportErr := workerSupportsActiveTurnSuspend(ctx, tx, execution)
			if supportErr != nil {
				return supportErr
			}
			if !supported {
				return nil
			}
		}
		completionMode, err := resourceSuspendCompletionMode(ctx, tx, worker, lease, execution)
		if err != nil {
			return err
		}
		if completionMode == "" {
			// Kubernetes Pod-terminal completion is available only to a Pod-bound
			// Worker identity. Other runtimes still require a separately signed
			// strict containment supervisor.
			return nil
		}

		var session persistence.AgentSession
		if err := tx.WithContext(ctx).
			Select(
				"id",
				"tenant_id",
				"created_by",
				"provider",
				"waiting_keep_alive_seconds",
				"suspend_after_idle_seconds",
				"resource_state",
				"meaningful_activity_at",
				"meaningful_activity_sequence",
			).
			Where("tenant_id = ? AND id = ? AND status = ?", execution.TenantID, execution.SessionID, "active").
			Take(&session).Error; err != nil {
			return problem.Wrap(409, "session_not_active", "The Session is not active for resource suspension.", err)
		}
		var active persistence.ExecutionSuspendAttempt
		activeErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ? AND generation = ? AND status = ?",
				execution.TenantID, execution.ID, execution.Generation, "checkpointing").
			Take(&active).Error
		if activeErr == nil {
			if active.CheckpointDeadlineAt.After(now) {
				directive = resourceDirectiveFromAttempt(active)
				return nil
			}
			const failureCode = "checkpoint_deadline_exceeded"
			const failureMessage = "The Worker did not complete the suspend Checkpoint before its deadline."
			if err := abortSuspendAttemptRow(ctx, tx, active, now, failureCode, failureMessage); err != nil {
				return err
			}
			appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
				EventType: "execution.suspend-aborted", ActorType: "system",
				ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
				Payload: map[string]any{
					"turnId": execution.TurnID, "suspendAttemptId": active.ID,
					"reason":      active.Reason,
					"failureCode": failureCode, "failureMessage": failureMessage,
				},
			})
			if err != nil {
				return err
			}
		} else if !errors.Is(activeErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "resource_suspend_attempt_load_failed", "The active resource suspension attempt could not be loaded.", activeErr)
		}

		var latest persistence.ExecutionSuspendAttempt
		latestErr := tx.WithContext(ctx).
			Where("tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, execution.Generation).
			Order("requested_at DESC, id DESC").Take(&latest).Error
		if latestErr == nil && latest.Status != "completed" {
			retryFrom := latest.RequestedAt
			if latest.AbortedAt != nil {
				retryFrom = *latest.AbortedAt
			}
			if retryFrom.Add(resourceSuspendRetryBackoff).After(now) {
				return nil
			}
		}
		if latestErr != nil && !errors.Is(latestErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "resource_suspend_attempt_load_failed", "The latest resource suspension attempt could not be loaded.", latestErr)
		}

		eligibility, err := s.resourceSuspendEligibility(ctx, tx, execution, worker.ID, session, now)
		if err != nil || eligibility == nil {
			return err
		}

		attemptID := uuid.New()
		attempt := persistence.ExecutionSuspendAttempt{
			ID: attemptID, TenantID: execution.TenantID, SessionID: execution.SessionID,
			TurnID: execution.TurnID, ExecutionID: execution.ID, WorkerID: worker.ID,
			ExecutionTargetID: execution.ExecutionTargetID,
			Generation:        execution.Generation, Reason: eligibility.Reason, Status: "checkpointing",
			CompletionMode: completionMode, WorkerIncarnation: worker.Incarnation,
			WorkerInstanceUID: worker.InstanceUID, WorkerClusterID: worker.ClusterID,
			WorkerNamespace: worker.Namespace, WorkerPodName: worker.PodName,
			RequestedAt: now, CheckpointDeadlineAt: now.Add(resourceSuspendCheckpointTimeout),
		}
		if eligibility.Reason == resourceSuspendReasonActiveIdleTimeout {
			if session.MeaningfulActivitySequence <= 0 {
				return problem.New(409, "active_suspend_activity_boundary_missing", "The active Turn does not have a durable semantic activity boundary.")
			}
			attempt.BoundaryMeaningfulActivitySequence = &session.MeaningfulActivitySequence
			command, commandErr := createActiveTurnSuspendControlCommand(
				ctx, tx, execution, session, worker.ID, attemptID, now,
			)
			if commandErr != nil {
				return commandErr
			}
			attempt.ControlCommandID = &command.ID
		}
		if err := tx.WithContext(ctx).Create(&attempt).Error; err != nil {
			return problem.Wrap(409, "resource_suspend_attempt_conflict", "The resource suspension attempt changed concurrently.", err)
		}
		eventPayload := map[string]any{
			"turnId": execution.TurnID, "suspendAttemptId": attempt.ID,
			"checkpointDeadlineAt": attempt.CheckpointDeadlineAt, "reason": attempt.Reason,
		}
		if attempt.ControlCommandID != nil {
			eventPayload["controlCommandId"] = *attempt.ControlCommandID
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.suspend-checkpointing", ActorType: "system",
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: eventPayload,
		})
		if err != nil {
			return err
		}
		directive = resourceDirectiveFromAttempt(attempt)
		return nil
	})
	if err == nil && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return directive, err
}

func workerSupportsStrictResourceSuspendContainment(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	execution persistence.AgentExecution,
) (bool, error) {
	if worker.CurrentManifestID == nil || execution.WorkerManifestID == nil ||
		*worker.CurrentManifestID != *execution.WorkerManifestID {
		return false, nil
	}
	var manifest persistence.WorkerManifest
	if err := tx.WithContext(ctx).
		Where("id = ?", *execution.WorkerManifestID).
		Take(&manifest).Error; err != nil {
		return false, problem.Wrap(500, "worker_manifest_lookup_failed", "The Worker process-containment Manifest could not be loaded.", err)
	}
	if !workerManifestSupportsStrictResourceSuspendContainment(manifest) ||
		manifest.ProcessContainmentTrustMode != executiontargets.ProcessContainmentTrustSignedV1 ||
		manifest.ProcessContainmentAttestationKeyID == nil || manifest.ProcessContainmentAttestationKeySHA256 == nil {
		return false, nil
	}
	var target persistence.ExecutionTarget
	if err := tx.WithContext(ctx).
		Where("id = ?", execution.ExecutionTargetID).
		Take(&target).Error; err != nil {
		return false, problem.Wrap(500, "worker_containment_target_lookup_failed", "The Worker process-containment Target policy could not be loaded.", err)
	}
	policy, err := executiontargets.ParseProcessContainmentPolicy(target.Capabilities)
	if err != nil {
		return false, problem.Wrap(500, "worker_containment_policy_invalid", "The Worker process-containment Target policy is invalid.", err)
	}
	return policy.TrustMode == executiontargets.ProcessContainmentTrustSignedV1 &&
		policy.KeyID == *manifest.ProcessContainmentAttestationKeyID &&
		policy.PublicKeySHA256 == *manifest.ProcessContainmentAttestationKeySHA256, nil
}

func resourceSuspendCompletionMode(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	lease persistence.WorkerLease,
	execution persistence.AgentExecution,
) (string, error) {
	strictContainment, err := workerSupportsStrictResourceSuspendContainment(ctx, tx, worker, execution)
	if err != nil {
		return "", err
	}
	if strictContainment {
		return ResourceSuspendCompletionWorkerAttestedV1, nil
	}
	if execution.TargetKind != "kubernetes" ||
		worker.TargetKind != "kubernetes" ||
		worker.RegistrationTrustMode != WorkerRegistrationTrustKubernetesPodBoundV1 ||
		worker.ExecutionTargetID != execution.ExecutionTargetID ||
		worker.ID != lease.WorkerID || worker.Incarnation != lease.WorkerIncarnation ||
		worker.InstanceUID == "" || worker.InstanceUID != lease.WorkerInstanceUID {
		return "", nil
	}
	return ResourceSuspendCompletionKubernetesPodTerminalV1, nil
}

func requireResourceSuspendCompletionModeStillSafe(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	execution persistence.AgentExecution,
	attempt persistence.ExecutionSuspendAttempt,
) error {
	switch attempt.CompletionMode {
	case ResourceSuspendCompletionWorkerAttestedV1:
		strictContainment, err := workerSupportsStrictResourceSuspendContainment(ctx, tx, worker, execution)
		if err != nil {
			return err
		}
		if !strictContainment {
			return problem.New(409, "resource_suspend_no_longer_safe", "The Worker no longer attests strict process containment for resource suspension.")
		}
	case ResourceSuspendCompletionKubernetesPodTerminalV1:
		if execution.TargetKind != "kubernetes" || worker.TargetKind != "kubernetes" ||
			worker.RegistrationTrustMode != WorkerRegistrationTrustKubernetesPodBoundV1 ||
			attempt.ExecutionTargetID != execution.ExecutionTargetID ||
			attempt.WorkerIncarnation != worker.Incarnation ||
			attempt.WorkerInstanceUID == "" || attempt.WorkerInstanceUID != worker.InstanceUID ||
			attempt.WorkerClusterID != worker.ClusterID || attempt.WorkerNamespace != worker.Namespace ||
			attempt.WorkerPodName != worker.PodName {
			return problem.New(409, "resource_suspend_no_longer_safe", "The Kubernetes Pod identity no longer matches the suspend attempt.")
		}
	default:
		return problem.New(409, "resource_suspend_no_longer_safe", "The resource suspension completion mode is not recognized.")
	}
	return nil
}

func workerManifestSupportsStrictResourceSuspendContainment(manifest persistence.WorkerManifest) bool {
	return executiontargets.WorkerManifestHasSupportedStrictProcessContainment(manifest)
}

func (s *Service) MarkResourceSuspendQuiesced(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input MarkResourceSuspendQuiescedInput,
	requestID string,
) (OperationResult[ResourceSuspendQuiesceReceipt], error) {
	if input.SuspendAttemptID == uuid.Nil {
		return OperationResult[ResourceSuspendQuiesceReceipt]{}, problem.New(400, "invalid_resource_suspend_quiesce", "suspendAttemptId is required.")
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.resource-suspend.quiesced", struct {
		ExecutionID uuid.UUID                        `json:"executionId"`
		Input       MarkResourceSuspendQuiescedInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (ResourceSuspendQuiesceReceipt, error) {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return ResourceSuspendQuiesceReceipt{}, err
		}
		now := s.now()
		if err := s.requireExecutionSessionWithinAbsoluteLifetime(ctx, tx, execution, now); err != nil {
			return ResourceSuspendQuiesceReceipt{}, err
		}
		attempt, err := lockSuspendAttempt(ctx, tx, execution, worker.ID, input.SuspendAttemptID)
		if err != nil {
			return ResourceSuspendQuiesceReceipt{}, err
		}
		if !suspendStatusMatchesReason(execution, attempt) {
			return ResourceSuspendQuiesceReceipt{}, problem.New(409, "resource_suspend_state_conflict", "The Execution no longer matches the suspend attempt boundary.")
		}
		if attempt.ProviderQuiescedAt != nil {
			if err := s.requireActiveTurnCheckpointReceipt(ctx, tx, execution, attempt); err != nil {
				return ResourceSuspendQuiesceReceipt{}, err
			}
			return resourceSuspendQuiesceReceiptFromAttempt(attempt), nil
		}
		if attempt.Reason == resourceSuspendReasonActiveIdleTimeout {
			return ResourceSuspendQuiesceReceipt{}, problem.New(409, "active_suspend_receipt_required", "SuspendTurn acknowledgement must atomically record the active-turn checkpoint receipt before quiesce can complete.")
		}
		if err := requireResourceSuspendCompletionModeStillSafe(ctx, tx, worker, execution, attempt); err != nil {
			return ResourceSuspendQuiesceReceipt{}, err
		}
		if !attempt.CheckpointDeadlineAt.After(now) {
			return ResourceSuspendQuiesceReceipt{}, problem.New(409, "resource_suspend_checkpoint_expired", "The suspend Checkpoint deadline has elapsed.")
		}
		if err := s.requireResourceSuspendStillSafe(ctx, tx, execution, worker.ID, attempt, now); err != nil {
			return ResourceSuspendQuiesceReceipt{}, err
		}

		updated := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ? AND execution_id = ? AND worker_id = ? AND generation = ? AND status = ? AND provider_quiesced_at IS NULL",
				execution.TenantID, attempt.ID, execution.ID, worker.ID, execution.Generation, "checkpointing").
			Update("provider_quiesced_at", now)
		if err := expectOne(updated, 409, "resource_suspend_attempt_conflict", "The resource suspension attempt changed concurrently."); err != nil {
			return ResourceSuspendQuiesceReceipt{}, err
		}
		attempt.ProviderQuiescedAt = &now
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.suspend-quiesced", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "suspendAttemptId": attempt.ID, "providerQuiescedAt": now,
			},
		})
		if err != nil {
			return ResourceSuspendQuiesceReceipt{}, err
		}
		return resourceSuspendQuiesceReceiptFromAttempt(attempt), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

// MarkResourceSuspendCheckpointReady records the last Worker-authored step of
// Kubernetes suspension. It deliberately does not release the Lease or mark
// the Execution suspended: only an external kubelet observation for this exact
// Pod UID can authorize that transition.
func (s *Service) MarkResourceSuspendCheckpointReady(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input MarkResourceSuspendCheckpointReadyInput,
	requestID string,
) (OperationResult[ResourceSuspendCheckpointReadyReceipt], error) {
	input.CheckpointStatus = strings.TrimSpace(input.CheckpointStatus)
	if input.SuspendAttemptID == uuid.Nil ||
		(input.CheckpointStatus != "ready" && input.CheckpointStatus != "unchanged") {
		return OperationResult[ResourceSuspendCheckpointReadyReceipt]{}, problem.New(
			400,
			"invalid_resource_suspend_checkpoint_ready",
			"suspendAttemptId and a ready or unchanged checkpointStatus are required.",
		)
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.resource-suspend.checkpoint-ready", struct {
		ExecutionID uuid.UUID                               `json:"executionId"`
		Input       MarkResourceSuspendCheckpointReadyInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (ResourceSuspendCheckpointReadyReceipt, error) {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}
		now := s.now()
		if err := s.requireExecutionSessionWithinAbsoluteLifetime(ctx, tx, execution, now); err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}
		attempt, err := lockSuspendAttempt(ctx, tx, execution, worker.ID, input.SuspendAttemptID)
		if err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}
		if !suspendStatusMatchesReason(execution, attempt) {
			return ResourceSuspendCheckpointReadyReceipt{}, problem.New(409, "resource_suspend_state_conflict", "The Execution no longer matches the suspend attempt boundary.")
		}
		if attempt.CompletionMode != ResourceSuspendCompletionKubernetesPodTerminalV1 {
			return ResourceSuspendCheckpointReadyReceipt{}, problem.New(409, "resource_suspend_checkpoint_mode_conflict", "Only Kubernetes Pod-terminal suspension accepts a checkpoint-ready handoff.")
		}
		if attempt.CheckpointReadyAt != nil {
			if attempt.CheckpointStatus == nil || *attempt.CheckpointStatus != input.CheckpointStatus {
				return ResourceSuspendCheckpointReadyReceipt{}, problem.New(409, "resource_suspend_checkpoint_conflict", "The suspend Checkpoint was already recorded with a different status.")
			}
			return resourceSuspendCheckpointReadyReceiptFromAttempt(attempt), nil
		}
		if attempt.ProviderQuiescedAt == nil {
			return ResourceSuspendCheckpointReadyReceipt{}, problem.New(409, "resource_suspend_quiesce_required", "The Provider must stop accepting callbacks before the suspend Checkpoint can be handed off.")
		}
		if err := s.requireActiveTurnCheckpointReceipt(ctx, tx, execution, attempt); err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}
		if !attempt.CheckpointDeadlineAt.After(now) {
			return ResourceSuspendCheckpointReadyReceipt{}, problem.New(409, "resource_suspend_checkpoint_expired", "The suspend Checkpoint deadline has elapsed.")
		}
		if err := requireResourceSuspendCompletionModeStillSafe(ctx, tx, worker, execution, attempt); err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}
		if err := s.requireResourceSuspendStillSafe(ctx, tx, execution, worker.ID, attempt, now); err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}
		if err := s.requireSuspendWorkspaceRecoverable(ctx, tx, execution); err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}

		updated := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
			Where(
				"tenant_id = ? AND id = ? AND execution_id = ? AND worker_id = ? AND generation = ? AND status = ? AND completion_mode = ? AND provider_quiesced_at IS NOT NULL AND checkpoint_ready_at IS NULL",
				execution.TenantID, attempt.ID, execution.ID, worker.ID, execution.Generation,
				"checkpointing", ResourceSuspendCompletionKubernetesPodTerminalV1,
			).
			Updates(map[string]any{"checkpoint_status": input.CheckpointStatus, "checkpoint_ready_at": now})
		if err := expectOne(updated, 409, "resource_suspend_checkpoint_conflict", "The suspend Checkpoint changed concurrently."); err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}
		attempt.CheckpointStatus = &input.CheckpointStatus
		attempt.CheckpointReadyAt = &now
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.suspend-checkpoint-ready", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "suspendAttemptId": attempt.ID,
				"checkpointStatus": input.CheckpointStatus, "checkpointReadyAt": now,
				"completionMode": attempt.CompletionMode, "podUid": attempt.WorkerInstanceUID,
			},
		})
		if err != nil {
			return ResourceSuspendCheckpointReadyReceipt{}, err
		}
		return resourceSuspendCheckpointReadyReceiptFromAttempt(attempt), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) CompleteResourceSuspend(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input CompleteResourceSuspendInput,
	requestID string,
) (OperationResult[Execution], error) {
	input.CheckpointStatus = strings.TrimSpace(input.CheckpointStatus)
	if input.SuspendAttemptID == uuid.Nil || (input.CheckpointStatus != "ready" && input.CheckpointStatus != "unchanged") {
		return OperationResult[Execution]{}, problem.New(400, "invalid_resource_suspend_completion", "suspendAttemptId and a ready or unchanged checkpointStatus are required.")
	}
	appended := make([]persistence.SessionEvent, 0, 2)
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.resource-suspend.complete", struct {
		ExecutionID uuid.UUID                    `json:"executionId"`
		Input       CompleteResourceSuspendInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (Execution, error) {
		lease, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return Execution{}, err
		}
		now := s.now()
		if err := s.requireExecutionSessionWithinAbsoluteLifetime(ctx, tx, execution, now); err != nil {
			return Execution{}, err
		}
		attempt, err := lockSuspendAttempt(ctx, tx, execution, worker.ID, input.SuspendAttemptID)
		if err != nil {
			return Execution{}, err
		}
		if !suspendStatusMatchesReason(execution, attempt) {
			return Execution{}, problem.New(409, "resource_suspend_state_conflict", "The Execution no longer matches the suspend attempt boundary.")
		}
		if attempt.ProviderQuiescedAt == nil {
			return Execution{}, problem.New(409, "resource_suspend_quiesce_required", "The Provider must be durably quiesced before resource suspension can complete.")
		}
		if err := s.requireActiveTurnCheckpointReceipt(ctx, tx, execution, attempt); err != nil {
			return Execution{}, err
		}
		if attempt.CompletionMode != ResourceSuspendCompletionWorkerAttestedV1 {
			return Execution{}, problem.New(409, "resource_suspend_terminal_proof_required", "Kubernetes Pod-terminal suspension must be finalized by the Control Plane after observing the exact Pod UID terminate successfully.")
		}
		if err := requireResourceSuspendCompletionModeStillSafe(ctx, tx, worker, execution, attempt); err != nil {
			return Execution{}, err
		}
		if !attempt.CheckpointDeadlineAt.After(now) {
			return Execution{}, problem.New(409, "resource_suspend_checkpoint_expired", "The suspend Checkpoint deadline has elapsed.")
		}
		if err := s.requireResourceSuspendStillSafe(ctx, tx, execution, worker.ID, attempt, now); err != nil {
			return Execution{}, err
		}
		if err := s.requireSuspendWorkspaceRecoverable(ctx, tx, execution); err != nil {
			return Execution{}, err
		}
		autoResume, err := s.prepareActiveTurnSuspendCompletion(ctx, tx, execution, attempt, lease)
		if err != nil {
			return Execution{}, err
		}

		completed := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ? AND execution_id = ? AND worker_id = ? AND generation = ? AND status = ? AND provider_quiesced_at IS NOT NULL",
				execution.TenantID, attempt.ID, execution.ID, worker.ID, execution.Generation, "checkpointing").
			Updates(map[string]any{
				"status": "completed", "completed_at": now,
				"checkpoint_status": input.CheckpointStatus, "checkpoint_ready_at": now,
			})
		if err := expectOne(completed, 409, "resource_suspend_attempt_conflict", "The resource suspension attempt changed concurrently."); err != nil {
			return Execution{}, err
		}
		leaseDelete := tx.WithContext(ctx).Delete(&lease)
		if err := expectOne(leaseDelete, 409, "lease_release_conflict", "The suspended Execution lease changed during release."); err != nil {
			return Execution{}, problem.Wrap(500, "lease_release_failed", "Failed to release the suspended Execution lease.", err)
		}
		if err := recordWorkerClaimReleaseFact(ctx, tx, executionClaimReleaseInput(
			lease, now, now, workerClaimReleaseResourceSuspendedWorkerAttested,
			workerClaimReleaseAuthorityWorker, worker.ID.String(), requestID,
		)); err != nil {
			return Execution{}, err
		}
		if err := transitionWorkerAfterLeaseReleasedLocked(ctx, tx, lease, now); err != nil {
			return Execution{}, err
		}
		recoveryReason := "suspend-resume"
		expectedStatus := suspendExpectedExecutionStatus(attempt)
		suspended := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ? AND worker_id = ? AND generation = ? AND status = ?",
				execution.TenantID, execution.ID, worker.ID, execution.Generation, expectedStatus).
			Updates(map[string]any{"status": "suspended", "worker_id": nil, "next_recovery_reason": recoveryReason})
		if err := expectOne(suspended, 409, "resource_suspend_conflict", "The Execution could not enter the suspended state."); err != nil {
			return Execution{}, err
		}
		execution.Status = "suspended"
		execution.WorkerID = nil
		execution.NextRecoveryReason = &recoveryReason
		suspendedEvent, err := s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.suspended", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "suspendAttemptId": attempt.ID,
				"checkpointStatus": input.CheckpointStatus, "suspendedAt": now,
			},
		})
		if err != nil {
			return Execution{}, err
		}
		appended = append(appended, suspendedEvent)
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &execution.TenantID, Topic: "execution.suspended", MessageKey: execution.ID.String(),
			Payload: map[string]any{
				"tenantId": execution.TenantID, "sessionId": execution.SessionID, "turnId": execution.TurnID,
				"executionId": execution.ID, "generation": execution.Generation,
				"suspendAttemptId": attempt.ID, "suspendedAt": now,
			},
		}); err != nil {
			return Execution{}, problem.Wrap(500, "resource_suspend_outbox_failed", "The suspended Execution event could not be queued.", err)
		}
		if autoResume {
			recoveringEvent, err := s.resumeSuspendedExecutionLocked(
				ctx, tx, &execution, "system", nil, &worker.ID,
				"activity_crossed_suspend_boundary", "active-idle-activity-resume", now,
			)
			if err != nil {
				return Execution{}, err
			}
			appended = append(appended, recoveringEvent)
		}
		return toExecution(execution), nil
	})
	if err == nil && !result.Replayed {
		for _, event := range appended {
			if event.EventID != uuid.Nil {
				s.sessions.PublishInternalEvent(event)
			}
		}
	}
	return result, err
}

// FinalizeKubernetesResourceSuspend consumes a kubelet-authored terminal Pod
// observation. Succeeded is intentionally the only accepted phase: a DELETE
// acknowledgement, object absence, Unknown, or Failed cannot prove that this
// exact Worker Pod completed the checkpoint handoff normally.
func (s *Service) FinalizeKubernetesResourceSuspend(
	ctx context.Context,
	proof KubernetesPodTerminalProof,
) (bool, error) {
	proof.PodUID = strings.TrimSpace(proof.PodUID)
	proof.Phase = strings.TrimSpace(proof.Phase)
	proof.Namespace = strings.TrimSpace(proof.Namespace)
	proof.PodName = strings.TrimSpace(proof.PodName)
	parsedPodUID, podUIDErr := uuid.Parse(proof.PodUID)
	if proof.ExecutionTargetID == uuid.Nil || proof.ExecutionID == uuid.Nil || proof.Generation <= 0 ||
		proof.Namespace == "" || proof.PodName == "" ||
		podUIDErr != nil || parsedPodUID == uuid.Nil || parsedPodUID.String() != proof.PodUID ||
		proof.Phase != "Succeeded" || proof.ObservedAt.IsZero() {
		return false, problem.New(400, "invalid_kubernetes_pod_terminal_proof", "An exact Succeeded Kubernetes Pod UID proof is required.")
	}
	proof.ObservedAt = proof.ObservedAt.UTC()
	var appended []persistence.SessionEvent
	finalized := false
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var lease persistence.WorkerLease
		leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("execution_id = ?", proof.ExecutionID).
			Take(&lease).Error
		if leaseErr != nil && !errors.Is(leaseErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "resource_suspend_lease_load_failed", "The Kubernetes suspend Lease could not be locked.", leaseErr)
		}

		var execution persistence.AgentExecution
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND execution_target_id = ? AND target_kind = ?", proof.ExecutionID, proof.ExecutionTargetID, "kubernetes").
			Take(&execution).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return problem.Wrap(500, "resource_suspend_execution_load_failed", "The Kubernetes suspend Execution could not be locked.", err)
		}

		var attempt persistence.ExecutionSuspendAttempt
		attemptErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where(
				"tenant_id = ? AND execution_id = ? AND generation = ? AND execution_target_id = ? AND completion_mode = ? AND worker_instance_uid = ?",
				execution.TenantID, execution.ID, proof.Generation, proof.ExecutionTargetID,
				ResourceSuspendCompletionKubernetesPodTerminalV1, proof.PodUID,
			).
			Order("requested_at DESC, id DESC").Take(&attempt).Error
		if errors.Is(attemptErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if attemptErr != nil {
			return problem.Wrap(500, "resource_suspend_attempt_load_failed", "The Kubernetes suspend attempt could not be locked.", attemptErr)
		}
		if proof.Namespace != attempt.WorkerNamespace || proof.PodName != attempt.WorkerPodName {
			return problem.New(409, "resource_suspend_terminal_proof_fenced", "The Kubernetes terminal proof does not match the frozen Pod name and namespace.")
		}
		if attempt.Status == "completed" {
			if attempt.PodTerminalObservedAt != nil && attempt.PodTerminalPhase != nil &&
				*attempt.PodTerminalPhase == proof.Phase {
				finalized = true
			}
			return nil
		}
		if attempt.Status != "checkpointing" || attempt.CheckpointStatus == nil || attempt.CheckpointReadyAt == nil ||
			attempt.ProviderQuiescedAt == nil {
			return nil
		}
		if errors.Is(leaseErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if attempt.CheckpointReadyAt.After(attempt.CheckpointDeadlineAt) || proof.ObservedAt.Before(*attempt.CheckpointReadyAt) {
			return problem.New(409, "resource_suspend_terminal_proof_time_conflict", "The Kubernetes terminal proof does not follow the durable Checkpoint handoff.")
		}
		if execution.Generation != proof.Generation || !suspendStatusMatchesReason(execution, attempt) ||
			execution.WorkerID == nil || *execution.WorkerID != attempt.WorkerID {
			return nil
		}

		if lease.TenantID != execution.TenantID || lease.WorkerID != attempt.WorkerID || lease.Generation != proof.Generation ||
			lease.WorkerIncarnation != attempt.WorkerIncarnation || lease.WorkerInstanceUID != proof.PodUID {
			return problem.New(409, "resource_suspend_terminal_proof_fenced", "The Kubernetes terminal proof does not match the active Worker Lease incarnation.")
		}
		// The attempt's immutable Pod snapshot was validated against a
		// pod-bound registration when it was created, and the Lease freezes the
		// same physical incarnation. Do not consult the mutable logical Worker
		// row here: a replacement Pod may legitimately re-register after this
		// exact UID exits, without invalidating its terminal proof.

		now := s.now()
		expired, event, err := s.cancelExecutionIfSessionAbsoluteExpiredLocked(ctx, tx, &execution, &lease, now)
		if err != nil {
			return err
		}
		if expired {
			appended = append(appended, event)
			if err := terminalizeWorkerIncarnationFromLeaseLocked(
				ctx, tx, lease, proof.ObservedAt, "kubernetes-terminal-observed",
			); err != nil {
				return err
			}
			return nil
		}
		if err := s.requireSuspendWorkspaceRecoverable(ctx, tx, execution); err != nil {
			return err
		}
		if err := s.requireResourceSuspendStillSafe(ctx, tx, execution, attempt.WorkerID, attempt, now); err != nil {
			return err
		}
		if err := s.requireActiveTurnCheckpointReceipt(ctx, tx, execution, attempt); err != nil {
			return err
		}
		autoResume, err := s.prepareActiveTurnSuspendCompletion(ctx, tx, execution, attempt, lease)
		if err != nil {
			return err
		}

		completed := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
			Where(
				"tenant_id = ? AND id = ? AND status = ? AND checkpoint_ready_at IS NOT NULL AND pod_terminal_observed_at IS NULL",
				attempt.TenantID, attempt.ID, "checkpointing",
			).
			Updates(map[string]any{
				"status": "completed", "completed_at": proof.ObservedAt,
				"pod_terminal_observed_at": proof.ObservedAt, "pod_terminal_phase": proof.Phase,
			})
		if err := expectOne(completed, 409, "resource_suspend_attempt_conflict", "The Kubernetes suspend proof changed concurrently."); err != nil {
			return err
		}
		leaseDelete := tx.WithContext(ctx).Where(
			"tenant_id = ? AND execution_id = ? AND worker_id = ? AND generation = ? AND worker_incarnation = ? AND worker_instance_uid = ?",
			lease.TenantID, lease.ExecutionID, lease.WorkerID, lease.Generation,
			lease.WorkerIncarnation, lease.WorkerInstanceUID,
		).Delete(&persistence.WorkerLease{})
		if err := expectOne(leaseDelete, 409, "lease_release_failed", "Failed to release the Pod-terminal suspended Execution Lease."); err != nil {
			return err
		}
		if err := recordWorkerClaimReleaseFact(ctx, tx, executionClaimReleaseInput(
			lease, proof.ObservedAt, now, workerClaimReleaseResourceSuspendedPodTerminal,
			workerClaimReleaseAuthorityKubernetes, proof.PodUID, "",
		)); err != nil {
			return err
		}
		if err := terminalizeWorkerIncarnationFromLeaseLocked(
			ctx, tx, lease, proof.ObservedAt, "kubernetes-terminal-observed",
		); err != nil {
			return err
		}

		recoveryReason := "suspend-resume"
		expectedStatus := suspendExpectedExecutionStatus(attempt)
		suspended := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ? AND worker_id = ? AND generation = ? AND status = ?",
				execution.TenantID, execution.ID, lease.WorkerID, lease.Generation, expectedStatus).
			Updates(map[string]any{"status": "suspended", "worker_id": nil, "next_recovery_reason": recoveryReason})
		if err := expectOne(suspended, 409, "resource_suspend_conflict", "The Execution could not enter Pod-terminal suspension."); err != nil {
			return err
		}
		execution.Status = "suspended"
		execution.WorkerID = nil
		execution.NextRecoveryReason = &recoveryReason
		suspendedEvent, err := s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.suspended", ActorType: "system",
			ExecutionID: &execution.ID, WorkerID: &lease.WorkerID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "suspendAttemptId": attempt.ID,
				"checkpointStatus": *attempt.CheckpointStatus, "suspendedAt": proof.ObservedAt,
				"completionMode": attempt.CompletionMode, "podUid": proof.PodUID,
				"podTerminalPhase": proof.Phase, "podTerminalObservedAt": proof.ObservedAt,
			},
		})
		if err != nil {
			return err
		}
		appended = append(appended, suspendedEvent)
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &execution.TenantID, Topic: "execution.suspended", MessageKey: execution.ID.String(),
			Payload: map[string]any{
				"tenantId": execution.TenantID, "sessionId": execution.SessionID, "turnId": execution.TurnID,
				"executionId": execution.ID, "generation": execution.Generation,
				"suspendAttemptId": attempt.ID, "suspendedAt": proof.ObservedAt,
				"completionMode": attempt.CompletionMode, "podUid": proof.PodUID,
			},
		}); err != nil {
			return problem.Wrap(500, "resource_suspend_outbox_failed", "The Pod-terminal suspended Execution event could not be queued.", err)
		}

		if attempt.Reason == resourceSuspendReasonWaitingKeepAlive {
			var pending int64
			if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
				Where("tenant_id = ? AND execution_id = ? AND status = ?", execution.TenantID, execution.ID, "pending").
				Count(&pending).Error; err != nil {
				return problem.Wrap(500, "interaction_pending_count_failed", "Pending interactions could not be checked after Pod-terminal suspension.", err)
			}
			if pending == 0 {
				recoveringEvent, err := s.resumeSuspendedExecutionLocked(
					ctx, tx, &execution, "system", nil, &lease.WorkerID,
					"interaction_resolved", "suspend-checkpoint-resolved", now,
				)
				if err != nil {
					return err
				}
				appended = append(appended, recoveringEvent)
			}
		} else if autoResume {
			recoveringEvent, err := s.resumeSuspendedExecutionLocked(
				ctx, tx, &execution, "system", nil, &lease.WorkerID,
				"activity_crossed_suspend_boundary", "active-idle-activity-resume", now,
			)
			if err != nil {
				return err
			}
			appended = append(appended, recoveringEvent)
		}
		finalized = true
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, event := range appended {
		if event.EventID != uuid.Nil {
			s.sessions.PublishInternalEvent(event)
		}
	}
	return finalized, nil
}

func (s *Service) AbortResourceSuspend(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input AbortResourceSuspendInput,
	requestID string,
) (OperationResult[Execution], error) {
	input.FailureCode = strings.TrimSpace(input.FailureCode)
	input.FailureMessage = strings.TrimSpace(input.FailureMessage)
	if input.SuspendAttemptID == uuid.Nil || input.FailureCode == "" || len(input.FailureCode) > 160 ||
		len(input.FailureMessage) > 2000 || strings.ContainsAny(input.FailureCode, "\r\n\t") {
		return OperationResult[Execution]{}, problem.New(400, "invalid_resource_suspend_failure", "The suspend failure payload is invalid.")
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.resource-suspend.abort", struct {
		ExecutionID uuid.UUID                 `json:"executionId"`
		Input       AbortResourceSuspendInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (Execution, error) {
		lease, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return Execution{}, err
		}
		now := s.now()
		expired, event, err := s.cancelExecutionIfSessionAbsoluteExpiredLocked(
			ctx, tx, &execution, &lease, now,
		)
		if err != nil {
			return Execution{}, err
		}
		if expired {
			appended = event
			return toExecution(execution), nil
		}
		attempt, err := lockSuspendAttempt(ctx, tx, execution, worker.ID, input.SuspendAttemptID)
		if err != nil {
			return Execution{}, err
		}
		if err := abortSuspendAttemptRow(ctx, tx, attempt, now, input.FailureCode, input.FailureMessage); err != nil {
			return Execution{}, err
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.suspend-aborted", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "suspendAttemptId": attempt.ID,
				"reason":      attempt.Reason,
				"failureCode": input.FailureCode, "failureMessage": input.FailureMessage,
			},
		})
		if err != nil {
			return Execution{}, err
		}
		resourceState := "waiting"
		if execution.Status == "leased" || execution.Status == "running" {
			resourceState = "active"
		}
		if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
			Updates(map[string]any{"resource_state": resourceState, "resource_idle_since": nil}).Error; err != nil {
			return Execution{}, problem.Wrap(500, "session_resource_state_update_failed", "Failed to restore the Session resource state after aborting suspension.", err)
		}
		return toExecution(execution), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

type resourceSuspendEligibility struct {
	Reason string
}

func (s *Service) resourceSuspendEligibility(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workerID uuid.UUID,
	session persistence.AgentSession,
	now time.Time,
) (*resourceSuspendEligibility, error) {
	switch execution.Status {
	case "waiting-for-approval":
		if session.WaitingKeepAliveSeconds <= 0 {
			return nil, nil
		}
		eligibleAt, eligible, err := s.resourceSuspendWaitingEligibleAt(ctx, tx, execution, workerID, now)
		if err != nil || !eligible {
			return nil, err
		}
		keepAlive := time.Duration(session.WaitingKeepAliveSeconds) * time.Second
		if now.Before(eligibleAt.Add(keepAlive)) {
			return nil, nil
		}
		return &resourceSuspendEligibility{Reason: resourceSuspendReasonWaitingKeepAlive}, nil
	case "running":
		eligible, err := s.resourceSuspendActiveTurnEligible(ctx, tx, execution, session, uuid.Nil, now)
		if err != nil || !eligible {
			return nil, err
		}
		return &resourceSuspendEligibility{Reason: resourceSuspendReasonActiveIdleTimeout}, nil
	default:
		return nil, nil
	}
}

func (s *Service) resourceSuspendActiveTurnEligible(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	session persistence.AgentSession,
	ignoredControlCommandID uuid.UUID,
	now time.Time,
) (bool, error) {
	resourceStateAllowed := session.ResourceState == "active" ||
		(ignoredControlCommandID != uuid.Nil && session.ResourceState == "checkpointing")
	if execution.Status != "running" || !resourceStateAllowed ||
		session.SuspendAfterIdleSeconds <= 0 || session.MeaningfulActivityAt.IsZero() ||
		now.Before(session.MeaningfulActivityAt.Add(time.Duration(session.SuspendAfterIdleSeconds)*time.Second)) {
		return false, nil
	}
	var pendingInteractions int64
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND status = ?", execution.TenantID, execution.ID, "pending").
		Count(&pendingInteractions).Error; err != nil {
		return false, problem.Wrap(500, "active_suspend_interaction_lookup_failed", "Pending interactions could not be checked for active-turn suspension.", err)
	}
	if pendingInteractions > 0 {
		return false, nil
	}
	return s.resourceSuspendSharedBlockersClear(ctx, tx, execution, ignoredControlCommandID)
}

func (s *Service) resourceSuspendWaitingEligibleAt(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workerID uuid.UUID,
	now time.Time,
) (time.Time, bool, error) {
	var oldestPending persistence.ExecutionInteraction
	err := tx.WithContext(ctx).
		Select("id", "requested_at").
		Where("tenant_id = ? AND execution_id = ? AND worker_id = ? AND generation = ? AND status = ? AND expires_at > ?",
			execution.TenantID, execution.ID, workerID, execution.Generation, "pending", now).
		Order("requested_at, id").Take(&oldestPending).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, problem.Wrap(500, "resource_suspend_interaction_lookup_failed", "Pending interactions could not be checked for resource suspension.", err)
	}
	clear, err := s.resourceSuspendSharedBlockersClear(ctx, tx, execution, uuid.Nil)
	if err != nil || !clear {
		return time.Time{}, false, err
	}
	return oldestPending.RequestedAt, true, nil
}

func (s *Service) resourceSuspendSharedBlockersClear(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	ignoredControlCommandID uuid.UUID,
) (bool, error) {
	checks := []struct {
		model any
		where string
		args  []any
	}{
		{&persistence.ExecutionInteraction{}, "tenant_id = ? AND execution_id = ? AND status = ? AND delivery_status IN ?", []any{execution.TenantID, execution.ID, "resolved", []string{"pending", "delivered", "failed"}}},
		{&persistence.Artifact{}, "tenant_id = ? AND execution_id = ? AND status = ?", []any{execution.TenantID, execution.ID, "pending"}},
		{&persistence.WorkspaceCheckpoint{}, "tenant_id = ? AND execution_id = ? AND generation = ? AND status IN ?", []any{execution.TenantID, execution.ID, execution.Generation, []string{"pending", "uploading"}}},
	}
	controlWhere := "tenant_id = ? AND execution_id = ? AND status IN ?"
	controlArgs := []any{execution.TenantID, execution.ID, []string{"pending", "delivered"}}
	if ignoredControlCommandID != uuid.Nil {
		controlWhere += " AND id <> ?"
		controlArgs = append(controlArgs, ignoredControlCommandID)
	}
	checks = append(checks, struct {
		model any
		where string
		args  []any
	}{&persistence.ExecutionControlCommand{}, controlWhere, controlArgs})
	for _, check := range checks {
		var count int64
		if err := tx.WithContext(ctx).Model(check.model).Where(check.where, check.args...).Count(&count).Error; err != nil {
			return false, problem.Wrap(500, "resource_suspend_blocker_lookup_failed", "Resource suspension blockers could not be checked.", err)
		}
		if count > 0 {
			return false, nil
		}
	}
	return true, nil
}

func (s *Service) requireResourceSuspendStillSafe(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workerID uuid.UUID,
	attempt persistence.ExecutionSuspendAttempt,
	now time.Time,
) error {
	switch attempt.Reason {
	case resourceSuspendReasonWaitingKeepAlive:
		if attempt.ProviderQuiescedAt != nil {
			if execution.Status != "waiting-for-approval" {
				return problem.New(409, "resource_suspend_no_longer_safe", "The waiting suspend boundary no longer owns the current Execution state.")
			}
			return nil
		}
		_, eligible, err := s.resourceSuspendWaitingEligibleAt(ctx, tx, execution, workerID, now)
		if err != nil {
			return err
		}
		if !eligible {
			return problem.New(409, "resource_suspend_no_longer_safe", "The Execution changed while its suspend Checkpoint was being prepared.")
		}
	case resourceSuspendReasonActiveIdleTimeout:
		var session persistence.AgentSession
		if err := tx.WithContext(ctx).
			Select("id", "tenant_id", "resource_state", "meaningful_activity_at", "meaningful_activity_sequence", "suspend_after_idle_seconds").
			Where("tenant_id = ? AND id = ? AND status = ?", execution.TenantID, execution.SessionID, "active").
			Take(&session).Error; err != nil {
			return problem.Wrap(409, "active_suspend_session_unavailable", "The active Session could not be verified for suspension.", err)
		}
		if attempt.BoundaryMeaningfulActivitySequence == nil ||
			*attempt.BoundaryMeaningfulActivitySequence <= 0 {
			return problem.New(409, "active_suspend_activity_boundary_missing", "The active-turn suspend attempt does not have a semantic activity boundary.")
		}
		// Before the Provider confirms an interrupted terminal, any semantic
		// activity cancels the idle decision. After the durable receipt exists,
		// the old Provider generation stays stopped; later user activity is queued
		// for the Recovery generation instead of reopening this boundary.
		if attempt.ProviderQuiescedAt != nil {
			if execution.Status != "running" || session.ResourceState != "checkpointing" {
				return problem.New(409, "resource_suspend_no_longer_safe", "The active-turn suspend boundary no longer owns the current Execution state.")
			}
			return nil
		}
		if session.MeaningfulActivitySequence != *attempt.BoundaryMeaningfulActivitySequence {
			return problem.New(409, "resource_suspend_no_longer_safe", "Semantic activity crossed the active-turn suspend boundary before the Provider checkpoint was committed.")
		}
		ignored := uuid.Nil
		if attempt.ControlCommandID != nil {
			ignored = *attempt.ControlCommandID
		}
		eligible, err := s.resourceSuspendActiveTurnEligible(ctx, tx, execution, session, ignored, now)
		if err != nil {
			return err
		}
		if !eligible {
			return problem.New(409, "resource_suspend_no_longer_safe", "Semantic activity or another durable operation crossed the active-turn suspend boundary.")
		}
	default:
		return problem.New(409, "resource_suspend_no_longer_safe", "The resource suspension attempt reason is no longer recognized.")
	}
	return nil
}

func (s *Service) requireSuspendWorkspaceRecoverable(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) error {
	if execution.RemoteWorkspaceID == nil {
		return nil
	}
	var workspace persistence.RemoteWorkspace
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ? AND session_id = ?", execution.TenantID, *execution.RemoteWorkspaceID, execution.SessionID).
		Take(&workspace).Error; err != nil {
		return problem.Wrap(409, "resource_suspend_workspace_unavailable", "The managed Workspace could not be locked for resource suspension.", err)
	}
	if workspace.State != "dirty" {
		return nil
	}
	if execution.RestoreCheckpointID == nil || workspace.CurrentCheckpointID == nil ||
		*execution.RestoreCheckpointID != *workspace.CurrentCheckpointID {
		return problem.New(409, "resource_suspend_checkpoint_required", "A dirty managed Workspace requires a current ready Checkpoint before suspension.")
	}
	var checkpoint persistence.WorkspaceCheckpoint
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND workspace_id = ? AND id = ? AND status = ?",
			execution.TenantID, workspace.ID, *execution.RestoreCheckpointID, "ready").
		Take(&checkpoint).Error; err != nil {
		return problem.Wrap(409, "resource_suspend_checkpoint_required", "The current managed Workspace Checkpoint is not ready.", err)
	}
	return nil
}

func lockSuspendAttempt(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workerID, attemptID uuid.UUID,
) (persistence.ExecutionSuspendAttempt, error) {
	var attempt persistence.ExecutionSuspendAttempt
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ? AND execution_id = ? AND worker_id = ? AND generation = ?",
			execution.TenantID, attemptID, execution.ID, workerID, execution.Generation).
		Take(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionSuspendAttempt{}, problem.New(404, "resource_suspend_attempt_not_found", "The resource suspension attempt was not found.")
	}
	if err != nil {
		return persistence.ExecutionSuspendAttempt{}, problem.Wrap(500, "resource_suspend_attempt_load_failed", "The resource suspension attempt could not be loaded.", err)
	}
	if attempt.Status != "checkpointing" {
		return persistence.ExecutionSuspendAttempt{}, problem.New(409, "resource_suspend_attempt_finished", "The resource suspension attempt is no longer active.")
	}
	return attempt, nil
}

func abortSuspendAttemptRow(
	ctx context.Context,
	tx *gorm.DB,
	attempt persistence.ExecutionSuspendAttempt,
	now time.Time,
	failureCode, failureMessage string,
) error {
	if attempt.ControlCommandID != nil {
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionControlCommand{}).
			Where("tenant_id = ? AND execution_id = ? AND id = ? AND status IN ?",
				attempt.TenantID, attempt.ExecutionID, *attempt.ControlCommandID, outstandingControlCommandStatuses).
			Updates(map[string]any{"status": "superseded", "delivery_error": failureMessage}).Error; err != nil {
			return problem.Wrap(500, "active_suspend_control_supersede_failed", "The active-turn Suspend command could not be fenced.", err)
		}
	}
	updated := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ? AND status = ?", attempt.TenantID, attempt.ID, "checkpointing").
		Updates(map[string]any{
			"status": "aborted", "aborted_at": now,
			"failure_code": failureCode, "failure_message": failureMessage,
		})
	return expectOne(updated, 409, "resource_suspend_attempt_conflict", "The resource suspension attempt changed concurrently.")
}

func supersedeResourceSuspendAttempts(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	now time.Time,
	failureCode, failureMessage string,
) error {
	activeAttempts := make([]persistence.ExecutionSuspendAttempt, 0)
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "execution_id", "control_command_id").
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND status = ? AND control_command_id IS NOT NULL",
			execution.TenantID, execution.ID, execution.Generation, "checkpointing").
		Find(&activeAttempts).Error; err != nil {
		return problem.Wrap(500, "resource_suspend_attempt_load_failed", "Active resource suspension commands could not be loaded for fencing.", err)
	}
	for _, attempt := range activeAttempts {
		if attempt.ControlCommandID == nil {
			continue
		}
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionControlCommand{}).
			Where("tenant_id = ? AND execution_id = ? AND id = ? AND status IN ?",
				execution.TenantID, execution.ID, *attempt.ControlCommandID, outstandingControlCommandStatuses).
			Updates(map[string]any{"status": "superseded", "delivery_error": failureMessage}).Error; err != nil {
			return problem.Wrap(500, "active_suspend_control_supersede_failed", "An obsolete active-turn Suspend command could not be fenced.", err)
		}
	}
	updated := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND status = ?",
			execution.TenantID, execution.ID, execution.Generation, "checkpointing").
		Updates(map[string]any{
			"status": "superseded", "aborted_at": now,
			"failure_code": failureCode, "failure_message": failureMessage,
		})
	if updated.Error != nil {
		return problem.Wrap(500, "resource_suspend_attempt_supersede_failed", "The obsolete resource suspension attempt could not be superseded.", updated.Error)
	}
	return nil
}

func resourceDirectiveFromAttempt(attempt persistence.ExecutionSuspendAttempt) *ResourceDirective {
	directive := &ResourceDirective{
		Action: "suspend", Reason: attempt.Reason, CompletionMode: attempt.CompletionMode,
		SuspendAttemptID: attempt.ID, RequestedAt: attempt.RequestedAt,
		CheckpointDeadlineAt: attempt.CheckpointDeadlineAt,
	}
	if attempt.ControlCommandID != nil {
		directive.ControlCommandID = *attempt.ControlCommandID
	}
	return directive
}

func resourceSuspendQuiesceReceiptFromAttempt(attempt persistence.ExecutionSuspendAttempt) ResourceSuspendQuiesceReceipt {
	return ResourceSuspendQuiesceReceipt{
		SuspendAttemptID:   attempt.ID,
		ProviderQuiescedAt: *attempt.ProviderQuiescedAt,
	}
}

func resourceSuspendCheckpointReadyReceiptFromAttempt(
	attempt persistence.ExecutionSuspendAttempt,
) ResourceSuspendCheckpointReadyReceipt {
	return ResourceSuspendCheckpointReadyReceipt{
		SuspendAttemptID: attempt.ID, CheckpointStatus: *attempt.CheckpointStatus,
		CheckpointReadyAt: *attempt.CheckpointReadyAt,
	}
}
