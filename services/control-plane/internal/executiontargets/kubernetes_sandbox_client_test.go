package executiontargets

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestObserveSandboxAcceptanceRequiresStoredCRDsAndUpdatingAssignmentFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.URL.Path, "/customresourcedefinitions/"):
			_, _ = writer.Write([]byte(`{"status":{"storedVersions":["v1beta1"],"conditions":[{"type":"Established","status":"True"}]}}`))
		case strings.HasSuffix(request.URL.Path, "/sandboxtemplates/synara-worker"):
			_, _ = writer.Write([]byte(`{
  "metadata":{"uid":"template-uid","resourceVersion":"42"},
  "spec":{"podTemplate":{"metadata":{"annotations":{"sandbox.cocoonstack.io/runtime":"standard"}},"spec":{
    "containers":[{"name":"agentd","image":"synara-agentd:test","env":[{"name":"SYNARA_AGENTD_ASSIGNED_EXECUTION_ID_FILE","value":"/var/run/synara/assignment/execution-id"}],"volumeMounts":[{"name":"assignment","mountPath":"/var/run/synara/assignment"}]}],
    "volumes":[{"name":"assignment","downwardAPI":{"items":[{"path":"execution-id","fieldRef":{"fieldPath":"metadata.labels['synara.io/assigned-execution-id']"}}]}}]
  }}}}`))
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
	observation, err := client.ObserveSandboxAcceptance(context.Background(), kubernetesTargetConfiguration{
		AllocationBackend: "sandbox-operator-standard", Namespace: "synara-workers",
		SandboxTemplateName: "synara-worker", SandboxWarmPoolName: "synara-interactive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.SandboxAPIReady || !observation.SandboxClaimAPIReady ||
		!observation.SandboxTemplateAPIReady || !observation.SandboxWarmPoolAPIReady ||
		!observation.OperatorReady || !observation.WarmPoolTemplateReady || !observation.WarmPoolReady ||
		!observation.AssignedExecutionFieldRefReady || observation.WarmPoolDesiredReplicas != 2 ||
		observation.TemplateIdentity != "template-uid:42" || observation.TemplateRuntime != "standard" ||
		observation.TemplateAgentdImage != "synara-agentd:test" || observation.WarmPoolUpdateStrategy != "Recreate" ||
		observation.TemplateSandboxRuntimeImage != "synara-agentd:test" ||
		!observation.WarmPoolTemplateImageFresh {
		t.Fatalf("Sandbox acceptance observation = %#v", observation)
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
	observation, err := client.ObserveSandboxAcceptance(context.Background(), kubernetesTargetConfiguration{
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
  {"metadata":{"labels":{"sandbox.cocoonstack.io/kvm-ready":"true"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
  {"metadata":{"labels":{"synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
		},
		{
			name: "static labels without a supervisor heartbeat are rejected",
			nodes: `{"items":[
  {"metadata":{"labels":{"sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
		},
		{
			name: "stale supervisor heartbeat is rejected",
			nodes: `{"items":[
  {"metadata":{"labels":{"sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"},"annotations":{"synara.io/host-supervisor-instance":"8debb31c-505a-4ad8-b42d-efa5f81e3261","synara.io/host-supervisor-observed-at":"$STALE$"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
		},
		{
			name: "complete live attestation on one node is accepted",
			nodes: `{"items":[
  {"metadata":{"labels":{"sandbox.cocoonstack.io/kvm-ready":"true","synara.io/host-supervisor":"v1","synara.io/provider-transport":"vsock-v2","synara.io/isolation-profile":"microvm-isolated-v1"},"annotations":{"synara.io/host-supervisor-instance":"8debb31c-505a-4ad8-b42d-efa5f81e3261","synara.io/host-supervisor-observed-at":"$FRESH$"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
]}`,
			wantSupervisorReady: true, wantFencedVSockReady: true, wantGuestIsolationReady: true,
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
  "spec":{"podTemplate":{"metadata":{"annotations":{"sandbox.cocoonstack.io/runtime":"vk-cocoon"}},"spec":{
					"containers":[{"name":"provider-guest","image":"synara-agentd:test"},{"name":"agentd","image":"ignored-agentd:old"}]
  }}}}`))
				case strings.HasSuffix(request.URL.Path, "/sandboxwarmpools/synara-interactive"):
					_, _ = writer.Write([]byte(`{"metadata":{"uid":"pool-uid"},"spec":{"replicas":1,"sandboxTemplateRef":{"name":"synara-worker"},"updateStrategy":{"type":"Recreate"}},"status":{"replicas":1,"readyReplicas":1}}`))
				case strings.HasSuffix(request.URL.Path, "/sandboxes"):
					_, _ = writer.Write([]byte(`{"items":[{"metadata":{"ownerReferences":[{"uid":"pool-uid"}]},"spec":{"podTemplate":{"spec":{"containers":[{"name":"provider-guest","image":"synara-agentd:test"},{"name":"agentd","image":"ignored-agentd:old"}]}}}}]}`))
				case request.URL.Path == "/api/v1/nodes":
					_, _ = writer.Write([]byte(nodes))
				default:
					http.Error(writer, fmt.Sprintf("unexpected path %s", request.URL.Path), http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
			observation, err := client.ObserveSandboxAcceptance(context.Background(), kubernetesTargetConfiguration{
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
