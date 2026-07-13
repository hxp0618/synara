package executions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const checkpointManifestMaxBytes = 512 << 10

func (s *Service) CreateWorkspaceCheckpoint(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input CreateWorkspaceCheckpointInput,
	requestID string,
) (OperationResult[WorkspaceCheckpoint], error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.Strategy = strings.TrimSpace(input.Strategy)
	input.BaseCommit = trimCheckpointString(input.BaseCommit)
	input.HeadCommit = trimCheckpointString(input.HeadCommit)
	input.CurrentBranch = trimCheckpointString(input.CurrentBranch)
	if err := validateCreateWorkspaceCheckpointInput(input); err != nil {
		return OperationResult[WorkspaceCheckpoint]{}, err
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker.ID, requestID, "workspace.checkpoint.create", map[string]any{
		"executionId": executionID, "tenantId": input.TenantID, "generation": input.Generation,
		"idempotencyKey": input.IdempotencyKey, "strategy": input.Strategy,
		"baseCommit": input.BaseCommit, "headCommit": input.HeadCommit,
		"currentBranch": input.CurrentBranch, "manifest": input.Manifest,
		"fileCount": input.FileCount, "totalBytes": input.TotalBytes, "expiresAt": input.ExpiresAt,
	}, 201, func(tx *gorm.DB) (WorkspaceCheckpoint, error) {
		_, execution, err := s.lockLease(ctx, tx, worker.ID, executionID, input.LeaseInput, true)
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		workspace, err := lockExecutionWorkspace(ctx, tx, execution)
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		var existing persistence.WorkspaceCheckpoint
		existingErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND workspace_id = ? AND idempotency_key = ?",
				execution.TenantID, workspace.ID, input.IdempotencyKey).
			Take(&existing).Error
		if existingErr == nil {
			if !workspaceCheckpointMatchesCreate(existing, execution, input) {
				return WorkspaceCheckpoint{}, problem.New(409, "checkpoint_idempotency_conflict", "The Checkpoint idempotency key was already used with different content.")
			}
			return toWorkspaceCheckpoint(existing), nil
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return WorkspaceCheckpoint{}, problem.Wrap(500, "checkpoint_load_failed", "Failed to inspect the existing Workspace Checkpoint.", existingErr)
		}
		var activeCount int64
		if err := tx.WithContext(ctx).Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND workspace_id = ? AND status IN ?",
				execution.TenantID, workspace.ID, []string{"pending", "uploading"}).
			Count(&activeCount).Error; err != nil {
			return WorkspaceCheckpoint{}, problem.Wrap(500, "checkpoint_active_lookup_failed", "Failed to inspect the active Workspace Checkpoint.", err)
		}
		if activeCount > 0 {
			return WorkspaceCheckpoint{}, problem.New(409, "checkpoint_in_progress", "The logical Workspace already has a Checkpoint in progress.")
		}
		allowedStates := []string{"dirty"}
		if input.Strategy == "git-reference" {
			allowedStates = []string{"ready"}
		} else if workspace.CurrentCheckpointID == nil {
			allowedStates = []string{"ready", "dirty"}
		}
		if !containsWorkspaceState(allowedStates, workspace.State) {
			return WorkspaceCheckpoint{}, problem.New(409, "checkpoint_workspace_state_invalid", "The Checkpoint strategy is not valid for the current Workspace state.")
		}
		if !checkpointMetadataMatchesWorkspace(workspace, input) {
			return WorkspaceCheckpoint{}, problem.New(409, "checkpoint_workspace_metadata_changed", "The Checkpoint metadata does not match the current logical Workspace.")
		}
		now := s.now()
		model := persistence.WorkspaceCheckpoint{
			ID: uuid.New(), TenantID: execution.TenantID, WorkspaceID: workspace.ID,
			SessionID: execution.SessionID, TurnID: &execution.TurnID, ExecutionID: execution.ID,
			Generation: execution.Generation, IdempotencyKey: input.IdempotencyKey,
			Strategy: input.Strategy, Status: "pending", BaseCommit: input.BaseCommit,
			HeadCommit: input.HeadCommit, CurrentBranch: input.CurrentBranch,
			Manifest: input.Manifest, FileCount: input.FileCount, TotalBytes: input.TotalBytes,
			CreatedAt: now, ExpiresAt: input.ExpiresAt,
		}
		if model.Manifest == nil {
			model.Manifest = map[string]any{}
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return WorkspaceCheckpoint{}, problem.Wrap(409, "checkpoint_create_conflict", "The Workspace Checkpoint could not be created.", err)
		}
		updated := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND last_worker_id = ? AND last_execution_id = ? AND last_generation = ? AND state IN ?",
				execution.TenantID, workspace.ID, worker.ID, execution.ID, execution.Generation,
				allowedStates).
			Updates(map[string]any{"state": "checkpointing", "last_used_at": now, "updated_at": now})
		if err := expectOne(updated, 409, "checkpoint_generation_conflict", "The Checkpoint belongs to an obsolete Worker Generation."); err != nil {
			return WorkspaceCheckpoint{}, err
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "checkpoint.created", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "workspaceId": workspace.ID,
				"checkpointId": model.ID, "strategy": model.Strategy,
			},
		})
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		return toWorkspaceCheckpoint(model), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) MarkWorkspaceCheckpointReady(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, checkpointID uuid.UUID,
	input WorkspaceCheckpointReadyInput,
	requestID string,
) (OperationResult[WorkspaceCheckpoint], error) {
	input.SHA256 = trimCheckpointString(input.SHA256)
	if err := validateWorkspaceCheckpointReadyInput(input); err != nil {
		return OperationResult[WorkspaceCheckpoint]{}, err
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker.ID, requestID, "workspace.checkpoint.ready", map[string]any{
		"executionId": executionID, "checkpointId": checkpointID, "tenantId": input.TenantID,
		"generation": input.Generation, "artifactId": input.ArtifactID, "sha256": input.SHA256,
	}, 200, func(tx *gorm.DB) (WorkspaceCheckpoint, error) {
		_, execution, err := s.lockLease(ctx, tx, worker.ID, executionID, input.LeaseInput, true)
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		workspace, err := lockExecutionWorkspace(ctx, tx, execution)
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		checkpoint, err := lockWorkspaceCheckpoint(ctx, tx, execution, workspace, checkpointID)
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		if checkpoint.Status == "ready" {
			if !sameUUIDReference(checkpoint.ArtifactID, input.ArtifactID) || !sameStringReference(checkpoint.SHA256, input.SHA256) {
				return WorkspaceCheckpoint{}, problem.New(409, "checkpoint_already_ready", "The Workspace Checkpoint is already ready with different Artifact metadata.")
			}
			return toWorkspaceCheckpoint(checkpoint), nil
		}
		if checkpoint.Status != "pending" && checkpoint.Status != "uploading" {
			return WorkspaceCheckpoint{}, problem.New(409, "checkpoint_not_pending", "The Workspace Checkpoint cannot become ready from its current state.")
		}
		if checkpoint.Strategy == "git-reference" {
			if input.ArtifactID != nil || input.SHA256 != nil {
				return WorkspaceCheckpoint{}, problem.New(400, "invalid_checkpoint_artifact", "Git-reference Checkpoints do not accept an Artifact.")
			}
		} else {
			if input.ArtifactID == nil || input.SHA256 == nil {
				return WorkspaceCheckpoint{}, problem.New(400, "checkpoint_artifact_required", "Patch and Snapshot Checkpoints require a ready Artifact.")
			}
			artifactKind := "checkpoint"
			if checkpoint.Strategy == "snapshot" {
				artifactKind = "workspace_snapshot"
			}
			var artifact persistence.Artifact
			if err := tx.WithContext(ctx).
				Where("tenant_id = ? AND id = ? AND session_id = ? AND execution_id = ? AND status = ? AND deleted_at IS NULL AND kind = ? AND sha256 = ?",
					execution.TenantID, *input.ArtifactID, execution.SessionID, execution.ID, "ready",
					artifactKind, strings.TrimSpace(*input.SHA256)).
				Take(&artifact).Error; err != nil {
				return WorkspaceCheckpoint{}, problem.Wrap(409, "checkpoint_artifact_unavailable", "The Checkpoint Artifact is not ready or does not match the Execution.", err)
			}
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND workspace_id = ? AND id = ? AND execution_id = ? AND generation = ? AND status IN ?",
				execution.TenantID, workspace.ID, checkpoint.ID, execution.ID, execution.Generation,
				[]string{"pending", "uploading"}).
			Updates(map[string]any{
				"status": "ready", "artifact_id": input.ArtifactID, "sha256": input.SHA256,
				"ready_at": now, "failed_at": nil, "failure_code": nil, "failure_message": nil,
			})
		if err := expectOne(updated, 409, "checkpoint_ready_conflict", "The Workspace Checkpoint changed concurrently."); err != nil {
			return WorkspaceCheckpoint{}, err
		}
		workspaceState := "ready"
		if checkpoint.Strategy != "git-reference" {
			workspaceState = "dirty"
		}
		workspaceUpdate := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND last_worker_id = ? AND last_execution_id = ? AND last_generation = ? AND state = ?",
				execution.TenantID, workspace.ID, worker.ID, execution.ID, execution.Generation, "checkpointing").
			Updates(map[string]any{
				"state": workspaceState, "current_checkpoint_id": checkpoint.ID,
				"last_used_at": now, "updated_at": now,
			})
		if err := expectOne(workspaceUpdate, 409, "checkpoint_generation_conflict", "The Checkpoint belongs to an obsolete Worker Generation."); err != nil {
			return WorkspaceCheckpoint{}, err
		}
		executionUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ? AND worker_id = ? AND generation = ? AND remote_workspace_id = ?",
				execution.TenantID, execution.ID, worker.ID, execution.Generation, workspace.ID).
			Update("restore_checkpoint_id", checkpoint.ID)
		if err := expectOne(executionUpdate, 409, "checkpoint_generation_conflict", "The Checkpoint could not be bound to the current Execution Generation."); err != nil {
			return WorkspaceCheckpoint{}, err
		}
		checkpoint.Status = "ready"
		checkpoint.ArtifactID = input.ArtifactID
		checkpoint.SHA256 = input.SHA256
		checkpoint.ReadyAt = &now
		checkpoint.FailedAt = nil
		checkpoint.FailureCode = nil
		checkpoint.FailureMessage = nil
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "checkpoint.ready", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "workspaceId": workspace.ID,
				"checkpointId": checkpoint.ID, "strategy": checkpoint.Strategy,
				"artifactId": checkpoint.ArtifactID, "sha256": checkpoint.SHA256,
			},
		})
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		return toWorkspaceCheckpoint(checkpoint), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func (s *Service) MarkWorkspaceCheckpointFailed(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, checkpointID uuid.UUID,
	input WorkspaceCheckpointFailedInput,
	requestID string,
) (OperationResult[WorkspaceCheckpoint], error) {
	input.FailureCode = strings.TrimSpace(input.FailureCode)
	input.FailureMessage = strings.TrimSpace(input.FailureMessage)
	if input.FailureCode == "" || len(input.FailureCode) > 160 || strings.ContainsAny(input.FailureCode, "\r\n\t") {
		return OperationResult[WorkspaceCheckpoint]{}, problem.New(400, "invalid_checkpoint_failure", "failureCode is invalid.")
	}
	if len(input.FailureMessage) > 10_000 {
		return OperationResult[WorkspaceCheckpoint]{}, problem.New(400, "invalid_checkpoint_failure", "failureMessage must not exceed 10000 characters.")
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker.ID, requestID, "workspace.checkpoint.failed", map[string]any{
		"executionId": executionID, "checkpointId": checkpointID, "tenantId": input.TenantID,
		"generation": input.Generation, "failureCode": input.FailureCode,
		"failureMessage": input.FailureMessage,
	}, 200, func(tx *gorm.DB) (WorkspaceCheckpoint, error) {
		_, execution, err := s.lockLease(ctx, tx, worker.ID, executionID, input.LeaseInput, true)
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		workspace, err := lockExecutionWorkspace(ctx, tx, execution)
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		checkpoint, err := lockWorkspaceCheckpoint(ctx, tx, execution, workspace, checkpointID)
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		if checkpoint.Status == "failed" {
			if checkpoint.FailureCode == nil || *checkpoint.FailureCode != input.FailureCode ||
				checkpoint.FailureMessage == nil || *checkpoint.FailureMessage != input.FailureMessage {
				return WorkspaceCheckpoint{}, problem.New(409, "checkpoint_already_failed", "The Workspace Checkpoint is already failed with different details.")
			}
			return toWorkspaceCheckpoint(checkpoint), nil
		}
		if checkpoint.Status != "pending" && checkpoint.Status != "uploading" {
			return WorkspaceCheckpoint{}, problem.New(409, "checkpoint_not_pending", "The Workspace Checkpoint cannot fail from its current state.")
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND workspace_id = ? AND id = ? AND execution_id = ? AND generation = ? AND status IN ?",
				execution.TenantID, workspace.ID, checkpoint.ID, execution.ID, execution.Generation,
				[]string{"pending", "uploading"}).
			Updates(map[string]any{
				"status": "failed", "failure_code": input.FailureCode,
				"failure_message": input.FailureMessage, "failed_at": now, "ready_at": nil,
			})
		if err := expectOne(updated, 409, "checkpoint_failed_conflict", "The Workspace Checkpoint changed concurrently."); err != nil {
			return WorkspaceCheckpoint{}, err
		}
		workspaceState := "ready"
		if checkpoint.Strategy != "git-reference" {
			workspaceState = "dirty"
		}
		workspaceUpdate := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND last_worker_id = ? AND last_execution_id = ? AND last_generation = ? AND state = ?",
				execution.TenantID, workspace.ID, worker.ID, execution.ID, execution.Generation, "checkpointing").
			Updates(map[string]any{"state": workspaceState, "last_used_at": now, "updated_at": now})
		if err := expectOne(workspaceUpdate, 409, "checkpoint_generation_conflict", "The Checkpoint belongs to an obsolete Worker Generation."); err != nil {
			return WorkspaceCheckpoint{}, err
		}
		checkpoint.Status = "failed"
		checkpoint.FailureCode = &input.FailureCode
		checkpoint.FailureMessage = &input.FailureMessage
		checkpoint.FailedAt = &now
		checkpoint.ReadyAt = nil
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "checkpoint.failed", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "workspaceId": workspace.ID,
				"checkpointId": checkpoint.ID, "strategy": checkpoint.Strategy,
				"failureCode": input.FailureCode, "failureMessage": input.FailureMessage,
			},
		})
		if err != nil {
			return WorkspaceCheckpoint{}, err
		}
		return toWorkspaceCheckpoint(checkpoint), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func validateCreateWorkspaceCheckpointInput(input CreateWorkspaceCheckpointInput) error {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.IdempotencyKey == "" || len(input.IdempotencyKey) > 200 || strings.ContainsAny(input.IdempotencyKey, "\r\n\t") {
		return problem.New(400, "invalid_checkpoint_idempotency_key", "idempotencyKey is invalid.")
	}
	if input.Strategy != "git-reference" && input.Strategy != "patch" && input.Strategy != "snapshot" {
		return problem.New(400, "invalid_checkpoint_strategy", "strategy is invalid.")
	}
	for name, value := range map[string]*string{"baseCommit": input.BaseCommit, "headCommit": input.HeadCommit} {
		if value != nil && !workspaceHashPattern.MatchString(strings.TrimSpace(*value)) {
			return problem.New(400, "invalid_checkpoint_metadata", name+" is invalid.")
		}
	}
	if input.CurrentBranch != nil {
		if _, err := gitpolicy.NormalizeBranch(strings.TrimSpace(*input.CurrentBranch), ""); err != nil {
			return problem.New(400, "invalid_checkpoint_metadata", "currentBranch is invalid.")
		}
	}
	if input.Strategy == "git-reference" && (input.HeadCommit == nil || input.CurrentBranch == nil) {
		return problem.New(400, "invalid_checkpoint_metadata", "Git-reference Checkpoints require headCommit and currentBranch.")
	}
	if input.Strategy != "git-reference" && (input.FileCount == nil || input.TotalBytes == nil) {
		return problem.New(400, "invalid_checkpoint_metadata", "Patch and Snapshot Checkpoints require fileCount and totalBytes.")
	}
	if input.FileCount != nil && *input.FileCount < 0 {
		return problem.New(400, "invalid_checkpoint_metadata", "fileCount must not be negative.")
	}
	if input.TotalBytes != nil && *input.TotalBytes < 0 {
		return problem.New(400, "invalid_checkpoint_metadata", "totalBytes must not be negative.")
	}
	if input.Manifest == nil || len(input.Manifest) == 0 {
		return problem.New(400, "invalid_checkpoint_manifest", "manifest must describe the Checkpoint content.")
	}
	encoded, err := json.Marshal(input.Manifest)
	if err != nil || len(encoded) > checkpointManifestMaxBytes {
		return problem.New(400, "invalid_checkpoint_manifest", "manifest is invalid or exceeds 524288 bytes.")
	}
	return nil
}

func trimCheckpointString(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func containsWorkspaceState(values []string, state string) bool {
	for _, value := range values {
		if value == state {
			return true
		}
	}
	return false
}

func checkpointMetadataMatchesWorkspace(
	workspace persistence.RemoteWorkspace,
	input CreateWorkspaceCheckpointInput,
) bool {
	for _, pair := range [][2]*string{
		{workspace.BaseCommit, input.BaseCommit},
		{workspace.HeadCommit, input.HeadCommit},
		{workspace.CurrentBranch, input.CurrentBranch},
	} {
		if pair[1] != nil && !sameStringReference(pair[0], pair[1]) {
			return false
		}
	}
	return true
}

func validateWorkspaceCheckpointReadyInput(input WorkspaceCheckpointReadyInput) error {
	if input.SHA256 != nil {
		value := strings.TrimSpace(*input.SHA256)
		if len(value) != 64 || !workspaceHashPattern.MatchString(value) {
			return problem.New(400, "invalid_checkpoint_artifact", "sha256 is invalid.")
		}
	}
	return nil
}

func lockWorkspaceCheckpoint(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workspace persistence.RemoteWorkspace,
	checkpointID uuid.UUID,
) (persistence.WorkspaceCheckpoint, error) {
	var checkpoint persistence.WorkspaceCheckpoint
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND workspace_id = ? AND id = ? AND session_id = ? AND execution_id = ? AND generation = ?",
			execution.TenantID, workspace.ID, checkpointID, execution.SessionID, execution.ID, execution.Generation).
		Take(&checkpoint).Error; err != nil {
		return persistence.WorkspaceCheckpoint{}, problem.Wrap(404, "checkpoint_not_found", "The Workspace Checkpoint is unavailable for this Execution Generation.", err)
	}
	return checkpoint, nil
}

func workspaceCheckpointMatchesCreate(
	checkpoint persistence.WorkspaceCheckpoint,
	execution persistence.AgentExecution,
	input CreateWorkspaceCheckpointInput,
) bool {
	if checkpoint.ExecutionID != execution.ID || checkpoint.Generation != execution.Generation ||
		checkpoint.Strategy != input.Strategy || !sameStringReference(checkpoint.BaseCommit, input.BaseCommit) ||
		!sameStringReference(checkpoint.HeadCommit, input.HeadCommit) ||
		!sameStringReference(checkpoint.CurrentBranch, input.CurrentBranch) ||
		!sameTimeReference(checkpoint.ExpiresAt, input.ExpiresAt) ||
		!sameIntReference(checkpoint.FileCount, input.FileCount) ||
		!sameInt64Reference(checkpoint.TotalBytes, input.TotalBytes) {
		return false
	}
	left, leftErr := json.Marshal(checkpoint.Manifest)
	right, rightErr := json.Marshal(input.Manifest)
	if input.Manifest == nil {
		right, rightErr = json.Marshal(map[string]any{})
	}
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func sameUUIDReference(left, right *uuid.UUID) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func sameStringReference(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func sameTimeReference(left, right *time.Time) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && left.Equal(*right))
}

func sameIntReference(left, right *int) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func sameInt64Reference(left, right *int64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func toWorkspaceCheckpoint(model persistence.WorkspaceCheckpoint) WorkspaceCheckpoint {
	manifest := model.Manifest
	if manifest == nil {
		manifest = map[string]any{}
	}
	return WorkspaceCheckpoint{
		ID: model.ID, WorkspaceID: model.WorkspaceID, SessionID: model.SessionID, TurnID: model.TurnID,
		ExecutionID: model.ExecutionID, Generation: model.Generation,
		IdempotencyKey: model.IdempotencyKey, Strategy: model.Strategy, Status: model.Status,
		BaseCommit: model.BaseCommit, HeadCommit: model.HeadCommit, CurrentBranch: model.CurrentBranch,
		ArtifactID: model.ArtifactID, Manifest: manifest, FileCount: model.FileCount,
		TotalBytes: model.TotalBytes, SHA256: model.SHA256, FailureCode: model.FailureCode,
		FailureMessage: model.FailureMessage, CreatedAt: model.CreatedAt, ReadyAt: model.ReadyAt,
		FailedAt: model.FailedAt, ExpiresAt: model.ExpiresAt,
	}
}
