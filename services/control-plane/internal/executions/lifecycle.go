package executions

import (
	"context"
	"crypto/subtle"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func (s *Service) Claim(
	ctx context.Context,
	worker persistence.WorkerInstance,
	input ClaimExecutionInput,
	requestID string,
) (OperationResult[ClaimResult], error) {
	normalizedTarget, err := normalizeClaimTarget(worker, input)
	if err != nil {
		return OperationResult[ClaimResult]{}, err
	}
	if err := s.RecoverExpired(ctx, 100); err != nil {
		return OperationResult[ClaimResult]{}, err
	}
	hash, err := requestHash("execution.claim", normalizedTarget)
	if err != nil {
		return OperationResult[ClaimResult]{}, problem.Wrap(500, "request_fingerprint_failed", "Failed to fingerprint the claim request.", err)
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 160 {
		return OperationResult[ClaimResult]{}, problem.New(400, "invalid_request_id", "X-Request-ID must contain between 1 and 160 characters.")
	}

	for attempt := 0; attempt < 2; attempt++ {
		var result ClaimResult
		var appended persistence.SessionEvent
		replayed := false
		err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
			var receipt persistence.WorkerRequestReceipt
			lookupErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("worker_id = ? AND request_id = ?", worker.ID, requestID).Take(&receipt).Error
			if err := lockCurrentWorkerIncarnation(ctx, tx, worker); err != nil {
				return err
			}
			if lookupErr == nil && receipt.WorkerIncarnation == worker.Incarnation && receipt.ExpiresAt.After(s.now()) {
				if receipt.Operation != "execution.claim" || receipt.RequestHash != hash {
					return problem.New(409, "request_id_reused", "X-Request-ID was already used for a different worker request.")
				}
				stored, decodeErr := decodeReceiptResponse[ClaimResult](receipt)
				if decodeErr != nil {
					return problem.Wrap(500, "receipt_decode_failed", "Failed to restore the claim response.", decodeErr)
				}
				if stored.Execution == nil || stored.Lease == nil {
					result = stored
					replayed = true
					return nil
				}
				lease, execution, leaseErr := s.lockLease(ctx, tx, worker, stored.Execution.ID, LeaseInput{
					TenantID: stored.Execution.TenantID, Generation: stored.Execution.Generation,
					LeaseToken: "",
				}, false)
				if leaseErr != nil {
					return problem.New(409, "claim_replay_no_longer_active", "The execution returned by this claim request is no longer leased to the worker.")
				}
				plainToken, tokenHash, tokenErr := secret.NewToken()
				if tokenErr != nil {
					return problem.Wrap(500, "lease_token_generation_failed", "Failed to rotate the replayed lease token.", tokenErr)
				}
				now := s.now()
				lease.LeaseTokenHash = tokenHash
				lease.HeartbeatAt = now
				lease.ExpiresAt = now.Add(s.leaseTTL)
				rotation := tx.WithContext(ctx).Model(&persistence.WorkerLease{}).
					Where("execution_id = ? AND worker_id = ? AND generation = ?", lease.ExecutionID, worker.ID, lease.Generation).
					Updates(map[string]any{
						"lease_token_hash": tokenHash, "heartbeat_at": lease.HeartbeatAt, "expires_at": lease.ExpiresAt,
					})
				if err := expectOne(rotation, 409, "lease_token_rotation_failed", "Failed to rotate the replayed lease token."); err != nil {
					return err
				}
				convertedExecution := toExecution(execution)
				convertedLease := toLease(lease, plainToken)
				resumeCursor, cursorErr := s.loadProviderCursor(ctx, tx, execution)
				if cursorErr != nil {
					return cursorErr
				}
				workload, workloadErr := s.loadWorkload(ctx, tx, execution)
				if workloadErr != nil {
					return workloadErr
				}
				result = ClaimResult{
					Execution: &convertedExecution, Lease: &convertedLease, Workload: &workload,
					ProviderResumeCursor: resumeCursor,
				}
				replayed = true
				return nil
			}
			if lookupErr == nil {
				if err := tx.WithContext(ctx).Delete(&receipt).Error; err != nil {
					return problem.Wrap(500, "receipt_expiry_cleanup_failed", "Failed to replace an expired claim receipt.", err)
				}
			} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
				return problem.Wrap(500, "receipt_lookup_failed", "Failed to inspect the claim receipt.", lookupErr)
			}

			claimWorker, err := s.requireClaimableWorker(ctx, tx, worker.ID)
			if err != nil {
				return err
			}
			controlCommandSupport, err := loadWorkerControlCommandSupport(ctx, tx, claimWorker)
			if err != nil {
				return err
			}
			var execution persistence.AgentExecution
			claimQuery := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "SKIP LOCKED").
				Joins("JOIN tenants AS claim_tenant ON claim_tenant.id = agent_executions.tenant_id AND claim_tenant.status = ? AND claim_tenant.deleted_at IS NULL", "active").
				Where("agent_executions.status IN ? AND agent_executions.execution_target_id = ? AND agent_executions.target_kind = ?", []string{"queued", "recovering"}, normalizedTarget.ExecutionTargetID, normalizedTarget.TargetKind)
			if normalizedTarget.ExecutionID != nil {
				claimQuery = claimQuery.Where("agent_executions.id = ?", *normalizedTarget.ExecutionID)
			}
			if claimWorker.CurrentManifestID != nil {
				claimQuery = claimQuery.Where(`agent_executions.provider IS NULL OR EXISTS (
					SELECT 1 FROM worker_provider_manifests provider_manifest
					WHERE provider_manifest.worker_manifest_id = ?
					  AND provider_manifest.provider = agent_executions.provider
					  AND provider_manifest.compatibility_status = 'compatible'
					)`, *claimWorker.CurrentManifestID)
			}
			claimQuery = controlCommandSupport.filterClaimQuery(claimQuery)
			claimErr := claimQuery.Order("agent_executions.queued_at, agent_executions.id").Take(&execution).Error
			if errors.Is(claimErr, gorm.ErrRecordNotFound) {
				if normalizedTarget.ExecutionID != nil {
					var assigned persistence.AgentExecution
					assignedErr := tx.WithContext(ctx).
						Where("id = ? AND status IN ? AND execution_target_id = ? AND target_kind = ?",
							*normalizedTarget.ExecutionID, []string{"queued", "recovering"},
							normalizedTarget.ExecutionTargetID, normalizedTarget.TargetKind).
						Take(&assigned).Error
					if assignedErr == nil {
						if claimWorker.CurrentManifestID != nil {
							supported, supportErr := workerSupportsProvider(tx, claimWorker, assigned.Provider)
							if supportErr != nil {
								return problem.Wrap(500, "worker_manifest_lookup_failed", "Failed to inspect Worker Provider compatibility.", supportErr)
							}
							if !supported {
								return problem.New(409, "worker_provider_incompatible", "The Worker manifest does not support the assigned Execution Provider.")
							}
						}
						commandsSupported, supportErr := controlCommandSupport.supportsExecution(ctx, tx, assigned)
						if supportErr != nil {
							return problem.Wrap(500, "control_command_capability_lookup_failed", "Failed to inspect pending Control command capabilities.", supportErr)
						}
						if !commandsSupported {
							return problem.New(409, "capability_unsupported", "The Worker manifest does not support every pending Provider control command.")
						}
					}
				}
				result = ClaimResult{}
			} else if claimErr != nil {
				return problem.Wrap(500, "execution_claim_lookup_failed", "Failed to find a claimable execution.", claimErr)
			} else {
				previousStatus := execution.Status
				previousGeneration := execution.Generation
				plainToken, tokenHash, tokenErr := secret.NewToken()
				if tokenErr != nil {
					return problem.Wrap(500, "lease_token_generation_failed", "Failed to generate an execution lease token.", tokenErr)
				}
				now := s.now()
				execution.Status = "leased"
				execution.WorkerID = &worker.ID
				execution.WorkerManifestID = claimWorker.CurrentManifestID
				execution.Generation++
				execution.FinishedAt = nil
				execution.FailureCode = nil
				execution.FailureMessage = nil
				claimUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
					Where("id = ? AND status = ? AND generation = ?", execution.ID, previousStatus, previousGeneration).
					Updates(map[string]any{
						"status": execution.Status, "worker_id": worker.ID, "generation": execution.Generation,
						"worker_manifest_id": execution.WorkerManifestID,
						"finished_at":        nil, "failure_code": nil, "failure_message": nil,
					})
				if err := expectOne(claimUpdate, 409, "execution_claim_conflict", "The execution was claimed concurrently."); err != nil {
					return err
				}
				lease := persistence.WorkerLease{
					ExecutionID: execution.ID, TenantID: execution.TenantID, WorkerID: worker.ID,
					Generation: execution.Generation, LeaseTokenHash: tokenHash,
					AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(s.leaseTTL),
				}
				if err := tx.WithContext(ctx).Create(&lease).Error; err != nil {
					return problem.Wrap(409, "execution_lease_conflict", "The execution already has an active lease.", err)
				}
				if err := bindExecutionRuntimeResources(ctx, tx, claimWorker, execution, now); err != nil {
					return err
				}
				if err := bindExecutionControlCommands(ctx, tx, execution, worker.ID, now); err != nil {
					return err
				}
				appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
					EventType: "execution.leased", ActorType: "worker", ActorID: &worker.ID,
					ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
					Payload: map[string]any{
						"turnId": execution.TurnID, "expiresAt": lease.ExpiresAt,
						"executionTargetId": execution.ExecutionTargetID, "targetKind": execution.TargetKind,
						"workerManifestId": execution.WorkerManifestID,
					},
				})
				if err != nil {
					return err
				}
				convertedExecution := toExecution(execution)
				convertedLease := toLease(lease, plainToken)
				resumeCursor, cursorErr := s.loadProviderCursor(ctx, tx, execution)
				if cursorErr != nil {
					return cursorErr
				}
				workload, workloadErr := s.loadWorkload(ctx, tx, execution)
				if workloadErr != nil {
					return workloadErr
				}
				result = ClaimResult{
					Execution: &convertedExecution, Lease: &convertedLease, Workload: &workload,
					ProviderResumeCursor: resumeCursor,
				}
			}

			stored := result
			if stored.Lease != nil {
				leaseCopy := *stored.Lease
				leaseCopy.LeaseToken = ""
				stored.Lease = &leaseCopy
			}
			stored.ProviderResumeCursor = nil
			mapped, mapErr := responseMap(stored)
			if mapErr != nil {
				return problem.Wrap(500, "receipt_encode_failed", "Failed to persist the claim response.", mapErr)
			}
			now := s.now()
			receipt = persistence.WorkerRequestReceipt{
				WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
				RequestID: requestID, Operation: "execution.claim", RequestHash: hash,
				StatusCode: 200, Response: mapped, CreatedAt: now, ExpiresAt: now.Add(s.receiptTTL),
			}
			if err := tx.WithContext(ctx).Create(&receipt).Error; err != nil {
				if errors.Is(err, gorm.ErrDuplicatedKey) {
					return errReceiptRace
				}
				return problem.Wrap(500, "receipt_create_failed", "Failed to persist the claim receipt.", err)
			}
			return nil
		})
		if errors.Is(err, errReceiptRace) {
			continue
		}
		if err != nil {
			return OperationResult[ClaimResult]{}, err
		}
		if appended.EventID != uuid.Nil {
			s.sessions.PublishInternalEvent(appended)
		}
		return OperationResult[ClaimResult]{Value: result, Replayed: replayed, StatusCode: 200}, nil
	}
	return OperationResult[ClaimResult]{}, problem.New(409, "request_receipt_conflict", "The claim request is still being committed; retry with the same X-Request-ID.")
}

func (s *Service) Renew(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input RenewLeaseInput,
	requestID string,
) (OperationResult[Lease], error) {
	return runIdempotent(ctx, s, worker, requestID, "execution.renew", struct {
		ExecutionID uuid.UUID       `json:"executionId"`
		Input       RenewLeaseInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (Lease, error) {
		lease, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return Lease{}, err
		}
		if err := requireExecutionTenantActive(ctx, tx, execution.TenantID); err != nil {
			return Lease{}, err
		}
		if err := s.storeProviderCursor(ctx, tx, execution, input.ProviderResumeCursor); err != nil {
			return Lease{}, err
		}
		now := s.now()
		lease.HeartbeatAt = now
		lease.ExpiresAt = now.Add(s.leaseTTL)
		renewal := tx.WithContext(ctx).Model(&persistence.WorkerLease{}).
			Where("execution_id = ? AND worker_id = ? AND generation = ?", executionID, worker.ID, input.Generation).
			Updates(map[string]any{"heartbeat_at": lease.HeartbeatAt, "expires_at": lease.ExpiresAt})
		if err := expectOne(renewal, 409, "lease_renew_failed", "Failed to renew the execution lease."); err != nil {
			return Lease{}, err
		}
		if err := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ?", worker.ID).Update("last_heartbeat_at", now).Error; err != nil {
			return Lease{}, problem.Wrap(500, "worker_heartbeat_failed", "Failed to update the worker heartbeat.", err)
		}
		return toLease(lease, ""), nil
	})
}

func (s *Service) Start(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input LeaseInput,
	requestID string,
) (OperationResult[Execution], error) {
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.start", struct {
		ExecutionID uuid.UUID  `json:"executionId"`
		Input       LeaseInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (Execution, error) {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input, true)
		if err != nil {
			return Execution{}, err
		}
		if err := requireExecutionTenantActive(ctx, tx, execution.TenantID); err != nil {
			return Execution{}, err
		}
		if execution.Status == "running" {
			return toExecution(execution), nil
		}
		now := s.now()
		execution.Status = "running"
		execution.StartedAt = &now
		startUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("id = ? AND tenant_id = ? AND worker_id = ? AND generation = ? AND status = ?", execution.ID, execution.TenantID, worker.ID, input.Generation, "leased").
			Updates(map[string]any{"status": "running", "started_at": now})
		if err := expectOne(startUpdate, 409, "execution_start_conflict", "The execution could not be started from its current state."); err != nil {
			return Execution{}, err
		}
		turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND session_id = ? AND id = ?", execution.TenantID, execution.SessionID, execution.TurnID).
			Updates(map[string]any{"status": "running", "started_at": now})
		if err := expectOne(turnUpdate, 500, "turn_start_failed", "Failed to mark the turn as running."); err != nil {
			return Execution{}, err
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.started", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{"turnId": execution.TurnID, "startedAt": now},
		})
		if err != nil {
			return Execution{}, err
		}
		return toExecution(execution), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) Complete(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input CompleteExecutionInput,
	requestID string,
) (OperationResult[Execution], error) {
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.complete", struct {
		ExecutionID uuid.UUID              `json:"executionId"`
		Input       CompleteExecutionInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (Execution, error) {
		lease, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return Execution{}, err
		}
		if execution.Status == "waiting-for-approval" {
			return Execution{}, problem.New(409, "execution_interaction_pending", "The execution is waiting for approval or user input.")
		}
		if execution.RemoteWorkspaceID != nil {
			workspace, err := lockExecutionWorkspace(ctx, tx, execution)
			if err != nil {
				return Execution{}, err
			}
			if workspace.LastWorkerID == nil || *workspace.LastWorkerID != worker.ID ||
				workspace.LastExecutionID == nil || *workspace.LastExecutionID != execution.ID ||
				workspace.LastGeneration == nil || *workspace.LastGeneration != execution.Generation {
				return Execution{}, problem.New(409, "workspace_generation_conflict", "The logical Workspace is not owned by the current Execution Generation.")
			}
			if workspace.State != "ready" && workspace.State != "dirty" {
				return Execution{}, problem.New(409, "workspace_checkpoint_required", "The logical Workspace must have a ready Checkpoint before the Execution can complete.")
			}
			if workspace.State == "dirty" {
				if workspace.CurrentCheckpointID == nil {
					return Execution{}, problem.New(409, "workspace_checkpoint_required", "The dirty logical Workspace must have a ready Checkpoint before the Execution can complete.")
				}
				var checkpoint persistence.WorkspaceCheckpoint
				if err := tx.WithContext(ctx).
					Where("tenant_id = ? AND workspace_id = ? AND id = ? AND status = ?",
						execution.TenantID, workspace.ID, *workspace.CurrentCheckpointID, "ready").
					Take(&checkpoint).Error; err != nil {
					return Execution{}, problem.Wrap(409, "workspace_checkpoint_required", "The dirty logical Workspace does not have an available ready Checkpoint.", err)
				}
				if checkpoint.ExecutionID != execution.ID || checkpoint.Generation != execution.Generation {
					return Execution{}, problem.New(409, "workspace_checkpoint_stale", "The dirty logical Workspace was not checkpointed by the current Execution Generation.")
				}
			}
		}
		if err := s.storeProviderCursor(ctx, tx, execution, input.ProviderResumeCursor); err != nil {
			return Execution{}, err
		}
		now := s.now()
		if err := tx.WithContext(ctx).Delete(&lease).Error; err != nil {
			return Execution{}, problem.Wrap(500, "lease_release_failed", "Failed to release the completed execution lease.", err)
		}
		execution.Status = "completed"
		execution.FinishedAt = &now
		completeUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("id = ? AND tenant_id = ? AND worker_id = ? AND generation = ? AND status IN ?", execution.ID, execution.TenantID, worker.ID, input.Generation, []string{"leased", "running"}).
			Updates(map[string]any{"status": "completed", "finished_at": now})
		if err := expectOne(completeUpdate, 409, "execution_complete_conflict", "The execution could not be completed from its current state."); err != nil {
			return Execution{}, err
		}
		turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND session_id = ? AND id = ?", execution.TenantID, execution.SessionID, execution.TurnID).
			Updates(map[string]any{"status": "completed", "completed_at": now})
		if err := expectOne(turnUpdate, 500, "turn_complete_failed", "Failed to complete the turn."); err != nil {
			return Execution{}, err
		}
		if err := supersedeControlCommands(ctx, tx, execution, uuid.Nil, "The Execution completed before the Control command was acknowledged."); err != nil {
			return Execution{}, err
		}
		payload := map[string]any{"turnId": execution.TurnID, "finishedAt": now}
		if input.Output != nil {
			payload["output"] = input.Output
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.completed", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: payload,
		})
		if err != nil {
			return Execution{}, err
		}
		return toExecution(execution), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) Fail(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input FailExecutionInput,
	requestID string,
) (OperationResult[Execution], error) {
	input.FailureCode = strings.TrimSpace(input.FailureCode)
	input.FailureMessage = strings.TrimSpace(input.FailureMessage)
	if input.FailureCode == "" || len(input.FailureCode) > 160 || strings.ContainsAny(input.FailureCode, "\r\n\t") {
		return OperationResult[Execution]{}, problem.New(400, "invalid_failure_code", "failureCode must contain between 1 and 160 characters.")
	}
	if len(input.FailureMessage) > 10_000 {
		return OperationResult[Execution]{}, problem.New(400, "invalid_failure_message", "failureMessage must not exceed 10000 characters.")
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.fail", struct {
		ExecutionID uuid.UUID          `json:"executionId"`
		Input       FailExecutionInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (Execution, error) {
		lease, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return Execution{}, err
		}
		if execution.Status == "waiting-for-approval" {
			return Execution{}, problem.New(409, "execution_interaction_pending", "The execution is waiting for approval or user input.")
		}
		if err := s.storeProviderCursor(ctx, tx, execution, input.ProviderResumeCursor); err != nil {
			return Execution{}, err
		}
		now := s.now()
		if err := tx.WithContext(ctx).Delete(&lease).Error; err != nil {
			return Execution{}, problem.Wrap(500, "lease_release_failed", "Failed to release the failed execution lease.", err)
		}
		execution.Status = "failed"
		execution.FinishedAt = &now
		execution.FailureCode = &input.FailureCode
		execution.FailureMessage = &input.FailureMessage
		failUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("id = ? AND tenant_id = ? AND worker_id = ? AND generation = ? AND status IN ?", execution.ID, execution.TenantID, worker.ID, input.Generation, []string{"leased", "running"}).
			Updates(map[string]any{
				"status": "failed", "finished_at": now, "failure_code": input.FailureCode,
				"failure_message": input.FailureMessage,
			})
		if err := expectOne(failUpdate, 409, "execution_fail_conflict", "The execution could not be failed from its current state."); err != nil {
			return Execution{}, err
		}
		turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND session_id = ? AND id = ?", execution.TenantID, execution.SessionID, execution.TurnID).
			Updates(map[string]any{"status": "failed", "completed_at": now})
		if err := expectOne(turnUpdate, 500, "turn_fail_failed", "Failed to fail the turn."); err != nil {
			return Execution{}, err
		}
		if err := supersedeControlCommands(ctx, tx, execution, uuid.Nil, "The Execution failed before the Control command was acknowledged."); err != nil {
			return Execution{}, err
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.failed", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "finishedAt": now,
				"failureCode": input.FailureCode, "failureMessage": input.FailureMessage,
			},
		})
		if err != nil {
			return Execution{}, err
		}
		return toExecution(execution), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) Release(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input ReleaseLeaseInput,
	requestID string,
) (OperationResult[Execution], error) {
	input.Reason = strings.TrimSpace(input.Reason)
	if len(input.Reason) > 1000 {
		return OperationResult[Execution]{}, problem.New(400, "invalid_release_reason", "reason must not exceed 1000 characters.")
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.release", struct {
		ExecutionID uuid.UUID         `json:"executionId"`
		Input       ReleaseLeaseInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (Execution, error) {
		lease, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return Execution{}, err
		}
		deleting, err := executionTenantDeleting(ctx, tx, execution.TenantID)
		if err != nil {
			return Execution{}, err
		}
		if deleting {
			appended, err = s.cancelExecutionLocked(
				ctx, tx, &execution, &lease, "worker", &worker.ID, s.now(), "tenant-delete",
			)
			if err != nil {
				return Execution{}, err
			}
			return toExecution(execution), nil
		}
		if err := tx.WithContext(ctx).Delete(&lease).Error; err != nil {
			return Execution{}, problem.Wrap(500, "lease_release_failed", "Failed to release the execution lease.", err)
		}
		if err := requeueExecutionControlCommands(ctx, tx, execution, lease, "The Worker released the Execution before the Control command was acknowledged."); err != nil {
			return Execution{}, err
		}
		execution.Status = "recovering"
		execution.WorkerID = nil
		releaseUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("id = ? AND tenant_id = ? AND worker_id = ? AND generation = ? AND status IN ?", execution.ID, execution.TenantID, worker.ID, input.Generation, []string{"leased", "running", "waiting-for-approval"}).
			Updates(map[string]any{"status": "recovering", "worker_id": nil})
		if err := expectOne(releaseUpdate, 409, "execution_release_conflict", "The execution lease could not be released from its current state."); err != nil {
			return Execution{}, err
		}
		turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND session_id = ? AND id = ?", execution.TenantID, execution.SessionID, execution.TurnID).
			Update("status", "queued")
		if err := expectOne(turnUpdate, 500, "turn_recovery_failed", "Failed to return the turn to the recovery queue."); err != nil {
			return Execution{}, err
		}
		if err := s.enqueueRecovery(ctx, tx, execution); err != nil {
			return Execution{}, err
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.recovering", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{"turnId": execution.TurnID, "reason": input.Reason},
		})
		if err != nil {
			return Execution{}, err
		}
		return toExecution(execution), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) RecoverExpired(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if err := s.markStaleWorkers(ctx); err != nil {
		return err
	}
	appended := make([]persistence.SessionEvent, 0)
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		leases := make([]persistence.WorkerLease, 0)
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "SKIP LOCKED").
			Where("expires_at <= ?", s.now()).Order("expires_at, execution_id").Limit(limit).Find(&leases).Error; err != nil {
			return problem.Wrap(500, "expired_lease_scan_failed", "Failed to scan expired execution leases.", err)
		}
		for index := range leases {
			lease := leases[index]
			var execution persistence.AgentExecution
			err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("id = ?", lease.ExecutionID).Take(&execution).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := tx.WithContext(ctx).Delete(&lease).Error; err != nil {
					return problem.Wrap(500, "orphan_lease_cleanup_failed", "Failed to remove an orphan execution lease.", err)
				}
				continue
			}
			if err != nil {
				return problem.Wrap(500, "expired_execution_lock_failed", "Failed to lock an expired execution.", err)
			}
			deleting, err := executionTenantDeleting(ctx, tx, execution.TenantID)
			if err != nil {
				return err
			}
			if deleting && containsExecutionStatus(nonterminalExecutionStatuses, execution.Status) {
				event, err := s.cancelExecutionLocked(
					ctx, tx, &execution, &lease, "system", nil, s.now(), "tenant-delete",
				)
				if err != nil {
					return err
				}
				appended = append(appended, event)
				continue
			}
			if err := tx.WithContext(ctx).Delete(&lease).Error; err != nil {
				return problem.Wrap(500, "expired_lease_release_failed", "Failed to release an expired execution lease.", err)
			}
			if execution.WorkerID == nil || *execution.WorkerID != lease.WorkerID || execution.Generation != lease.Generation ||
				(execution.Status != "leased" && execution.Status != "running" && execution.Status != "waiting-for-approval") {
				continue
			}
			if err := s.supersedeInteractionGeneration(ctx, tx, execution, lease); err != nil {
				return err
			}
			if err := requeueExecutionControlCommands(ctx, tx, execution, lease, "The Worker lease expired before the Control command was acknowledged."); err != nil {
				return err
			}
			var restoreCheckpointID *uuid.UUID
			if execution.RemoteWorkspaceID != nil {
				var workspace persistence.RemoteWorkspace
				if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
					Where("tenant_id = ? AND id = ? AND session_id = ?", execution.TenantID, *execution.RemoteWorkspaceID, execution.SessionID).
					Take(&workspace).Error; err != nil {
					return problem.Wrap(500, "workspace_recovery_load_failed", "Failed to load the logical Workspace during Execution recovery.", err)
				}
				restoreCheckpointID = workspace.CurrentCheckpointID
				if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
					Where("tenant_id = ? AND id = ?", workspace.TenantID, workspace.ID).
					Updates(map[string]any{"state": "recovering", "updated_at": s.now()}).Error; err != nil {
					return problem.Wrap(500, "workspace_recovery_update_failed", "Failed to mark the logical Workspace for recovery.", err)
				}
			}
			execution.Status = "recovering"
			execution.WorkerID = nil
			recoveryUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
				Where("id = ? AND worker_id = ? AND generation = ? AND status IN ?", execution.ID, lease.WorkerID, lease.Generation, []string{"leased", "running", "waiting-for-approval"}).
				Updates(map[string]any{
					"status": "recovering", "worker_id": nil,
					"restore_checkpoint_id": restoreCheckpointID,
				})
			if err := expectOne(recoveryUpdate, 409, "execution_recovery_failed", "Failed to move an expired execution into recovery."); err != nil {
				return err
			}
			turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
				Where("tenant_id = ? AND session_id = ? AND id = ?", execution.TenantID, execution.SessionID, execution.TurnID).
				Update("status", "queued")
			if err := expectOne(turnUpdate, 500, "turn_recovery_failed", "Failed to return the expired turn to the recovery queue."); err != nil {
				return err
			}
			if err := s.enqueueRecovery(ctx, tx, execution); err != nil {
				return err
			}
			event, err := s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
				EventType: "execution.recovering", ActorType: "system",
				ExecutionID: &execution.ID, WorkerID: &lease.WorkerID, Generation: &execution.Generation,
				Payload: map[string]any{"turnId": execution.TurnID, "reason": "lease_expired"},
			})
			if err != nil {
				return err
			}
			appended = append(appended, event)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, event := range appended {
		s.sessions.PublishInternalEvent(event)
	}
	return nil
}

func (s *Service) lockLease(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input LeaseInput,
	requireToken bool,
) (persistence.WorkerLease, persistence.AgentExecution, error) {
	if input.TenantID == uuid.Nil || input.Generation <= 0 || (requireToken && strings.TrimSpace(input.LeaseToken) == "") {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.New(400, "invalid_lease_envelope", "tenantId, generation, and leaseToken are required.")
	}
	if err := lockCurrentWorkerIncarnation(ctx, tx, worker); err != nil {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, err
	}
	var lease persistence.WorkerLease
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("execution_id = ? AND tenant_id = ?", executionID, input.TenantID).Take(&lease).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.New(409, "lease_not_current", "The execution lease is no longer current.")
	}
	if err != nil {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.Wrap(500, "lease_lock_failed", "Failed to lock the execution lease.", err)
	}
	if lease.WorkerID != worker.ID || lease.Generation != input.Generation {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.New(409, "generation_fenced", "The worker generation is no longer current.")
	}
	if requireToken && subtle.ConstantTimeCompare(lease.LeaseTokenHash, secret.HashToken(strings.TrimSpace(input.LeaseToken))) != 1 {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.New(401, "invalid_lease_token", "The execution lease token is invalid.")
	}
	if !lease.ExpiresAt.After(s.now()) {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.New(409, "lease_expired", "The execution lease has expired and must be recovered.")
	}
	var execution persistence.AgentExecution
	err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND tenant_id = ?", executionID, input.TenantID).Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.New(404, "execution_not_found", "Execution not found.")
	}
	if err != nil {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.Wrap(500, "execution_lock_failed", "Failed to lock the execution.", err)
	}
	if execution.WorkerID == nil || *execution.WorkerID != worker.ID || execution.Generation != input.Generation ||
		(execution.Status != "leased" && execution.Status != "running" && execution.Status != "waiting-for-approval") {
		return persistence.WorkerLease{}, persistence.AgentExecution{}, problem.New(409, "generation_fenced", "The worker generation is no longer current.")
	}
	return lease, execution, nil
}

func (s *Service) requireClaimableWorker(ctx context.Context, tx *gorm.DB, workerID uuid.UUID) (persistence.WorkerInstance, error) {
	var worker persistence.WorkerInstance
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", workerID).Take(&worker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerInstance{}, problem.New(401, "worker_not_found", "Worker not found.")
	}
	if err != nil {
		return persistence.WorkerInstance{}, problem.Wrap(500, "worker_lock_failed", "Failed to lock the worker.", err)
	}
	if worker.ProtocolVersion != WorkerProtocolVersion {
		return persistence.WorkerInstance{}, unsupportedWorkerProtocol(worker.ProtocolVersion)
	}
	if worker.Status != "online" {
		return persistence.WorkerInstance{}, problem.New(409, "worker_not_claimable", "Only online workers can claim executions.")
	}
	if worker.CompatibilityStatus == "incompatible" || worker.CompatibilityStatus == "revoked" {
		return persistence.WorkerInstance{}, problem.New(409, "worker_manifest_incompatible", "The Worker manifest is not compatible with this Control Plane.")
	}
	kind, err := platform.ParseExecutionTargetKind(worker.TargetKind)
	if err != nil {
		return persistence.WorkerInstance{}, problem.Wrap(500, "invalid_persisted_execution_target", "The worker target kind is invalid.", err)
	}
	if platform.IsRemoteTarget(kind) && (!worker.LeaseSupported || !worker.FencingSupported) {
		return persistence.WorkerInstance{}, problem.New(409, "remote_worker_protocol_required", "Remote workers must support execution leases and generation fencing.")
	}
	if worker.LastHeartbeatAt.Before(s.now().Add(-s.heartbeatTimeout)) {
		if err := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).Update("status", "offline").Error; err != nil {
			return persistence.WorkerInstance{}, problem.Wrap(500, "worker_offline_update_failed", "Failed to mark the stale worker offline.", err)
		}
		if err := enqueueWorkerOffline(ctx, tx, worker); err != nil {
			return persistence.WorkerInstance{}, problem.Wrap(500, "worker_offline_outbox_failed", "Failed to queue the offline Worker event.", err)
		}
		return persistence.WorkerInstance{}, problem.New(409, "worker_heartbeat_stale", "Worker heartbeat is stale; send a heartbeat before claiming work.")
	}
	return worker, nil
}

func (s *Service) enqueueRecovery(ctx context.Context, tx *gorm.DB, execution persistence.AgentExecution) error {
	tenantID := execution.TenantID
	messageKey := execution.ID.String() + ":" + formatGeneration(execution.Generation)
	err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
		TenantID: &tenantID, Topic: "execution.recovering", MessageKey: messageKey,
		Payload: map[string]any{
			"executionId": execution.ID, "tenantId": execution.TenantID,
			"sessionId": execution.SessionID, "turnId": execution.TurnID,
			"generation": execution.Generation, "executionTargetId": execution.ExecutionTargetID,
			"targetKind": execution.TargetKind,
		},
		Headers: map[string]any{"eventVersion": 1},
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil
	}
	if err != nil {
		return problem.Wrap(500, "execution_recovery_outbox_failed", "Failed to queue the recovering execution.", err)
	}
	return nil
}

func (s *Service) storeProviderCursor(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	cursor *string,
) error {
	if cursor == nil || strings.TrimSpace(*cursor) == "" {
		return nil
	}
	if len(*cursor) > 1_000_000 {
		return problem.New(400, "provider_cursor_too_large", "providerResumeCursor must not exceed 1000000 characters.")
	}
	encrypted, err := s.cursorCipher.Encrypt(*cursor)
	if err != nil {
		return problem.Wrap(503, "provider_cursor_encryption_unavailable", "Provider resume cursor encryption is not configured.", err)
	}
	if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
		Update("provider_resume_cursor_encrypted", encrypted).Error; err != nil {
		return problem.Wrap(500, "provider_cursor_store_failed", "Failed to store the encrypted provider resume cursor.", err)
	}
	if execution.ProviderRuntimeBindingID != nil {
		now := s.now()
		updates := map[string]any{
			"cursor_updated_at": now, "resume_strategy": "native-cursor",
			"last_execution_id": execution.ID, "last_generation": execution.Generation,
			"updated_at": now,
		}
		var binding persistence.ProviderRuntimeBinding
		if err := tx.WithContext(ctx).
			Select("capability_descriptor_hash").
			Where("tenant_id = ? AND id = ?", execution.TenantID, *execution.ProviderRuntimeBindingID).
			Take(&binding).Error; err != nil {
			return problem.Wrap(500, "runtime_binding_cursor_load_failed", "Failed to load the Provider runtime binding for Cursor persistence.", err)
		}
		if binding.CapabilityDescriptorHash != nil {
			updates["cursor_compatibility_key"] = *binding.CapabilityDescriptorHash
		}
		if err := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
			Where("tenant_id = ? AND id = ?", execution.TenantID, *execution.ProviderRuntimeBindingID).
			Updates(updates).Error; err != nil {
			return problem.Wrap(500, "runtime_binding_cursor_store_failed", "Failed to update Provider Cursor compatibility metadata.", err)
		}
	}
	return nil
}

func (s *Service) loadProviderCursor(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (*string, error) {
	if execution.ProviderRuntimeBindingID != nil {
		var binding persistence.ProviderRuntimeBinding
		if err := tx.WithContext(ctx).
			Select("status", "resume_strategy", "cursor_compatibility_key", "capability_descriptor_hash").
			Where("tenant_id = ? AND id = ?", execution.TenantID, *execution.ProviderRuntimeBindingID).
			Take(&binding).Error; err != nil {
			return nil, problem.Wrap(500, "runtime_binding_cursor_load_failed", "Failed to load Provider Cursor compatibility metadata.", err)
		}
		if binding.Status == "incompatible" || binding.Status == "released" || binding.ResumeStrategy == "none" {
			return nil, nil
		}
		if binding.CapabilityDescriptorHash != nil &&
			(binding.CursorCompatibilityKey == nil || *binding.CursorCompatibilityKey != *binding.CapabilityDescriptorHash) {
			return nil, nil
		}
	}
	var session persistence.AgentSession
	if err := tx.WithContext(ctx).
		Select("provider_resume_cursor_encrypted").
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
		Take(&session).Error; err != nil {
		return nil, problem.Wrap(500, "provider_cursor_load_failed", "Failed to load the provider resume cursor.", err)
	}
	if len(session.ProviderResumeCursorEncrypted) == 0 {
		return nil, nil
	}
	plain, err := s.cursorCipher.Decrypt(session.ProviderResumeCursorEncrypted)
	if err != nil {
		return nil, problem.Wrap(503, "provider_cursor_decryption_unavailable", "Provider resume cursor decryption is unavailable.", err)
	}
	return &plain, nil
}

func normalizeClaimTarget(worker persistence.WorkerInstance, input ClaimExecutionInput) (ClaimExecutionInput, error) {
	if input.ExecutionID != nil && *input.ExecutionID == uuid.Nil {
		return ClaimExecutionInput{}, problem.New(400, "invalid_execution_id", "executionId must be a UUID when provided.")
	}
	if input.ExecutionTargetID == uuid.Nil {
		input.ExecutionTargetID = worker.ExecutionTargetID
	}
	if strings.TrimSpace(input.TargetKind) == "" {
		input.TargetKind = worker.TargetKind
	}
	kind, err := platform.ParseExecutionTargetKind(input.TargetKind)
	if err != nil {
		return ClaimExecutionInput{}, problem.New(400, "invalid_execution_target_kind", err.Error()+".")
	}
	input.TargetKind = string(kind)
	if input.ExecutionTargetID != worker.ExecutionTargetID || input.TargetKind != worker.TargetKind {
		return ClaimExecutionInput{}, problem.New(409, "worker_execution_target_mismatch", "A worker can claim only executions for its registered execution target.")
	}
	return input, nil
}

func formatGeneration(value int64) string {
	return strconv.FormatInt(value, 10)
}
