package executions

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

// PullControlUpdates serves the Worker runner control loop in one request.
//
// A running Execution polls for Control commands and Interaction resolutions
// on the same sub-second interval. Pulled separately they cost two HTTP round
// trips and two transactions, and each transaction takes the same three
// fencing row locks (Worker incarnation, Lease, Execution) that the other one
// just took. Both reads are answered from a single lease-verified snapshot
// here, which halves the idle polling cost per running Execution and removes
// the window where the two pulls observe different fencing state.
//
// The delivery precedence is the runner's, not the server's: both sets are
// returned and the runner keeps preferring a Control command over an
// Interaction resolution, exactly as when it issued the two pulls in order.
func (s *Service) PullControlUpdates(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input PullControlUpdatesInput,
) (ControlUpdates, error) {
	commandLimit, err := normalizeControlCommandPullLimit(input.ControlCommandLimit)
	if err != nil {
		return ControlUpdates{}, err
	}
	resolutionLimit, err := normalizeInteractionResolutionPullLimit(input.InteractionResolutionLimit)
	if err != nil {
		return ControlUpdates{}, err
	}
	updates := ControlUpdates{
		ControlCommands:        make([]ControlCommandDelivery, 0),
		InteractionResolutions: make([]InteractionResolutionDelivery, 0),
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return err
		}
		commands, err := s.loadControlCommandDeliveries(
			ctx, tx, worker, execution, input.Generation, commandLimit,
		)
		if err != nil {
			return err
		}
		resolutions, err := s.loadInteractionResolutionDeliveries(
			ctx, tx, worker, execution, input.Generation, resolutionLimit,
		)
		if err != nil {
			return err
		}
		updates.ControlCommands = commands
		updates.InteractionResolutions = resolutions
		return nil
	})
	if err != nil {
		return ControlUpdates{}, err
	}
	return updates, nil
}
