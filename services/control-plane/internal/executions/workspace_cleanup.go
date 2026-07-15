package executions

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

const (
	workspaceLayoutVersionCurrent = 3
	workspaceCleanupMaxAttempts   = 10
)

var activeWorkspaceCleanupStatuses = []string{"pending", "leased", "running"}

type WorkspaceCleanupClaimResult struct {
	Cleanup *WorkspaceCleanupClaim `json:"cleanup"`
}

func (s *Service) ReconcileWorkspaceCleanup(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	if now.IsZero() {
		now = s.now()
	}
	if _, err := s.RecoverExpiredWorkspaceCleanupLeases(ctx, now, limit); err != nil {
		return 0, err
	}

	var candidateIDs []uuid.UUID
	if err := s.db.WithContext(ctx).Table("workspace_materializations AS materialization").
		Select("materialization.id").
		Joins("JOIN remote_workspaces AS workspace ON workspace.tenant_id = materialization.tenant_id AND workspace.id = materialization.workspace_id").
		Where("materialization.state IN ?", []string{"active", "retired", "cleanup-pending"}).
		Where(`(
			materialization.cleanup_requested_at IS NOT NULL
			OR (workspace.retention_until IS NOT NULL AND workspace.retention_until <= ?)
		)`, now).
		Where(`NOT EXISTS (
			SELECT 1 FROM workspace_cleanup_commands AS cleanup
			WHERE cleanup.tenant_id = materialization.tenant_id
			  AND cleanup.materialization_id = materialization.id
			  AND cleanup.status IN ('pending', 'leased', 'running')
		)`).
		Order("COALESCE(materialization.cleanup_requested_at, workspace.retention_until), materialization.updated_at, materialization.id").
		Limit(limit).Scan(&candidateIDs).Error; err != nil {
		return 0, problem.Wrap(500, "workspace_cleanup_candidates_load_failed", "Failed to load eligible Workspace cleanup intents.", err)
	}

	createdCount := 0
	for _, candidateID := range candidateIDs {
		created, err := s.enqueueWorkspaceCleanup(ctx, candidateID, now)
		if err != nil {
			return createdCount, err
		}
		if created {
			createdCount++
		}
	}
	return createdCount, nil
}

func (s *Service) enqueueWorkspaceCleanup(ctx context.Context, materializationID uuid.UUID, now time.Time) (bool, error) {
	created := false
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		materialization, workspace, err := lockWorkspaceCleanupScope(ctx, tx, materializationID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return problem.Wrap(500, "workspace_cleanup_scope_lock_failed", "Failed to lock the Workspace cleanup scope.", err)
		}
		if !workspaceCleanupIntentDue(materialization, workspace, now) {
			return nil
		}
		if blocker, err := workspaceCleanupBlocker(ctx, tx, materialization, workspace, now, uuid.Nil); err != nil {
			return err
		} else if blocker != "" {
			return nil
		}

		var activeCount int64
		if err := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
			Where("tenant_id = ? AND materialization_id = ? AND status IN ?",
				materialization.TenantID, materialization.ID, activeWorkspaceCleanupStatuses).
			Count(&activeCount).Error; err != nil {
			return problem.Wrap(500, "workspace_cleanup_command_check_failed", "Failed to inspect existing Workspace cleanup commands.", err)
		}
		if activeCount != 0 {
			return nil
		}

		reason := "retention-expired"
		requestedAt := now
		if materialization.CleanupReason != nil {
			reason = *materialization.CleanupReason
		}
		if materialization.CleanupRequestedAt != nil {
			requestedAt = *materialization.CleanupRequestedAt
		}
		command := persistence.WorkspaceCleanupCommand{
			ID: uuid.New(), TenantID: materialization.TenantID,
			MaterializationID: materialization.ID, MaterializationIncarnationID: materialization.IncarnationID,
			WorkspaceID: materialization.WorkspaceID, ExecutionTargetID: materialization.ExecutionTargetID,
			TargetKind: materialization.TargetKind, StorageScope: materialization.StorageScope,
			LayoutVersion: materialization.LayoutVersion, Reason: reason, Status: "pending",
			DeliveryAvailableAt: now, RequestedAt: requestedAt, CreatedAt: now, UpdatedAt: now,
		}
		if workspacePodPlacementUnknown(materialization) && !workspacePodNeverMaterialized(materialization) {
			code := "workspace_cleanup_placement_unknown"
			message := "The Pod-scoped Workspace has no durable Worker instance identity; cleanup requires manual reconciliation."
			command.Status = "failed"
			command.FailedAt = &now
			command.LastErrorCode = &code
			command.LastErrorMessage = &message
		}
		if err := tx.WithContext(ctx).Create(&command).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return nil
			}
			return problem.Wrap(500, "workspace_cleanup_command_create_failed", "Failed to create the Workspace cleanup command.", err)
		}
		updates := map[string]any{
			"state": "cleanup-pending", "cleanup_reason": reason, "cleanup_requested_at": requestedAt,
			"failure_code": nil, "failure_message": nil, "failed_at": nil, "updated_at": now,
		}
		if command.Status == "failed" {
			updates["state"] = "failed"
			updates["failure_code"] = *command.LastErrorCode
			updates["failure_message"] = *command.LastErrorMessage
			updates["failed_at"] = now
		}
		updated := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND id = ? AND incarnation_id = ? AND state IN ?",
				materialization.TenantID, materialization.ID, materialization.IncarnationID,
				[]string{"active", "retired", "cleanup-pending"}).Updates(updates)
		if err := expectOne(updated, 409, "workspace_cleanup_enqueue_conflict", "The Workspace materialization changed while cleanup was being queued."); err != nil {
			return err
		}
		if workspace.CurrentMaterializationID != nil && *workspace.CurrentMaterializationID == materialization.ID && workspace.State != "cleaned" {
			logicalState := "cleanup-pending"
			if command.Status == "failed" {
				logicalState = "failed"
			}
			if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
				Where("tenant_id = ? AND id = ? AND current_materialization_id = ?", workspace.TenantID, workspace.ID, materialization.ID).
				Updates(map[string]any{"state": logicalState, "updated_at": now, "cleaned_at": nil}).Error; err != nil {
				return problem.Wrap(500, "workspace_cleanup_logical_state_failed", "Failed to update the logical Workspace cleanup state.", err)
			}
		}
		if workspacePodNeverMaterialized(materialization) {
			if err := acknowledgeWorkspaceCleanup(ctx, tx, command, now); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	return created, err
}

func (s *Service) RecoverExpiredWorkspaceCleanupLeases(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	recovered := 0
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		commands := make([]persistence.WorkspaceCleanupCommand, 0, limit)
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "SKIP LOCKED").
			Where("status IN ? AND lease_expires_at <= ?", []string{"leased", "running"}, now).
			Order("lease_expires_at, id").Limit(limit).Find(&commands).Error; err != nil {
			return err
		}
		for _, command := range commands {
			wasRecovered, err := s.recoverWorkspaceCleanupCommandLocked(
				ctx, tx, command, now,
				"workspace_cleanup_lease_expired",
				"The Workspace cleanup lease expired before acknowledgement.",
			)
			if err != nil {
				return err
			}
			if wasRecovered {
				recovered++
			}
		}
		return nil
	})
	if err != nil {
		return 0, problem.Wrap(500, "workspace_cleanup_lease_recovery_failed", "Failed to recover expired Workspace cleanup leases.", err)
	}
	return recovered, nil
}

func (s *Service) recoverWorkspaceCleanupCommandLocked(
	ctx context.Context,
	tx *gorm.DB,
	command persistence.WorkspaceCleanupCommand,
	now time.Time,
	code, message string,
) (bool, error) {
	if command.DeliveryWorkerID == nil || command.DeliveryWorkerIncarnation == nil {
		return false, nil
	}
	status := "pending"
	materializationState := "cleanup-pending"
	var failedAt any
	if command.DeliveryAttempts >= workspaceCleanupMaxAttempts {
		status = "failed"
		materializationState = "failed"
		code = "workspace_cleanup_attempts_exhausted"
		message = "Workspace cleanup exhausted its delivery attempt limit. " + message
		failedAt = now
	}
	updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
		Where(
			"id = ? AND status IN ? AND dispatch_generation = ? AND delivery_worker_id = ? AND delivery_worker_incarnation = ?",
			command.ID, []string{"leased", "running"}, command.DispatchGeneration,
			*command.DeliveryWorkerID, *command.DeliveryWorkerIncarnation,
		).
		Updates(map[string]any{
			"status": status, "lease_token_hash": nil,
			"delivery_worker_id": nil, "delivery_worker_incarnation": nil,
			"delivery_available_at": now, "lease_expires_at": nil,
			"failed_at": failedAt, "last_error_code": code, "last_error_message": message, "updated_at": now,
		})
	if updated.Error != nil {
		return false, updated.Error
	}
	if updated.RowsAffected != 1 {
		return false, nil
	}
	materialization, _, err := lockWorkspaceCleanupScope(ctx, tx, command.MaterializationID)
	if err != nil || materialization.IncarnationID != command.MaterializationIncarnationID {
		return false, problem.New(409, "workspace_cleanup_scope_fenced", "The Workspace cleanup scope changed while its lease was recovered.")
	}
	materializationUpdates := map[string]any{"state": materializationState, "updated_at": now}
	if status == "failed" {
		materializationUpdates["failure_code"] = code
		materializationUpdates["failure_message"] = message
		materializationUpdates["failed_at"] = now
	}
	if err := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
		Where("tenant_id = ? AND id = ? AND incarnation_id = ? AND state IN ?",
			command.TenantID, command.MaterializationID, command.MaterializationIncarnationID,
			[]string{"cleanup-pending", "cleaning"}).
		Updates(materializationUpdates).Error; err != nil {
		return false, err
	}
	if status == "failed" {
		if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND current_materialization_id = ?",
				command.TenantID, command.WorkspaceID, command.MaterializationID).
			Updates(map[string]any{"state": "failed", "updated_at": now, "cleaned_at": nil}).Error; err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *Service) ClaimWorkspaceCleanup(
	ctx context.Context,
	worker persistence.WorkerInstance,
	input WorkspaceCleanupClaimInput,
	requestID string,
) (OperationResult[WorkspaceCleanupClaimResult], error) {
	targetID, targetKind, err := normalizeWorkspaceCleanupTarget(worker, input)
	if err != nil {
		return OperationResult[WorkspaceCleanupClaimResult]{}, err
	}
	if _, err := s.RecoverExpiredWorkspaceCleanupLeases(ctx, s.now(), 100); err != nil {
		return OperationResult[WorkspaceCleanupClaimResult]{}, err
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 160 {
		return OperationResult[WorkspaceCleanupClaimResult]{}, problem.New(400, "invalid_request_id", "X-Request-ID must contain between 1 and 160 characters.")
	}
	fingerprint := WorkspaceCleanupClaimInput{ExecutionTargetID: targetID, TargetKind: targetKind}
	hash, err := requestHash("workspace_cleanup.claim", fingerprint)
	if err != nil {
		return OperationResult[WorkspaceCleanupClaimResult]{}, problem.Wrap(500, "request_fingerprint_failed", "Failed to fingerprint the Workspace cleanup claim.", err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		result := WorkspaceCleanupClaimResult{}
		replayed := false
		err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
			var receipt persistence.WorkerRequestReceipt
			lookupErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("worker_id = ? AND request_id = ?", worker.ID, requestID).Take(&receipt).Error
			if err := lockCurrentWorkerIncarnation(ctx, tx, worker); err != nil {
				return err
			}
			if lookupErr == nil {
				if receipt.WorkerIncarnation == worker.Incarnation && receipt.ExpiresAt.After(s.now()) {
					if receipt.Operation != "workspace_cleanup.claim" || receipt.RequestHash != hash {
						return problem.New(409, "request_id_reused", "X-Request-ID was already used for a different worker request.")
					}
					stored, decodeErr := decodeReceiptResponse[WorkspaceCleanupClaimResult](receipt)
					if decodeErr != nil {
						return problem.Wrap(500, "receipt_decode_failed", "Failed to restore the Workspace cleanup claim response.", decodeErr)
					}
					if stored.Cleanup == nil {
						result = stored
						replayed = true
						return nil
					}
					command, commandErr := s.lockWorkspaceCleanupLease(ctx, tx, worker, stored.Cleanup.CleanupID, WorkspaceCleanupLeaseInput{
						DispatchGeneration: stored.Cleanup.DispatchGeneration,
					}, false)
					if commandErr != nil {
						return problem.New(409, "workspace_cleanup_claim_replay_fenced", "The Workspace cleanup returned by this claim is no longer leased to the Worker.")
					}
					plainToken, tokenHash, tokenErr := secret.NewToken()
					if tokenErr != nil {
						return problem.Wrap(500, "workspace_cleanup_token_generation_failed", "Failed to rotate the Workspace cleanup lease token.", tokenErr)
					}
					now := s.now()
					expiresAt := now.Add(s.leaseTTL)
					rotated := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
						Where("id = ? AND status = ? AND delivery_worker_id = ? AND delivery_worker_incarnation = ? AND dispatch_generation = ?",
							command.ID, "leased", worker.ID, worker.Incarnation, command.DispatchGeneration).
						Updates(map[string]any{"lease_token_hash": tokenHash, "lease_expires_at": expiresAt, "updated_at": now})
					if err := expectOne(rotated, 409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current."); err != nil {
						return err
					}
					claim, err := loadWorkspaceCleanupClaim(ctx, tx, command, plainToken, expiresAt)
					if err != nil {
						return err
					}
					result.Cleanup = &claim
					replayed = true
					return nil
				}
				if err := tx.WithContext(ctx).Delete(&receipt).Error; err != nil {
					return problem.Wrap(500, "receipt_expiry_cleanup_failed", "Failed to replace an expired Worker receipt.", err)
				}
			} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
				return problem.Wrap(500, "receipt_lookup_failed", "Failed to inspect the Worker request receipt.", lookupErr)
			}
			claimWorker, err := s.requireClaimableWorker(ctx, tx, worker.ID)
			if err != nil {
				return err
			}
			if claimWorker.Incarnation != worker.Incarnation || claimWorker.InstanceUID != worker.InstanceUID {
				return problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
			}

			var command persistence.WorkspaceCleanupCommand
			claimErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "SKIP LOCKED").
				Table("workspace_cleanup_commands AS cleanup").
				Select("cleanup.*").
				Joins("JOIN workspace_materializations AS materialization ON materialization.tenant_id = cleanup.tenant_id AND materialization.id = cleanup.materialization_id").
				Where("cleanup.execution_target_id = ? AND cleanup.target_kind = ?", targetID, targetKind).
				Where("cleanup.status = ? AND cleanup.delivery_available_at <= ?", "pending", s.now()).
				Where("cleanup.delivery_attempts < ?", workspaceCleanupMaxAttempts).
				Where("cleanup.storage_scope = ?", "target").
				Where("materialization.state = ?", "cleanup-pending").
				Order("cleanup.delivery_available_at, cleanup.requested_at, cleanup.id").Take(&command).Error
			if errors.Is(claimErr, gorm.ErrRecordNotFound) {
				result.Cleanup = nil
			} else if claimErr != nil {
				return problem.Wrap(500, "workspace_cleanup_claim_failed", "Failed to claim a Workspace cleanup command.", claimErr)
			} else {
				plainToken, tokenHash, tokenErr := secret.NewToken()
				if tokenErr != nil {
					return problem.Wrap(500, "workspace_cleanup_token_generation_failed", "Failed to create the Workspace cleanup lease token.", tokenErr)
				}
				now := s.now()
				expiresAt := now.Add(s.leaseTTL)
				command.Status = "leased"
				command.DispatchGeneration++
				command.DeliveryAttempts++
				command.LeaseTokenHash = tokenHash
				command.DeliveryWorkerID = &worker.ID
				command.DeliveryWorkerIncarnation = &worker.Incarnation
				command.LeasedAt = &now
				command.LeaseExpiresAt = &expiresAt
				command.StartedAt = nil
				command.UpdatedAt = now
				updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
					Where("id = ? AND status = ? AND dispatch_generation = ?", command.ID, "pending", command.DispatchGeneration-1).
					Updates(map[string]any{
						"status": command.Status, "dispatch_generation": command.DispatchGeneration,
						"delivery_attempts": command.DeliveryAttempts, "lease_token_hash": command.LeaseTokenHash,
						"delivery_worker_id": worker.ID, "delivery_worker_incarnation": worker.Incarnation,
						"leased_at": now, "started_at": nil, "lease_expires_at": expiresAt, "updated_at": now,
					})
				if err := expectOne(updated, 409, "workspace_cleanup_claim_conflict", "The Workspace cleanup command was claimed concurrently."); err != nil {
					return err
				}
				claim, err := loadWorkspaceCleanupClaim(ctx, tx, command, plainToken, expiresAt)
				if err != nil {
					return err
				}
				result.Cleanup = &claim
			}

			stored := result
			if stored.Cleanup != nil {
				copy := *stored.Cleanup
				copy.Lease.LeaseToken = ""
				stored.Cleanup = &copy
			}
			mapped, mapErr := responseMap(stored)
			if mapErr != nil {
				return problem.Wrap(500, "receipt_encode_failed", "Failed to persist the Workspace cleanup claim response.", mapErr)
			}
			now := s.now()
			receipt = persistence.WorkerRequestReceipt{
				WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, RequestID: requestID,
				Operation: "workspace_cleanup.claim", RequestHash: hash, StatusCode: 200,
				Response: mapped, CreatedAt: now, ExpiresAt: now.Add(s.receiptTTL),
			}
			if err := tx.WithContext(ctx).Create(&receipt).Error; err != nil {
				if errors.Is(err, gorm.ErrDuplicatedKey) {
					return errReceiptRace
				}
				return problem.Wrap(500, "receipt_create_failed", "Failed to persist the Workspace cleanup claim receipt.", err)
			}
			return nil
		})
		if errors.Is(err, errReceiptRace) {
			continue
		}
		if err != nil {
			return OperationResult[WorkspaceCleanupClaimResult]{}, err
		}
		return OperationResult[WorkspaceCleanupClaimResult]{Value: result, Replayed: replayed, StatusCode: 200}, nil
	}
	return OperationResult[WorkspaceCleanupClaimResult]{}, problem.New(409, "request_receipt_conflict", "The Workspace cleanup claim is still being committed; retry with the same X-Request-ID.")
}

func (s *Service) RenewWorkspaceCleanup(
	ctx context.Context,
	worker persistence.WorkerInstance,
	cleanupID uuid.UUID,
	input WorkspaceCleanupLeaseInput,
	requestID string,
) (OperationResult[WorkspaceCleanupLease], error) {
	return runIdempotent(ctx, s, worker, requestID, "workspace_cleanup.renew", struct {
		CleanupID uuid.UUID `json:"cleanupId"`
		WorkspaceCleanupLeaseInput
	}{cleanupID, input}, 200, func(tx *gorm.DB) (WorkspaceCleanupLease, error) {
		command, err := s.lockWorkspaceCleanupLease(ctx, tx, worker, cleanupID, input, true)
		if err != nil {
			return WorkspaceCleanupLease{}, err
		}
		now := s.now()
		expiresAt := now.Add(s.leaseTTL)
		updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
			Where("id = ? AND dispatch_generation = ? AND delivery_worker_id = ? AND delivery_worker_incarnation = ? AND status IN ?",
				command.ID, input.DispatchGeneration, worker.ID, worker.Incarnation, []string{"leased", "running"}).
			Updates(map[string]any{"lease_expires_at": expiresAt, "updated_at": now})
		if err := expectOne(updated, 409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current."); err != nil {
			return WorkspaceCleanupLease{}, err
		}
		return WorkspaceCleanupLease{
			CleanupID: command.ID, DispatchGeneration: command.DispatchGeneration,
			ExpiresAt: expiresAt,
		}, nil
	})
}

func (s *Service) StartWorkspaceCleanup(
	ctx context.Context,
	worker persistence.WorkerInstance,
	cleanupID uuid.UUID,
	input WorkspaceCleanupLeaseInput,
	requestID string,
) (OperationResult[WorkspaceCleanupState], error) {
	return runIdempotent(ctx, s, worker, requestID, "workspace_cleanup.started", struct {
		CleanupID uuid.UUID `json:"cleanupId"`
		WorkspaceCleanupLeaseInput
	}{cleanupID, input}, 200, func(tx *gorm.DB) (WorkspaceCleanupState, error) {
		command, err := s.lockWorkspaceCleanupLease(ctx, tx, worker, cleanupID, input, true)
		if err != nil {
			return WorkspaceCleanupState{}, err
		}
		if command.Status == "running" {
			return toWorkspaceCleanupState(command), nil
		}
		materialization, workspace, err := lockWorkspaceCleanupScope(ctx, tx, command.MaterializationID)
		if err != nil {
			return WorkspaceCleanupState{}, problem.Wrap(409, "workspace_cleanup_scope_fenced", "The Workspace cleanup scope is no longer available.", err)
		}
		if materialization.IncarnationID != command.MaterializationIncarnationID {
			return WorkspaceCleanupState{}, problem.New(409, "workspace_cleanup_scope_fenced", "The Workspace materialization incarnation changed.")
		}
		if blocker, err := workspaceCleanupBlocker(ctx, tx, materialization, workspace, s.now(), command.ID); err != nil {
			return WorkspaceCleanupState{}, err
		} else if blocker != "" {
			err := problem.New(409, "workspace_cleanup_blocked", "The Workspace cleanup is blocked by active or incomplete runtime state.")
			err.Details = map[string]any{"blocker": blocker}
			return WorkspaceCleanupState{}, err
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
			Where("id = ? AND status = ? AND dispatch_generation = ? AND delivery_worker_id = ? AND delivery_worker_incarnation = ?",
				command.ID, "leased", input.DispatchGeneration, worker.ID, worker.Incarnation).
			Updates(map[string]any{"status": "running", "started_at": now, "updated_at": now})
		if err := expectOne(updated, 409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current."); err != nil {
			return WorkspaceCleanupState{}, err
		}
		materializationUpdate := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND id = ? AND incarnation_id = ? AND state = ?",
				command.TenantID, command.MaterializationID, command.MaterializationIncarnationID, "cleanup-pending").
			Updates(map[string]any{"state": "cleaning", "updated_at": now})
		if err := expectOne(materializationUpdate, 409, "workspace_cleanup_materialization_fenced", "The Workspace materialization is no longer cleanup-pending."); err != nil {
			return WorkspaceCleanupState{}, err
		}
		command.Status = "running"
		command.StartedAt = &now
		command.UpdatedAt = now
		return toWorkspaceCleanupState(command), nil
	})
}

func (s *Service) AcknowledgeWorkspaceCleanup(
	ctx context.Context,
	worker persistence.WorkerInstance,
	cleanupID uuid.UUID,
	input WorkspaceCleanupLeaseInput,
	requestID string,
) (OperationResult[WorkspaceCleanupState], error) {
	return runIdempotent(ctx, s, worker, requestID, "workspace_cleanup.acknowledged", struct {
		CleanupID uuid.UUID `json:"cleanupId"`
		WorkspaceCleanupLeaseInput
	}{cleanupID, input}, 200, func(tx *gorm.DB) (WorkspaceCleanupState, error) {
		command, err := s.lockWorkspaceCleanupLease(ctx, tx, worker, cleanupID, input, true)
		if err != nil {
			return WorkspaceCleanupState{}, err
		}
		if command.Status != "running" {
			return WorkspaceCleanupState{}, problem.New(409, "workspace_cleanup_not_started", "Workspace cleanup must be marked started before acknowledgement.")
		}
		materialization, _, err := lockWorkspaceCleanupScope(ctx, tx, command.MaterializationID)
		if err != nil || materialization.IncarnationID != command.MaterializationIncarnationID {
			return WorkspaceCleanupState{}, problem.New(409, "workspace_cleanup_scope_fenced", "The Workspace cleanup scope is no longer current.")
		}
		now := s.now()
		if err := acknowledgeWorkspaceCleanup(ctx, tx, command, now); err != nil {
			return WorkspaceCleanupState{}, err
		}
		command.Status = "acknowledged"
		command.LeaseTokenHash = nil
		command.DeliveryWorkerID = nil
		command.DeliveryWorkerIncarnation = nil
		command.LeaseExpiresAt = nil
		command.AcknowledgedAt = &now
		command.UpdatedAt = now
		return toWorkspaceCleanupState(command), nil
	})
}

func (s *Service) FailWorkspaceCleanup(
	ctx context.Context,
	worker persistence.WorkerInstance,
	cleanupID uuid.UUID,
	input WorkspaceCleanupFailedInput,
	requestID string,
) (OperationResult[WorkspaceCleanupState], error) {
	code := strings.TrimSpace(input.ErrorCode)
	message := strings.TrimSpace(input.ErrorMessage)
	if code == "" || len(code) > 160 || len(message) > 10000 {
		return OperationResult[WorkspaceCleanupState]{}, problem.New(400, "invalid_workspace_cleanup_failure", "errorCode is required and failure metadata is too large.")
	}
	return runIdempotent(ctx, s, worker, requestID, "workspace_cleanup.failed", struct {
		CleanupID uuid.UUID                   `json:"cleanupId"`
		Input     WorkspaceCleanupFailedInput `json:"input"`
	}{cleanupID, input}, 200, func(tx *gorm.DB) (WorkspaceCleanupState, error) {
		command, err := s.lockWorkspaceCleanupLease(ctx, tx, worker, cleanupID, input.WorkspaceCleanupLeaseInput, true)
		if err != nil {
			return WorkspaceCleanupState{}, err
		}
		materialization, _, err := lockWorkspaceCleanupScope(ctx, tx, command.MaterializationID)
		if err != nil || materialization.IncarnationID != command.MaterializationIncarnationID {
			return WorkspaceCleanupState{}, problem.New(409, "workspace_cleanup_scope_fenced", "The Workspace cleanup scope is no longer current.")
		}
		now := s.now()
		retry := input.Retryable && command.DeliveryAttempts < workspaceCleanupMaxAttempts
		status := "failed"
		availableAt := command.DeliveryAvailableAt
		materializationState := "failed"
		if retry {
			status = "pending"
			materializationState = "cleanup-pending"
			availableAt = now.Add(workspaceCleanupBackoff(command.DeliveryAttempts))
		}
		updates := map[string]any{
			"status": status, "lease_token_hash": nil, "delivery_worker_id": nil,
			"delivery_worker_incarnation": nil, "lease_expires_at": nil,
			"delivery_available_at": availableAt, "last_error_code": code,
			"last_error_message": message, "updated_at": now,
		}
		if retry {
			updates["failed_at"] = nil
		} else {
			updates["failed_at"] = now
		}
		updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
			Where("id = ? AND status IN ? AND dispatch_generation = ? AND delivery_worker_id = ? AND delivery_worker_incarnation = ?",
				command.ID, []string{"leased", "running"}, input.DispatchGeneration, worker.ID, worker.Incarnation).
			Updates(updates)
		if err := expectOne(updated, 409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current."); err != nil {
			return WorkspaceCleanupState{}, err
		}
		materializationUpdates := map[string]any{"state": materializationState, "updated_at": now}
		if retry {
			materializationUpdates["failure_code"] = nil
			materializationUpdates["failure_message"] = nil
			materializationUpdates["failed_at"] = nil
		} else {
			materializationUpdates["failure_code"] = code
			materializationUpdates["failure_message"] = message
			materializationUpdates["failed_at"] = now
		}
		materializationUpdate := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND id = ? AND incarnation_id = ? AND state IN ?",
				command.TenantID, command.MaterializationID, command.MaterializationIncarnationID,
				[]string{"cleanup-pending", "cleaning"}).Updates(materializationUpdates)
		if err := expectOne(materializationUpdate, 409, "workspace_cleanup_materialization_fenced", "The Workspace materialization changed while cleanup failure was recorded."); err != nil {
			return WorkspaceCleanupState{}, err
		}
		if !retry {
			if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
				Where("tenant_id = ? AND id = ? AND current_materialization_id = ?",
					command.TenantID, command.WorkspaceID, command.MaterializationID).
				Updates(map[string]any{"state": "failed", "updated_at": now, "cleaned_at": nil}).Error; err != nil {
				return WorkspaceCleanupState{}, problem.Wrap(500, "workspace_cleanup_logical_failure_failed", "Failed to record the logical Workspace cleanup failure.", err)
			}
		}
		command.Status = status
		command.LeaseTokenHash = nil
		command.DeliveryWorkerID = nil
		command.DeliveryWorkerIncarnation = nil
		command.LeaseExpiresAt = nil
		command.DeliveryAvailableAt = availableAt
		command.LastErrorCode = &code
		command.LastErrorMessage = &message
		command.UpdatedAt = now
		if !retry {
			command.FailedAt = &now
		}
		return toWorkspaceCleanupState(command), nil
	})
}

func (s *Service) ReleaseWorkspaceCleanup(
	ctx context.Context,
	worker persistence.WorkerInstance,
	cleanupID uuid.UUID,
	input WorkspaceCleanupLeaseInput,
	requestID string,
) (OperationResult[WorkspaceCleanupState], error) {
	return runIdempotent(ctx, s, worker, requestID, "workspace_cleanup.release", struct {
		CleanupID uuid.UUID `json:"cleanupId"`
		WorkspaceCleanupLeaseInput
	}{cleanupID, input}, 200, func(tx *gorm.DB) (WorkspaceCleanupState, error) {
		command, err := s.lockWorkspaceCleanupLease(ctx, tx, worker, cleanupID, input, true)
		if err != nil {
			return WorkspaceCleanupState{}, err
		}
		if command.Status != "leased" {
			return WorkspaceCleanupState{}, problem.New(409, "workspace_cleanup_release_after_start", "A started Workspace cleanup must be acknowledged or failed, not released.")
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
			Where("id = ? AND status = ? AND dispatch_generation = ? AND delivery_worker_id = ? AND delivery_worker_incarnation = ?",
				command.ID, "leased", input.DispatchGeneration, worker.ID, worker.Incarnation).
			Updates(map[string]any{
				"status": "pending", "lease_token_hash": nil, "delivery_worker_id": nil,
				"delivery_worker_incarnation": nil, "lease_expires_at": nil,
				"delivery_available_at": now, "updated_at": now,
			})
		if err := expectOne(updated, 409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current."); err != nil {
			return WorkspaceCleanupState{}, err
		}
		command.Status = "pending"
		command.LeaseTokenHash = nil
		command.DeliveryWorkerID = nil
		command.DeliveryWorkerIncarnation = nil
		command.LeaseExpiresAt = nil
		command.DeliveryAvailableAt = now
		command.UpdatedAt = now
		return toWorkspaceCleanupState(command), nil
	})
}

func (s *Service) ReconcileEphemeralWorkspaceCleanup(
	ctx context.Context,
	executionTargetID uuid.UUID,
	presentInstanceUIDs []string,
	confirmedAt time.Time,
) (int, error) {
	if executionTargetID == uuid.Nil {
		return 0, problem.New(400, "invalid_ephemeral_workspace_cleanup", "executionTargetId is required.")
	}
	if confirmedAt.IsZero() {
		confirmedAt = s.now()
	}
	present := make(map[string]struct{}, len(presentInstanceUIDs))
	for _, value := range presentInstanceUIDs {
		value = strings.TrimSpace(value)
		if value == "" {
			return 0, problem.New(400, "invalid_ephemeral_workspace_cleanup", "presentInstanceUids must not contain an empty value.")
		}
		present[value] = struct{}{}
	}
	var commandIDs []uuid.UUID
	if err := s.db.WithContext(ctx).Table("workspace_cleanup_commands AS cleanup").
		Select("cleanup.id").
		Joins("JOIN workspace_materializations AS materialization ON materialization.tenant_id = cleanup.tenant_id AND materialization.id = cleanup.materialization_id").
		Where("cleanup.execution_target_id = ? AND cleanup.storage_scope = ?", executionTargetID, "pod").
		Where("materialization.worker_instance_uid IS NOT NULL").
		Where("cleanup.status IN ?", activeWorkspaceCleanupStatuses).
		Order("cleanup.requested_at, cleanup.id").Scan(&commandIDs).Error; err != nil {
		return 0, problem.Wrap(500, "ephemeral_workspace_cleanup_load_failed", "Failed to load ephemeral Workspace cleanup commands.", err)
	}
	acknowledged := 0
	for _, commandID := range commandIDs {
		changed := false
		err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
			var command persistence.WorkspaceCleanupCommand
			if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("id = ? AND execution_target_id = ? AND storage_scope = ? AND status IN ?",
					commandID, executionTargetID, "pod", activeWorkspaceCleanupStatuses).
				Take(&command).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}
			materialization, workspace, err := lockWorkspaceCleanupScope(ctx, tx, command.MaterializationID)
			if err != nil {
				return err
			}
			if materialization.StorageScope != "pod" || materialization.WorkerInstanceUID == nil ||
				materialization.IncarnationID != command.MaterializationIncarnationID {
				return problem.New(409, "ephemeral_workspace_cleanup_fenced", "The ephemeral Workspace materialization no longer matches the confirmed Pod UID.")
			}
			if _, exists := present[*materialization.WorkerInstanceUID]; exists {
				return nil
			}
			if blocker, err := workspaceCleanupBlocker(ctx, tx, materialization, workspace, confirmedAt, command.ID); err != nil {
				return err
			} else if blocker != "" {
				return nil
			}
			if err := acknowledgeWorkspaceCleanup(ctx, tx, command, confirmedAt); err != nil {
				return err
			}
			changed = true
			return nil
		})
		if err != nil {
			return acknowledged, problem.Wrap(500, "ephemeral_workspace_cleanup_failed", "Failed to acknowledge ephemeral Workspace cleanup.", err)
		}
		if changed {
			acknowledged++
		}
	}
	return acknowledged, nil
}

func ensureExecutionWorkspaceMaterialization(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	execution *persistence.AgentExecution,
	now time.Time,
) (persistence.WorkspaceMaterialization, error) {
	if execution.RemoteWorkspaceID == nil {
		return persistence.WorkspaceMaterialization{}, problem.New(409, "workspace_not_bound", "The Execution does not have a logical Workspace binding.")
	}
	var workspace persistence.RemoteWorkspace
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ? AND session_id = ?", execution.TenantID, *execution.RemoteWorkspaceID, execution.SessionID).
		Take(&workspace).Error; err != nil {
		return persistence.WorkspaceMaterialization{}, problem.Wrap(409, "workspace_not_bound", "The logical Workspace is unavailable for this Execution.", err)
	}

	var current persistence.WorkspaceMaterialization
	if workspace.CurrentMaterializationID != nil {
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ? AND workspace_id = ?", execution.TenantID, *workspace.CurrentMaterializationID, workspace.ID).
			Take(&current).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return persistence.WorkspaceMaterialization{}, problem.Wrap(500, "workspace_materialization_load_failed", "Failed to load the current Workspace materialization.", err)
		}
	}

	reusable := current.ID != uuid.Nil && current.State == "active" &&
		current.ExecutionTargetID == execution.ExecutionTargetID && current.TargetKind == execution.TargetKind
	if reusable && current.StorageScope == "pod" {
		reusable = workspacePodNeverMaterialized(current) ||
			(current.WorkerInstanceUID != nil && *current.WorkerInstanceUID == worker.InstanceUID)
	}
	if reusable {
		updates := map[string]any{
			"worker_id": worker.ID, "worker_incarnation": worker.Incarnation,
			"worker_instance_uid": worker.InstanceUID, "last_execution_id": execution.ID,
			"last_generation": execution.Generation, "updated_at": now,
		}
		updated := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND id = ? AND incarnation_id = ? AND state = ?",
				current.TenantID, current.ID, current.IncarnationID, "active").Updates(updates)
		if err := expectOne(updated, 409, "workspace_materialization_bind_fenced", "The Workspace materialization changed while it was bound to the Worker."); err != nil {
			return persistence.WorkspaceMaterialization{}, err
		}
		current.WorkerID = &worker.ID
		current.WorkerIncarnation = &worker.Incarnation
		current.WorkerInstanceUID = &worker.InstanceUID
		current.LastExecutionID = &execution.ID
		current.LastGeneration = &execution.Generation
		current.UpdatedAt = now
		if execution.WorkspaceMaterializationID == nil || *execution.WorkspaceMaterializationID != current.ID {
			if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND id = ?", execution.TenantID, execution.ID).
				Update("workspace_materialization_id", current.ID).Error; err != nil {
				return persistence.WorkspaceMaterialization{}, problem.Wrap(500, "execution_materialization_bind_failed", "Failed to bind the Execution to its Workspace materialization.", err)
			}
			execution.WorkspaceMaterializationID = &current.ID
		}
		return current, nil
	}
	if current.ID != uuid.Nil && workspace.State == "dirty" {
		if err := requireReadyWorkspaceCheckpoint(ctx, tx, workspace, current); err != nil {
			return persistence.WorkspaceMaterialization{}, err
		}
	}

	if current.ID != uuid.Nil && current.State == "active" {
		reason := "workspace-rematerialized"
		if current.StorageScope == "pod" {
			reason = "worker-instance-replaced"
		}
		retired := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND id = ? AND incarnation_id = ? AND state = ?",
				current.TenantID, current.ID, current.IncarnationID, "active").
			Updates(map[string]any{
				"state": "retired", "cleanup_reason": reason, "cleanup_requested_at": now, "updated_at": now,
			})
		if err := expectOne(retired, 409, "workspace_materialization_retire_fenced", "The previous Workspace materialization changed while it was retired."); err != nil {
			return persistence.WorkspaceMaterialization{}, err
		}
	}

	storageScope := "target"
	if execution.TargetKind == "kubernetes" {
		storageScope = "pod"
	}
	materialization := persistence.WorkspaceMaterialization{
		ID: uuid.New(), TenantID: workspace.TenantID, WorkspaceID: workspace.ID,
		OrganizationID: workspace.OrganizationID, ProjectID: workspace.ProjectID, SessionID: workspace.SessionID,
		ExecutionTargetID: execution.ExecutionTargetID, TargetKind: execution.TargetKind,
		StorageScope: storageScope, LayoutVersion: workspaceLayoutVersionCurrent, IncarnationID: uuid.New(),
		WorkerID: &worker.ID, WorkerIncarnation: &worker.Incarnation, WorkerInstanceUID: &worker.InstanceUID,
		LastExecutionID: &execution.ID, LastGeneration: &execution.Generation, State: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.WithContext(ctx).Create(&materialization).Error; err != nil {
		return persistence.WorkspaceMaterialization{}, problem.Wrap(500, "workspace_materialization_create_failed", "Failed to create a fenced Workspace materialization.", err)
	}
	if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", workspace.TenantID, workspace.ID).
		Update("current_materialization_id", materialization.ID).Error; err != nil {
		return persistence.WorkspaceMaterialization{}, problem.Wrap(500, "workspace_materialization_pointer_failed", "Failed to update the current Workspace materialization.", err)
	}
	if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.ID).
		Update("workspace_materialization_id", materialization.ID).Error; err != nil {
		return persistence.WorkspaceMaterialization{}, problem.Wrap(500, "execution_materialization_bind_failed", "Failed to bind the Execution to its Workspace materialization.", err)
	}
	execution.WorkspaceMaterializationID = &materialization.ID
	return materialization, nil
}

func normalizeWorkspaceCleanupTarget(worker persistence.WorkerInstance, input WorkspaceCleanupClaimInput) (uuid.UUID, string, error) {
	targetID := input.ExecutionTargetID
	if targetID == uuid.Nil {
		targetID = worker.ExecutionTargetID
	}
	targetKind := strings.TrimSpace(input.TargetKind)
	if targetKind == "" {
		targetKind = worker.TargetKind
	}
	if targetID != worker.ExecutionTargetID || targetKind != worker.TargetKind {
		return uuid.Nil, "", problem.New(403, "workspace_cleanup_target_forbidden", "Worker may only claim cleanup commands for its registered Execution Target.")
	}
	return targetID, targetKind, nil
}

func (s *Service) lockWorkspaceCleanupLease(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	cleanupID uuid.UUID,
	input WorkspaceCleanupLeaseInput,
	verifyToken bool,
) (persistence.WorkspaceCleanupCommand, error) {
	if cleanupID == uuid.Nil || input.DispatchGeneration <= 0 || (verifyToken && strings.TrimSpace(input.LeaseToken) == "") {
		return persistence.WorkspaceCleanupCommand{}, problem.New(400, "invalid_workspace_cleanup_lease", "cleanupId, dispatchGeneration, and leaseToken are required.")
	}
	if err := lockCurrentWorkerIncarnation(ctx, tx, worker); err != nil {
		return persistence.WorkspaceCleanupCommand{}, err
	}
	var command persistence.WorkspaceCleanupCommand
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ?", cleanupID).Take(&command).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return persistence.WorkspaceCleanupCommand{}, problem.New(409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current.")
		}
		return persistence.WorkspaceCleanupCommand{}, problem.Wrap(500, "workspace_cleanup_lease_load_failed", "Failed to load the Workspace cleanup lease.", err)
	}
	if command.Status != "leased" && command.Status != "running" {
		return persistence.WorkspaceCleanupCommand{}, problem.New(409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current.")
	}
	if command.DeliveryWorkerID == nil || *command.DeliveryWorkerID != worker.ID ||
		command.DeliveryWorkerIncarnation == nil || *command.DeliveryWorkerIncarnation != worker.Incarnation ||
		command.DispatchGeneration != input.DispatchGeneration || command.LeaseExpiresAt == nil || !command.LeaseExpiresAt.After(s.now()) {
		return persistence.WorkspaceCleanupCommand{}, problem.New(409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current.")
	}
	if verifyToken {
		expected := secret.HashToken(strings.TrimSpace(input.LeaseToken))
		if len(expected) != len(command.LeaseTokenHash) || subtle.ConstantTimeCompare(expected, command.LeaseTokenHash) != 1 {
			return persistence.WorkspaceCleanupCommand{}, problem.New(409, "workspace_cleanup_lease_fenced", "The Workspace cleanup lease is no longer current.")
		}
	}
	return command, nil
}

func loadWorkspaceCleanupClaim(
	ctx context.Context,
	tx *gorm.DB,
	command persistence.WorkspaceCleanupCommand,
	plainToken string,
	expiresAt time.Time,
) (WorkspaceCleanupClaim, error) {
	var materialization persistence.WorkspaceMaterialization
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND id = ? AND incarnation_id = ?",
			command.TenantID, command.MaterializationID, command.MaterializationIncarnationID).
		Take(&materialization).Error; err != nil {
		return WorkspaceCleanupClaim{}, problem.Wrap(409, "workspace_cleanup_scope_fenced", "The Workspace cleanup materialization is unavailable.", err)
	}
	return WorkspaceCleanupClaim{
		CleanupID: command.ID, TenantID: materialization.TenantID,
		OrganizationID: materialization.OrganizationID, ProjectID: materialization.ProjectID,
		SessionID: materialization.SessionID, LogicalWorkspaceID: materialization.WorkspaceID,
		MaterializationID: materialization.ID, IncarnationID: materialization.IncarnationID,
		ExecutionTargetID: materialization.ExecutionTargetID, TargetKind: materialization.TargetKind,
		StorageScope: materialization.StorageScope, LayoutVersion: materialization.LayoutVersion,
		Reason: command.Reason, DispatchGeneration: command.DispatchGeneration,
		Lease: WorkspaceCleanupLease{
			CleanupID: command.ID, DispatchGeneration: command.DispatchGeneration,
			LeaseToken: plainToken, ExpiresAt: expiresAt,
		},
	}, nil
}

func lockWorkspaceCleanupScope(
	ctx context.Context,
	tx *gorm.DB,
	materializationID uuid.UUID,
) (persistence.WorkspaceMaterialization, persistence.RemoteWorkspace, error) {
	var identity persistence.WorkspaceMaterialization
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "workspace_id", "incarnation_id").
		Where("id = ?", materializationID).Take(&identity).Error; err != nil {
		return persistence.WorkspaceMaterialization{}, persistence.RemoteWorkspace{}, err
	}
	var workspace persistence.RemoteWorkspace
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ?", identity.TenantID, identity.WorkspaceID).
		Take(&workspace).Error; err != nil {
		return persistence.WorkspaceMaterialization{}, persistence.RemoteWorkspace{}, err
	}
	var materialization persistence.WorkspaceMaterialization
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND tenant_id = ? AND workspace_id = ? AND incarnation_id = ?",
			identity.ID, identity.TenantID, identity.WorkspaceID, identity.IncarnationID).
		Take(&materialization).Error; err != nil {
		return persistence.WorkspaceMaterialization{}, persistence.RemoteWorkspace{}, err
	}
	return materialization, workspace, nil
}

func workspaceCleanupIntentDue(materialization persistence.WorkspaceMaterialization, workspace persistence.RemoteWorkspace, now time.Time) bool {
	if materialization.CleanupRequestedAt != nil && !materialization.CleanupRequestedAt.After(now) {
		return true
	}
	return workspace.RetentionUntil != nil && !workspace.RetentionUntil.After(now)
}

func workspacePodPlacementUnknown(materialization persistence.WorkspaceMaterialization) bool {
	return materialization.StorageScope == "pod" && materialization.WorkerInstanceUID == nil
}

func workspacePodNeverMaterialized(materialization persistence.WorkspaceMaterialization) bool {
	return workspacePodPlacementUnknown(materialization) && materialization.LayoutVersion == workspaceLayoutVersionCurrent &&
		materialization.WorkerID == nil && materialization.WorkerIncarnation == nil &&
		materialization.LastExecutionID == nil && materialization.LastGeneration == nil
}

func workspaceCleanupBlocker(
	ctx context.Context,
	tx *gorm.DB,
	materialization persistence.WorkspaceMaterialization,
	workspace persistence.RemoteWorkspace,
	now time.Time,
	ignoreCommandID uuid.UUID,
) (string, error) {
	if !workspaceCleanupIntentDue(materialization, workspace, now) {
		return "cleanup-not-due", nil
	}
	if materialization.State == "cleaned" {
		return "materialization-cleaned", nil
	}
	var count int64
	if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND workspace_materialization_id = ? AND status NOT IN ?",
			materialization.TenantID, materialization.ID, terminalExecutionStatuses).
		Count(&count).Error; err != nil {
		return "", problem.Wrap(500, "workspace_cleanup_execution_check_failed", "Failed to inspect active Workspace executions.", err)
	}
	if count != 0 {
		return "active-execution", nil
	}
	if err := tx.WithContext(ctx).Table("worker_leases AS lease").
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = lease.tenant_id AND execution.id = lease.execution_id").
		Where("execution.workspace_materialization_id = ? AND lease.expires_at > ?", materialization.ID, now).
		Count(&count).Error; err != nil {
		return "", problem.Wrap(500, "workspace_cleanup_lease_check_failed", "Failed to inspect active Workspace leases.", err)
	}
	if count != 0 {
		return "active-lease", nil
	}
	if err := tx.WithContext(ctx).Table("workspace_checkpoints AS checkpoint").
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = checkpoint.tenant_id AND execution.id = checkpoint.execution_id").
		Where("execution.workspace_materialization_id = ? AND checkpoint.status IN ?",
			materialization.ID, []string{"pending", "uploading"}).Count(&count).Error; err != nil {
		return "", problem.Wrap(500, "workspace_cleanup_checkpoint_check_failed", "Failed to inspect incomplete Workspace Checkpoints.", err)
	}
	if count != 0 {
		return "checkpoint-incomplete", nil
	}
	if err := tx.WithContext(ctx).Table("artifacts AS artifact").
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = artifact.tenant_id AND execution.id = artifact.execution_id").
		Where("execution.workspace_materialization_id = ? AND artifact.status = ? AND artifact.deleted_at IS NULL",
			materialization.ID, "pending").Count(&count).Error; err != nil {
		return "", problem.Wrap(500, "workspace_cleanup_artifact_check_failed", "Failed to inspect pending Workspace Artifact uploads.", err)
	}
	if count != 0 {
		return "artifact-upload-pending", nil
	}
	if workspace.State == "dirty" {
		if workspace.CurrentCheckpointID == nil {
			return "dirty-workspace-checkpoint-missing", nil
		}
		checkpointQuery := tx.WithContext(ctx).Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND workspace_id = ? AND id = ? AND status = ?",
				workspace.TenantID, workspace.ID, *workspace.CurrentCheckpointID, "ready")
		if workspace.CurrentMaterializationID != nil && *workspace.CurrentMaterializationID == materialization.ID {
			if materialization.LastExecutionID == nil || materialization.LastGeneration == nil {
				return "dirty-workspace-checkpoint-missing", nil
			}
			checkpointQuery = checkpointQuery.Where("execution_id = ? AND generation = ?",
				*materialization.LastExecutionID, *materialization.LastGeneration)
		}
		if err := checkpointQuery.Count(&count).Error; err != nil {
			return "", problem.Wrap(500, "workspace_cleanup_checkpoint_fence_failed", "Failed to validate the dirty Workspace recovery checkpoint.", err)
		}
		if count == 0 {
			return "dirty-workspace-checkpoint-stale", nil
		}
	}
	query := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
		Where("tenant_id = ? AND materialization_id = ? AND status IN ?",
			materialization.TenantID, materialization.ID, activeWorkspaceCleanupStatuses)
	if ignoreCommandID != uuid.Nil {
		query = query.Where("id <> ?", ignoreCommandID)
	}
	if err := query.Count(&count).Error; err != nil {
		return "", problem.Wrap(500, "workspace_cleanup_command_check_failed", "Failed to inspect concurrent Workspace cleanup commands.", err)
	}
	if count != 0 {
		return "cleanup-command-active", nil
	}
	return "", nil
}

func acknowledgeWorkspaceCleanup(
	ctx context.Context,
	tx *gorm.DB,
	command persistence.WorkspaceCleanupCommand,
	now time.Time,
) error {
	updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
		Where("id = ? AND materialization_id = ? AND materialization_incarnation_id = ? AND dispatch_generation = ? AND status IN ?",
			command.ID, command.MaterializationID, command.MaterializationIncarnationID,
			command.DispatchGeneration, activeWorkspaceCleanupStatuses).
		Updates(map[string]any{
			"status": "acknowledged", "lease_token_hash": nil, "delivery_worker_id": nil,
			"delivery_worker_incarnation": nil, "lease_expires_at": nil,
			"acknowledged_at": now, "updated_at": now,
		})
	if err := expectOne(updated, 409, "workspace_cleanup_acknowledgement_fenced", "The Workspace cleanup acknowledgement is no longer current."); err != nil {
		return err
	}
	materializationUpdate := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
		Where("tenant_id = ? AND id = ? AND incarnation_id = ? AND state IN ?",
			command.TenantID, command.MaterializationID, command.MaterializationIncarnationID,
			[]string{"cleanup-pending", "cleaning", "retired", "failed"}).
		Updates(map[string]any{
			"state": "cleaned", "cleaned_at": now, "failure_code": nil,
			"failure_message": nil, "failed_at": nil, "updated_at": now,
		})
	if err := expectOne(materializationUpdate, 409, "workspace_cleanup_materialization_fenced", "The Workspace materialization is no longer current for this cleanup command."); err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ? AND current_materialization_id = ?", command.TenantID, command.WorkspaceID, command.MaterializationID).
		Updates(map[string]any{"state": "cleaned", "cleaned_at": now, "updated_at": now}).Error; err != nil {
		return problem.Wrap(500, "workspace_cleanup_logical_acknowledgement_failed", "Failed to acknowledge the current logical Workspace cleanup.", err)
	}
	return nil
}

func requireReadyWorkspaceCheckpoint(
	ctx context.Context,
	tx *gorm.DB,
	workspace persistence.RemoteWorkspace,
	materialization persistence.WorkspaceMaterialization,
) error {
	if workspace.CurrentCheckpointID == nil || materialization.LastExecutionID == nil || materialization.LastGeneration == nil {
		return problem.New(409, "workspace_recovery_checkpoint_required", "Dirty Workspace rematerialization requires a current ready Checkpoint for the exact Execution generation.")
	}
	var count int64
	if err := tx.WithContext(ctx).Model(&persistence.WorkspaceCheckpoint{}).
		Where("tenant_id = ? AND workspace_id = ? AND id = ? AND execution_id = ? AND generation = ? AND status = ?",
			workspace.TenantID, workspace.ID, *workspace.CurrentCheckpointID,
			*materialization.LastExecutionID, *materialization.LastGeneration, "ready").
		Count(&count).Error; err != nil {
		return problem.Wrap(500, "workspace_recovery_checkpoint_check_failed", "Failed to validate the dirty Workspace recovery Checkpoint.", err)
	}
	if count != 1 {
		return problem.New(409, "workspace_recovery_checkpoint_required", "Dirty Workspace rematerialization requires a current ready Checkpoint for the exact Execution generation.")
	}
	return nil
}

func workspaceCleanupBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<(attempt-1)) * time.Second
}

func toWorkspaceCleanupState(command persistence.WorkspaceCleanupCommand) WorkspaceCleanupState {
	return WorkspaceCleanupState{
		CleanupID: command.ID, MaterializationID: command.MaterializationID, Status: command.Status,
		DispatchGeneration: command.DispatchGeneration, DeliveryAttempts: command.DeliveryAttempts,
		DeliveryAvailableAt: command.DeliveryAvailableAt, LeaseExpiresAt: command.LeaseExpiresAt,
		UpdatedAt: command.UpdatedAt,
	}
}
