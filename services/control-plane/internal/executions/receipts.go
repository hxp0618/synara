package executions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var errReceiptRace = errors.New("worker request receipt raced")

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
		err = persistence.InTransaction(ctx, service.db, func(tx *gorm.DB) error {
			var receipt persistence.WorkerRequestReceipt
			lookupErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("worker_id = ? AND request_id = ?", worker.ID, requestID).Take(&receipt).Error

			if err := lockCurrentWorkerIncarnation(ctx, tx, worker); err != nil {
				return err
			}
			if lookupErr == nil {
				if receipt.WorkerIncarnation == worker.Incarnation && receipt.ExpiresAt.After(service.now()) {
					if receipt.Operation != operation || receipt.RequestHash != hash {
						return problem.New(409, "request_id_reused", "X-Request-ID was already used for a different worker request.")
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
		return OperationResult[T]{Value: value, Replayed: replayed, StatusCode: storedStatus}, nil
	}
	return OperationResult[T]{}, problem.New(409, "request_receipt_conflict", "The worker request is still being committed; retry with the same X-Request-ID.")
}
