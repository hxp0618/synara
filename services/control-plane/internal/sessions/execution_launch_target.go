package sessions

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
)

type ExecutionLaunchPolicyScope struct {
	TenantID       uuid.UUID
	OrganizationID uuid.UUID
	Provider       string
}

type ExecutionLaunchTarget struct {
	Target                   persistence.ExecutionTarget
	RoutingSelection         *routing.Selection
	PlacementSelection       placement.Selection
	SchedulingPolicySnapshot schedulingpolicy.Snapshot
	PlacementRegion          string
	PlacementClusterID       string
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
	policyScope ExecutionLaunchPolicyScope,
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
	if tx == nil {
		return ExecutionLaunchTarget{}, problem.New(
			500,
			"execution_target_selection_transaction_required",
			"Execution target selection requires an active transaction.",
		)
	}
	if policyScope.TenantID == uuid.Nil || policyScope.OrganizationID == uuid.Nil {
		return ExecutionLaunchTarget{}, problem.New(
			500,
			"execution_scheduling_policy_scope_required",
			"Tenant and Organization are required for Execution Scheduling Policy admission.",
		)
	}
	if routeRequest != nil && (routeRequest.TenantID != policyScope.TenantID ||
		routeRequest.OrganizationID != policyScope.OrganizationID || routeRequest.Provider != policyScope.Provider) {
		return ExecutionLaunchTarget{}, problem.New(
			500,
			"execution_scheduling_policy_scope_mismatch",
			"Routing and Execution Scheduling Policy scopes do not match.",
		)
	}

	planner := placement.NewService(nonNilDB(tx))
	policyService := schedulingpolicy.NewService(nonNilDB(tx))
	var fixedPolicySnapshot schedulingpolicy.Snapshot
	if routeRequest == nil {
		organizationID := policyScope.OrganizationID
		var err error
		fixedPolicySnapshot, err = policyService.Resolve(
			ctx,
			tx,
			policyScope.TenantID,
			&organizationID,
		)
		if err != nil {
			return ExecutionLaunchTarget{}, executionSchedulingPolicyLoadProblem(err)
		}
	}
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
		policySnapshot := fixedPolicySnapshot
		if selection != nil {
			policySnapshot = selection.SchedulingPolicySnapshot
		}
		placementRegion, placementClusterID, err := effectiveExecutionPlacementLocation(selection, previewSelection.Pool)
		if err != nil {
			if routeRequest != nil && isRetryableExecutionLaunchCandidateError(err) {
				lastRejected = err
				excluded = appendExcludedTarget(excluded, target.ID)
				continue
			}
			return ExecutionLaunchTarget{}, err
		}
		if !schedulingpolicy.AllowsTarget(policySnapshot.Effective, schedulingpolicy.Target{
			ID: target.ID, Region: placementRegion, Cluster: placementClusterID, Provider: policyScope.Provider,
		}) || !schedulingpolicy.AllowsPlacement(policySnapshot.Effective, previewSelection.CapacityClass) {
			err := executionSchedulingPolicyDenied(policySnapshot)
			if routeRequest != nil {
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
		if selection != nil {
			// Candidate preview and the first capability check deliberately run
			// without routing locks so an unsupported destination can be skipped
			// cheaply. Before placement takes its commit locks, linearize the
			// routing decision against every mutable routing authority. A stale
			// decision must escape to the idempotent transaction boundary rather
			// than being treated as a reason to select a different destination.
			lockedSelection, err := routing.NewService(nonNilDB(tx)).LockSelectionForCommit(
				ctx,
				tx,
				*routeRequest,
				*selection,
			)
			if err != nil {
				return ExecutionLaunchTarget{}, err
			}
			selection = &lockedSelection
			target = lockedSelection.Target
			policySnapshot = lockedSelection.SchedulingPolicySnapshot
		} else {
			organizationID := policyScope.OrganizationID
			if err := policyService.LockEffectiveForCommit(
				ctx,
				tx,
				policyScope.TenantID,
				&organizationID,
				policySnapshot,
			); err != nil {
				if errors.Is(err, schedulingpolicy.ErrPolicyStale) {
					return ExecutionLaunchTarget{}, problem.New(
						409,
						"execution_scheduling_policy_stale",
						"Execution Scheduling Policy changed after selection; retry the launch.",
					)
				}
				return ExecutionLaunchTarget{}, executionSchedulingPolicyLoadProblem(err)
			}
			target, err = lockFixedExecutionTargetForCommit(ctx, tx, target.ID, policyScope)
			if err != nil {
				return ExecutionLaunchTarget{}, err
			}
		}

		finalSelection, err := planner.SelectExecution(ctx, tx, target, warmPoolMode)
		if err != nil {
			return ExecutionLaunchTarget{}, err
		}
		placementRegion, placementClusterID, err = effectiveExecutionPlacementLocation(selection, finalSelection.Pool)
		if err != nil {
			return ExecutionLaunchTarget{}, err
		}
		if !schedulingpolicy.AllowsTarget(policySnapshot.Effective, schedulingpolicy.Target{
			ID: target.ID, Region: placementRegion, Cluster: placementClusterID, Provider: policyScope.Provider,
		}) || !schedulingpolicy.AllowsPlacement(policySnapshot.Effective, finalSelection.CapacityClass) {
			return ExecutionLaunchTarget{}, executionSchedulingPolicyDenied(policySnapshot)
		}
		// Preview keeps candidate iteration cheap, but only the final selection is
		// protected by the Target/placement locks retained through the caller's
		// transaction. Always repeat the capability decision after those locks are
		// acquired: Worker registration and heartbeat projection may change even
		// when the selected pool and policy versions do not.
		if capabilityGate != nil {
			if err := capabilityGate(ctx, tx, target, finalSelection, selection); err != nil {
				return ExecutionLaunchTarget{}, err
			}
		}

		return ExecutionLaunchTarget{
			Target:                   target,
			RoutingSelection:         selection,
			PlacementSelection:       finalSelection,
			SchedulingPolicySnapshot: policySnapshot,
			PlacementRegion:          placementRegion,
			PlacementClusterID:       placementClusterID,
		}, nil
	}
}

func ApplyExecutionLaunchTarget(execution *persistence.AgentExecution, launchTarget ExecutionLaunchTarget) {
	if execution == nil {
		return
	}
	placement.ApplySelection(execution, launchTarget.PlacementSelection)
	execution.PlacementRegion = launchTarget.PlacementRegion
	execution.PlacementClusterID = launchTarget.PlacementClusterID
	execution.TenantSchedulingPolicyVersion = launchTarget.SchedulingPolicySnapshot.Tenant.Version
	execution.TenantSchedulingPolicyDigest = launchTarget.SchedulingPolicySnapshot.Tenant.Digest
	execution.OrganizationSchedulingPolicyVersion = 0
	execution.OrganizationSchedulingPolicyDigest = schedulingpolicy.UnrestrictedDigest
	if organization := launchTarget.SchedulingPolicySnapshot.Organization; organization != nil {
		execution.OrganizationSchedulingPolicyVersion = organization.Version
		execution.OrganizationSchedulingPolicyDigest = organization.Digest
	}
	if launchTarget.RoutingSelection != nil {
		routing.ApplyExecutionSelection(execution, *launchTarget.RoutingSelection)
	}
}

func effectiveExecutionPlacementLocation(
	routeSelection *routing.Selection,
	pool placement.Pool,
) (string, string, error) {
	poolRegion := strings.TrimSpace(pool.Region)
	poolClusterID := strings.TrimSpace(pool.ClusterID)
	if poolRegion != pool.Region || poolClusterID != pool.ClusterID {
		return "", "", problem.New(
			409,
			"execution_placement_location_invalid",
			"Execution placement location contains non-canonical whitespace.",
		)
	}
	if routeSelection == nil {
		return poolRegion, poolClusterID, nil
	}
	memberRegion := routeSelection.Member.Region
	memberClusterID := routeSelection.Member.ClusterID
	if (poolRegion != "" && poolRegion != memberRegion) ||
		(poolClusterID != "" && poolClusterID != memberClusterID) {
		return "", "", problem.New(
			409,
			"execution_placement_location_mismatch",
			"The selected Worker Pool location does not match its Target Group member authority.",
		)
	}
	if poolRegion == "" {
		poolRegion = memberRegion
	}
	if poolClusterID == "" {
		poolClusterID = memberClusterID
	}
	return poolRegion, poolClusterID, nil
}

func lockFixedExecutionTargetForCommit(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	policyScope ExecutionLaunchPolicyScope,
) (persistence.ExecutionTarget, error) {
	var target persistence.ExecutionTarget
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND status = ?", targetID, "active").
		Where("tenant_id IS NULL OR tenant_id = ?", policyScope.TenantID).
		Where("organization_id IS NULL OR organization_id = ?", policyScope.OrganizationID).
		Take(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, problem.New(
			409,
			"execution_target_unavailable",
			"The selected Execution Target is no longer active or within the launch scope.",
		)
	}
	if err != nil {
		return persistence.ExecutionTarget{}, problem.Wrap(
			500,
			"execution_target_lookup_failed",
			"The selected Execution Target could not be locked for launch.",
			err,
		)
	}
	return target, nil
}

func executionSchedulingPolicyLoadProblem(err error) error {
	return problem.Wrap(
		500,
		"execution_scheduling_policy_load_failed",
		"The effective Execution Scheduling Policy could not be loaded.",
		err,
	)
}

func executionSchedulingPolicyDenied(snapshot schedulingpolicy.Snapshot) error {
	apiError := problem.New(
		409,
		"execution_scheduling_policy_denied",
		"The effective Execution Scheduling Policy does not allow this launch placement.",
	)
	details := map[string]any{
		"tenantPolicyVersion": snapshot.Tenant.Version,
		"tenantPolicyDigest":  snapshot.Tenant.Digest,
	}
	if snapshot.Organization != nil {
		details["organizationPolicyVersion"] = snapshot.Organization.Version
		details["organizationPolicyDigest"] = snapshot.Organization.Digest
	}
	apiError.Details = details
	return apiError
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

func executionLaunchCandidateExhausted(err error) bool {
	switch executionLaunchProblemCode(err) {
	case "target_group_has_no_members", "target_group_no_eligible_destination", "target_group_no_policy_eligible_destination":
		return true
	default:
		return false
	}
}

func isRetryableExecutionLaunchCandidateError(err error) bool {
	switch executionLaunchProblemCode(err) {
	case
		"capability_unsupported",
		"execution_placement_location_invalid",
		"execution_placement_location_mismatch",
		"execution_placement_policy_invalid",
		"execution_placement_pool_not_found",
		"execution_scheduling_policy_denied",
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
