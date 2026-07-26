package executions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	workerClaimReleaseAuthorityWorker       = "worker"
	workerClaimReleaseAuthorityUser         = "user"
	workerClaimReleaseAuthorityControlPlane = "control-plane"
	workerClaimReleaseAuthorityKubernetes   = "kubernetes"
)

const (
	workerClaimReleaseExecutionCompleted              = "execution_completed"
	workerClaimReleaseExecutionFailed                 = "execution_failed"
	workerClaimReleaseWorkerReleased                  = "worker_released"
	workerClaimReleaseOrphanLeaseExpired              = "orphan_lease_expired"
	workerClaimReleaseLeaseExpired                    = "lease_expired"
	workerClaimReleaseUserCancelled                   = "user_cancelled"
	workerClaimReleaseTenantDeleted                   = "tenant_deleted"
	workerClaimReleaseSessionAbsoluteExpired          = "session_absolute_expired"
	workerClaimReleaseInteractionExpired              = "interaction_expired"
	workerClaimReleaseResourceSuspendedWorkerAttested = "resource_suspended_worker_attested"
	workerClaimReleaseResourceSuspendedPodTerminal    = "resource_suspended_pod_terminal"
	workerClaimReleaseControlInterrupted              = "control_interrupted"
	workerClaimReleaseControlOperationCompleted       = "control_operation_completed"
	workerClaimReleaseWorkerRevoked                   = "worker_revoked"
	workerClaimReleaseCleanupAcknowledged             = "cleanup_acknowledged"
	workerClaimReleaseCleanupFailedRetryable          = "cleanup_failed_retryable"
	workerClaimReleaseCleanupFailedTerminal           = "cleanup_failed_terminal"
	workerClaimReleaseCleanupWorkerReleased           = "cleanup_worker_released"
	workerClaimReleaseCleanupLeaseExpired             = "cleanup_lease_expired"
	workerClaimReleaseCleanupAttemptsExhausted        = "cleanup_attempts_exhausted"
	workerClaimReleaseCleanupWorkerRevoked            = "cleanup_worker_revoked"
	workerClaimReleaseCleanupPodConfirmedAbsent       = "cleanup_pod_confirmed_absent"
)

var validWorkerClaimReleaseReasons = map[string]struct{}{
	workerClaimReleaseExecutionCompleted: {}, workerClaimReleaseExecutionFailed: {},
	workerClaimReleaseWorkerReleased: {}, workerClaimReleaseOrphanLeaseExpired: {},
	workerClaimReleaseLeaseExpired: {}, workerClaimReleaseUserCancelled: {},
	workerClaimReleaseTenantDeleted: {}, workerClaimReleaseSessionAbsoluteExpired: {},
	workerClaimReleaseInteractionExpired: {}, workerClaimReleaseResourceSuspendedWorkerAttested: {},
	workerClaimReleaseResourceSuspendedPodTerminal: {}, workerClaimReleaseControlInterrupted: {},
	workerClaimReleaseControlOperationCompleted: {}, workerClaimReleaseWorkerRevoked: {},
	workerClaimReleaseCleanupAcknowledged: {}, workerClaimReleaseCleanupFailedRetryable: {},
	workerClaimReleaseCleanupFailedTerminal: {}, workerClaimReleaseCleanupWorkerReleased: {},
	workerClaimReleaseCleanupLeaseExpired: {}, workerClaimReleaseCleanupAttemptsExhausted: {},
	workerClaimReleaseCleanupWorkerRevoked: {}, workerClaimReleaseCleanupPodConfirmedAbsent: {},
}

var validWorkerClaimReleaseAuthorities = map[string]struct{}{
	workerClaimReleaseAuthorityWorker: {}, workerClaimReleaseAuthorityUser: {},
	workerClaimReleaseAuthorityControlPlane: {}, workerClaimReleaseAuthorityKubernetes: {},
}

type workerClaimReleaseInput struct {
	ClaimKind                 string
	ExecutionID               *uuid.UUID
	ExecutionGeneration       *int64
	CleanupCommandID          *uuid.UUID
	CleanupDispatchGeneration *int64
	ReleasedAt                time.Time
	RecordedAt                time.Time
	ReleaseReason             string
	AuthorityKind             string
	AuthorityID               string
	RequestID                 string
	Metadata                  map[string]any
}

func recordWorkerClaimReleaseFact(
	ctx context.Context,
	tx *gorm.DB,
	input workerClaimReleaseInput,
) error {
	if !workerClaimReleaseFactsAvailable(tx) {
		return nil
	}
	if _, ok := validWorkerClaimReleaseReasons[input.ReleaseReason]; !ok {
		return problem.New(500, "worker_claim_release_reason_invalid", "The Worker claim release reason is invalid.")
	}
	if _, ok := validWorkerClaimReleaseAuthorities[input.AuthorityKind]; !ok {
		return problem.New(500, "worker_claim_release_authority_invalid", "The Worker claim release authority is invalid.")
	}
	if input.ReleasedAt.IsZero() || input.RecordedAt.IsZero() {
		return problem.New(500, "worker_claim_release_time_missing", "The Worker claim release timestamps are missing.")
	}
	if input.RecordedAt.Before(input.ReleasedAt) {
		return problem.New(500, "worker_claim_release_time_invalid", "The Worker claim release record precedes its effective release time.")
	}

	claimFactID, found, err := findWorkerClaimFactID(ctx, tx, input)
	if err != nil {
		return err
	}
	if !found {
		// First-phase rolling upgrade compatibility: migration-era claims can lack
		// a claim fact. A later backfill + writer-version gate can make this strict.
		return nil
	}

	metadata := input.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return problem.Wrap(500, "worker_claim_release_metadata_invalid", "The Worker claim release metadata is invalid or too large.", err)
	}
	if len(metadataJSON) > 4000 {
		return problem.New(500, "worker_claim_release_metadata_invalid", "The Worker claim release metadata is invalid or too large.")
	}
	authorityID := normalizedOptionalString(input.AuthorityID)
	requestID := normalizedOptionalString(input.RequestID)
	model := persistence.WorkerClaimReleaseFact{
		ClaimFactID: claimFactID, ReleasedAt: input.ReleasedAt.UTC(), RecordedAt: input.RecordedAt.UTC(),
		ReleaseReason: input.ReleaseReason, AuthorityKind: input.AuthorityKind,
		AuthorityID: authorityID, RequestID: requestID, Metadata: metadata,
	}
	created := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&model)
	if created.Error != nil {
		return problem.Wrap(500, "worker_claim_release_create_failed", "The Worker claim release fact could not be recorded.", created.Error)
	}
	if created.RowsAffected == 1 {
		return nil
	}

	var existing persistence.WorkerClaimReleaseFact
	if err := tx.WithContext(ctx).Where("claim_fact_id = ?", claimFactID).Take(&existing).Error; err != nil {
		return problem.Wrap(500, "worker_claim_release_load_failed", "The existing Worker claim release fact could not be loaded.", err)
	}
	if !workerClaimReleaseFactsEqual(existing, model) {
		return problem.New(409, "worker_claim_release_conflict", "The Worker claim was already released with different authoritative facts.")
	}
	return nil
}

func findWorkerClaimFactID(
	ctx context.Context,
	tx *gorm.DB,
	input workerClaimReleaseInput,
) (uuid.UUID, bool, error) {
	query := tx.WithContext(ctx).Model(&persistence.WorkerClaimFact{}).Select("id")
	switch {
	case input.ClaimKind == workerClaimKindExecution && input.ExecutionID != nil && input.ExecutionGeneration != nil:
		query = query.Where(
			"claim_kind = ? AND execution_id = ? AND execution_generation = ?",
			workerClaimKindExecution, *input.ExecutionID, *input.ExecutionGeneration,
		)
	case input.ClaimKind == workerClaimKindWorkspaceCleanup && input.CleanupCommandID != nil && input.CleanupDispatchGeneration != nil:
		query = query.Where(
			"claim_kind = ? AND cleanup_command_id = ? AND cleanup_dispatch_generation = ?",
			workerClaimKindWorkspaceCleanup, *input.CleanupCommandID, *input.CleanupDispatchGeneration,
		)
	default:
		return uuid.Nil, false, problem.New(500, "worker_claim_release_identity_invalid", "The Worker claim release identity is invalid.")
	}
	var claim persistence.WorkerClaimFact
	if err := query.Take(&claim).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, problem.Wrap(500, "worker_claim_release_claim_load_failed", "The Worker claim fact could not be loaded for release.", err)
	}
	return claim.ID, true, nil
}

func workerClaimReleaseFactsEqual(left, right persistence.WorkerClaimReleaseFact) bool {
	if left.ClaimFactID != right.ClaimFactID || !left.ReleasedAt.Equal(right.ReleasedAt) ||
		!left.RecordedAt.Equal(right.RecordedAt) || left.ReleaseReason != right.ReleaseReason ||
		left.AuthorityKind != right.AuthorityKind || !optionalStringsEqual(left.AuthorityID, right.AuthorityID) ||
		!optionalStringsEqual(left.RequestID, right.RequestID) {
		return false
	}
	leftJSON, leftErr := json.Marshal(left.Metadata)
	rightJSON, rightErr := json.Marshal(right.Metadata)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func normalizedOptionalString(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func optionalStringsEqual(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func workerClaimReleaseFactsAvailable(tx *gorm.DB) bool {
	return tx != nil && (tx.Dialector.Name() != "sqlite" || tx.Migrator().HasTable(&persistence.WorkerClaimReleaseFact{}))
}

func executionClaimReleaseInput(
	lease persistence.WorkerLease,
	releasedAt, recordedAt time.Time,
	reason, authorityKind, authorityID, requestID string,
) workerClaimReleaseInput {
	generation := lease.Generation
	return workerClaimReleaseInput{
		ClaimKind: workerClaimKindExecution, ExecutionID: &lease.ExecutionID, ExecutionGeneration: &generation,
		ReleasedAt: releasedAt, RecordedAt: recordedAt, ReleaseReason: reason,
		AuthorityKind: authorityKind, AuthorityID: authorityID, RequestID: requestID,
	}
}

func workspaceCleanupClaimReleaseInput(
	command persistence.WorkspaceCleanupCommand,
	releasedAt, recordedAt time.Time,
	reason, authorityKind, authorityID, requestID string,
) workerClaimReleaseInput {
	dispatchGeneration := command.DispatchGeneration
	return workerClaimReleaseInput{
		ClaimKind:        workerClaimKindWorkspaceCleanup,
		CleanupCommandID: &command.ID, CleanupDispatchGeneration: &dispatchGeneration,
		ReleasedAt: releasedAt, RecordedAt: recordedAt, ReleaseReason: reason,
		AuthorityKind: authorityKind, AuthorityID: authorityID, RequestID: requestID,
	}
}
