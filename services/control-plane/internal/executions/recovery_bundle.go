package executions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type recoveryBundlePayload struct {
	SchemaVersion                int                       `json:"schemaVersion"`
	ExecutionID                  uuid.UUID                 `json:"executionId"`
	SessionID                    uuid.UUID                 `json:"sessionId"`
	TurnID                       uuid.UUID                 `json:"turnId"`
	Generation                   int64                     `json:"generation"`
	RecoveryReason               string                    `json:"recoveryReason"`
	PreviousBundleID             *uuid.UUID                `json:"previousBundleId,omitempty"`
	AuthoritativeHistorySequence int64                     `json:"authoritativeHistorySequence"`
	Execution                    RecoveryExecutionSnapshot `json:"execution"`
	Workload                     Workload                  `json:"workload"`
}

func (s *Service) createRecoveryBundle(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workload Workload,
	createdAt time.Time,
) (RecoveryBundle, error) {
	if workload.ResumeSnapshot == nil {
		return RecoveryBundle{}, problem.New(
			500,
			"recovery_bundle_resume_snapshot_missing",
			"The Recovery Bundle requires an authoritative Resume Snapshot.",
		)
	}
	if workload.MemoryReferences == nil {
		workload.MemoryReferences = make([]RecoveryMemoryReference, 0)
	}
	workload.RecoveryBundle = nil

	reason, previousID, err := resolveRecoveryBundleLineage(ctx, tx, execution)
	if err != nil {
		return RecoveryBundle{}, err
	}

	executionSnapshot := RecoveryExecutionSnapshot{
		ExecutionTargetID:          execution.ExecutionTargetID,
		TargetKind:                 execution.TargetKind,
		TargetGroupID:              execution.TargetGroupID,
		TargetGroupVersion:         execution.TargetGroupVersion,
		TargetGroupMemberVersion:   execution.TargetGroupMemberVersion,
		SelectedRegion:             execution.SelectedRegion,
		SelectedClusterID:          execution.SelectedClusterID,
		RoutingReason:              execution.RoutingReason,
		PredecessorExecutionID:     execution.PredecessorExecutionID,
		WorkerManifestID:           execution.WorkerManifestID,
		WorkerReleaseRevisionID:    execution.WorkerReleaseRevisionID,
		WorkerReleaseChannel:       execution.WorkerReleaseChannel,
		Provider:                   execution.Provider,
		ProviderRuntimeBindingID:   execution.ProviderRuntimeBindingID,
		ProviderCredentialID:       execution.ProviderCredentialIDSnapshot,
		ProviderCredentialVersion:  execution.ProviderCredentialVersionSnapshot,
		ProviderResumeStrategy:     execution.ProviderResumeStrategySnapshot,
		RemoteWorkspaceID:          execution.RemoteWorkspaceID,
		WorkspaceMaterializationID: execution.WorkspaceMaterializationID,
		RestoreCheckpointID:        execution.RestoreCheckpointID,
	}
	payload := recoveryBundlePayload{
		SchemaVersion:                RecoveryBundleSchemaVersionV1,
		ExecutionID:                  execution.ID,
		SessionID:                    execution.SessionID,
		TurnID:                       execution.TurnID,
		Generation:                   execution.Generation,
		RecoveryReason:               reason,
		PreviousBundleID:             previousID,
		AuthoritativeHistorySequence: workload.ResumeSnapshot.AuthoritativeHistorySequence,
		Execution:                    executionSnapshot,
		Workload:                     workload,
	}
	payloadMap, payloadSHA256, err := encodeRecoveryBundlePayload(payload)
	if err != nil {
		return RecoveryBundle{}, problem.Wrap(
			500,
			"recovery_bundle_encode_failed",
			"The Recovery Bundle could not be encoded deterministically.",
			err,
		)
	}

	model := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: execution.TenantID, SessionID: execution.SessionID,
		TurnID: execution.TurnID, ExecutionID: execution.ID, Generation: execution.Generation,
		SchemaVersion: RecoveryBundleSchemaVersionV1, RecoveryReason: reason,
		PreviousBundleID: previousID, AuthoritativeHistorySequence: payload.AuthoritativeHistorySequence,
		Payload: payloadMap, PayloadSHA256: payloadSHA256, CreatedAt: createdAt,
	}
	if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
		return RecoveryBundle{}, problem.Wrap(
			409,
			"recovery_bundle_create_conflict",
			"The Execution generation Recovery Bundle could not be frozen atomically.",
			err,
		)
	}
	return recoveryBundleFromModel(model, payload), nil
}

// resolveRecoveryBundleLineage is the single authority for the immutable
// recovery reason. Dispatch facts are written before Claim, so they must use
// the exact same predecessor rule as the Recovery Bundle created at Claim.
func resolveRecoveryBundleLineage(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (string, *uuid.UUID, error) {
	if execution.NextRecoveryReason != nil && *execution.NextRecoveryReason == "disaster-recovery" {
		if execution.PredecessorExecutionID == nil {
			return "", nil, problem.New(
				409,
				"disaster_recovery_predecessor_missing",
				"The disaster-recovery Execution does not declare its fenced predecessor.",
			)
		}
		var predecessor persistence.ExecutionRecoveryBundle
		err := tx.WithContext(ctx).
			Where("tenant_id = ? AND execution_id = ?", execution.TenantID, *execution.PredecessorExecutionID).
			Order("generation DESC").Take(&predecessor).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil, problem.New(
				409,
				"disaster_recovery_bundle_missing",
				"The disaster-recovery predecessor does not have an immutable Recovery Bundle.",
			)
		}
		if err != nil {
			return "", nil, problem.Wrap(
				500,
				"disaster_recovery_bundle_load_failed",
				"The disaster-recovery predecessor Recovery Bundle could not be loaded.",
				err,
			)
		}
		predecessorID := predecessor.ID
		return "disaster-recovery", &predecessorID, nil
	}
	var previous persistence.ExecutionRecoveryBundle
	previousErr := tx.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ? AND generation < ?", execution.TenantID, execution.ID, execution.Generation).
		Order("generation DESC").Take(&previous).Error
	if previousErr != nil && !errors.Is(previousErr, gorm.ErrRecordNotFound) {
		return "", nil, problem.Wrap(
			500,
			"recovery_bundle_predecessor_load_failed",
			"The previous Recovery Bundle could not be loaded.",
			previousErr,
		)
	}
	if previousErr == nil {
		reason := "execution-recovery"
		if execution.NextRecoveryReason != nil && *execution.NextRecoveryReason == "suspend-resume" {
			reason = "suspend-resume"
		}
		previousID := previous.ID
		return reason, &previousID, nil
	}
	if execution.Generation > 1 {
		// Active generations created before migration 000043 have no historical
		// Bundle row. Adopt the frozen claim receipt once, then require exact
		// predecessor lineage for every later generation.
		return "legacy-adoption", nil, nil
	}
	return "initial-claim", nil, nil
}

func (s *Service) loadRecoveryBundle(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (RecoveryBundle, Workload, error) {
	var model persistence.ExecutionRecoveryBundle
	err := tx.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, execution.Generation).
		Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RecoveryBundle{}, Workload{}, problem.New(
			409,
			"recovery_bundle_missing",
			"The current Execution generation does not have a frozen Recovery Bundle.",
		)
	}
	if err != nil {
		return RecoveryBundle{}, Workload{}, problem.Wrap(
			500,
			"recovery_bundle_load_failed",
			"The current Recovery Bundle could not be loaded.",
			err,
		)
	}

	payload, actualSHA256, err := decodeRecoveryBundlePayload(model.Payload)
	if err != nil {
		return RecoveryBundle{}, Workload{}, problem.Wrap(
			500,
			"recovery_bundle_decode_failed",
			"The current Recovery Bundle payload is invalid.",
			err,
		)
	}
	if actualSHA256 != model.PayloadSHA256 {
		return RecoveryBundle{}, Workload{}, problem.New(
			409,
			"recovery_bundle_integrity_failed",
			"The current Recovery Bundle payload hash does not match its immutable envelope.",
		)
	}
	if err := validateRecoveryBundleEnvelope(model, payload); err != nil {
		return RecoveryBundle{}, Workload{}, err
	}

	bundle := recoveryBundleFromModel(model, payload)
	workload := payload.Workload
	workload.RecoveryBundle = &bundle
	if workload.MemoryReferences == nil {
		workload.MemoryReferences = make([]RecoveryMemoryReference, 0)
	}
	return bundle, workload, nil
}

func encodeRecoveryBundlePayload(payload recoveryBundlePayload) (map[string]any, string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(canonical)
	return normalized, hex.EncodeToString(digest[:]), nil
}

func decodeRecoveryBundlePayload(payload map[string]any) (recoveryBundlePayload, string, error) {
	canonical, err := json.Marshal(payload)
	if err != nil {
		return recoveryBundlePayload{}, "", err
	}
	digest := sha256.Sum256(canonical)
	var decoded recoveryBundlePayload
	if err := json.Unmarshal(canonical, &decoded); err != nil {
		return recoveryBundlePayload{}, "", err
	}
	return decoded, hex.EncodeToString(digest[:]), nil
}

func recoveryBundleFromModel(
	model persistence.ExecutionRecoveryBundle,
	payload recoveryBundlePayload,
) RecoveryBundle {
	return RecoveryBundle{
		ID: model.ID, SchemaVersion: model.SchemaVersion, ExecutionID: model.ExecutionID,
		SessionID: model.SessionID, TurnID: model.TurnID, Generation: model.Generation,
		RecoveryReason: model.RecoveryReason, PreviousBundleID: model.PreviousBundleID,
		AuthoritativeHistorySequence: model.AuthoritativeHistorySequence,
		Execution:                    payload.Execution, PayloadSHA256: model.PayloadSHA256, CreatedAt: model.CreatedAt,
	}
}

func validateRecoveryBundleEnvelope(
	model persistence.ExecutionRecoveryBundle,
	payload recoveryBundlePayload,
) error {
	if payload.SchemaVersion != RecoveryBundleSchemaVersionV1 ||
		model.SchemaVersion != payload.SchemaVersion ||
		model.ExecutionID != payload.ExecutionID || model.SessionID != payload.SessionID ||
		model.TurnID != payload.TurnID || model.Generation != payload.Generation ||
		model.RecoveryReason != payload.RecoveryReason ||
		!equalUUIDPointers(model.PreviousBundleID, payload.PreviousBundleID) ||
		model.AuthoritativeHistorySequence != payload.AuthoritativeHistorySequence ||
		payload.Workload.SessionID != model.SessionID || payload.Workload.TurnID != model.TurnID ||
		payload.Workload.ResumeSnapshot == nil ||
		payload.Workload.ResumeSnapshot.AuthoritativeHistorySequence != model.AuthoritativeHistorySequence {
		return problem.New(
			409,
			"recovery_bundle_envelope_mismatch",
			"The current Recovery Bundle payload does not match its immutable execution envelope.",
		)
	}
	return nil
}

// ValidateRecoveryBundle verifies that the Workload received by agentd is the
// exact, content-hashed payload frozen by the Control Plane for this
// generation. Kubernetes executions are the Stage 4 scale-to-zero boundary
// and fail closed without a Bundle. Other target kinds temporarily retain the
// rolling-upgrade compatibility path until Recovery Bundle support is a
// negotiated Worker capability.
func ValidateRecoveryBundle(execution Execution, workload Workload) error {
	bundle := workload.RecoveryBundle
	if bundle == nil {
		if execution.TargetKind == "kubernetes" {
			return problem.New(
				409,
				"recovery_bundle_required",
				"Kubernetes executions require an immutable Recovery Bundle before Provider startup.",
			)
		}
		return nil
	}
	if bundle.SchemaVersion != RecoveryBundleSchemaVersionV1 ||
		bundle.ExecutionID != execution.ID || bundle.SessionID != execution.SessionID ||
		bundle.TurnID != execution.TurnID || bundle.Generation != execution.Generation ||
		bundle.Execution.ExecutionTargetID != execution.ExecutionTargetID ||
		bundle.Execution.TargetKind != execution.TargetKind ||
		!equalUUIDPointers(bundle.Execution.TargetGroupID, execution.TargetGroupID) ||
		!equalInt64Pointers(bundle.Execution.TargetGroupVersion, execution.TargetGroupVersion) ||
		!equalInt64Pointers(bundle.Execution.TargetGroupMemberVersion, execution.TargetGroupMemberVersion) ||
		!equalStringPointers(bundle.Execution.SelectedRegion, execution.SelectedRegion) ||
		!equalStringPointers(bundle.Execution.SelectedClusterID, execution.SelectedClusterID) ||
		!equalStringPointers(bundle.Execution.RoutingReason, execution.RoutingReason) ||
		!equalUUIDPointers(bundle.Execution.PredecessorExecutionID, execution.PredecessorExecutionID) ||
		!equalUUIDPointers(bundle.Execution.WorkerManifestID, execution.WorkerManifestID) ||
		!equalUUIDPointers(bundle.Execution.WorkerReleaseRevisionID, execution.WorkerReleaseRevisionID) ||
		!equalStringPointers(bundle.Execution.WorkerReleaseChannel, execution.WorkerReleaseChannel) ||
		!equalStringPointers(bundle.Execution.Provider, execution.Provider) ||
		!equalUUIDPointers(bundle.Execution.ProviderRuntimeBindingID, execution.ProviderRuntimeBindingID) ||
		!equalUUIDPointers(bundle.Execution.RemoteWorkspaceID, execution.RemoteWorkspaceID) ||
		!equalUUIDPointers(bundle.Execution.WorkspaceMaterializationID, execution.WorkspaceMaterializationID) ||
		!equalUUIDPointers(bundle.Execution.RestoreCheckpointID, execution.RestoreCheckpointID) ||
		!equalUUIDPointers(bundle.Execution.ProviderCredentialID, workload.ProviderCredentialID) ||
		workload.ResumeSnapshot == nil ||
		bundle.AuthoritativeHistorySequence != workload.ResumeSnapshot.AuthoritativeHistorySequence {
		return problem.New(
			409,
			"recovery_bundle_claim_mismatch",
			"The claimed Workload does not match its immutable Recovery Bundle envelope.",
		)
	}

	frozenWorkload := workload
	frozenWorkload.RecoveryBundle = nil
	if frozenWorkload.MemoryReferences == nil {
		frozenWorkload.MemoryReferences = make([]RecoveryMemoryReference, 0)
	}
	payload := recoveryBundlePayload{
		SchemaVersion: bundle.SchemaVersion, ExecutionID: bundle.ExecutionID,
		SessionID: bundle.SessionID, TurnID: bundle.TurnID, Generation: bundle.Generation,
		RecoveryReason: bundle.RecoveryReason, PreviousBundleID: bundle.PreviousBundleID,
		AuthoritativeHistorySequence: bundle.AuthoritativeHistorySequence,
		Execution:                    bundle.Execution, Workload: frozenWorkload,
	}
	_, actualSHA256, err := encodeRecoveryBundlePayload(payload)
	if err != nil {
		return problem.Wrap(
			500,
			"recovery_bundle_claim_encode_failed",
			"The claimed Workload Recovery Bundle could not be verified.",
			err,
		)
	}
	if actualSHA256 != bundle.PayloadSHA256 {
		return problem.New(
			409,
			"recovery_bundle_claim_integrity_failed",
			"The claimed Workload does not match the Recovery Bundle content hash.",
		)
	}
	return nil
}

func equalUUIDPointers(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalStringPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalInt64Pointers(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
