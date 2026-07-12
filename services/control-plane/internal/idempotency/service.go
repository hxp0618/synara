package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type Scope struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	Key           string
	Operation     string
	Request       any
	SuccessStatus int
}

type Result[T any] struct {
	Value      T
	StatusCode int
	Replayed   bool
}

func Execute[T any](
	ctx context.Context,
	db *gorm.DB,
	scope Scope,
	operation func(*gorm.DB) (T, error),
) (Result[T], error) {
	var zero T
	scope.Key = strings.TrimSpace(scope.Key)
	if scope.Key == "" {
		var value T
		err := persistence.InTransaction(ctx, db, func(tx *gorm.DB) error {
			var err error
			value, err = operation(tx)
			return err
		})
		return Result[T]{Value: value, StatusCode: scope.SuccessStatus}, err
	}
	if err := validateScope(scope); err != nil {
		return Result[T]{}, err
	}
	requestHash, err := hashRequest(scope.Operation, scope.Request)
	if err != nil {
		return Result[T]{}, problem.Wrap(500, "idempotency_hash_failed", "The idempotent request could not be hashed.", err)
	}

	result := Result[T]{}
	err = persistence.InTransaction(ctx, db, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		record := persistence.APIIdempotencyKey{
			TenantID: scope.TenantID, ActorID: scope.ActorID, IdempotencyKey: scope.Key,
			Operation: scope.Operation, RequestHash: requestHash, StatusCode: 0,
			Response: map[string]any{}, CreatedAt: now,
		}
		created := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
		if created.Error != nil {
			return problem.Wrap(500, "idempotency_claim_failed", "The idempotency key could not be claimed.", created.Error)
		}
		if created.RowsAffected == 0 {
			stored, lookupErr := loadLocked(ctx, tx, scope)
			if lookupErr != nil {
				return lookupErr
			}
			if stored.Operation != scope.Operation || stored.RequestHash != requestHash {
				return problem.New(409, "idempotency_conflict", "Idempotency-Key was already used for a different request.")
			}
			if stored.CompletedAt == nil || stored.StatusCode < 200 || stored.StatusCode > 299 {
				return problem.New(409, "idempotency_in_progress", "The idempotent request is still being committed; retry with the same key.")
			}
			value, decodeErr := decodeResponse[T](stored.Response)
			if decodeErr != nil {
				return problem.Wrap(500, "idempotency_response_decode_failed", "The stored idempotent response could not be restored.", decodeErr)
			}
			result = Result[T]{Value: value, StatusCode: stored.StatusCode, Replayed: true}
			return nil
		}

		value, operationErr := operation(tx)
		if operationErr != nil {
			return operationErr
		}
		mapped, mapErr := encodeResponse(value)
		if mapErr != nil {
			return problem.Wrap(500, "idempotency_response_encode_failed", "The idempotent response could not be persisted.", mapErr)
		}
		completedAt := time.Now().UTC()
		updated := tx.WithContext(ctx).Model(&persistence.APIIdempotencyKey{}).
			Where("tenant_id = ? AND actor_id = ? AND idempotency_key = ? AND completed_at IS NULL",
				scope.TenantID, scope.ActorID, scope.Key).
			Updates(&persistence.APIIdempotencyKey{
				StatusCode: scope.SuccessStatus, Response: mapped, CompletedAt: &completedAt,
			})
		if updated.Error != nil {
			return problem.Wrap(500, "idempotency_complete_failed", "The idempotent response could not be committed.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return problem.New(409, "idempotency_state_conflict", "The idempotency key changed while the request was being committed.")
		}
		result = Result[T]{Value: value, StatusCode: scope.SuccessStatus}
		return nil
	})
	if err != nil {
		return Result[T]{Value: zero}, err
	}
	return result, nil
}

func validateScope(scope Scope) error {
	if scope.TenantID == uuid.Nil || scope.ActorID == uuid.Nil {
		return problem.New(500, "invalid_idempotency_scope", "The idempotency scope is incomplete.")
	}
	if len(scope.Key) > 200 {
		return problem.New(400, "invalid_idempotency_key", "Idempotency-Key must contain at most 200 characters.")
	}
	for _, character := range scope.Key {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return problem.New(400, "invalid_idempotency_key", "Idempotency-Key must not contain whitespace or control characters.")
		}
	}
	if strings.TrimSpace(scope.Operation) == "" || len(scope.Operation) > 120 {
		return problem.New(500, "invalid_idempotency_operation", "The idempotency operation is invalid.")
	}
	if scope.SuccessStatus < 200 || scope.SuccessStatus > 299 {
		return problem.New(500, "invalid_idempotency_status", "The idempotency success status is invalid.")
	}
	return nil
}

func hashRequest(operation string, request any) (string, error) {
	encoded, err := json.Marshal(struct {
		Operation string `json:"operation"`
		Request   any    `json:"request"`
	}{Operation: operation, Request: request})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func loadLocked(ctx context.Context, tx *gorm.DB, scope Scope) (persistence.APIIdempotencyKey, error) {
	var stored persistence.APIIdempotencyKey
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND actor_id = ? AND idempotency_key = ?", scope.TenantID, scope.ActorID, scope.Key).
		Take(&stored).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return stored, problem.New(409, "idempotency_race", "The idempotency key is being committed; retry with the same key.")
	}
	if err != nil {
		return stored, problem.Wrap(500, "idempotency_lookup_failed", "The idempotency key could not be loaded.", err)
	}
	return stored, nil
}

func encodeResponse[T any](value T) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeResponse[T any](response map[string]any) (T, error) {
	var result T
	encoded, err := json.Marshal(response)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, err
	}
	return result, nil
}
