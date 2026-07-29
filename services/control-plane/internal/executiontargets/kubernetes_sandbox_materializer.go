package executiontargets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	kubernetesSandboxAssignedExecutionLabel = "synara.io/assigned-execution-id"
	kubernetesSandboxAllocationBackendLabel = "synara.io/allocation-backend"
)

type kubernetesSandboxMaterializationResult struct {
	Allocated        int
	Bound            int
	Deleted          int
	Active           int
	Acknowledgements []persistence.ExecutionKubernetesAllocation
}

func (r *KubernetesReconciler) fenceKubernetesAllocationBackendTransition(
	ctx context.Context,
	client kubernetesClient,
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
) error {
	var allocations []persistence.ExecutionKubernetesAllocation
	if err := r.targets.db.WithContext(ctx).
		Where("execution_target_id = ? AND status <> ?", target.ID, "deleted").
		Find(&allocations).Error; err != nil {
		return problem.Wrap(500, "kubernetes_allocation_transition_load_failed", "Allocation backend transition state could not be loaded.", err)
	}
	if configuration.AllocationBackend == string(kubernetesAllocationBackendNativePod) {
		if len(allocations) == 0 {
			return nil
		}
		sandboxClient, ok := client.(kubernetesSandboxClient)
		if !ok {
			return problem.New(503, "kubernetes_allocation_transition_client_unavailable", "The previous Sandbox allocations cannot be fenced by this Kubernetes client.")
		}
		allDeleted := true
		for _, allocation := range allocations {
			deleted, err := r.deleteKubernetesSandboxAllocation(ctx, sandboxClient, allocation, r.now())
			if err != nil {
				return err
			}
			allDeleted = allDeleted && deleted
		}
		if !allDeleted {
			return problem.New(409, "kubernetes_allocation_backend_transition_pending", "Previous Sandbox allocations are still being deleted before native Pod allocation can resume.")
		}
		return problem.New(409, "kubernetes_allocation_backend_transition_observed", "Previous Sandbox allocations were deleted; native Pod allocation will resume on the next reconciliation.")
	}
	if len(allocations) > 0 {
		return nil
	}
	pods, err := client.ListPods(ctx, configuration.Namespace, target.ID)
	if err != nil {
		return problem.Wrap(502, "kubernetes_allocation_transition_pods_load_failed", "Native Pod state could not be observed before enabling Sandbox allocation.", err)
	}
	for _, pod := range pods {
		if pod.ControllerOwnerKind == "Sandbox" && strings.TrimSpace(pod.ControllerOwnerUID) != "" {
			continue
		}
		if strings.TrimSpace(pod.UID) != "" {
			return problem.New(409, "kubernetes_allocation_backend_native_pods_present", "Native Pods must be drained before Sandbox allocation can become authoritative.")
		}
	}
	return nil
}

func (r *KubernetesReconciler) reconcileSandboxAllocations(
	ctx context.Context,
	client kubernetesClient,
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	executions []kubernetesExecution,
) (kubernetesSandboxMaterializationResult, error) {
	sandboxClient, ok := client.(kubernetesSandboxClient)
	if !ok {
		return kubernetesSandboxMaterializationResult{}, problem.New(
			503, "kubernetes_sandbox_client_unavailable", "The Kubernetes client does not support SandboxClaim materialization.",
		)
	}
	observedAcceptance, err := sandboxClient.ObserveSandboxAcceptance(ctx, configuration)
	if err != nil {
		return kubernetesSandboxMaterializationResult{}, problem.Wrap(
			503, "kubernetes_sandbox_acceptance_unavailable", "Sandbox operator target acceptance could not be observed.", err,
		)
	}
	templateReady := observedAcceptance.TemplateIdentity != ""
	if configuration.AllocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
		templateReady = templateReady && observedAcceptance.TemplateSandboxRuntimeImage != ""
	} else {
		templateReady = templateReady && observedAcceptance.AssignedExecutionFieldRefReady
	}
	acceptanceObservation := KubernetesAllocationAcceptanceObservation{
		SandboxAPIReady:            observedAcceptance.SandboxAPIReady,
		SandboxClaimAPIReady:       observedAcceptance.SandboxClaimAPIReady,
		SandboxWarmPoolAPIReady:    observedAcceptance.SandboxWarmPoolAPIReady && observedAcceptance.SandboxTemplateAPIReady,
		OperatorReady:              observedAcceptance.OperatorReady,
		TemplateReady:              templateReady,
		WarmPoolReady:              observedAcceptance.WarmPoolReady && observedAcceptance.WarmPoolTemplateReady,
		StandardRuntimeReady:       observedAcceptance.TemplateRuntime == "standard",
		CocoonVirtualNodeReady:     observedAcceptance.VirtualNodeReady,
		CocoonKVMRuntimeReady:      observedAcceptance.KVMRuntimeReady,
		CocoonHostSupervisorReady:  observedAcceptance.HostSupervisorReady,
		CocoonFencedVSockReady:     observedAcceptance.FencedVSockReady,
		CocoonGuestIsolationReady:  observedAcceptance.GuestIsolationReady,
		CocoonGuestContainerReady:  observedAcceptance.TemplateSandboxRuntimeName == kubernetesCocoonGuestContainerName,
		CocoonSchedulingFenceReady: observedAcceptance.CocoonTemplateSchedulingReady,
	}
	if configuration.AllocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) &&
		observedAcceptance.TemplateRuntime != "vk-cocoon" {
		acceptanceObservation.CocoonVirtualNodeReady = false
	}
	acceptance, err := EvaluateKubernetesAllocationAcceptance(ctx, configuration.AllocationBackend, acceptanceObservation)
	if err != nil {
		return kubernetesSandboxMaterializationResult{}, err
	}
	if !acceptance.Accepted {
		return kubernetesSandboxMaterializationResult{}, problem.New(
			503, acceptance.ReasonCode, "The sandbox-operator target has not passed allocation acceptance.",
		)
	}
	desiredWarmReplicas, err := r.kubernetesSandboxDesiredWarmReplicas(ctx, target.ID, configuration, executions, r.now())
	if err != nil {
		return kubernetesSandboxMaterializationResult{}, err
	}
	if err := client.Apply(
		ctx,
		kubernetesSandboxNamespacedPath(configuration.Namespace, "sandboxwarmpools", configuration.SandboxWarmPoolName),
		kubernetesSandboxWarmPool(configuration, desiredWarmReplicas),
	); err != nil {
		return kubernetesSandboxMaterializationResult{}, problem.Wrap(
			502, "kubernetes_sandbox_warm_pool_apply_failed", "The SandboxWarmPool desired capacity could not be projected.", err,
		)
	}
	if observedAcceptance.WarmPoolDesiredReplicas != desiredWarmReplicas {
		return kubernetesSandboxMaterializationResult{}, problem.New(
			503, "kubernetes_sandbox_warm_pool_projection_pending", "The SandboxWarmPool has not observed Synara's desired idle capacity yet.",
		)
	}
	if observedAcceptance.WarmPoolUpdateStrategy != "Recreate" || !observedAcceptance.WarmPoolTemplateImageFresh {
		return kubernetesSandboxMaterializationResult{}, problem.New(
			503,
			"kubernetes_sandbox_warm_pool_rollout_pending",
			"The SandboxWarmPool has not completed Recreate rollout to the current template image.",
		)
	}

	now := r.now()
	active := make(map[string]kubernetesExecution)
	result := kubernetesSandboxMaterializationResult{}
	var allocations []persistence.ExecutionKubernetesAllocation
	if err := r.targets.db.WithContext(ctx).
		Where("execution_target_id = ? AND status <> ?", target.ID, "deleted").
		Find(&allocations).Error; err != nil {
		return result, problem.Wrap(500, "kubernetes_sandbox_allocations_load_failed", "Sandbox allocations could not be loaded.", err)
	}
	existingByKey := make(map[string]persistence.ExecutionKubernetesAllocation, len(allocations))
	latestByExecution := make(map[uuid.UUID]persistence.ExecutionKubernetesAllocation, len(allocations))
	for _, allocation := range allocations {
		existingByKey[kubernetesSandboxAllocationKey(allocation.ExecutionID, allocation.Generation)] = allocation
		latest, found := latestByExecution[allocation.ExecutionID]
		if !found || allocation.Generation > latest.Generation {
			latestByExecution[allocation.ExecutionID] = allocation
		}
	}
	occupied := len(allocations)
	for _, execution := range executions {
		if !kubernetesExecutionWithinAbsoluteLifetime(execution, now) {
			continue
		}
		if execution.Status != "queued" && execution.Status != "recovering" {
			if allocation, found := latestByExecution[execution.ID]; found {
				active[kubernetesSandboxAllocationKey(allocation.ExecutionID, allocation.Generation)] = execution
			}
			continue
		}
		generation := execution.Generation + 1
		if execution.WorkerReleaseChannel != nil && *execution.WorkerReleaseChannel == "canary" {
			return result, problem.New(
				409,
				"kubernetes_sandbox_canary_release_unsupported",
				"A single SandboxWarmPool cannot safely materialize promoted and canary Worker releases concurrently.",
			)
		}
		expectedImage, err := kubernetesExecutionImage(configuration.Image, execution)
		if err != nil {
			return result, err
		}
		if kubernetesSandboxAcceptanceRuntimeImage(configuration.AllocationBackend, observedAcceptance) != strings.TrimSpace(expectedImage) {
			return result, problem.New(
				503,
				"kubernetes_sandbox_worker_release_template_mismatch",
				"The SandboxTemplate runtime image does not match the Execution Worker release selection.",
			)
		}
		digest, err := kubernetesSandboxAllocationConfigurationDigest(
			configuration, observedAcceptance.TemplateIdentity, expectedImage,
		)
		if err != nil {
			return result, err
		}
		key := kubernetesSandboxAllocationKey(execution.ID, generation)
		if _, found := existingByKey[key]; !found {
			if occupied >= configuration.MaxActivePods {
				continue
			}
			occupied++
		}
		active[key] = execution
		result.Active++
		allocation, created, err := r.ensureKubernetesSandboxAllocation(
			ctx, target, configuration, execution, generation, digest, now,
		)
		if err != nil {
			return result, err
		}
		if allocation.Status == "deleted" {
			return result, problem.New(
				409,
				"kubernetes_sandbox_allocation_terminal",
				fmt.Sprintf("Sandbox allocation for Execution %s Generation %d is terminal and cannot be reused.", allocation.ExecutionID, allocation.Generation),
			)
		}
		if allocation.Status == "deleting" || allocation.Status == "failed" {
			if _, err := r.deleteKubernetesSandboxAllocation(ctx, sandboxClient, allocation, now); err != nil {
				return result, err
			}
			return result, problem.New(503, "kubernetes_sandbox_allocation_failed", "The Sandbox allocation failed and is being cleaned up.")
		}
		currentClaim, claimFound, err := sandboxClient.GetSandboxClaim(
			ctx, configuration.Namespace, allocation.ClaimName,
		)
		if err != nil {
			return result, problem.Wrap(502, "kubernetes_sandbox_claim_load_failed", "The SandboxClaim could not be checked before apply.", err)
		}
		if claimFound {
			if currentClaim.ConfigurationDigest != digest ||
				currentClaim.WarmPoolName != configuration.SandboxWarmPoolName ||
				(allocation.ClaimUID != nil && strings.TrimSpace(*allocation.ClaimUID) != currentClaim.UID) {
				return result, problem.New(409, "kubernetes_sandbox_claim_drift", "An existing SandboxClaim does not match the immutable allocation identity.")
			}
		}
		claim := kubernetesSandboxClaim(target, configuration, execution, generation, allocation.ClaimName, digest)
		if err := client.Apply(
			ctx,
			kubernetesSandboxNamespacedPath(configuration.Namespace, "sandboxclaims", allocation.ClaimName),
			claim,
		); err != nil {
			return result, problem.Wrap(502, "kubernetes_sandbox_claim_apply_failed", "The SandboxClaim could not be applied.", err)
		}
		if created {
			result.Allocated++
		}
		bound, err := r.observeKubernetesSandboxAllocation(
			ctx, sandboxClient, client, allocation, configuration.AllocationBackend, expectedImage,
			time.Duration(configuration.SandboxClaimReadyTimeoutSeconds)*time.Second, now,
		)
		if err != nil {
			return result, err
		}
		if bound {
			result.Bound++
			var boundAllocation persistence.ExecutionKubernetesAllocation
			if err := r.targets.db.WithContext(ctx).First(
				&boundAllocation, "tenant_id = ? AND execution_id = ? AND generation = ?",
				allocation.TenantID, allocation.ExecutionID, allocation.Generation,
			).Error; err != nil {
				return result, problem.Wrap(500, "kubernetes_sandbox_allocation_load_failed", "The bound Sandbox allocation could not be loaded.", err)
			}
			result.Acknowledgements = append(result.Acknowledgements, boundAllocation)
		}
	}

	for _, allocation := range allocations {
		if _, found := active[kubernetesSandboxAllocationKey(allocation.ExecutionID, allocation.Generation)]; found {
			continue
		}
		deleted, err := r.deleteKubernetesSandboxAllocation(ctx, sandboxClient, allocation, r.now())
		if err != nil {
			return result, err
		}
		if deleted {
			result.Deleted++
		}
	}
	return result, nil
}

func (r *KubernetesReconciler) ensureKubernetesSandboxAllocation(
	ctx context.Context,
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	execution kubernetesExecution,
	generation int64,
	digest string,
	now time.Time,
) (persistence.ExecutionKubernetesAllocation, bool, error) {
	row := persistence.ExecutionKubernetesAllocation{
		TenantID: execution.TenantID, ExecutionID: execution.ID, Generation: generation,
		ExecutionTargetID: target.ID, Backend: configuration.AllocationBackend,
		Namespace: configuration.Namespace, SandboxTemplateName: configuration.SandboxTemplateName,
		SandboxWarmPoolName: configuration.SandboxWarmPoolName, ConfigurationDigest: digest,
		ClaimName: kubernetesSandboxClaimName(target.ID, execution, generation), Status: "materializing",
		MaterializationStartedAt: now, LastObservedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	result := r.targets.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil {
		return persistence.ExecutionKubernetesAllocation{}, false, problem.Wrap(
			500, "kubernetes_sandbox_allocation_create_failed", "The Sandbox allocation identity could not be created.", result.Error,
		)
	}
	created := result.RowsAffected == 1
	var stored persistence.ExecutionKubernetesAllocation
	if err := r.targets.db.WithContext(ctx).First(
		&stored, "tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, generation,
	).Error; err != nil {
		return persistence.ExecutionKubernetesAllocation{}, false, problem.Wrap(
			500, "kubernetes_sandbox_allocation_load_failed", "The Sandbox allocation identity could not be loaded.", err,
		)
	}
	if stored.ExecutionTargetID != target.ID || stored.Backend != configuration.AllocationBackend ||
		stored.Namespace != configuration.Namespace || stored.SandboxTemplateName != configuration.SandboxTemplateName ||
		stored.SandboxWarmPoolName != configuration.SandboxWarmPoolName || stored.ConfigurationDigest != digest ||
		stored.ClaimName != row.ClaimName {
		return persistence.ExecutionKubernetesAllocation{}, false, problem.New(
			409, "kubernetes_sandbox_allocation_drift", "The persisted Sandbox allocation does not match the immutable Generation configuration.",
		)
	}
	return stored, created, nil
}

func (r *KubernetesReconciler) kubernetesSandboxDesiredWarmReplicas(
	ctx context.Context,
	targetID uuid.UUID,
	configuration kubernetesTargetConfiguration,
	executions []kubernetesExecution,
	now time.Time,
) (int, error) {
	warmPools, err := r.loadKubernetesWarmPools(ctx, targetID)
	if err != nil {
		return 0, err
	}
	desiredIdle := 0
	for _, pool := range warmPools {
		if pool.Status == "active" {
			desiredIdle += pool.DesiredIdleUnits
		}
	}
	claimed := 0
	for _, execution := range executions {
		if kubernetesExecutionWithinAbsoluteLifetime(execution, now) {
			claimed++
		}
	}
	availableIdleHeadroom := configuration.MaxActivePods - claimed
	if availableIdleHeadroom < 0 {
		availableIdleHeadroom = 0
	}
	desiredReplicas := desiredIdle
	if desiredReplicas > availableIdleHeadroom {
		desiredReplicas = availableIdleHeadroom
	}
	if desiredReplicas < 0 {
		desiredReplicas = 0
	}
	return desiredReplicas, nil
}

func (r *KubernetesReconciler) observeKubernetesSandboxAllocation(
	ctx context.Context,
	sandboxClient kubernetesSandboxClient,
	client kubernetesClient,
	allocation persistence.ExecutionKubernetesAllocation,
	backend string,
	expectedImage string,
	readyTimeout time.Duration,
	observedAt time.Time,
) (bool, error) {
	claim, found, err := sandboxClient.GetSandboxClaim(ctx, allocation.Namespace, allocation.ClaimName)
	if err != nil {
		return false, problem.Wrap(502, "kubernetes_sandbox_claim_load_failed", "The SandboxClaim could not be observed.", err)
	}
	if found && !claim.Ready && kubernetesSandboxClaimTerminalRejection(claim.Reason) {
		reasonCode := "sandbox-claim-rejected-" + strings.ToLower(strings.TrimSpace(claim.Reason))
		if err := r.failKubernetesSandboxAllocation(ctx, allocation, claim, true, reasonCode, observedAt); err != nil {
			return false, err
		}
		if strings.TrimSpace(claim.UID) != "" {
			if err := sandboxClient.DeleteSandboxClaim(ctx, allocation.Namespace, allocation.ClaimName, claim.UID); err != nil {
				return false, problem.Wrap(502, "kubernetes_sandbox_claim_delete_failed", "The rejected SandboxClaim could not be deleted.", err)
			}
		}
		return false, problem.New(409, "kubernetes_sandbox_claim_rejected", "The SandboxClaim was terminally rejected: "+strings.TrimSpace(claim.Reason)+".")
	}
	if !found || !claim.Ready || claim.UID == "" || claim.SandboxName == "" {
		if observedAt.Sub(allocation.MaterializationStartedAt) >= readyTimeout {
			if err := r.failKubernetesSandboxAllocation(
				ctx, allocation, claim, found, "sandbox-claim-ready-timeout", observedAt,
			); err != nil {
				return false, err
			}
			if found && strings.TrimSpace(claim.UID) != "" {
				if err := sandboxClient.DeleteSandboxClaim(ctx, allocation.Namespace, allocation.ClaimName, claim.UID); err != nil {
					return false, problem.Wrap(502, "kubernetes_sandbox_claim_delete_failed", "The timed-out SandboxClaim could not be deleted.", err)
				}
			}
			return false, problem.New(504, "kubernetes_sandbox_claim_ready_timeout", "The SandboxClaim did not become Ready within its configured timeout.")
		}
		return false, r.touchKubernetesSandboxAllocation(ctx, allocation, observedAt)
	}
	if allocation.ClaimUID != nil && !kubernetesOptionalIdentityMatches(allocation.ClaimUID, claim.UID) {
		return false, problem.New(409, "kubernetes_sandbox_claim_uid_changed", "The SandboxClaim UID changed during materialization.")
	}
	if allocation.SandboxName != nil && !kubernetesOptionalIdentityMatches(allocation.SandboxName, claim.SandboxName) {
		return false, problem.New(409, "kubernetes_sandbox_name_changed", "The Sandbox identity changed during materialization.")
	}
	if allocation.ClaimReadyAt == nil {
		claimReadyAt := observedAt
		result := r.targets.db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
			Where("tenant_id = ? AND execution_id = ? AND generation = ? AND status = ?",
				allocation.TenantID, allocation.ExecutionID, allocation.Generation, "materializing").
			Updates(map[string]any{
				"claim_uid": claim.UID, "sandbox_name": claim.SandboxName, "claim_ready_at": claimReadyAt,
				"last_observed_at": observedAt, "updated_at": observedAt,
			})
		if result.Error != nil {
			return false, problem.Wrap(500, "kubernetes_sandbox_claim_ready_record_failed", "SandboxClaim readiness identity could not be recorded.", result.Error)
		}
		if result.RowsAffected != 1 {
			return false, problem.New(409, "kubernetes_sandbox_claim_ready_record_conflict", "The Sandbox allocation changed before Claim readiness could be recorded.")
		}
		allocation.ClaimUID = &claim.UID
		allocation.SandboxName = &claim.SandboxName
		allocation.ClaimReadyAt = &claimReadyAt
	}
	sandbox, found, err := sandboxClient.GetSandbox(ctx, allocation.Namespace, claim.SandboxName)
	if err != nil {
		return false, problem.Wrap(502, "kubernetes_sandbox_load_failed", "The Sandbox could not be observed.", err)
	}
	if !found || sandbox.UID == "" || sandbox.PodName == "" {
		return false, r.touchKubernetesSandboxAllocation(ctx, allocation, observedAt)
	}
	pods, err := client.ListPods(ctx, allocation.Namespace, allocation.ExecutionTargetID)
	if err != nil {
		return false, problem.Wrap(502, "kubernetes_sandbox_pods_load_failed", "Sandbox backing Pods could not be observed.", err)
	}
	for _, pod := range pods {
		if pod.Name != sandbox.PodName || strings.TrimSpace(pod.UID) == "" {
			continue
		}
		observedImage := kubernetesSandboxPodRuntimeImage(backend, pod)
		if observedImage != strings.TrimSpace(expectedImage) {
			if err := r.failKubernetesSandboxAllocation(ctx, allocation, claim, true, "worker-release-image-mismatch", observedAt); err != nil {
				return false, err
			}
			if err := sandboxClient.DeleteSandboxClaim(ctx, allocation.Namespace, allocation.ClaimName, claim.UID); err != nil {
				return false, problem.Wrap(502, "kubernetes_sandbox_claim_delete_failed", "The Worker release-mismatched SandboxClaim could not be deleted.", err)
			}
			return false, problem.New(
				409,
				"kubernetes_sandbox_worker_release_pod_mismatch",
				"The bound Sandbox Pod image does not match the immutable Execution Worker release selection.",
			)
		}
		if allocation.Status == "bound" {
			if !kubernetesOptionalIdentityMatches(allocation.ClaimUID, claim.UID) ||
				!kubernetesOptionalIdentityMatches(allocation.SandboxName, claim.SandboxName) ||
				!kubernetesOptionalIdentityMatches(allocation.SandboxUID, sandbox.UID) ||
				!kubernetesOptionalIdentityMatches(allocation.PodName, sandbox.PodName) ||
				!kubernetesOptionalIdentityMatches(allocation.PodUID, pod.UID) {
				return false, problem.New(409, "kubernetes_sandbox_allocation_identity_changed", "The bound Sandbox allocation identity changed during observation.")
			}
			return true, r.touchKubernetesSandboxAllocation(ctx, allocation, observedAt)
		}
		boundAt := observedAt
		updates := map[string]any{
			"claim_uid": claim.UID, "sandbox_name": claim.SandboxName, "sandbox_uid": sandbox.UID,
			"pod_name": sandbox.PodName, "pod_uid": pod.UID, "status": "bound", "bound_at": boundAt,
			"last_observed_at": observedAt, "updated_at": observedAt, "failure_reason_code": nil,
		}
		result := r.targets.db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
			Where("tenant_id = ? AND execution_id = ? AND generation = ? AND status IN ?",
				allocation.TenantID, allocation.ExecutionID, allocation.Generation, []string{"materializing", "bound"}).
			Updates(updates)
		if result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return false, problem.Wrap(500, "kubernetes_sandbox_allocation_bind_failed", "The Sandbox allocation identity could not be bound.", result.Error)
			}
			return false, problem.New(409, "kubernetes_sandbox_allocation_bind_conflict", "The Sandbox allocation changed before its identity could be bound.")
		}
		return true, nil
	}
	return false, r.touchKubernetesSandboxAllocation(ctx, allocation, observedAt)
}

func kubernetesSandboxClaimTerminalRejection(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "InvalidMetadata", "EnvVarsInjectionRejected", "VolumeClaimTemplatesError":
		return true
	default:
		return false
	}
}

func (r *KubernetesReconciler) failKubernetesSandboxAllocation(
	ctx context.Context,
	allocation persistence.ExecutionKubernetesAllocation,
	claim kubernetesSandboxClaimObservation,
	claimFound bool,
	reasonCode string,
	now time.Time,
) error {
	updates := map[string]any{
		"status": "failed", "failure_reason_code": reasonCode,
		"last_observed_at": now, "updated_at": now,
	}
	if claimFound && strings.TrimSpace(claim.UID) != "" {
		updates["claim_uid"] = strings.TrimSpace(claim.UID)
		updates["status"] = "deleting"
		updates["delete_requested_at"] = now
	}
	result := r.targets.db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND status IN ?",
			allocation.TenantID, allocation.ExecutionID, allocation.Generation, []string{"materializing", "bound"}).
		Updates(updates)
	if result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return problem.Wrap(500, "kubernetes_sandbox_allocation_fail_failed", "The failed Sandbox allocation could not be fenced.", result.Error)
		}
		return problem.New(409, "kubernetes_sandbox_allocation_fail_conflict", "The Sandbox allocation changed before it could be fenced.")
	}
	return nil
}

func (r *KubernetesReconciler) touchKubernetesSandboxAllocation(
	ctx context.Context,
	allocation persistence.ExecutionKubernetesAllocation,
	observedAt time.Time,
) error {
	return r.targets.db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?",
			allocation.TenantID, allocation.ExecutionID, allocation.Generation).
		Updates(map[string]any{"last_observed_at": observedAt, "updated_at": observedAt}).Error
}

func (r *KubernetesReconciler) deleteKubernetesSandboxAllocation(
	ctx context.Context,
	client kubernetesSandboxClient,
	allocation persistence.ExecutionKubernetesAllocation,
	now time.Time,
) (bool, error) {
	claim, found, err := client.GetSandboxClaim(ctx, allocation.Namespace, allocation.ClaimName)
	if err != nil {
		return false, problem.Wrap(502, "kubernetes_sandbox_claim_load_failed", "The SandboxClaim could not be loaded for cleanup.", err)
	}
	if !found {
		lineagePresent, err := kubernetesSandboxAllocationLineagePresent(ctx, client, allocation)
		if err != nil {
			return false, err
		}
		if lineagePresent {
			return false, r.touchKubernetesSandboxAllocation(ctx, allocation, now)
		}
		return true, r.markKubernetesSandboxAllocationDeleted(ctx, allocation, now)
	}
	claimUID := strings.TrimSpace(claim.UID)
	if allocation.ClaimUID != nil && strings.TrimSpace(*allocation.ClaimUID) != claimUID {
		return false, problem.New(409, "kubernetes_sandbox_claim_uid_changed", "The SandboxClaim UID changed before cleanup; deletion was fenced.")
	}
	if claimUID == "" {
		return false, problem.New(502, "kubernetes_sandbox_claim_uid_missing", "The SandboxClaim omitted its UID; deletion was fenced.")
	}
	deleteRequestedAt := now
	if allocation.DeleteRequestedAt != nil {
		deleteRequestedAt = *allocation.DeleteRequestedAt
	}
	updates := map[string]any{
		"claim_uid": claimUID, "status": "deleting", "delete_requested_at": deleteRequestedAt,
		"last_observed_at": now, "updated_at": now,
	}
	if err := r.targets.db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND status <> ?",
			allocation.TenantID, allocation.ExecutionID, allocation.Generation, "deleted").
		Updates(updates).Error; err != nil {
		return false, problem.Wrap(500, "kubernetes_sandbox_allocation_delete_record_failed", "Sandbox cleanup intent could not be recorded.", err)
	}
	if err := client.DeleteSandboxClaim(ctx, allocation.Namespace, allocation.ClaimName, claimUID); err != nil {
		return false, problem.Wrap(502, "kubernetes_sandbox_claim_delete_failed", "The exact SandboxClaim could not be deleted.", err)
	}
	return false, nil
}

func kubernetesSandboxAllocationLineagePresent(
	ctx context.Context,
	client kubernetesSandboxClient,
	allocation persistence.ExecutionKubernetesAllocation,
) (bool, error) {
	present := false
	if allocation.SandboxName != nil && allocation.SandboxUID != nil {
		sandbox, found, err := client.GetSandbox(ctx, allocation.Namespace, strings.TrimSpace(*allocation.SandboxName))
		if err != nil {
			return false, problem.Wrap(502, "kubernetes_sandbox_lineage_load_failed", "Sandbox cleanup lineage could not be observed.", err)
		}
		if found && strings.TrimSpace(sandbox.UID) == strings.TrimSpace(*allocation.SandboxUID) {
			present = true
		}
	}
	if allocation.PodName != nil && allocation.PodUID != nil {
		pods, err := client.ListPods(ctx, allocation.Namespace, allocation.ExecutionTargetID)
		if err != nil {
			return false, problem.Wrap(502, "kubernetes_sandbox_pod_lineage_load_failed", "Sandbox Pod cleanup lineage could not be observed.", err)
		}
		for _, pod := range pods {
			if pod.Name != strings.TrimSpace(*allocation.PodName) {
				continue
			}
			if strings.TrimSpace(pod.UID) == strings.TrimSpace(*allocation.PodUID) {
				present = true
			}
			break
		}
	}
	return present, nil
}

func (r *KubernetesReconciler) markKubernetesSandboxAllocationDeleted(
	ctx context.Context,
	allocation persistence.ExecutionKubernetesAllocation,
	now time.Time,
) error {
	deleteRequestedAt := allocation.DeleteRequestedAt
	if deleteRequestedAt == nil {
		deleteRequestedAt = &now
	}
	return r.targets.db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?",
			allocation.TenantID, allocation.ExecutionID, allocation.Generation).
		Updates(map[string]any{
			"status": "deleted", "delete_requested_at": deleteRequestedAt, "deleted_at": now,
			"last_observed_at": now, "updated_at": now,
		}).Error
}

func kubernetesSandboxClaim(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	execution kubernetesExecution,
	generation int64,
	claimName, digest string,
) map[string]any {
	labels := kubernetesTargetLabels(target)
	labels[kubernetesExecutionLabel] = execution.ID.String()
	labels[kubernetesGenerationLabel] = strconv.FormatInt(generation, 10)
	labels[kubernetesWorkerModeLabel] = kubernetesWorkerModeExecutionPinned
	labels[kubernetesSandboxAssignedExecutionLabel] = execution.ID.String()
	labels[kubernetesSandboxAllocationBackendLabel] = configuration.AllocationBackend
	return map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxClaim",
		"metadata": map[string]any{
			"name": claimName, "namespace": configuration.Namespace, "labels": labels,
			"annotations": map[string]any{kubernetesConfigAnnotation: digest},
		},
		"spec": map[string]any{
			"warmPoolRef": map[string]any{"name": configuration.SandboxWarmPoolName},
			"additionalPodMetadata": map[string]any{
				"labels": labels, "annotations": map[string]any{kubernetesConfigAnnotation: digest},
			},
			"lifecycle": map[string]any{"shutdownPolicy": "DeleteForeground"},
		},
	}
}

func kubernetesSandboxWarmPool(
	configuration kubernetesTargetConfiguration,
	replicas int,
) map[string]any {
	return map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxWarmPool",
		"metadata": map[string]any{
			"name": configuration.SandboxWarmPoolName, "namespace": configuration.Namespace,
		},
		"spec": map[string]any{
			"replicas":           replicas,
			"sandboxTemplateRef": map[string]any{"name": configuration.SandboxTemplateName},
			"updateStrategy":     map[string]any{"type": "Recreate"},
		},
	}
}

func kubernetesSandboxClaimName(targetID uuid.UUID, execution kubernetesExecution, generation int64) string {
	targetPrefix := strings.ReplaceAll(targetID.String(), "-", "")[:8]
	executionPrefix := strings.ReplaceAll(execution.ID.String(), "-", "")[:24]
	return "synara-claim-" + targetPrefix + "-" + executionPrefix + "-g" + strconv.FormatInt(generation, 16)
}

func kubernetesSandboxAllocationConfigurationDigest(configuration kubernetesTargetConfiguration, templateIdentity, runtimeImage string) (string, error) {
	if configuration.AllocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
		payload, err := json.Marshal(struct {
			Version           string
			Backend           string
			Namespace         string
			TemplateName      string
			TemplateID        string
			WarmPoolName      string
			GuestImage        string
			HostSupervisor    string
			ProviderTransport string
			IsolationProfile  string
			Timeout           int
		}{
			Version: "cocoon-v1", Backend: configuration.AllocationBackend, Namespace: configuration.Namespace,
			TemplateName: configuration.SandboxTemplateName, TemplateID: templateIdentity,
			WarmPoolName: configuration.SandboxWarmPoolName, GuestImage: strings.TrimSpace(runtimeImage),
			HostSupervisor:    kubernetesCocoonHostSupervisorV1,
			ProviderTransport: kubernetesCocoonProviderTransportV2,
			IsolationProfile:  kubernetesCocoonIsolationProfileV1,
			Timeout:           configuration.SandboxClaimReadyTimeoutSeconds,
		})
		if err != nil {
			return "", fmt.Errorf("encode Kubernetes Cocoon Sandbox allocation configuration: %w", err)
		}
		digest := sha256.Sum256(payload)
		return hex.EncodeToString(digest[:]), nil
	}
	payload, err := json.Marshal(struct {
		Backend      string
		Namespace    string
		TemplateName string
		TemplateID   string
		WarmPoolName string
		AgentdImage  string
		Timeout      int
	}{
		Backend: configuration.AllocationBackend, Namespace: configuration.Namespace,
		TemplateName: configuration.SandboxTemplateName, TemplateID: templateIdentity,
		WarmPoolName: configuration.SandboxWarmPoolName,
		AgentdImage:  strings.TrimSpace(runtimeImage),
		Timeout:      configuration.SandboxClaimReadyTimeoutSeconds,
	})
	if err != nil {
		return "", fmt.Errorf("encode Kubernetes Sandbox allocation configuration: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func kubernetesSandboxAllocationKey(executionID uuid.UUID, generation int64) string {
	return executionID.String() + ":" + strconv.FormatInt(generation, 10)
}

func kubernetesOptionalIdentityMatches(stored *string, observed string) bool {
	return stored != nil && strings.TrimSpace(*stored) == strings.TrimSpace(observed)
}
