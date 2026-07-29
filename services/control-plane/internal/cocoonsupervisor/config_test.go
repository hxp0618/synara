package cocoonsupervisor

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestLoadConfigRequiresTargetScopedHostBoundary(t *testing.T) {
	root := t.TempDir()
	targetID := uuid.New()
	setSupervisorConfigEnvironment(t, root, targetID)
	config, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.ExecutionTargetID != targetID || config.VirtualNodeName != "vk-cocoon-stage-c-a" ||
		config.Namespace != "synara-workers" || config.PIDsMax != 512 || config.CapabilitiesJSON != "{}" ||
		config.AgentRequestTimeout != 30*time.Second ||
		!slices.Equal(config.GuestProviderCommand, []string{"/usr/local/bin/provider-host"}) {
		t.Fatalf("Cocoon supervisor config = %#v", config)
	}
	t.Setenv("SYNARA_COCOON_SUPERVISOR_GUEST_PROVIDER_COMMAND_JSON", `["/opt/synara/acceptance/provider-host","--fixture"]`)
	config, err = LoadConfig()
	if err != nil || !slices.Equal(config.GuestProviderCommand, []string{"/opt/synara/acceptance/provider-host", "--fixture"}) {
		t.Fatalf("custom Cocoon guest Provider command = %#v err=%v", config.GuestProviderCommand, err)
	}
}

func TestLoadConfigRejectsUnsafeSupervisorBoundary(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
		want  string
	}{
		{name: "invalid target", field: "SYNARA_COCOON_SUPERVISOR_EXECUTION_TARGET_ID", value: "target", want: "UUID"},
		{name: "invalid node", field: "SYNARA_COCOON_SUPERVISOR_VIRTUAL_NODE", value: "../../node", want: "Kubernetes names"},
		{name: "relative binary", field: "SYNARA_COCOON_SUPERVISOR_AGENTD", value: "agentd", want: "absolute path"},
		{name: "unbounded pids", field: platform.KubernetesPIDsLimitEnvironment, value: "0", want: platform.KubernetesPIDsLimitEnvironment},
		{name: "non object capabilities", field: "SYNARA_AGENTD_CAPABILITIES_JSON", value: "[]", want: "JSON object"},
		{name: "relative guest provider", field: "SYNARA_COCOON_SUPERVISOR_GUEST_PROVIDER_COMMAND_JSON", value: `["provider-host"]`, want: "absolute provider-host path"},
		{name: "wrong guest executable", field: "SYNARA_COCOON_SUPERVISOR_GUEST_PROVIDER_COMMAND_JSON", value: `["/usr/local/bin/node"]`, want: "absolute provider-host path"},
		{name: "excessive request timeout", field: "SYNARA_AGENTD_REQUEST_TIMEOUT", value: "6m", want: "between 5s and 5m"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setSupervisorConfigEnvironment(t, t.TempDir(), uuid.New())
			t.Setenv(test.field, test.value)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe supervisor config returned %v", err)
			}
		})
	}
}

func TestValidateAssignedPodRequiresExactGenerationVMAndWorkspaceFence(t *testing.T) {
	targetID := uuid.New()
	d := NewDaemon(Config{
		VirtualNodeName: "vk-cocoon-stage-c-a", Namespace: "synara-workers", ExecutionTargetID: targetID,
	}, nil)
	valid := validAssignedPodItem(t, targetID)
	pod, eligible, err := d.validateAssignedPod(valid)
	if err != nil || !eligible || pod.VMID != "YPBQMIFLFUYNGYRQX5YDOUOMRA" || pod.Generation != 7 {
		t.Fatalf("valid assigned Pod = %#v eligible=%t err=%v", pod, eligible, err)
	}
	tests := []struct {
		name   string
		mutate func(*podListItem)
		want   string
	}{
		{name: "wrong assigned execution", mutate: func(item *podListItem) { item.Metadata.Labels[assignedExecutionLabel] = uuid.NewString() }, want: "identity or Generation"},
		{name: "workspace prerequisite absent", mutate: func(item *podListItem) { delete(item.Metadata.Annotations, sharedMemoryAnnotation) }, want: "supervisor boundary"},
		{name: "Kubernetes token automount", mutate: func(item *podListItem) { value := true; item.Spec.AutomountServiceAccountToken = &value }, want: "runtime identity"},
		{name: "mutable guest tag", mutate: func(item *podListItem) { item.Spec.Containers[0].Image = "synara/guest:latest" }, want: "not immutable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := validAssignedPodItem(t, targetID)
			test.mutate(&item)
			if _, _, err := d.validateAssignedPod(item); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid assigned Pod returned %v", err)
			}
		})
	}
	item := validAssignedPodItem(t, targetID)
	item.Status.Conditions[0].Status = "False"
	if _, eligible, err := d.validateAssignedPod(item); err != nil || eligible {
		t.Fatalf("not-Ready assigned Pod eligible=%t err=%v", eligible, err)
	}
}

func setSupervisorConfigEnvironment(t *testing.T, root string, targetID uuid.UUID) {
	t.Helper()
	t.Setenv("SYNARA_COCOON_SUPERVISOR_EXECUTION_TARGET_ID", targetID.String())
	t.Setenv("SYNARA_CONTROL_PLANE_URL", "https://control.example.test")
	t.Setenv("SYNARA_COCOON_SUPERVISOR_VIRTUAL_NODE", "vk-cocoon-stage-c-a")
	t.Setenv("SYNARA_COCOON_SUPERVISOR_NAMESPACE", "synara-workers")
	t.Setenv(platform.KubernetesPIDsLimitEnvironment, "512")
	t.Setenv("SYNARA_AGENTD_CAPABILITIES_JSON", "{}")
	t.Setenv("SYNARA_COCOON_SUPERVISOR_STATE_ROOT", filepath.Join(root, "state"))
	t.Setenv("SYNARA_COCOON_SUPERVISOR_HEALTH_SOCKET", filepath.Join(root, "run", "health.sock"))
	for _, name := range []string{
		"SYNARA_COCOON_SUPERVISOR_KUBECTL", "SYNARA_COCOON_SUPERVISOR_COCOON",
		"SYNARA_COCOON_SUPERVISOR_VIRTIOFSD", "SYNARA_COCOON_SUPERVISOR_AGENTD",
		"SYNARA_COCOON_SUPERVISOR_TRANSPORT", "SYNARA_COCOON_SUPERVISOR_POLL_INTERVAL",
		"SYNARA_COCOON_SUPERVISOR_GUEST_PROVIDER_COMMAND_JSON",
		"SYNARA_AGENTD_VERSION", "SYNARA_AGENTD_BUILD_GIT_SHA", "SYNARA_AGENTD_CLUSTER_ID",
		"SYNARA_AGENTD_REQUEST_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}

func validAssignedPodItem(t *testing.T, targetID uuid.UUID) podListItem {
	t.Helper()
	executionID := uuid.NewString()
	encoded := `{
  "metadata":{
    "name":"synara-worker-1","namespace":"synara-workers","uid":"` + uuid.NewString() + `",
    "labels":{
      "synara.io/execution-target-id":"` + targetID.String() + `",
      "synara.io/execution-id":"` + executionID + `",
      "synara.io/assigned-execution-id":"` + executionID + `",
      "synara.io/generation":"7","synara.io/worker-mode":"execution-pinned",
      "synara.io/allocation-backend":"sandbox-operator-cocoon"
    },
    "annotations":{
      "vm.cocoonstack.io/id":"YPBQMIFLFUYNGYRQX5YDOUOMRA",
      "vm.cocoonstack.io/shared-memory":"true"
    }
  },
  "spec":{
    "nodeName":"vk-cocoon-stage-c-a","serviceAccountName":"synara-worker",
    "automountServiceAccountToken":false,
    "containers":[{"name":"agent","image":"registry.example/synara/guest@sha256:` + strings.Repeat("a", 64) + `"}]
  },
  "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}
}`
	var item podListItem
	if err := json.Unmarshal([]byte(encoded), &item); err != nil {
		t.Fatal(err)
	}
	return item
}
