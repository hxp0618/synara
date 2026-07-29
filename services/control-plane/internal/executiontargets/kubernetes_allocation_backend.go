package executiontargets

import (
	"context"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type kubernetesAllocationBackend string

const (
	kubernetesAllocationBackendNativePod               kubernetesAllocationBackend = "native-pod"
	kubernetesAllocationBackendSandboxOperatorStandard kubernetesAllocationBackend = "sandbox-operator-standard"
	kubernetesAllocationBackendSandboxOperatorCocoon   kubernetesAllocationBackend = "sandbox-operator-cocoon"

	kubernetesDefaultSandboxClaimReadyTimeoutSeconds = 30
	kubernetesMaxSandboxClaimReadyTimeoutSeconds     = 300
)

type KubernetesAllocationAcceptanceObservation struct {
	SandboxAPIReady            bool
	SandboxClaimAPIReady       bool
	SandboxWarmPoolAPIReady    bool
	OperatorReady              bool
	TemplateReady              bool
	WarmPoolReady              bool
	StandardRuntimeReady       bool
	CocoonVirtualNodeReady     bool
	CocoonKVMRuntimeReady      bool
	CocoonHostSupervisorReady  bool
	CocoonFencedVSockReady     bool
	CocoonGuestIsolationReady  bool
	CocoonGuestContainerReady  bool
	CocoonSchedulingFenceReady bool
	CocoonCleanupPolicyReady   bool
	CocoonWorkspaceReady       bool
}

type KubernetesAllocationAcceptance struct {
	Backend    string
	Accepted   bool
	ReasonCode string
}

type kubernetesAllocationAdapter interface {
	Backend() kubernetesAllocationBackend
	Accept(context.Context, KubernetesAllocationAcceptanceObservation) KubernetesAllocationAcceptance
}

func EvaluateKubernetesAllocationAcceptance(
	ctx context.Context,
	backend string,
	observation KubernetesAllocationAcceptanceObservation,
) (KubernetesAllocationAcceptance, error) {
	adapter, err := kubernetesAllocationAdapterFor(backend)
	if err != nil {
		return KubernetesAllocationAcceptance{}, err
	}
	return adapter.Accept(ctx, observation), nil
}

type nativePodAllocationAdapter struct{}

func (nativePodAllocationAdapter) Backend() kubernetesAllocationBackend {
	return kubernetesAllocationBackendNativePod
}

func (adapter nativePodAllocationAdapter) Accept(
	_ context.Context,
	_ KubernetesAllocationAcceptanceObservation,
) KubernetesAllocationAcceptance {
	return acceptedKubernetesAllocation(adapter.Backend())
}

type sandboxOperatorAllocationAdapter struct {
	backend kubernetesAllocationBackend
}

func (adapter sandboxOperatorAllocationAdapter) Backend() kubernetesAllocationBackend {
	return adapter.backend
}

func (adapter sandboxOperatorAllocationAdapter) Accept(
	_ context.Context,
	observation KubernetesAllocationAcceptanceObservation,
) KubernetesAllocationAcceptance {
	if !observation.SandboxAPIReady || !observation.SandboxClaimAPIReady || !observation.SandboxWarmPoolAPIReady {
		return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-api-unavailable")
	}
	if !observation.OperatorReady {
		return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-controller-unavailable")
	}
	if !observation.TemplateReady || !observation.WarmPoolReady {
		return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-capacity-unavailable")
	}
	switch adapter.Backend() {
	case kubernetesAllocationBackendSandboxOperatorStandard:
		if !observation.StandardRuntimeReady {
			return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-standard-runtime-unavailable")
		}
	case kubernetesAllocationBackendSandboxOperatorCocoon:
		if !observation.CocoonVirtualNodeReady || !observation.CocoonKVMRuntimeReady {
			return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-cocoon-runtime-unavailable")
		}
		if !observation.CocoonGuestContainerReady {
			return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-cocoon-guest-contract-unavailable")
		}
		if !observation.CocoonSchedulingFenceReady {
			return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-cocoon-scheduling-fence-unavailable")
		}
		if !observation.CocoonCleanupPolicyReady {
			return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-cocoon-cleanup-policy-unavailable")
		}
		if !observation.CocoonWorkspaceReady {
			return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-cocoon-workspace-unavailable")
		}
		if !observation.CocoonHostSupervisorReady || !observation.CocoonFencedVSockReady ||
			!observation.CocoonGuestIsolationReady {
			return rejectedKubernetesAllocation(adapter.Backend(), "sandbox-operator-cocoon-supervisor-unavailable")
		}
	default:
		return rejectedKubernetesAllocation(adapter.Backend(), "kubernetes-allocation-backend-unsupported")
	}
	return acceptedKubernetesAllocation(adapter.Backend())
}

func kubernetesAllocationAdapterFor(backend string) (kubernetesAllocationAdapter, error) {
	switch kubernetesAllocationBackend(strings.TrimSpace(backend)) {
	case kubernetesAllocationBackendNativePod:
		return nativePodAllocationAdapter{}, nil
	case kubernetesAllocationBackendSandboxOperatorStandard:
		return sandboxOperatorAllocationAdapter{backend: kubernetesAllocationBackendSandboxOperatorStandard}, nil
	case kubernetesAllocationBackendSandboxOperatorCocoon:
		return sandboxOperatorAllocationAdapter{backend: kubernetesAllocationBackendSandboxOperatorCocoon}, nil
	default:
		return nil, problem.New(
			400,
			"invalid_kubernetes_allocation_backend",
			"Kubernetes allocationBackend must be native-pod, sandbox-operator-standard, or sandbox-operator-cocoon.",
		)
	}
}

func normalizeKubernetesAllocationConfiguration(configuration *kubernetesTargetConfiguration) error {
	configuration.AllocationBackend = strings.TrimSpace(configuration.AllocationBackend)
	if configuration.AllocationBackend == "" {
		configuration.AllocationBackend = string(kubernetesAllocationBackendNativePod)
	}
	if _, err := kubernetesAllocationAdapterFor(configuration.AllocationBackend); err != nil {
		return err
	}
	configuration.SandboxTemplateName = strings.TrimSpace(configuration.SandboxTemplateName)
	configuration.SandboxWarmPoolName = strings.TrimSpace(configuration.SandboxWarmPoolName)
	if configuration.AllocationBackend == string(kubernetesAllocationBackendNativePod) {
		if configuration.SandboxTemplateName != "" || configuration.SandboxWarmPoolName != "" ||
			configuration.SandboxClaimReadyTimeoutSeconds != 0 {
			return problem.New(
				400,
				"invalid_kubernetes_allocation_backend_configuration",
				"Native Kubernetes Pod allocation cannot include sandbox-operator settings.",
			)
		}
		return nil
	}
	if !kubernetesNamePattern.MatchString(configuration.SandboxTemplateName) ||
		len(configuration.SandboxTemplateName) > 63 ||
		!kubernetesNamePattern.MatchString(configuration.SandboxWarmPoolName) ||
		len(configuration.SandboxWarmPoolName) > 63 {
		return problem.New(
			400,
			"invalid_kubernetes_allocation_backend_configuration",
			"Sandbox operator template and warm-pool names must be valid Kubernetes names.",
		)
	}
	if configuration.SandboxClaimReadyTimeoutSeconds == 0 {
		configuration.SandboxClaimReadyTimeoutSeconds = kubernetesDefaultSandboxClaimReadyTimeoutSeconds
	}
	if configuration.SandboxClaimReadyTimeoutSeconds < 1 ||
		configuration.SandboxClaimReadyTimeoutSeconds > kubernetesMaxSandboxClaimReadyTimeoutSeconds {
		return problem.New(
			400,
			"invalid_kubernetes_allocation_backend_configuration",
			"Sandbox claim readiness timeout must be between 1 and 300 seconds.",
		)
	}
	return nil
}

func acceptedKubernetesAllocation(backend kubernetesAllocationBackend) KubernetesAllocationAcceptance {
	return KubernetesAllocationAcceptance{Backend: string(backend), Accepted: true}
}

func rejectedKubernetesAllocation(
	backend kubernetesAllocationBackend,
	reasonCode string,
) KubernetesAllocationAcceptance {
	return KubernetesAllocationAcceptance{Backend: string(backend), ReasonCode: reasonCode}
}
