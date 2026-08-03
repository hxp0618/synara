package executions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var errReceiptRace = errors.New("worker request receipt raced")

type workerRequestExecutionAuthority struct {
	ExecutionID uuid.UUID
	Lease       LeaseInput
}

type committedWorkerRequestError struct {
	err error
}

func (e *committedWorkerRequestError) Error() string {
	return e.err.Error()
}

func (e *committedWorkerRequestError) Unwrap() error {
	return e.err
}

func commitWorkerRequestError(err error) error {
	return &committedWorkerRequestError{err: err}
}

func requestHash(operation string, input any) (string, error) {
	encoded, err := json.Marshal(struct {
		Operation string `json:"operation"`
		Input     any    `json:"input"`
	}{Operation: operation, Input: input})
	if err != nil {
		return "", fmt.Errorf("encode worker request fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func responseMap(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode worker response: %w", err)
	}
	result := make(map[string]any)
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("normalize worker response: %w", err)
	}
	return result, nil
}

func decodeReceiptResponse[T any](receipt persistence.WorkerRequestReceipt) (T, error) {
	var result T
	encoded, err := json.Marshal(receipt.Response)
	if err != nil {
		return result, fmt.Errorf("encode stored worker response: %w", err)
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, fmt.Errorf("decode stored worker response: %w", err)
	}
	return result, nil
}

func runIdempotent[T any](
	ctx context.Context,
	service *Service,
	worker persistence.WorkerInstance,
	requestID, operation string,
	input any,
	statusCode int,
	apply func(*gorm.DB) (T, error),
) (OperationResult[T], error) {
	return runIdempotentWithExecutionAuthority(
		ctx, service, worker, requestID, operation, input, statusCode, nil, apply,
	)
}

func runExecutionIdempotent[T any](
	ctx context.Context,
	service *Service,
	worker persistence.WorkerInstance,
	requestID, operation string,
	executionID uuid.UUID,
	lease LeaseInput,
	input any,
	statusCode int,
	apply func(*gorm.DB) (T, error),
) (OperationResult[T], error) {
	return runIdempotentWithExecutionAuthority(
		ctx,
		service,
		worker,
		requestID,
		operation,
		input,
		statusCode,
		&workerRequestExecutionAuthority{ExecutionID: executionID, Lease: lease},
		apply,
	)
}

func runIdempotentWithExecutionAuthority[T any](
	ctx context.Context,
	service *Service,
	worker persistence.WorkerInstance,
	requestID, operation string,
	input any,
	statusCode int,
	authority *workerRequestExecutionAuthority,
	apply func(*gorm.DB) (T, error),
) (OperationResult[T], error) {
	var zero T
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 160 {
		return OperationResult[T]{}, problem.New(400, "invalid_request_id", "X-Request-ID must contain between 1 and 160 characters.")
	}
	hash, err := requestHash(operation, input)
	if err != nil {
		return OperationResult[T]{}, problem.Wrap(500, "request_fingerprint_failed", "Failed to fingerprint the worker request.", err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		value := zero
		replayed := false
		storedStatus := statusCode
		var committedError error
		err = persistence.InTransaction(ctx, service.db, func(tx *gorm.DB) error {
			receipt, lookupErr, lockErr := lockWorkerAndLoadRequestReceipt(ctx, tx, worker, requestID)
			if lockErr != nil {
				return lockErr
			}
			if lookupErr == nil {
				if receipt.WorkerIncarnation == worker.Incarnation && receipt.ExpiresAt.After(service.now()) {
					if receipt.Operation != operation || receipt.RequestHash != hash {
						return problem.New(409, "request_id_reused", "X-Request-ID was already used for a different worker request.")
					}
					if authority != nil {
						if err := validateWorkerRequestExecutionAuthority(ctx, tx, worker, receipt, *authority); err != nil {
							return err
						}
					}
					decoded, decodeErr := decodeReceiptResponse[T](receipt)
					if decodeErr != nil {
						return problem.Wrap(500, "receipt_decode_failed", "Failed to restore the idempotent worker response.", decodeErr)
					}
					value = decoded
					storedStatus = receipt.StatusCode
					replayed = true
					return nil
				}
				if err := tx.WithContext(ctx).Delete(&receipt).Error; err != nil {
					return problem.Wrap(500, "receipt_expiry_cleanup_failed", "Failed to replace an expired worker receipt.", err)
				}
			} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
				return problem.Wrap(500, "receipt_lookup_failed", "Failed to inspect the worker request receipt.", lookupErr)
			}

			value, err = apply(tx)
			if err != nil {
				var committed *committedWorkerRequestError
				if errors.As(err, &committed) {
					committedError = committed.err
					return nil
				}
				return err
			}
			mapped, mapErr := responseMap(value)
			if mapErr != nil {
				return problem.Wrap(500, "receipt_encode_failed", "Failed to persist the worker response.", mapErr)
			}
			receipt = persistence.WorkerRequestReceipt{
				WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
				RequestID: requestID, Operation: operation,
				RequestHash: hash, StatusCode: statusCode, Response: mapped,
				CreatedAt: service.now(), ExpiresAt: service.now().Add(service.receiptTTL),
			}
			if authority != nil {
				if err := bindWorkerRequestExecutionAuthority(ctx, tx, worker, &receipt, *authority); err != nil {
					return err
				}
			}
			if err := tx.WithContext(ctx).Create(&receipt).Error; err != nil {
				if errors.Is(err, gorm.ErrDuplicatedKey) {
					return errReceiptRace
				}
				return problem.Wrap(500, "receipt_create_failed", "Failed to persist the worker request receipt.", err)
			}
			return nil
		})
		if errors.Is(err, errReceiptRace) {
			continue
		}
		if err != nil {
			return OperationResult[T]{}, err
		}
		if committedError != nil {
			return OperationResult[T]{}, committedError
		}
		return OperationResult[T]{Value: value, Replayed: replayed, StatusCode: storedStatus}, nil
	}
	return OperationResult[T]{}, problem.New(409, "request_receipt_conflict", "The worker request is still being committed; retry with the same X-Request-ID.")
}

func bindWorkerRequestExecutionAuthority(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	receipt *persistence.WorkerRequestReceipt,
	authority workerRequestExecutionAuthority,
) error {
	execution, err := loadWorkerRequestAuthorityExecution(ctx, tx, authority)
	if err != nil {
		return err
	}
	if execution.ExecutionTargetID != worker.ExecutionTargetID {
		return problem.New(409, "worker_execution_target_mismatch", "The worker request no longer belongs to the Worker's registered execution target.")
	}
	if err := requireWorkerRequestSessionTarget(ctx, tx, execution, execution.ExecutionTargetID); err != nil {
		return err
	}
	receipt.TenantID = workerReceiptUUIDPointer(execution.TenantID)
	receipt.ExecutionID = workerReceiptUUIDPointer(execution.ID)
	receipt.ExecutionTargetID = workerReceiptUUIDPointer(execution.ExecutionTargetID)
	receipt.ExecutionGeneration = workerReceiptInt64Pointer(execution.Generation)
	return nil
}

func validateWorkerRequestExecutionAuthority(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	receipt persistence.WorkerRequestReceipt,
	authority workerRequestExecutionAuthority,
) error {
	if receipt.TenantID == nil || receipt.ExecutionID == nil || receipt.ExecutionTargetID == nil ||
		receipt.ExecutionGeneration == nil {
		return problem.New(409, "worker_request_authority_unbound", "This legacy Worker receipt is not bound to an Execution Generation and cannot be replayed.")
	}
	if *receipt.TenantID != authority.Lease.TenantID || *receipt.ExecutionID != authority.ExecutionID ||
		*receipt.ExecutionGeneration != authority.Lease.Generation {
		return problem.New(409, "generation_fenced", "The Worker request receipt belongs to another Execution Generation.")
	}
	execution, err := loadWorkerRequestAuthorityExecution(ctx, tx, authority)
	if err != nil {
		return err
	}
	if execution.Generation != *receipt.ExecutionGeneration {
		return problem.New(409, "generation_fenced", "The Worker request receipt generation is no longer current.")
	}
	if execution.ExecutionTargetID != *receipt.ExecutionTargetID ||
		execution.ExecutionTargetID != worker.ExecutionTargetID {
		return problem.New(409, "worker_execution_target_mismatch", "The Worker request receipt target is no longer current.")
	}
	if err := requireWorkerRequestSessionTarget(ctx, tx, execution, *receipt.ExecutionTargetID); err != nil {
		return err
	}
	return nil
}

func requireWorkerRequestSessionTarget(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	expectedTargetID uuid.UUID,
) error {
	var session persistence.AgentSession
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Select("id", "tenant_id", "execution_target_id").
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
		Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(409, "worker_execution_target_mismatch", "The Worker request receipt Session target is no longer current.")
	}
	if err != nil {
		return problem.Wrap(500, "worker_request_session_authority_lookup_failed", "Failed to inspect the Worker request Session target.", err)
	}
	if session.ExecutionTargetID != expectedTargetID {
		return problem.New(409, "worker_execution_target_mismatch", "The Worker request receipt Session target is no longer current.")
	}
	return nil
}

func loadWorkerRequestAuthorityExecution(
	ctx context.Context,
	tx *gorm.DB,
	authority workerRequestExecutionAuthority,
) (persistence.AgentExecution, error) {
	if authority.ExecutionID == uuid.Nil || authority.Lease.TenantID == uuid.Nil || authority.Lease.Generation <= 0 {
		return persistence.AgentExecution{}, problem.New(400, "invalid_lease_envelope", "tenantId and generation are required for an Execution Worker receipt.")
	}
	var execution persistence.AgentExecution
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ?", authority.Lease.TenantID, authority.ExecutionID).
		Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.AgentExecution{}, problem.New(409, "generation_fenced", "The Worker request receipt execution is no longer current.")
	}
	if err != nil {
		return persistence.AgentExecution{}, problem.Wrap(500, "worker_request_authority_lookup_failed", "Failed to inspect the Worker request receipt authority.", err)
	}
	if execution.Generation != authority.Lease.Generation {
		return persistence.AgentExecution{}, problem.New(409, "generation_fenced", "The Worker request receipt generation is no longer current.")
	}
	return execution, nil
}

func workerReceiptUUIDPointer(value uuid.UUID) *uuid.UUID { return &value }

func workerReceiptInt64Pointer(value int64) *int64 { return &value }

func lockWorkerAndLoadRequestReceipt(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	requestID string,
) (receipt persistence.WorkerRequestReceipt, lookupErr error, lockErr error) {
	// The Worker row is the serialization point for all requests from one
	// incarnation. Lock it before receipt lookup so a waiter cannot carry a
	// stale "not found" result past the preceding transaction's commit.
	if err := lockCurrentWorkerIncarnation(ctx, tx, worker); err != nil {
		return receipt, nil, err
	}
	lookupErr = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("worker_id = ? AND request_id = ?", worker.ID, requestID).
		Take(&receipt).Error
	return receipt, lookupErr, nil
}
