package executions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const (
	resourceSuspendReasonActiveIdleTimeout = "active-idle-timeout"
	activeTurnSuspendCommandType           = "SuspendTurn"
	activeTurnSuspendCapabilityID          = "suspend-active-turn"
	activeTurnSuspendCheckpointProtocol    = "provider-host-suspend-terminal-v1"
	activeTurnSuspendProviderHostMajor     = 2
	activeTurnSuspendProviderHostMinor     = 2
)

type activeTurnCheckpointHashPayload struct {
	SuspendAttemptID             uuid.UUID `json:"suspendAttemptId"`
	TenantID                     uuid.UUID `json:"tenantId"`
	SessionID                    uuid.UUID `json:"sessionId"`
	TurnID                       uuid.UUID `json:"turnId"`
	ExecutionID                  uuid.UUID `json:"executionId"`
	Generation                   int64     `json:"generation"`
	WorkerID                     uuid.UUID `json:"workerId"`
	WorkerIncarnation            int64     `json:"workerIncarnation"`
	WorkerInstanceUID            string    `json:"workerInstanceUid"`
	ControlCommandID             uuid.UUID `json:"controlCommandId"`
	BoundaryMeaningfulActivity   int64     `json:"boundaryMeaningfulActivitySequence"`
	ActiveCommandID              string    `json:"activeCommandId"`
	CheckpointHistorySequence    int64     `json:"checkpointHistorySequence"`
	CurrentTurnSequence          int64     `json:"currentTurnSequence"`
	ProviderRuntimeBindingID     uuid.UUID `json:"providerRuntimeBindingId"`
	ProviderCheckpointProtocol   string    `json:"providerCheckpointProtocol"`
	ProviderCursorSourceID       uuid.UUID `json:"providerCursorSourceExecutionId"`
	ProviderCursorSourceGen      int64     `json:"providerCursorSourceGeneration"`
	ProviderCursorHistory        int64     `json:"providerCursorHistorySequence"`
	ProviderCursorBindingVersion int       `json:"providerCursorBindingVersion"`
	ProviderCursorBindingDigest  string    `json:"providerCursorBindingDigest"`
	ProviderCursorSHA256         string    `json:"providerCursorSha256"`
}

func activeTurnCheckpointReceiptSHA256(attempt persistence.ExecutionSuspendAttempt) ([]byte, error) {
	if attempt.ControlCommandID == nil || attempt.BoundaryMeaningfulActivitySequence == nil ||
		*attempt.BoundaryMeaningfulActivitySequence <= 0 || attempt.ActiveCommandID == nil ||
		attempt.CheckpointHistorySequence == nil || attempt.CurrentTurnSequence == nil ||
		attempt.ProviderRuntimeBindingID == nil || attempt.ProviderCheckpointProtocol == nil ||
		attempt.ProviderCursorSourceExecutionID == nil || attempt.ProviderCursorSourceGeneration == nil ||
		attempt.ProviderCursorHistorySequence == nil || attempt.ProviderCursorBindingVersion == nil ||
		len(attempt.ProviderCursorBindingDigest) != sha256.Size || len(attempt.ProviderCursorSHA256) != sha256.Size {
		return nil, errors.New("active-turn checkpoint receipt is incomplete")
	}
	payload := activeTurnCheckpointHashPayload{
		SuspendAttemptID: attempt.ID, TenantID: attempt.TenantID, SessionID: attempt.SessionID,
		TurnID: attempt.TurnID, ExecutionID: attempt.ExecutionID, Generation: attempt.Generation,
		WorkerID: attempt.WorkerID, WorkerIncarnation: attempt.WorkerIncarnation,
		WorkerInstanceUID: attempt.WorkerInstanceUID, ControlCommandID: *attempt.ControlCommandID,
		BoundaryMeaningfulActivity: *attempt.BoundaryMeaningfulActivitySequence,
		ActiveCommandID:            *attempt.ActiveCommandID, CheckpointHistorySequence: *attempt.CheckpointHistorySequence,
		CurrentTurnSequence: *attempt.CurrentTurnSequence, ProviderRuntimeBindingID: *attempt.ProviderRuntimeBindingID,
		ProviderCheckpointProtocol:   *attempt.ProviderCheckpointProtocol,
		ProviderCursorSourceID:       *attempt.ProviderCursorSourceExecutionID,
		ProviderCursorSourceGen:      *attempt.ProviderCursorSourceGeneration,
		ProviderCursorHistory:        *attempt.ProviderCursorHistorySequence,
		ProviderCursorBindingVersion: *attempt.ProviderCursorBindingVersion,
		ProviderCursorBindingDigest:  hex.EncodeToString(attempt.ProviderCursorBindingDigest),
		ProviderCursorSHA256:         hex.EncodeToString(attempt.ProviderCursorSHA256),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func workerSupportsActiveTurnSuspend(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (bool, error) {
	if execution.WorkerManifestID == nil || execution.Provider == nil ||
		strings.TrimSpace(*execution.Provider) == "" || execution.ProviderResumeStrategySnapshot != "native-cursor" ||
		execution.ProviderRuntimeBindingID == nil || execution.ProviderCursorBindingVersion == nil ||
		len(execution.ProviderCursorBindingDigest) != sha256.Size {
		return false, nil
	}
	var manifest persistence.WorkerProviderManifest
	err := tx.WithContext(ctx).
		Where("worker_manifest_id = ? AND provider = ?", *execution.WorkerManifestID, strings.TrimSpace(*execution.Provider)).
		Take(&manifest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, problem.Wrap(500, "active_suspend_manifest_load_failed", "The active-turn suspend Provider manifest could not be loaded.", err)
	}
	return manifest.CompatibilityStatus == "compatible" &&
		manifest.ProviderHostMajor == activeTurnSuspendProviderHostMajor &&
		manifest.ProviderHostMinor >= activeTurnSuspendProviderHostMinor &&
		containsString(manifest.ResumeStrategies, "native-cursor") &&
		isSupportedProviderCapability(manifest.Capabilities[activeTurnSuspendCapabilityID]), nil
}

func createActiveTurnSuspendControlCommand(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	session persistence.AgentSession,
	workerID uuid.UUID,
	attemptID uuid.UUID,
	now time.Time,
) (persistence.ExecutionControlCommand, error) {
	provider := strings.TrimSpace(session.Provider)
	if execution.Provider != nil && strings.TrimSpace(*execution.Provider) != "" {
		provider = strings.TrimSpace(*execution.Provider)
	}
	if provider == "" || execution.Generation <= 0 {
		return persistence.ExecutionControlCommand{}, problem.New(409, "active_suspend_provider_missing", "The active Execution does not have a suspend-capable Provider binding.")
	}
	commandID := uuid.New()
	generation := execution.Generation
	command := persistence.ExecutionControlCommand{
		ID: commandID, TenantID: execution.TenantID, ExecutionID: execution.ID,
		SessionID: execution.SessionID, TurnID: execution.TurnID, Provider: provider,
		CommandType: activeTurnSuspendCommandType, CommandID: "suspend:" + attemptID.String(),
		Payload: map[string]any{"turnId": execution.TurnID.String(), "suspendAttemptId": attemptID.String()},
		Status:  "pending", RequestedBy: session.CreatedBy, RequestedAt: now,
		DeliveryWorkerID: &workerID, DeliveryGeneration: &generation, DeliveryAvailableAt: now,
	}
	if err := tx.WithContext(ctx).Create(&command).Error; err != nil {
		return persistence.ExecutionControlCommand{}, problem.Wrap(409, "active_suspend_control_conflict", "The active-turn Suspend command could not be created atomically.", err)
	}
	return command, nil
}

func (s *Service) acknowledgeSuspendControlCommand(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	execution persistence.AgentExecution,
	command persistence.ExecutionControlCommand,
	input ControlCommandDeliveryInput,
	now time.Time,
	appended *[]persistence.SessionEvent,
) (ControlCommand, error) {
	if err := s.requireExecutionSessionWithinAbsoluteLifetime(ctx, tx, execution, now); err != nil {
		return ControlCommand{}, err
	}
	if execution.Status != "running" {
		return ControlCommand{}, problem.New(409, "active_suspend_state_conflict", "The active Turn is no longer running.")
	}
	quiesced, _ := input.Result["quiesced"].(bool)
	activeCommandID, _ := input.Result["targetCommandId"].(string)
	checkpointProtocol, _ := input.Result["checkpointProtocol"].(string)
	activeCommandID = strings.TrimSpace(activeCommandID)
	checkpointProtocol = strings.TrimSpace(checkpointProtocol)
	if !quiesced || activeCommandID == "" || len(activeCommandID) > 240 ||
		strings.ContainsAny(activeCommandID, "\r\n\t") || checkpointProtocol != activeTurnSuspendCheckpointProtocol ||
		input.ProviderResumeCursor == nil || strings.TrimSpace(*input.ProviderResumeCursor) == "" {
		return ControlCommand{}, problem.New(409, "active_suspend_checkpoint_invalid", "The Provider did not return a complete active-turn suspend checkpoint.")
	}

	var attempt persistence.ExecutionSuspendAttempt
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND worker_id = ? AND control_command_id = ? AND reason = ? AND status = ?",
			execution.TenantID, execution.ID, execution.Generation, worker.ID, command.ID,
			resourceSuspendReasonActiveIdleTimeout, "checkpointing").
		Take(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ControlCommand{}, problem.New(409, "active_suspend_attempt_missing", "The active-turn suspend attempt is no longer current.")
	}
	if err != nil {
		return ControlCommand{}, problem.Wrap(500, "active_suspend_attempt_load_failed", "The active-turn suspend attempt could not be locked.", err)
	}
	if attempt.ProviderQuiescedAt != nil {
		return ControlCommand{}, problem.New(409, "active_suspend_checkpoint_conflict", "The active-turn checkpoint receipt was already recorded.")
	}
	if !attempt.CheckpointDeadlineAt.After(now) {
		return ControlCommand{}, problem.New(409, "resource_suspend_checkpoint_expired", "The suspend Checkpoint deadline has elapsed.")
	}
	if attempt.BoundaryMeaningfulActivitySequence == nil || *attempt.BoundaryMeaningfulActivitySequence <= 0 {
		return ControlCommand{}, problem.New(409, "active_suspend_activity_boundary_missing", "The active-turn suspend attempt does not have a semantic activity boundary.")
	}
	if err := s.storeProviderCursor(ctx, tx, execution, input.ProviderResumeCursor, true); err != nil {
		return ControlCommand{}, err
	}
	cursor, err := s.loadProviderCursor(ctx, tx, execution, false, 0)
	if err != nil {
		return ControlCommand{}, err
	}
	if cursor.SelectedStrategy != "native-cursor" || cursor.Cursor == nil ||
		*cursor.Cursor != strings.TrimSpace(*input.ProviderResumeCursor) || cursor.CursorSourceExecutionID == nil ||
		*cursor.CursorSourceExecutionID != execution.ID || cursor.CursorSourceGeneration == nil ||
		*cursor.CursorSourceGeneration != execution.Generation || cursor.CursorHistorySequence == nil {
		return ControlCommand{}, problem.New(409, "active_suspend_cursor_unavailable", "The Provider Cursor could not be frozen at the active-turn suspend boundary.")
	}
	currentTurnSequence, found, err := loadCurrentTurnSequence(ctx, tx, execution)
	if err != nil {
		return ControlCommand{}, err
	}
	if !found || currentTurnSequence <= 0 || execution.ProviderRuntimeBindingID == nil ||
		execution.ProviderCursorBindingVersion == nil || len(execution.ProviderCursorBindingDigest) != sha256.Size {
		return ControlCommand{}, problem.New(409, "active_suspend_lineage_unavailable", "The active Turn checkpoint lineage is incomplete.")
	}
	cursorDigest := sha256.Sum256([]byte(*cursor.Cursor))
	attempt.ActiveCommandID = &activeCommandID
	attempt.CheckpointHistorySequence = cursor.CursorHistorySequence
	attempt.CurrentTurnSequence = &currentTurnSequence
	attempt.ProviderRuntimeBindingID = execution.ProviderRuntimeBindingID
	attempt.ProviderCheckpointProtocol = &checkpointProtocol
	attempt.ProviderCursorSourceExecutionID = cursor.CursorSourceExecutionID
	attempt.ProviderCursorSourceGeneration = cursor.CursorSourceGeneration
	attempt.ProviderCursorHistorySequence = cursor.CursorHistorySequence
	attempt.ProviderCursorBindingVersion = execution.ProviderCursorBindingVersion
	attempt.ProviderCursorBindingDigest = append([]byte(nil), execution.ProviderCursorBindingDigest...)
	attempt.ProviderCursorSHA256 = append([]byte(nil), cursorDigest[:]...)
	attempt.ProviderQuiescedAt = &now
	receiptSHA, err := activeTurnCheckpointReceiptSHA256(attempt)
	if err != nil {
		return ControlCommand{}, problem.Wrap(500, "active_suspend_receipt_encode_failed", "The active-turn checkpoint receipt could not be hashed.", err)
	}
	attempt.CheckpointReceiptSHA256 = receiptSHA
	updated := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ? AND status = ? AND provider_quiesced_at IS NULL",
			attempt.TenantID, attempt.ID, "checkpointing").
		Updates(map[string]any{
			"active_command_id": activeCommandID, "checkpoint_history_sequence": *cursor.CursorHistorySequence,
			"current_turn_sequence": currentTurnSequence, "provider_runtime_binding_id": *execution.ProviderRuntimeBindingID,
			"provider_checkpoint_protocol":        checkpointProtocol,
			"provider_cursor_source_execution_id": execution.ID,
			"provider_cursor_source_generation":   execution.Generation,
			"provider_cursor_history_sequence":    *cursor.CursorHistorySequence,
			"provider_cursor_binding_version":     *execution.ProviderCursorBindingVersion,
			"provider_cursor_binding_digest":      execution.ProviderCursorBindingDigest,
			"provider_cursor_sha256":              cursorDigest[:], "checkpoint_receipt_sha256": receiptSHA,
			"provider_quiesced_at": now,
		})
	if err := expectOne(updated, 409, "active_suspend_receipt_conflict", "The active-turn checkpoint receipt changed concurrently."); err != nil {
		return ControlCommand{}, err
	}
	if err := acknowledgeControlCommandRow(ctx, tx, execution, command, now); err != nil {
		return ControlCommand{}, err
	}
	event, err := s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
		EventType: "execution.suspend-quiesced", ActorType: "worker", ActorID: &worker.ID,
		ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
		Payload: map[string]any{
			"turnId": execution.TurnID, "suspendAttemptId": attempt.ID,
			"controlCommandId": command.ID, "activeCommandId": activeCommandID,
			"checkpointHistorySequence": *cursor.CursorHistorySequence,
			"checkpointReceiptSha256":   hex.EncodeToString(receiptSHA), "providerQuiescedAt": now,
		},
	})
	if err != nil {
		return ControlCommand{}, err
	}
	*appended = append(*appended, event)
	command.Status = "acknowledged"
	command.AcknowledgedAt = &now
	command.DeliveryError = nil
	return toControlCommand(command), nil
}

func (s *Service) requireActiveTurnCheckpointReceipt(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	attempt persistence.ExecutionSuspendAttempt,
) error {
	if attempt.Reason != resourceSuspendReasonActiveIdleTimeout {
		return nil
	}
	if attempt.ProviderQuiescedAt == nil || len(attempt.CheckpointReceiptSHA256) != sha256.Size ||
		attempt.ProviderCursorSourceExecutionID == nil || *attempt.ProviderCursorSourceExecutionID != execution.ID ||
		attempt.ProviderCursorSourceGeneration == nil || *attempt.ProviderCursorSourceGeneration != execution.Generation ||
		attempt.ProviderCursorHistorySequence == nil || attempt.CheckpointHistorySequence == nil ||
		*attempt.ProviderCursorHistorySequence != *attempt.CheckpointHistorySequence ||
		attempt.ProviderCursorBindingVersion == nil || execution.ProviderCursorBindingVersion == nil ||
		*attempt.ProviderCursorBindingVersion != *execution.ProviderCursorBindingVersion ||
		!bytes.Equal(attempt.ProviderCursorBindingDigest, execution.ProviderCursorBindingDigest) ||
		attempt.ProviderRuntimeBindingID == nil || execution.ProviderRuntimeBindingID == nil ||
		*attempt.ProviderRuntimeBindingID != *execution.ProviderRuntimeBindingID {
		return problem.New(409, "active_suspend_receipt_invalid", "The active-turn checkpoint receipt does not match the current Execution generation.")
	}
	expectedSHA, err := activeTurnCheckpointReceiptSHA256(attempt)
	if err != nil || !bytes.Equal(expectedSHA, attempt.CheckpointReceiptSHA256) {
		return problem.New(409, "active_suspend_receipt_integrity_failed", "The active-turn checkpoint receipt failed integrity validation.")
	}
	var command persistence.ExecutionControlCommand
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ? AND id = ? AND session_id = ? AND turn_id = ? AND command_type = ? AND status = ? AND delivery_worker_id = ? AND delivery_generation = ?",
			execution.TenantID, execution.ID, *attempt.ControlCommandID, execution.SessionID, execution.TurnID,
			activeTurnSuspendCommandType, "acknowledged", attempt.WorkerID, attempt.Generation).
		Take(&command).Error; err != nil {
		return problem.Wrap(409, "active_suspend_control_unacknowledged", "The durable SuspendTurn command acknowledgement is unavailable.", err)
	}
	cursor, err := s.loadProviderCursor(ctx, tx, execution, false, *attempt.CheckpointHistorySequence)
	if err != nil {
		return err
	}
	if cursor.SelectedStrategy != "native-cursor" || cursor.Cursor == nil ||
		cursor.CursorSourceExecutionID == nil || *cursor.CursorSourceExecutionID != execution.ID ||
		cursor.CursorSourceGeneration == nil || *cursor.CursorSourceGeneration != execution.Generation ||
		cursor.CursorHistorySequence == nil || *cursor.CursorHistorySequence != *attempt.ProviderCursorHistorySequence {
		return problem.New(409, "active_suspend_cursor_drift", "The Provider Cursor no longer matches the active-turn checkpoint receipt.")
	}
	digest := sha256.Sum256([]byte(*cursor.Cursor))
	if !bytes.Equal(digest[:], attempt.ProviderCursorSHA256) {
		return problem.New(409, "active_suspend_cursor_drift", "The Provider Cursor changed after the active-turn checkpoint was recorded.")
	}
	return nil
}

func activeTurnCheckpointProjection(attempt persistence.ExecutionSuspendAttempt) (*ResumeActiveTurnCheckpoint, error) {
	if attempt.Reason != resourceSuspendReasonActiveIdleTimeout || attempt.Status != "completed" ||
		attempt.ResumeBundleID != nil || attempt.ResumeGeneration != nil || attempt.ResumeBoundAt != nil ||
		attempt.ResumeOutcomeUnknownAt != nil || attempt.ActiveCommandID == nil ||
		attempt.BoundaryMeaningfulActivitySequence == nil || *attempt.BoundaryMeaningfulActivitySequence <= 0 ||
		attempt.CheckpointHistorySequence == nil || attempt.CurrentTurnSequence == nil ||
		attempt.ProviderCheckpointProtocol == nil || len(attempt.CheckpointReceiptSHA256) != sha256.Size {
		return nil, errors.New("active-turn checkpoint is not available for recovery")
	}
	return &ResumeActiveTurnCheckpoint{
		SuspendAttemptID: attempt.ID, SourceGeneration: attempt.Generation,
		BoundaryMeaningfulActivitySequence: *attempt.BoundaryMeaningfulActivitySequence,
		ActiveCommandID:                    *attempt.ActiveCommandID, CheckpointHistorySequence: *attempt.CheckpointHistorySequence,
		CurrentTurnSequence: *attempt.CurrentTurnSequence, CheckpointProtocol: *attempt.ProviderCheckpointProtocol,
		ReceiptSHA256: hex.EncodeToString(attempt.CheckpointReceiptSHA256),
	}, nil
}

func loadUnboundActiveTurnCheckpoint(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (*ResumeActiveTurnCheckpoint, error) {
	if execution.Generation <= 1 || execution.NextRecoveryReason == nil || *execution.NextRecoveryReason != "suspend-resume" {
		return nil, nil
	}
	var attempt persistence.ExecutionSuspendAttempt
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND reason = ? AND status = ?",
			execution.TenantID, execution.ID, execution.Generation-1,
			resourceSuspendReasonActiveIdleTimeout, "completed").
		Order("requested_at DESC, id DESC").Take(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, problem.Wrap(500, "active_suspend_receipt_load_failed", "The active-turn checkpoint receipt could not be loaded for recovery.", err)
	}
	projection, err := activeTurnCheckpointProjection(attempt)
	if err != nil {
		return nil, problem.Wrap(409, "active_suspend_receipt_consumed", "The active-turn checkpoint receipt cannot be replayed into another recovery generation.", err)
	}
	expected, err := activeTurnCheckpointReceiptSHA256(attempt)
	if err != nil || !bytes.Equal(expected, attempt.CheckpointReceiptSHA256) {
		return nil, problem.New(409, "active_suspend_receipt_integrity_failed", "The active-turn checkpoint receipt failed integrity validation before recovery.")
	}
	return projection, nil
}

func bindActiveTurnCheckpointToRecoveryBundle(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	snapshot *ResumeSnapshot,
	bundle RecoveryBundle,
	resume providerCursorLoadResult,
	now time.Time,
) error {
	checkpoint := snapshot.ActiveTurnCheckpoint
	if checkpoint == nil {
		return nil
	}
	if resume.SelectedStrategy != "native-cursor" || resume.Cursor == nil ||
		resume.CursorSourceExecutionID == nil || *resume.CursorSourceExecutionID != execution.ID ||
		resume.CursorSourceGeneration == nil || *resume.CursorSourceGeneration != checkpoint.SourceGeneration ||
		resume.CursorHistorySequence == nil || *resume.CursorHistorySequence != checkpoint.CheckpointHistorySequence {
		return problem.New(409, "active_suspend_native_resume_required", "Active-turn suspend recovery requires the exact checkpointed native Provider Cursor.")
	}
	receiptSHA, err := hex.DecodeString(checkpoint.ReceiptSHA256)
	if err != nil || len(receiptSHA) != sha256.Size {
		return problem.New(409, "active_suspend_receipt_invalid", "The active-turn checkpoint receipt hash is invalid.")
	}
	updated := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ? AND execution_id = ? AND generation = ? AND reason = ? AND status = ? AND resume_bundle_id IS NULL AND resume_generation IS NULL AND resume_bound_at IS NULL AND resume_outcome_unknown_at IS NULL AND checkpoint_receipt_sha256 = ?",
			execution.TenantID, checkpoint.SuspendAttemptID, execution.ID, checkpoint.SourceGeneration,
			resourceSuspendReasonActiveIdleTimeout, "completed", receiptSHA).
		Updates(map[string]any{"resume_bundle_id": bundle.ID, "resume_generation": bundle.Generation, "resume_bound_at": now})
	return expectOne(updated, 409, "active_suspend_receipt_bind_conflict", "The active-turn checkpoint receipt was already consumed by another Recovery Bundle.")
}

func markBoundActiveTurnCheckpointOutcomeUnknown(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	lease persistence.WorkerLease,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND execution_id = ? AND reason = ? AND status = ? AND resume_generation = ? AND resume_bundle_id IS NOT NULL AND resume_bound_at IS NOT NULL AND resume_outcome_unknown_at IS NULL",
			execution.TenantID, execution.ID, resourceSuspendReasonActiveIdleTimeout, "completed", lease.Generation).
		Update("resume_outcome_unknown_at", now)
	if result.Error != nil {
		return false, problem.Wrap(500, "active_suspend_resume_outcome_unknown_failed", "The consumed active-turn checkpoint receipt could not be fenced.", result.Error)
	}
	return result.RowsAffected > 0, nil
}

func (s *Service) prepareActiveTurnSuspendCompletion(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	attempt persistence.ExecutionSuspendAttempt,
	lease persistence.WorkerLease,
) (bool, error) {
	if attempt.Reason != resourceSuspendReasonActiveIdleTimeout {
		return false, nil
	}
	if attempt.BoundaryMeaningfulActivitySequence == nil || *attempt.BoundaryMeaningfulActivitySequence <= 0 {
		return false, problem.New(409, "active_suspend_activity_boundary_missing", "The active-turn suspend receipt does not have a semantic activity boundary.")
	}
	outcomeUnknown, err := requeueExecutionControlCommands(
		ctx, tx, execution, lease,
		"The active Provider generation was durably suspended before the Control command could complete.",
	)
	if err != nil {
		return false, err
	}
	if outcomeUnknown {
		return false, problem.New(409, "active_suspend_primary_outcome_unknown", "A primary Provider operation crossed the active-turn suspend boundary and cannot be replayed safely.")
	}
	var session persistence.AgentSession
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "meaningful_activity_sequence").
		Where("tenant_id = ? AND id = ? AND status = ?", execution.TenantID, execution.SessionID, "active").
		Take(&session).Error; err != nil {
		return false, problem.Wrap(409, "active_suspend_session_unavailable", "The active Session could not be checked for crossed semantic activity.", err)
	}
	return session.MeaningfulActivitySequence > *attempt.BoundaryMeaningfulActivitySequence, nil
}

func activeTurnSuspendOutcomeUnknownForLostGeneration(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (bool, error) {
	var attempt persistence.ExecutionSuspendAttempt
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND reason = ? AND status IN ?",
			execution.TenantID, execution.ID, execution.Generation,
			resourceSuspendReasonActiveIdleTimeout, []string{"checkpointing", "aborted", "superseded"}).
		Order("requested_at DESC, id DESC").Take(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, problem.Wrap(500, "active_suspend_attempt_load_failed", "The active-turn suspend attempt could not be checked before generation recovery.", err)
	}
	if attempt.ProviderQuiescedAt != nil || len(attempt.CheckpointReceiptSHA256) > 0 {
		return true, nil
	}
	if attempt.ControlCommandID == nil {
		return false, nil
	}
	var command persistence.ExecutionControlCommand
	err = tx.WithContext(ctx).
		Select("id", "status", "delivered_at").
		Where("tenant_id = ? AND execution_id = ? AND id = ? AND command_type = ?",
			execution.TenantID, execution.ID, *attempt.ControlCommandID, activeTurnSuspendCommandType).
		Take(&command).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, problem.Wrap(500, "active_suspend_control_load_failed", "The active-turn Suspend command could not be checked before generation recovery.", err)
	}
	return command.DeliveredAt != nil || command.Status == "delivered" || command.Status == "acknowledged", nil
}

func (s *Service) activeTurnResumeCursorAvailable(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (bool, error) {
	if execution.NextRecoveryReason == nil || *execution.NextRecoveryReason != "suspend-resume" {
		return true, nil
	}
	var attempt persistence.ExecutionSuspendAttempt
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND reason = ? AND status = ?",
			execution.TenantID, execution.ID, execution.Generation,
			resourceSuspendReasonActiveIdleTimeout, "completed").
		Order("requested_at DESC, id DESC").Take(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, problem.Wrap(500, "active_suspend_receipt_load_failed", "The active-turn checkpoint receipt could not be checked before resume.", err)
	}
	if attempt.ResumeBundleID != nil || attempt.ResumeGeneration != nil || attempt.ResumeBoundAt != nil ||
		attempt.ResumeOutcomeUnknownAt != nil {
		return false, nil
	}
	if err := s.requireActiveTurnCheckpointReceipt(ctx, tx, execution, attempt); err != nil {
		var apiError *problem.Error
		if errors.As(err, &apiError) && apiError.Status < 500 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func suspendStatusMatchesReason(execution persistence.AgentExecution, attempt persistence.ExecutionSuspendAttempt) bool {
	switch attempt.Reason {
	case resourceSuspendReasonWaitingKeepAlive:
		return execution.Status == "waiting-for-approval"
	case resourceSuspendReasonActiveIdleTimeout:
		return execution.Status == "running"
	default:
		return false
	}
}

func suspendExpectedExecutionStatus(attempt persistence.ExecutionSuspendAttempt) string {
	if attempt.Reason == resourceSuspendReasonActiveIdleTimeout {
		return "running"
	}
	return "waiting-for-approval"
}
