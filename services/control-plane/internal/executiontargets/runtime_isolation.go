package executiontargets

import (
	"slices"
	"strings"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	runtimeIsolationModeExplicit = "explicit"
	runtimeIsolationModeAuto     = "auto"
	runtimeIsolationModeLegacy   = "legacy-native"

	runtimeIsolationRunc        = "runc"
	runtimeIsolationGVisor      = "gvisor"
	runtimeIsolationFirecracker = "firecracker"

	runtimeIsolationFallbackFailClosed = "fail-closed"
	runtimeIsolationFallbackAllowLower = "allow-lower"

	kubernetesDefaultGVisorRuntimeClassName = "synara-gvisor"
	kubernetesApprovedGVisorRuntimeHandler  = "runsc"
)

type runtimeIsolationConfiguration struct {
	Mode                      string                                   `json:"mode"`
	Runtime                   string                                   `json:"runtime,omitempty"`
	Preferred                 []string                                 `json:"preferred,omitempty"`
	MinimumProfile            platform.ExecutionTargetIsolationProfile `json:"minimumProfile"`
	FallbackPolicy            string                                   `json:"fallbackPolicy"`
	RuntimeClassName          string                                   `json:"runtimeClassName,omitempty"`
	GVisorCompatibleProviders []string                                 `json:"gvisorCompatibleProviders,omitempty"`
}

type runtimeIsolationCapability struct {
	Runtime              string
	Profile              platform.ExecutionTargetIsolationProfile
	RuntimeClassName     string
	AttestationDigest    string
	AttestedAt           *time.Time
	AttestationExpiresAt *time.Time
	EligibleNodeNames    []string
}

type runtimeIsolationDecision struct {
	RequestedRuntime     string
	RequestedProfile     platform.ExecutionTargetIsolationProfile
	EffectiveRuntime     string
	EffectiveProfile     platform.ExecutionTargetIsolationProfile
	PolicySource         string
	Decision             string
	DecisionReasonCode   string
	RuntimeClassName     string
	AttestationDigest    string
	AttestedAt           *time.Time
	AttestationExpiresAt *time.Time
	EligibleNodeNames    []string
}

func normalizeKubernetesRuntimeIsolationConfiguration(configuration *kubernetesTargetConfiguration) error {
	if configuration.RuntimeIsolation == nil {
		configuration.RuntimeIsolation = &runtimeIsolationConfiguration{
			Mode:           runtimeIsolationModeLegacy,
			FallbackPolicy: runtimeIsolationFallbackFailClosed,
		}
		if configuration.AllocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
			configuration.RuntimeIsolation.Runtime = runtimeIsolationFirecracker
			configuration.RuntimeIsolation.MinimumProfile = platform.IsolationMicroVM
		} else {
			configuration.RuntimeIsolation.Runtime = runtimeIsolationRunc
			configuration.RuntimeIsolation.MinimumProfile = platform.IsolationKubernetesRestricted
		}
		return nil
	}

	policy := configuration.RuntimeIsolation
	policy.Mode = strings.TrimSpace(policy.Mode)
	policy.Runtime = strings.TrimSpace(policy.Runtime)
	policy.FallbackPolicy = strings.TrimSpace(policy.FallbackPolicy)
	policy.RuntimeClassName = strings.TrimSpace(policy.RuntimeClassName)
	if policy.FallbackPolicy == "" {
		policy.FallbackPolicy = runtimeIsolationFallbackFailClosed
	}
	if policy.FallbackPolicy != runtimeIsolationFallbackFailClosed &&
		policy.FallbackPolicy != runtimeIsolationFallbackAllowLower {
		return invalidRuntimeIsolationConfiguration("runtimeIsolation.fallbackPolicy must be fail-closed or allow-lower.")
	}
	if policy.Mode != runtimeIsolationModeExplicit && policy.Mode != runtimeIsolationModeAuto {
		return invalidRuntimeIsolationConfiguration("runtimeIsolation.mode must be explicit or auto.")
	}

	preferred := make([]string, 0, len(policy.Preferred))
	seen := make(map[string]struct{}, len(policy.Preferred))
	for _, candidate := range policy.Preferred {
		candidate = strings.TrimSpace(candidate)
		if !validRuntimeIsolationRuntime(candidate) {
			return invalidRuntimeIsolationConfiguration("runtimeIsolation.preferred contains an unsupported runtime.")
		}
		if _, duplicate := seen[candidate]; duplicate {
			return invalidRuntimeIsolationConfiguration("runtimeIsolation.preferred cannot contain duplicate runtimes.")
		}
		seen[candidate] = struct{}{}
		preferred = append(preferred, candidate)
	}
	policy.Preferred = preferred
	compatibleProviders, err := normalizeGVisorCompatibleProviders(policy.GVisorCompatibleProviders)
	if err != nil {
		return err
	}
	policy.GVisorCompatibleProviders = compatibleProviders

	if policy.Mode == runtimeIsolationModeExplicit {
		if !validRuntimeIsolationRuntime(policy.Runtime) {
			return invalidRuntimeIsolationConfiguration("Explicit runtimeIsolation requires runtime runc, gvisor, or firecracker.")
		}
		if len(policy.Preferred) != 0 {
			return invalidRuntimeIsolationConfiguration("Explicit runtimeIsolation cannot include preferred runtimes.")
		}
	} else {
		if policy.Runtime != "" {
			return invalidRuntimeIsolationConfiguration("Auto runtimeIsolation cannot include a fixed runtime.")
		}
		if len(policy.Preferred) == 0 {
			switch configuration.AllocationBackend {
			case string(kubernetesAllocationBackendSandboxOperatorCocoon):
				policy.Preferred = []string{runtimeIsolationFirecracker}
			case string(kubernetesAllocationBackendNativePod):
				policy.Preferred = []string{runtimeIsolationGVisor, runtimeIsolationRunc}
			default:
				policy.Preferred = []string{runtimeIsolationRunc}
			}
		}
	}

	if policy.MinimumProfile == "" {
		if configuration.AllocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
			policy.MinimumProfile = platform.IsolationMicroVM
		} else {
			policy.MinimumProfile = platform.IsolationKubernetesRestricted
		}
	}
	if !validRuntimeIsolationProfile(policy.MinimumProfile) {
		return invalidRuntimeIsolationConfiguration("runtimeIsolation.minimumProfile is unsupported.")
	}

	candidates := policy.Preferred
	if policy.Mode == runtimeIsolationModeExplicit {
		candidates = []string{policy.Runtime}
	}
	for _, candidate := range candidates {
		if err := validateKubernetesRuntimeBackendCompatibility(candidate, configuration.AllocationBackend); err != nil {
			return err
		}
	}
	if slices.Contains(candidates, runtimeIsolationGVisor) {
		if policy.RuntimeClassName == "" {
			policy.RuntimeClassName = kubernetesDefaultGVisorRuntimeClassName
		}
		if !kubernetesNamePattern.MatchString(policy.RuntimeClassName) || len(policy.RuntimeClassName) > 63 {
			return invalidRuntimeIsolationConfiguration("runtimeIsolation.runtimeClassName must be a valid Kubernetes name.")
		}
	} else if policy.RuntimeClassName != "" {
		return invalidRuntimeIsolationConfiguration("runtimeIsolation.runtimeClassName requires the gvisor runtime.")
	} else if len(policy.GVisorCompatibleProviders) != 0 {
		return invalidRuntimeIsolationConfiguration("runtimeIsolation.gvisorCompatibleProviders requires the gvisor runtime.")
	}
	return nil
}

func resolveKubernetesRuntimeIsolation(
	configuration kubernetesTargetConfiguration,
	capabilities []runtimeIsolationCapability,
) (runtimeIsolationDecision, error) {
	policy := configuration.RuntimeIsolation
	if policy == nil {
		return runtimeIsolationDecision{}, invalidRuntimeIsolationConfiguration("runtimeIsolation was not normalized.")
	}
	candidates := policy.Preferred
	policySource := "target-auto"
	requestedRuntime := "auto"
	if policy.Mode == runtimeIsolationModeExplicit || policy.Mode == runtimeIsolationModeLegacy {
		candidates = []string{policy.Runtime}
		requestedRuntime = policy.Runtime
		policySource = "target-explicit"
		if policy.Mode == runtimeIsolationModeLegacy {
			policySource = "legacy-native"
		}
	}
	fallbackForbidden := false
	preferredRank := runtimeIsolationRuntimeProfileRank(candidates[0], configuration.AllocationBackend)

	for index, runtime := range candidates {
		for _, capability := range capabilities {
			if capability.Runtime != runtime ||
				runtimeIsolationProfileRank(capability.Profile) < runtimeIsolationProfileRank(policy.MinimumProfile) {
				continue
			}
			if index > 0 && runtimeIsolationProfileRank(capability.Profile) < preferredRank &&
				policy.FallbackPolicy != runtimeIsolationFallbackAllowLower {
				fallbackForbidden = true
				continue
			}
			decision := "selected"
			if index > 0 {
				decision = "fallback"
			}
			return runtimeIsolationDecision{
				RequestedRuntime: requestedRuntime, RequestedProfile: policy.MinimumProfile,
				EffectiveRuntime: runtime, EffectiveProfile: capability.Profile,
				PolicySource: policySource, Decision: decision,
				RuntimeClassName: capability.RuntimeClassName, AttestationDigest: capability.AttestationDigest,
				AttestedAt: capability.AttestedAt, AttestationExpiresAt: capability.AttestationExpiresAt,
				EligibleNodeNames: append([]string(nil), capability.EligibleNodeNames...),
			}, nil
		}
	}
	reasonCode := "runtime_isolation_minimum_unavailable"
	message := "No attested runtime satisfies the requested minimum isolation profile."
	if fallbackForbidden {
		reasonCode = "runtime_isolation_fallback_forbidden"
		message = "A lower runtime is available, but the runtime isolation policy forbids fallback."
	}
	return runtimeIsolationDecision{
		RequestedRuntime: requestedRuntime, RequestedProfile: policy.MinimumProfile,
		PolicySource: policySource, Decision: "rejected",
		DecisionReasonCode: reasonCode,
	}, problem.New(503, reasonCode, message)
}

func validRuntimeIsolationRuntime(runtime string) bool {
	return runtime == runtimeIsolationRunc || runtime == runtimeIsolationGVisor || runtime == runtimeIsolationFirecracker
}

func validRuntimeIsolationProfile(profile platform.ExecutionTargetIsolationProfile) bool {
	return runtimeIsolationProfileRank(profile) >= 0
}

func runtimeIsolationProfileRank(profile platform.ExecutionTargetIsolationProfile) int {
	switch profile {
	case platform.IsolationSingleTenantTrusted:
		return 0
	case platform.IsolationKubernetesRestricted:
		return 1
	case platform.IsolationGVisorSandboxed:
		return 2
	case platform.IsolationMicroVM:
		return 3
	default:
		return -1
	}
}

func runtimeIsolationRuntimeProfileRank(runtime, allocationBackend string) int {
	switch runtime {
	case runtimeIsolationFirecracker:
		return runtimeIsolationProfileRank(platform.IsolationMicroVM)
	case runtimeIsolationGVisor:
		return runtimeIsolationProfileRank(platform.IsolationGVisorSandboxed)
	case runtimeIsolationRunc:
		if allocationBackend == string(kubernetesAllocationBackendNativePod) {
			return runtimeIsolationProfileRank(platform.IsolationKubernetesRestricted)
		}
		return runtimeIsolationProfileRank(platform.IsolationSingleTenantTrusted)
	default:
		return -1
	}
}

func validateKubernetesRuntimeBackendCompatibility(runtime, backend string) error {
	switch runtime {
	case runtimeIsolationRunc:
		if backend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
			return invalidRuntimeIsolationConfiguration("The Cocoon allocation backend requires firecracker runtime isolation.")
		}
	case runtimeIsolationGVisor:
		if backend != string(kubernetesAllocationBackendNativePod) {
			return invalidRuntimeIsolationConfiguration("The first gVisor release supports only native-pod allocation.")
		}
	case runtimeIsolationFirecracker:
		if backend != string(kubernetesAllocationBackendSandboxOperatorCocoon) {
			return invalidRuntimeIsolationConfiguration("The firecracker runtime requires sandbox-operator-cocoon allocation.")
		}
	default:
		return invalidRuntimeIsolationConfiguration("runtimeIsolation runtime is unsupported.")
	}
	return nil
}

func invalidRuntimeIsolationConfiguration(detail string) error {
	return problem.New(400, "runtime_isolation_configuration_invalid", detail)
}

func defaultRuntimeIsolationForNewTarget(
	kind platform.ExecutionTargetKind,
	deploymentProfile platform.DeploymentProfile,
	configuration map[string]any,
) map[string]any {
	if kind != platform.TargetKubernetes && kind != platform.TargetDocker {
		return configuration
	}
	normalized := make(map[string]any, len(configuration)+1)
	for key, value := range configuration {
		normalized[key] = value
	}
	if _, configured := normalized["runtimeIsolation"]; configured {
		return normalized
	}
	if kind == platform.TargetDocker {
		if deploymentProfile != platform.ProfileEnterprise {
			normalized["runtimeIsolation"] = map[string]any{
				"mode": "auto", "preferred": []string{runtimeIsolationRunc},
				"minimumProfile": string(platform.IsolationSingleTenantTrusted),
				"fallbackPolicy": runtimeIsolationFallbackFailClosed,
			}
			return normalized
		}
		normalized["runtimeIsolation"] = map[string]any{
			"mode": "auto", "preferred": []string{runtimeIsolationGVisor, runtimeIsolationRunc},
			"minimumProfile": string(platform.IsolationSingleTenantTrusted),
			"fallbackPolicy": runtimeIsolationFallbackFailClosed,
		}
		return normalized
	}
	backend, _ := normalized["allocationBackend"].(string)
	if strings.TrimSpace(backend) == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
		normalized["runtimeIsolation"] = map[string]any{
			"mode": "auto", "preferred": []string{runtimeIsolationFirecracker},
			"minimumProfile": string(platform.IsolationMicroVM),
			"fallbackPolicy": runtimeIsolationFallbackFailClosed,
		}
		return normalized
	}
	normalized["runtimeIsolation"] = map[string]any{
		"mode": "auto", "preferred": []string{runtimeIsolationGVisor, runtimeIsolationRunc},
		"minimumProfile":   string(platform.IsolationKubernetesRestricted),
		"fallbackPolicy":   runtimeIsolationFallbackAllowLower,
		"runtimeClassName": kubernetesDefaultGVisorRuntimeClassName,
	}
	return normalized
}

func normalizeDockerRuntimeIsolationConfiguration(configuration *dockerTargetConfiguration) error {
	if configuration.RuntimeIsolation == nil {
		configuration.RuntimeIsolation = &runtimeIsolationConfiguration{
			Mode: runtimeIsolationModeLegacy, Runtime: runtimeIsolationRunc,
			MinimumProfile: platform.IsolationSingleTenantTrusted,
			FallbackPolicy: runtimeIsolationFallbackFailClosed,
		}
		return nil
	}
	policy := configuration.RuntimeIsolation
	policy.Mode = strings.TrimSpace(policy.Mode)
	policy.Runtime = strings.TrimSpace(policy.Runtime)
	policy.FallbackPolicy = strings.TrimSpace(policy.FallbackPolicy)
	if policy.FallbackPolicy == "" {
		policy.FallbackPolicy = runtimeIsolationFallbackFailClosed
	}
	if policy.Mode != runtimeIsolationModeExplicit && policy.Mode != runtimeIsolationModeAuto {
		return invalidRuntimeIsolationConfiguration("Docker runtimeIsolation.mode must be explicit or auto.")
	}
	if policy.FallbackPolicy != runtimeIsolationFallbackFailClosed &&
		policy.FallbackPolicy != runtimeIsolationFallbackAllowLower {
		return invalidRuntimeIsolationConfiguration("Docker runtimeIsolation.fallbackPolicy must be fail-closed or allow-lower.")
	}
	if policy.MinimumProfile == "" {
		policy.MinimumProfile = platform.IsolationSingleTenantTrusted
	}
	if policy.MinimumProfile != platform.IsolationSingleTenantTrusted {
		return invalidRuntimeIsolationConfiguration("Docker runsc remains single-tenant-trusted until the complete Docker isolation contract is accepted.")
	}
	if strings.TrimSpace(policy.RuntimeClassName) != "" {
		return invalidRuntimeIsolationConfiguration("Docker runtimeIsolation cannot include a Kubernetes RuntimeClass.")
	}
	compatibleProviders, err := normalizeGVisorCompatibleProviders(policy.GVisorCompatibleProviders)
	if err != nil {
		return err
	}
	policy.GVisorCompatibleProviders = compatibleProviders
	if policy.Mode == runtimeIsolationModeExplicit {
		if policy.Runtime != runtimeIsolationRunc && policy.Runtime != runtimeIsolationGVisor {
			return invalidRuntimeIsolationConfiguration("Docker explicit runtimeIsolation requires runc or gvisor.")
		}
		if len(policy.Preferred) != 0 {
			return invalidRuntimeIsolationConfiguration("Docker explicit runtimeIsolation cannot include preferred runtimes.")
		}
		if policy.Runtime != runtimeIsolationGVisor && len(policy.GVisorCompatibleProviders) != 0 {
			return invalidRuntimeIsolationConfiguration("Docker runtimeIsolation.gvisorCompatibleProviders requires the gvisor runtime.")
		}
		return nil
	}
	if policy.Runtime != "" {
		return invalidRuntimeIsolationConfiguration("Docker auto runtimeIsolation cannot include a fixed runtime.")
	}
	if len(policy.Preferred) == 0 {
		policy.Preferred = []string{runtimeIsolationGVisor, runtimeIsolationRunc}
	}
	seen := map[string]struct{}{}
	for index, candidate := range policy.Preferred {
		candidate = strings.TrimSpace(candidate)
		if candidate != runtimeIsolationRunc && candidate != runtimeIsolationGVisor {
			return invalidRuntimeIsolationConfiguration("Docker runtimeIsolation.preferred contains an unsupported runtime.")
		}
		if _, duplicate := seen[candidate]; duplicate {
			return invalidRuntimeIsolationConfiguration("Docker runtimeIsolation.preferred cannot contain duplicate runtimes.")
		}
		seen[candidate] = struct{}{}
		policy.Preferred[index] = candidate
	}
	if !slices.Contains(policy.Preferred, runtimeIsolationGVisor) && len(policy.GVisorCompatibleProviders) != 0 {
		return invalidRuntimeIsolationConfiguration("Docker runtimeIsolation.gvisorCompatibleProviders requires the gvisor runtime.")
	}
	return nil
}

func normalizeGVisorCompatibleProviders(providers []string) ([]string, error) {
	selected := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		canonical, valid := CanonicalStage3Provider(provider)
		if !valid {
			return nil, invalidRuntimeIsolationConfiguration("runtimeIsolation.gvisorCompatibleProviders contains an unknown Provider.")
		}
		if _, duplicate := selected[canonical]; duplicate {
			return nil, invalidRuntimeIsolationConfiguration("runtimeIsolation.gvisorCompatibleProviders cannot contain duplicate Providers.")
		}
		selected[canonical] = struct{}{}
	}
	result := make([]string, 0, len(selected))
	for _, provider := range stage3ProviderOrder {
		if _, found := selected[provider]; found {
			result = append(result, provider)
		}
	}
	return result, nil
}

func resolveDockerRuntimeIsolation(
	configuration dockerTargetConfiguration,
	runtimeNames []string,
) (runtimeIsolationDecision, error) {
	policy := configuration.RuntimeIsolation
	if policy == nil {
		return runtimeIsolationDecision{}, invalidRuntimeIsolationConfiguration("Docker runtimeIsolation was not normalized.")
	}
	available := map[string]bool{runtimeIsolationRunc: false, runtimeIsolationGVisor: false}
	for _, capability := range detectDockerRuntimeIsolationCapabilities(runtimeNames) {
		available[capability.Runtime] = true
	}
	candidates := policy.Preferred
	requestedRuntime := "auto"
	policySource := "target-auto"
	if policy.Mode == runtimeIsolationModeExplicit || policy.Mode == runtimeIsolationModeLegacy {
		candidates = []string{policy.Runtime}
		requestedRuntime = policy.Runtime
		policySource = "target-explicit"
		if policy.Mode == runtimeIsolationModeLegacy {
			policySource = "legacy-native"
		}
	}
	for index, candidate := range candidates {
		if !available[candidate] {
			continue
		}
		decision := "selected"
		if index > 0 {
			decision = "fallback"
		}
		return runtimeIsolationDecision{
			RequestedRuntime: requestedRuntime, RequestedProfile: platform.IsolationSingleTenantTrusted,
			EffectiveRuntime: candidate, EffectiveProfile: platform.IsolationSingleTenantTrusted,
			PolicySource: policySource, Decision: decision,
		}, nil
	}
	return runtimeIsolationDecision{
		RequestedRuntime: requestedRuntime, RequestedProfile: platform.IsolationSingleTenantTrusted,
		PolicySource: policySource, Decision: "rejected",
		DecisionReasonCode: "runtime_isolation_minimum_unavailable",
	}, problem.New(503, "runtime_isolation_minimum_unavailable", "No configured Docker runtime satisfies the runtime isolation policy.")
}

func detectDockerRuntimeIsolationCapabilities(runtimeNames []string) []runtimeIsolationCapability {
	available := map[string]bool{}
	for _, name := range runtimeNames {
		switch strings.TrimSpace(name) {
		case "runc":
			available[runtimeIsolationRunc] = true
		case "runsc":
			available[runtimeIsolationGVisor] = true
		}
	}
	capabilities := make([]runtimeIsolationCapability, 0, len(available))
	for _, runtime := range []string{runtimeIsolationGVisor, runtimeIsolationRunc} {
		if available[runtime] {
			capabilities = append(capabilities, runtimeIsolationCapability{
				Runtime: runtime, Profile: platform.IsolationSingleTenantTrusted,
			})
		}
	}
	return capabilities
}
