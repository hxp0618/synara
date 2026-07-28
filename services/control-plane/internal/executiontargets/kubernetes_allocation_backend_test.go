package executiontargets

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestKubernetesAllocationBackendDefaultsToNativePod(t *testing.T) {
	configuration := kubernetesTargetConfiguration{}
	if err := normalizeKubernetesAllocationConfiguration(&configuration); err != nil {
		t.Fatalf("normalize allocation backend: %v", err)
	}
	if configuration.AllocationBackend != "native-pod" {
		t.Fatalf("allocation backend = %q, want native-pod", configuration.AllocationBackend)
	}
	adapter, err := kubernetesAllocationAdapterFor(configuration.AllocationBackend)
	if err != nil {
		t.Fatalf("resolve native adapter: %v", err)
	}
	if acceptance := adapter.Accept(context.Background(), KubernetesAllocationAcceptanceObservation{}); !acceptance.Accepted {
		t.Fatalf("native target acceptance = %#v, want accepted", acceptance)
	}
}

func TestKubernetesSandboxExecutionTenantAllowlist(t *testing.T) {
	allowedTenantID := uuid.New()
	configuration := kubernetesTargetConfiguration{SandboxAllowedTenantIDs: []uuid.UUID{allowedTenantID}}
	if err := validateKubernetesSandboxExecutionTenants(
		configuration,
		[]kubernetesExecution{{TenantID: allowedTenantID}},
	); err != nil {
		t.Fatalf("allow listed Execution tenant: %v", err)
	}
	err := validateKubernetesSandboxExecutionTenants(
		configuration,
		[]kubernetesExecution{{TenantID: uuid.New()}},
	)
	assertProblemCode(t, err, 403, "kubernetes_sandbox_execution_tenant_not_allowed")
}

func TestKubernetesAllocationBackendConfiguration(t *testing.T) {
	for _, backend := range []string{"sandbox-operator-standard", "sandbox-operator-cocoon"} {
		t.Run(backend, func(t *testing.T) {
			configuration := kubernetesTargetConfiguration{
				AllocationBackend:   backend,
				SandboxTemplateName: "synara-worker",
				SandboxWarmPoolName: "synara-interactive",
			}
			if err := normalizeKubernetesAllocationConfiguration(&configuration); err != nil {
				t.Fatalf("normalize allocation backend: %v", err)
			}
			if configuration.SandboxClaimReadyTimeoutSeconds != 30 {
				t.Fatalf("claim timeout = %d, want 30", configuration.SandboxClaimReadyTimeoutSeconds)
			}
		})
	}
}

func TestKubernetesAllocationBackendRejectsUnsafeConfiguration(t *testing.T) {
	tests := []kubernetesTargetConfiguration{
		{AllocationBackend: "unknown"},
		{AllocationBackend: "native-pod", SandboxTemplateName: "unexpected"},
		{AllocationBackend: "sandbox-operator-standard", SandboxTemplateName: "missing-pool"},
		{
			AllocationBackend: "sandbox-operator-cocoon", SandboxTemplateName: "synara-worker",
			SandboxWarmPoolName: "synara-interactive", SandboxClaimReadyTimeoutSeconds: 301,
		},
	}
	for index := range tests {
		if err := normalizeKubernetesAllocationConfiguration(&tests[index]); err == nil {
			t.Fatalf("case %d accepted unsafe allocation configuration: %#v", index, tests[index])
		}
	}
}

func TestKubernetesAllocationTargetAcceptance(t *testing.T) {
	base := KubernetesAllocationAcceptanceObservation{
		SandboxAPIReady: true, SandboxClaimAPIReady: true, SandboxWarmPoolAPIReady: true,
		OperatorReady: true, TemplateReady: true, WarmPoolReady: true,
	}
	tests := []struct {
		name        string
		backend     string
		observation KubernetesAllocationAcceptanceObservation
		accepted    bool
		reasonCode  string
	}{
		{name: "native", backend: "native-pod", accepted: true},
		{
			name: "standard", backend: "sandbox-operator-standard",
			observation: withStandardRuntime(base), accepted: true,
		},
		{
			name: "standard runtime missing", backend: "sandbox-operator-standard",
			observation: base, reasonCode: "sandbox-operator-standard-runtime-unavailable",
		},
		{
			name: "cocoon", backend: "sandbox-operator-cocoon",
			observation: withCocoonRuntime(base), accepted: true,
		},
		{
			name: "cocoon kvm missing", backend: "sandbox-operator-cocoon",
			observation: KubernetesAllocationAcceptanceObservation{
				SandboxAPIReady: true, SandboxClaimAPIReady: true, SandboxWarmPoolAPIReady: true,
				OperatorReady: true, TemplateReady: true, WarmPoolReady: true, CocoonVirtualNodeReady: true,
			},
			reasonCode: "sandbox-operator-cocoon-runtime-unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acceptance, err := EvaluateKubernetesAllocationAcceptance(
				context.Background(), test.backend, test.observation,
			)
			if err != nil {
				t.Fatalf("evaluate target acceptance: %v", err)
			}
			if acceptance.Accepted != test.accepted || acceptance.ReasonCode != test.reasonCode {
				t.Fatalf("acceptance = %#v, want accepted=%v reason=%q", acceptance, test.accepted, test.reasonCode)
			}
		})
	}
}

func withStandardRuntime(observation KubernetesAllocationAcceptanceObservation) KubernetesAllocationAcceptanceObservation {
	observation.StandardRuntimeReady = true
	return observation
}

func withCocoonRuntime(observation KubernetesAllocationAcceptanceObservation) KubernetesAllocationAcceptanceObservation {
	observation.CocoonVirtualNodeReady = true
	observation.CocoonKVMRuntimeReady = true
	return observation
}
