package executions

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

// AuthorizeMemoryArtifactRead validates that the Worker owns the current
// Generation and that the requested immutable Memory Revision is frozen in
// that Generation's Recovery Bundle. A mutable Memory Head is never consulted
// on this delivery path.
func (s *Service) AuthorizeMemoryArtifactRead(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	executionID, revisionID uuid.UUID,
	input LeaseInput,
) (persistence.AgentExecution, RecoveryMemoryReference, error) {
	execution, err := s.AuthorizeLease(ctx, tx, worker, executionID, input)
	if err != nil {
		return persistence.AgentExecution{}, RecoveryMemoryReference{}, err
	}
	_, workload, err := s.loadRecoveryBundle(ctx, tx, execution)
	if err != nil {
		return persistence.AgentExecution{}, RecoveryMemoryReference{}, err
	}
	for _, reference := range workload.MemoryReferences {
		if reference.RevisionID == revisionID {
			return execution, reference, nil
		}
	}
	return persistence.AgentExecution{}, RecoveryMemoryReference{}, problem.New(
		409,
		"memory_revision_not_frozen",
		"The Agent Memory Revision is not part of this Execution Generation Recovery Bundle.",
	)
}

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
	_, execution, err := s.lockLease(ctx, tx, worker, executionID, input, true)
	return execution, err
}

// AuthorizeLeaseWithinSessionLifetime additionally fences delivery of runtime
// secrets and other capabilities at the immutable Session absolute boundary.
// Terminal reporting and cancellation use AuthorizeLease directly so an
// expired Worker can still converge safely without receiving new authority.
func (s *Service) AuthorizeLeaseWithinSessionLifetime(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input LeaseInput,
) (persistence.AgentExecution, error) {
	execution, err := s.AuthorizeLease(ctx, tx, worker, executionID, input)
	if err != nil {
		return persistence.AgentExecution{}, err
	}
	if err := requireExecutionTenantActive(ctx, tx, execution.TenantID); err != nil {
		return persistence.AgentExecution{}, err
	}
	if err := s.requireExecutionSessionWithinAbsoluteLifetime(ctx, tx, execution, s.now()); err != nil {
		return persistence.AgentExecution{}, err
	}
	return execution, nil
}
