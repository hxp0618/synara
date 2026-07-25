package sessions

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

type ExecutionLaunchTarget struct {
	Target             persistence.ExecutionTarget
	RoutingSelection   *routing.Selection
	PlacementSelection placement.Selection
}

type PoolCapabilityGate func(
	context.Context,
	*gorm.DB,
	persistence.ExecutionTarget,
	placement.Selection,
	*routing.Selection,
) error

func SelectExecutionLaunchTarget(
	ctx context.Context,
	tx *gorm.DB,
	fixedTarget *persistence.ExecutionTarget,
	routeRequest *routing.SelectRequest,
	warmPoolMode string,
	capabilityGate PoolCapabilityGate,
) (ExecutionLaunchTarget, error) {
	if fixedTarget == nil && routeRequest == nil {
		return ExecutionLaunchTarget{}, problem.New(
			500,
			"execution_target_selection_scope_required",
			"Execution target selection requires either a fixed target or a routing request.",
		)
	}

	planner := placement.NewService(nonNilDB(tx))
	excluded := cloneExcludedTargets(routeRequest)
	var lastRejected error
	for {
		target, selection, err := nextExecutionLaunchCandidate(ctx, tx, fixedTarget, routeRequest, excluded)
		if err != nil {
			if lastRejected != nil && executionLaunchCandidateExhausted(err) {
				return ExecutionLaunchTarget{}, lastRejected
			}
			return ExecutionLaunchTarget{}, err
		}

		previewSelection, err := planner.PreviewExecution(ctx, tx, target, warmPoolMode)
		if err != nil {
			if routeRequest != nil && isRetryableExecutionLaunchCandidateError(err) {
				lastRejected = err
				excluded = appendExcludedTarget(excluded, target.ID)
				continue
			}
			return ExecutionLaunchTarget{}, err
		}
		if capabilityGate != nil {
			if err := capabilityGate(ctx, tx, target, previewSelection, selection); err != nil {
				if routeRequest != nil && isRetryableExecutionLaunchCandidateError(err) {
					lastRejected = err
					excluded = appendExcludedTarget(excluded, target.ID)
					continue
				}
				return ExecutionLaunchTarget{}, err
			}
		}

		finalSelection, err := planner.SelectExecution(ctx, tx, target, warmPoolMode)
		if err != nil {
			if routeRequest != nil && isRetryableExecutionLaunchCandidateError(err) {
				lastRejected = err
				excluded = appendExcludedTarget(excluded, target.ID)
				continue
			}
			return ExecutionLaunchTarget{}, err
		}
		if capabilityGate != nil && !samePlacementSelection(previewSelection, finalSelection) {
			if err := capabilityGate(ctx, tx, target, finalSelection, selection); err != nil {
				if routeRequest != nil && isRetryableExecutionLaunchCandidateError(err) {
					lastRejected = err
					excluded = appendExcludedTarget(excluded, target.ID)
					continue
				}
				return ExecutionLaunchTarget{}, err
			}
		}

		return ExecutionLaunchTarget{
			Target:             target,
			RoutingSelection:   selection,
			PlacementSelection: finalSelection,
		}, nil
	}
}

func nextExecutionLaunchCandidate(
	ctx context.Context,
	tx *gorm.DB,
	fixedTarget *persistence.ExecutionTarget,
	routeRequest *routing.SelectRequest,
	excluded []uuid.UUID,
) (persistence.ExecutionTarget, *routing.Selection, error) {
	if routeRequest == nil {
		return *fixedTarget, nil, nil
	}
	request := *routeRequest
	request.ExcludedTargetIDs = append([]uuid.UUID(nil), excluded...)
	selection, err := routing.NewService(nonNilDB(tx)).Select(ctx, tx, request)
	if err != nil {
		return persistence.ExecutionTarget{}, nil, err
	}
	return selection.Target, &selection, nil
}

func cloneExcludedTargets(request *routing.SelectRequest) []uuid.UUID {
	if request == nil || len(request.ExcludedTargetIDs) == 0 {
		return nil
	}
	return append([]uuid.UUID(nil), request.ExcludedTargetIDs...)
}

func appendExcludedTarget(excluded []uuid.UUID, targetID uuid.UUID) []uuid.UUID {
	if targetID == uuid.Nil {
		return excluded
	}
	for _, existing := range excluded {
		if existing == targetID {
			return excluded
		}
	}
	return append(excluded, targetID)
}

func samePlacementSelection(left, right placement.Selection) bool {
	return left.Pool.ID == right.Pool.ID &&
		left.Pool.Version == right.Pool.Version &&
		left.CapacityClass == right.CapacityClass &&
		left.PolicyVersion == right.PolicyVersion
}

func executionLaunchCandidateExhausted(err error) bool {
	switch executionLaunchProblemCode(err) {
	case "target_group_has_no_members", "target_group_no_eligible_destination":
		return true
	default:
		return false
	}
}

func isRetryableExecutionLaunchCandidateError(err error) bool {
	switch executionLaunchProblemCode(err) {
	case
		"capability_unsupported",
		"execution_placement_policy_invalid",
		"execution_placement_pool_not_found",
		"execution_target_unavailable",
		"provider_not_installed",
		"provider_version_incompatible",
		"worker_manifest_reregistration_required",
		"worker_manifest_required":
		return true
	default:
		return false
	}
}

func executionLaunchProblemCode(err error) string {
	var apiError *problem.Error
	if !errors.As(err, &apiError) {
		return ""
	}
	return apiError.Code
}

func nonNilDB(tx *gorm.DB) *gorm.DB {
	if tx != nil {
		return tx
	}
	return nil
}
