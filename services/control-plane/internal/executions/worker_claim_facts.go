package executions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	workerClaimKindExecution        = "execution"
	workerClaimKindWorkspaceCleanup = "workspace-cleanup"
)

var workerClaimFactNamespace = uuid.MustParse("f0859c7b-b213-4125-8f2c-0c7ed804d020")

type workerClaimFactInput struct {
	Worker                    persistence.WorkerInstance
	TenantID                  uuid.UUID
	ExecutionTargetID         uuid.UUID
	TargetKind                string
	ClaimKind                 string
	RequestID                 string
	ClaimedAt                 time.Time
	ExecutionID               *uuid.UUID
	ExecutionGeneration       *int64
	CleanupCommandID          *uuid.UUID
	CleanupDispatchGeneration *int64
}

func recordWorkerClaimFactLocked(
	ctx context.Context,
	tx *gorm.DB,
	input workerClaimFactInput,
) error {
	if !workerClaimFactsAvailable(tx) {
		return nil
	}

	requestID := strings.TrimSpace(input.RequestID)
	if requestID == "" {
		return problem.New(500, "worker_claim_fact_request_missing", "The authoritative Worker claim ledger request id is missing.")
	}

	model := persistence.WorkerClaimFact{
		ID:                        deterministicWorkerClaimFactID(input),
		WorkerID:                  input.Worker.ID,
		WorkerIncarnation:         input.Worker.Incarnation,
		TenantID:                  input.TenantID,
		ExecutionTargetID:         input.ExecutionTargetID,
		TargetKind:                input.TargetKind,
		ClaimKind:                 input.ClaimKind,
		RequestID:                 requestID,
		ClaimedAt:                 input.ClaimedAt.UTC(),
		ExecutionID:               cloneUUIDPointer(input.ExecutionID),
		ExecutionGeneration:       cloneInt64Pointer(input.ExecutionGeneration),
		CleanupCommandID:          cloneUUIDPointer(input.CleanupCommandID),
		CleanupDispatchGeneration: cloneInt64Pointer(input.CleanupDispatchGeneration),
		CreatedAt:                 input.ClaimedAt.UTC(),
	}
	if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "request_id_reused", "X-Request-ID was already used for a different worker request.")
		}
		return problem.Wrap(500, "worker_claim_fact_create_failed", "The authoritative Worker claim ledger row could not be recorded.", err)
	}
	return nil
}

func deterministicWorkerClaimFactID(input workerClaimFactInput) uuid.UUID {
	identity := ""
	switch {
	case input.ClaimKind == workerClaimKindExecution && input.ExecutionID != nil && input.ExecutionGeneration != nil:
		identity = fmt.Sprintf("execution:%s:%d", *input.ExecutionID, *input.ExecutionGeneration)
	case input.ClaimKind == workerClaimKindWorkspaceCleanup && input.CleanupCommandID != nil && input.CleanupDispatchGeneration != nil:
		identity = fmt.Sprintf("workspace-cleanup:%s:%d", *input.CleanupCommandID, *input.CleanupDispatchGeneration)
	default:
		identity = fmt.Sprintf(
			"invalid:%s:%d:%s",
			input.Worker.ID,
			input.Worker.Incarnation,
			strings.TrimSpace(input.RequestID),
		)
	}
	return uuid.NewSHA1(workerClaimFactNamespace, []byte(identity))
}

func workerClaimFactsAvailable(tx *gorm.DB) bool {
	// Focused SQLite tests may intentionally stop before migration 000068.
	return tx != nil && (tx.Dialector.Name() != "sqlite" || tx.Migrator().HasTable(&persistence.WorkerClaimFact{}))
}
