package executions

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

// AuthorizeArtifactWrite validates the same lease token and generation used by
// runtime events and execution completion. Callers should use the transaction
// that will create the pending Artifact so a lease cannot be replaced between
// authorization and metadata creation.
func (s *Service) AuthorizeArtifactWrite(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input LeaseInput,
) (persistence.AgentExecution, error) {
	return s.AuthorizeLease(ctx, tx, worker, executionID, input)
}

// AuthorizeLease validates that a Worker still owns the current, unexpired
// Generation for an Execution. Services that expose execution-scoped runtime
// resources should call this inside the transaction that loads those resources.
func (s *Service) AuthorizeLease(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input LeaseInput,
) (persistence.AgentExecution, error) {
	_, execution, err := s.lockLease(ctx, tx, worker.ID, executionID, input, true)
	return execution, err
}
