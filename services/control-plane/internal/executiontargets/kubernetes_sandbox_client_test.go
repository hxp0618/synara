package executiontargets

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var sandboxAcceptanceTargetID = uuid.MustParse("11111111-1111-4111-8111-111111111111")

func TestObserveSandboxAcceptanceRequiresStoredCRDsAndUpdatingAssignmentFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.URL.Path, "/customresourcedefinitions/"):
			_, _ = writer.Write([]byte(`{"status":{"storedVersions":["v1beta1"],"conditions":[{"type":"Established","status":"True"}]}}`))
		case strings.HasSuffix(request.URL.Path, "/sandboxtemplates/synara-worker"):
			_, _ = writer.Write(standardSandboxTemplateResponse(t, sandboxAcceptanceTargetID))
		case strings.HasSuffix(request.URL.Path, "/sandboxwarmpools/synara-interactive"):
			_, _ = writer.Write([]byte(`{"metadata":{"uid":"pool-uid"},"spec":{"replicas":2,"sandboxTemplateRef":{"name":"synara-worker"},"updateStrategy":{"type":"Recreate"}},"status":{"replicas":2,"readyReplicas":2}}`))
		case strings.HasSuffix(request.URL.Path, "/sandboxes"):
			_, _ = writer.Write([]byte(`{"items":[
  {"metadata":{"ownerReferences":[{"uid":"pool-uid"}]},"spec":{"podTemplate":{"spec":{"containers":[{"name":"agentd","image":"synara-agentd:test"}]}}}},
  {"metadata":{"ownerReferences":[{"uid":"pool-uid"}]},"spec":{"podTemplate":{"spec":{"containers":[{"name":"agentd","image":"synara-agentd:test"}]}}}}
]}`))
		default:
			http.Error(writer, fmt.Sprintf("unexpected path %s", request.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
	observation, err := client.ObserveSandboxAcceptance(context.Background(), sandboxAcceptanceTargetID, kubernetesTargetConfiguration{
		AllocationBackend: "sandbox-operator-standard", Namespace: "synara-workers",
		SandboxTemplateName: "synara-worker", SandboxWarmPoolName: "synara-interactive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.SandboxAPIReady || !observation.SandboxClaimAPIReady ||
		!observation.SandboxTemplateAPIReady || !observation.SandboxWarmPoolAPIReady ||
		!observation.OperatorReady || !observation.WarmPoolTemplateReady || !observation.WarmPoolReady ||
		!observation.AssignedExecutionFieldRefReady || !observation.TemplateObservabilityReady ||
		observation.WarmPoolDesiredReplicas != 2 ||
		observation.TemplateIdentity != "template-uid:42" || observation.TemplateRuntime != "standard" ||
		observation.TemplateAgentdImage != "synara-agentd:test" || observation.WarmPoolUpdateStrategy != "Recreate" ||
		observation.TemplateSandboxRuntimeImage != "synara-agentd:test" ||
		!observation.WarmPoolTemplateImageFresh {
		t.Fatalf("Sandbox acceptance observation = %#v", observation)
	}
}

func standardSandboxTemplateResponse(t *testing.T, targetID uuid.UUID) []byte {
	t.Helper()
	environment := append([]any{
		map[string]any{
			"name":  "SYNARA_AGENTD_ASSIGNED_EXECUTION_ID_FILE",
			"value": "/var/run/synara/assignment/execution-id",
		},
	}, kubernetesObservabilityEnvironment(targetID)...)
	mounts := []any{
		map[string]any{"name": "assignment", "mountPath": "/var/run/synara/assignment"},
	}
	volumes := []any{
		map[string]any{
			"name": "assignment",
			"downwardAPI": map[string]any{"items": []any{map[string]any{
				"path": "execution-id",
				"fieldRef": map[string]any{
					"fieldPath": "metadata.labels['synara.io/assigned-execution-id']",
				},
			}}},
		},
	}
	encoded, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": "template-uid", "resourceVersion": "42"},
		"spec": map[string]any{"podTemplate": map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{"sandbox.cocoonstack.io/runtime": "standard"}},
			"spec": map[string]any{
				"containers": []any{map[string]any{
					"name": "agentd", "image": "synara-agentd:test", "env": environment, "volumeMounts": mounts,
				}},
				"volumes": volumes,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestKubernetesSandboxTemplateObservabilityRequiresFixedOperatorAuthority(t *testing.T) {
	container := kubernetesSandboxTemplateContainer{Name: "agentd"}
	decodeJSONFixture(t, kubernetesObservabilityEnvironment(sandboxAcceptanceTargetID), &container.Env)
	if !kubernetesSandboxTemplateObservabilityReady(sandboxAcceptanceTargetID, container) {
		t.Fatal("valid operator observability template was rejected")
	}

	tests := []struct {
		name   string
		weaken func(*kubernetesSandboxTemplateContainer)
	}{
		{name: "renamed config", weaken: func(container *kubernetesSandboxTemplateContainer) {
			container.Env[0].ValueFrom.ConfigMapKeyRef.Name = "tenant-selected-config"
		}},
		{name: "required config", weaken: func(container *kubernetesSandboxTemplateContainer) {
			optional := false
			container.Env[0].ValueFrom.ConfigMapKeyRef.Optional = &optional
		}},
		{name: "client identity", weaken: func(container *kubernetesSandboxTemplateContainer) {
			container.Env = append(container.Env, kubernetesSandboxTemplateEnvironment{
				Name: "OTEL_EXPORTER_OTLP_CLIENT_KEY", Value: "/data/client.key",
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateContainer := container
			decodeJSONFixture(t, container, &candidateContainer)
			test.weaken(&candidateContainer)
			if kubernetesSandboxTemplateObservabilityReady(sandboxAcceptanceTargetID, candidateContainer) {
				t.Fatal("weakened observability template was accepted")
			}
		})
	}
}

func decodeJSONFixture(t *testing.T, input, output any) {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, output); err != nil {
		t.Fatal(err)
	}
}

func TestObserveSandboxAcceptanceRejectsAssignmentFileMountedOnlyBySidecar(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.URL.Path, "/customresourcedefinitions/"):
			_, _ = writer.Write([]byte(`{"status":{"storedVersions":["v1beta1"],"conditions":[{"type":"Established","status":"True"}]}}`))
		case strings.HasSuffix(request.URL.Path, "/sandboxtemplates/synara-worker"):
			_, _ = writer.Write([]byte(`{
  "metadata":{"uid":"template-uid","resourceVersion":"42"},
  "spec":{"podTemplate":{"metadata":{"annotations":{"sandbox.cocoonstack.io/runtime":"standard"}},"spec":{
    "containers":[
      {"name":"agentd","image":"synara-agentd:test"},
      {"name":"assignment-sidecar","env":[{"name":"SYNARA_AGENTD_ASSIGNED_EXECUTION_ID_FILE","value":"/var/run/synara/assignment/execution-id"}],"volumeMounts":[{"name":"assignment","mountPath":"/var/run/synara/assignment"}]}
    ],
    "volumes":[{"name":"assignment","downwardAPI":{"items":[{"path":"execution-id","fieldRef":{"fieldPath":"metadata.labels['synara.io/assigned-execution-id']"}}]}}]
  }}}}`))
		case strings.HasSuffix(request.URL.Path, "/sandboxwarmpools/synara-interactive"):
			_, _ = writer.Write([]byte(`{"metadata":{"uid":"pool-uid"},"spec":{"replicas":0,"sandboxTemplateRef":{"name":"synara-worker"},"updateStrategy":{"type":"Recreate"}},"status":{"replicas":0,"readyReplicas":0}}`))
		case strings.HasSuffix(request.URL.Path, "/sandboxes"):
			_, _ = writer.Write([]byte(`{"items":[]}`))
		default:
			http.Error(writer, fmt.Sprintf("unexpected path %s", request.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
	observation, err := client.ObserveSandboxAcceptance(context.Background(), sandboxAcceptanceTargetID, kubernetesTargetConfiguration{
		AllocationBackend: "sandbox-operator-standard", Namespace: "synara-workers",
		SandboxTemplateName: "synara-worker", SandboxWarmPoolName: "synara-interactive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.AssignedExecutionFieldRefReady {
		t.Fatal("assignment file mounted only by a sidecar must not satisfy the agentd template contract")
	}
}

func TestObserveSandboxAcceptanceRequiresCocoonAttestationOnSameReadyKVMNode(t *testing.T) {
	tests := []struct {
		name                    string
		nodes                   string
		wantSupervisorReady     bool
		wantFencedVSockReady    bool
		wantGuestIsolationReady bool
	}{
		{
			name: "split labels across nodes are rejected",
			nodes: `{"items":[
  {"metadata":{"labels":{"node.kubernetes.io/instance-type":"virtual-node","sandbox.cocoonstack.io/kvm-ready":"true"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
  {"metadata":{"labels":{"node.kubernetes.io/instance-type":"virtual-node","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
		},
		{
			name: "static labels without a supervisor heartbeat are rejected",
			nodes: `{"items":[
  {"metadata":{"labels":{"node.kubernetes.io/instance-type":"virtual-node","sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
		},
		{
			name: "stale supervisor heartbeat is rejected",
			nodes: `{"items":[
  {"metadata":{"labels":{"node.kubernetes.io/instance-type":"virtual-node","sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"},"annotations":{"synara.io/host-supervisor-instance":"8debb31c-505a-4ad8-b42d-efa5f81e3261","synara.io/host-supervisor-observed-at":"$STALE$"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
		},
		{
			name: "complete live attestation on one node is accepted",
			nodes: `{"items":[
  {"metadata":{"labels":{"node.kubernetes.io/instance-type":"virtual-node","sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"},"annotations":{"synara.io/host-supervisor-instance":"8debb31c-505a-4ad8-b42d-efa5f81e3261","synara.io/host-supervisor-observed-at":"$FRESH$"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
			wantSupervisorReady: true, wantFencedVSockReady: true, wantGuestIsolationReady: true,
		},
		{
			name: "one stale schedulable node poisons a fresh candidate",
			nodes: `{"items":[
  {"metadata":{"labels":{"node.kubernetes.io/instance-type":"virtual-node","sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"},"annotations":{"synara.io/host-supervisor-instance":"8debb31c-505a-4ad8-b42d-efa5f81e3261","synara.io/host-supervisor-observed-at":"$FRESH$"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
  {"metadata":{"labels":{"node.kubernetes.io/instance-type":"virtual-node","sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"},"annotations":{"synara.io/host-supervisor-instance":"1d9757fd-2ccc-4978-8be2-fdd0d6675f77","synara.io/host-supervisor-observed-at":"$STALE$"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nodes := strings.ReplaceAll(test.nodes, "$FRESH$", time.Now().UTC().Format(time.RFC3339Nano))
			nodes = strings.ReplaceAll(nodes, "$STALE$", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano))
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(request.URL.Path, "/customresourcedefinitions/"):
					_, _ = writer.Write([]byte(`{"status":{"storedVersions":["v1beta1"],"conditions":[{"type":"Established","status":"True"}]}}`))
				case strings.HasSuffix(request.URL.Path, "/sandboxtemplates/synara-worker"):
					_, _ = writer.Write([]byte(`{
  "metadata":{"uid":"template-uid","resourceVersion":"42"},
  "spec":{"podTemplate":{"metadata":{"annotations":{"sandbox.cocoonstack.io/runtime":"vk-cocoon","cocoonset.cocoonstack.io/snapshot-policy":"never","vm.cocoonstack.io/shared-memory":"true"}},"spec":{
					"nodeSelector":{"node.kubernetes.io/instance-type":"virtual-node","sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"},
					"tolerations":[
						{"key":"virtual-kubelet.io/provider","operator":"Equal","value":"cocoon","effect":"NoSchedule"}
					],
					"containers":[{"name":"agent","image":"synara-agentd:test"},{"name":"agentd","image":"ignored-agentd:old"}]
  }}}}`))
				case strings.HasSuffix(request.URL.Path, "/sandboxwarmpools/synara-interactive"):
					_, _ = writer.Write([]byte(`{"metadata":{"uid":"pool-uid"},"spec":{"replicas":1,"sandboxTemplateRef":{"name":"synara-worker"},"updateStrategy":{"type":"Recreate"}},"status":{"replicas":1,"readyReplicas":1}}`))
				case strings.HasSuffix(request.URL.Path, "/sandboxes"):
					_, _ = writer.Write([]byte(`{"items":[{"metadata":{"ownerReferences":[{"uid":"pool-uid"}]},"spec":{"podTemplate":{"spec":{"containers":[{"name":"agent","image":"synara-agentd:test"},{"name":"agentd","image":"ignored-agentd:old"}]}}}}]}`))
				case request.URL.Path == "/api/v1/nodes":
					_, _ = writer.Write([]byte(nodes))
				default:
					http.Error(writer, fmt.Sprintf("unexpected path %s", request.URL.Path), http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
			observation, err := client.ObserveSandboxAcceptance(context.Background(), sandboxAcceptanceTargetID, kubernetesTargetConfiguration{
				AllocationBackend: "sandbox-operator-cocoon", Namespace: "synara-workers",
				SandboxTemplateName: "synara-worker", SandboxWarmPoolName: "synara-interactive",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !observation.VirtualNodeReady || !observation.KVMRuntimeReady {
				t.Fatalf("base Cocoon runtime observation = %#v, want ready virtual KVM node", observation)
			}
			if observation.TemplateSandboxRuntimeImage != "synara-agentd:test" || observation.TemplateAgentdImage != "ignored-agentd:old" || observation.AssignedExecutionFieldRefReady {
				t.Fatalf("Cocoon guest template reused the in-Pod agentd contract: %#v", observation)
			}
			if !observation.WarmPoolTemplateImageFresh {
				t.Fatalf("Cocoon warm member guest image was not recognized: %#v", observation)
			}
			if observation.TemplateSandboxRuntimeName != "agent" || !observation.CocoonTemplateSchedulingReady ||
				!observation.CocoonTemplateCleanupReady || !observation.CocoonTemplateWorkspaceReady {
				t.Fatalf("Cocoon template scheduling contract = %#v, want attested-node fence", observation)
			}
			if observation.HostSupervisorReady != test.wantSupervisorReady ||
				observation.FencedVSockReady != test.wantFencedVSockReady ||
				observation.GuestIsolationReady != test.wantGuestIsolationReady {
				t.Fatalf("Cocoon attestation observation = %#v", observation)
			}
		})
	}
}

func TestKubernetesCocoonSupervisorHeartbeatReadyUsesBoundedFreshness(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	base := map[string]string{kubernetesCocoonSupervisorInstanceAnnotation: "8debb31c-505a-4ad8-b42d-efa5f81e3261"}
	tests := []struct {
		name      string
		heartbeat time.Time
		want      bool
	}{
		{name: "fresh", heartbeat: now.Add(-30 * time.Second), want: true},
		{name: "maximum age", heartbeat: now.Add(-kubernetesCocoonSupervisorHeartbeatMaxAge), want: true},
		{name: "stale", heartbeat: now.Add(-kubernetesCocoonSupervisorHeartbeatMaxAge - time.Nanosecond)},
		{name: "allowed future skew", heartbeat: now.Add(kubernetesCocoonSupervisorHeartbeatFutureSkew), want: true},
		{name: "future", heartbeat: now.Add(kubernetesCocoonSupervisorHeartbeatFutureSkew + time.Nanosecond)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			annotations := maps.Clone(base)
			annotations[kubernetesCocoonSupervisorObservedAtAnnotation] = test.heartbeat.Format(time.RFC3339Nano)
			if got := kubernetesCocoonSupervisorHeartbeatReady(annotations, now); got != test.want {
				t.Fatalf("heartbeat ready = %t, want %t", got, test.want)
			}
		})
	}
}

func TestKubernetesCocoonTemplateSchedulingRequiresAttestedNodesAndExactProviderTaint(t *testing.T) {
	selectors := map[string]string{
		"node.kubernetes.io/instance-type":     "virtual-node",
		kubernetesCocoonKVMReadyLabel:          "true",
		kubernetesCocoonHostSupervisorLabel:    kubernetesCocoonHostSupervisorV1,
		kubernetesCocoonProviderTransportLabel: kubernetesCocoonProviderTransportV2,
		kubernetesCocoonIsolationProfileLabel:  kubernetesCocoonIsolationProfileV1,
	}
	provider := []kubernetesSandboxTemplateToleration{{
		Key: "virtual-kubelet.io/provider", Operator: "Equal", Value: "cocoon", Effect: "NoSchedule",
	}}
	if !kubernetesCocoonTemplateSchedulingReady(selectors, provider) {
		t.Fatal("complete Cocoon scheduling fence was rejected")
	}
	missingIsolation := maps.Clone(selectors)
	delete(missingIsolation, kubernetesCocoonIsolationProfileLabel)
	if kubernetesCocoonTemplateSchedulingReady(missingIsolation, provider) {
		t.Fatal("template without an isolation-profile selector was accepted")
	}
	if kubernetesCocoonTemplateSchedulingReady(selectors, []kubernetesSandboxTemplateToleration{{
		Operator: "Exists", Effect: "NoSchedule",
	}}) {
		t.Fatal("broad NoSchedule toleration replaced the exact Cocoon provider fence")
	}
	if kubernetesCocoonTemplateSchedulingReady(selectors, []kubernetesSandboxTemplateToleration{{
		Key: "virtual-kubelet.io/provider", Operator: "Equal", Value: "cocoon",
	}}) {
		t.Fatal("provider toleration without an exact NoSchedule effect was accepted")
	}
}
