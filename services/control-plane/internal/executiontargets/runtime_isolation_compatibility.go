package executiontargets

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func gvisorCompatibilityAccepted(
	ctx context.Context,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	policy *runtimeIsolationConfiguration,
) error {
	if policy == nil || !runtimeIsolationPolicyIncludes(policy, runtimeIsolationGVisor) {
		return nil
	}
	if len(policy.GVisorCompatibleProviders) == 0 {
		return gvisorCompatibilityUnaccepted()
	}
	providerPolicy, err := ParseProviderPolicy(target.Capabilities)
	if err != nil {
		return err
	}
	for _, provider := range providerPolicy.ExperimentalProviders {
		if !slices.Contains(policy.GVisorCompatibleProviders, provider) {
			return gvisorCompatibilityUnaccepted()
		}
	}

	var releasePolicy persistence.WorkerReleasePolicy
	err = db.WithContext(ctx).Where("execution_target_id = ?", target.ID).Take(&releasePolicy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// An unmanaged base image has no independent Release object. In that
		// mode the encrypted Target declaration is the explicit acceptance
		// authority.
		return nil
	}
	if err != nil {
		return problem.Wrap(500, "worker_release_policy_lookup_failed", "Worker release compatibility could not be loaded.", err)
	}
	revisionIDs := []uuid.UUID{releasePolicy.PromotedRevisionID}
	if releasePolicy.CanaryRevisionID != nil {
		revisionIDs = append(revisionIDs, *releasePolicy.CanaryRevisionID)
	}
	var revisions []persistence.WorkerReleaseRevision
	if err := db.WithContext(ctx).
		Where("execution_target_id = ? AND id IN ?", target.ID, revisionIDs).
		Find(&revisions).Error; err != nil {
		return problem.Wrap(500, "worker_release_revision_lookup_failed", "Worker release compatibility could not be loaded.", err)
	}
	if len(revisions) != len(revisionIDs) {
		return problem.New(500, "worker_release_revision_missing", "An active Worker release revision is missing.")
	}
	for _, revision := range revisions {
		for _, provider := range policy.GVisorCompatibleProviders {
			if !slices.Contains(revision.GVisorCompatibleProviders, provider) {
				return gvisorCompatibilityUnaccepted()
			}
		}
	}
	return nil
}

func filterDockerGVisorRuntimeNames(runtimeNames []string, compatibilityErr error) []string {
	if compatibilityErr == nil {
		return runtimeNames
	}
	filtered := make([]string, 0, len(runtimeNames))
	for _, name := range runtimeNames {
		switch strings.TrimSpace(name) {
		case "runsc":
			continue
		default:
			filtered = append(filtered, name)
		}
	}
	return filtered
}

func filterGVisorCapability(
	capabilities []runtimeIsolationCapability,
	compatibilityErr error,
) []runtimeIsolationCapability {
	if compatibilityErr == nil {
		return capabilities
	}
	filtered := make([]runtimeIsolationCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability.Runtime != runtimeIsolationGVisor {
			filtered = append(filtered, capability)
		}
	}
	return filtered
}

func runtimeIsolationPolicyIncludes(policy *runtimeIsolationConfiguration, runtime string) bool {
	if policy == nil {
		return false
	}
	if policy.Mode == runtimeIsolationModeExplicit || policy.Mode == runtimeIsolationModeLegacy {
		return policy.Runtime == runtime
	}
	return slices.Contains(policy.Preferred, runtime)
}

func runtimeIsolationRequiresGVisor(policy *runtimeIsolationConfiguration) bool {
	if policy == nil {
		return false
	}
	return (policy.Mode == runtimeIsolationModeExplicit && policy.Runtime == runtimeIsolationGVisor) ||
		runtimeIsolationProfileRank(policy.MinimumProfile) >= runtimeIsolationProfileRank(platform.IsolationGVisorSandboxed)
}

func preferGVisorCompatibilityError(
	decision *runtimeIsolationDecision,
	decisionErr, compatibilityErr error,
) error {
	if decisionErr == nil || compatibilityErr == nil {
		return decisionErr
	}
	decision.DecisionReasonCode = "gvisor_provider_compatibility_unaccepted"
	return compatibilityErr
}

func gvisorCompatibilityUnaccepted() error {
	return problem.New(
		409,
		"gvisor_provider_compatibility_unaccepted",
		"The Target and every active Worker release must explicitly accept gVisor for each enabled Provider.",
	)
}
