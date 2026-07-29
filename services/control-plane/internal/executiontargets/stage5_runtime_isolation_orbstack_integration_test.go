package executiontargets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

const kubernetesStage5RuntimeIsolationIntegrationEnv = "SYNARA_KUBERNETES_STAGE5_RUNTIME_ISOLATION_TEST"
const kubernetesStage5NodeNameEnv = "SYNARA_TEST_KUBERNETES_NODE_NAME"
const kubernetesStage5AllNodesEnv = "SYNARA_TEST_KUBERNETES_ALL_NODES"
const kubernetesStage5NodeSelectorEnv = "SYNARA_TEST_KUBERNETES_NODE_SELECTOR"
const kubernetesStage5ImagePullPolicyEnv = "SYNARA_TEST_KUBERNETES_IMAGE_PULL_POLICY"

func TestKubernetesStage5RuntimeIsolation(t *testing.T) {
	if os.Getenv(kubernetesStage5RuntimeIsolationIntegrationEnv) != "1" {
		t.Skip(kubernetesStage5RuntimeIsolationIntegrationEnv + "=1 is required")
	}
	namespace := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_NAMESPACE"))
	image := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_WORKER_IMAGE"))
	if namespace == "" || image == "" {
		t.Fatal("SYNARA_TEST_KUBERNETES_NAMESPACE and SYNARA_TEST_KUBERNETES_WORKER_IMAGE are required")
	}
	kubernetesContext := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CONTEXT"))
	if kubernetesContext == "" {
		kubernetesContext = "orbstack"
	}
	imagePullPolicy := stage5ImagePullPolicy(t)
	nodeNames := stage5RuntimeIsolationNodeNames(t, kubernetesContext)
	for _, nodeName := range nodeNames {
		nodeName := nodeName
		testName := "scheduler-selected"
		if nodeName != "" {
			testName = strings.NewReplacer("/", "_", "\\", "_").Replace(nodeName)
		}
		t.Run(testName, func(t *testing.T) {
			runKubernetesStage5RuntimeIsolation(
				t, kubernetesContext, namespace, image, imagePullPolicy, nodeName,
			)
		})
	}
}

func runKubernetesStage5RuntimeIsolation(
	t *testing.T,
	kubernetesContext, namespace, image, imagePullPolicy, nodeName string,
) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	peerName := "synara-stage5-peer-" + suffix
	pressureName := "synara-stage5-pressure-" + suffix
	memoryName := "synara-stage5-memory-" + suffix
	policyName := "synara-stage5-egress-" + suffix
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	manifest := stage5RuntimeIsolationManifest(
		namespace, image, imagePullPolicy, nodeName, suffix, peerName, pressureName, memoryName, policyName,
	)
	if output, err := runStage5Kubectl(ctx, manifest, "--context", kubernetesContext, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply Stage 5 runtime isolation Pods: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, resource := range []string{
			"pod/" + peerName,
			"pod/" + pressureName,
			"pod/" + memoryName,
			"networkpolicy/" + policyName,
		} {
			_, _ = runStage5Kubectl(
				cleanupCtx, nil, "--context", kubernetesContext, "-n", namespace,
				"delete", resource, "--ignore-not-found=true", "--wait=true", "--timeout=25s",
			)
		}
	})
	for _, podName := range []string{peerName, pressureName, memoryName} {
		if output, err := runStage5Kubectl(
			ctx, nil, "--context", kubernetesContext, "-n", namespace,
			"wait", "--for=condition=PodScheduled", "pod/"+podName, "--timeout=45s",
		); err != nil {
			t.Fatalf("wait for Stage 5 Pod %s scheduling: %v\n%s", podName, err, output)
		}
	}
	pressureNode := stage5PodNodeName(t, ctx, kubernetesContext, namespace, pressureName)
	for _, podName := range []string{peerName, memoryName} {
		podNode := stage5PodNodeName(t, ctx, kubernetesContext, namespace, podName)
		if podNode != pressureNode {
			t.Fatalf("Stage 5 pressure probes were not co-located: pressure=%s peer=%s node=%s", pressureNode, podName, podNode)
		}
	}
	if nodeName != "" && pressureNode != nodeName {
		t.Fatalf("Stage 5 pressure probes requested node %s but ran on %s", nodeName, pressureNode)
	}
	if output, err := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace,
		"wait", "--for=condition=Ready", "pod/"+peerName, "--timeout=45s",
	); err != nil {
		t.Fatalf("wait for peer Pod: %v\n%s", err, output)
	}
	peerChecks := 0
	for index := 0; index < 12; index++ {
		output, err := runStage5Kubectl(
			ctx, nil, "--context", kubernetesContext, "-n", namespace,
			"exec", peerName, "--", "node", "-e", "process.stdout.write('peer-ok')",
		)
		if err == nil && strings.TrimSpace(output) == "peer-ok" {
			peerChecks++
		}
		time.Sleep(250 * time.Millisecond)
	}
	if output, err := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace,
		"wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+pressureName, "--timeout=60s",
	); err != nil {
		t.Fatalf("wait for fork/CPU pressure Pod: %v\n%s\n%s", err, output, stage5PodDiagnostics(kubernetesContext, namespace, pressureName))
	}
	pressureLogs, err := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace, "logs", pressureName,
	)
	if err != nil {
		t.Fatalf("read fork/CPU pressure result: %v\n%s", err, pressureLogs)
	}
	var pressureResult struct {
		MetadataBlocked bool `json:"metadataBlocked"`
		ForksStarted    int  `json:"forksStarted"`
		ForksRejected   int  `json:"forksRejected"`
		CPUMaxFinite    bool `json:"cpuMaxFinite"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(pressureLogs)), &pressureResult); err != nil {
		t.Fatalf("decode fork/CPU pressure result %q: %v", pressureLogs, err)
	}
	if !pressureResult.MetadataBlocked || pressureResult.ForksStarted < 64 ||
		pressureResult.ForksRejected == 0 || !pressureResult.CPUMaxFinite {
		t.Fatalf("fork/CPU/metadata assertions failed: %#v", pressureResult)
	}
	if output, err := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace,
		"wait", "--for=jsonpath={.status.containerStatuses[0].state.terminated.reason}=OOMKilled",
		"pod/"+memoryName, "--timeout=60s",
	); err != nil {
		t.Fatalf("memory pressure Pod was not OOM-killed: %v\n%s\n%s", err, output, stage5PodDiagnostics(kubernetesContext, namespace, memoryName))
	}
	if peerChecks != 12 {
		t.Fatalf("peer Pod lost responsiveness during bounded pressure: %d/12 checks passed", peerChecks)
	}
	t.Logf(
		"Stage 5 Kubernetes runtime isolation PASS namespace=%s node=%s peerChecks=%d forksStarted=%d forksRejected=%d metadataBlocked=%t cpuMaxFinite=%t memory=OOMKilled",
		namespace,
		pressureNode,
		peerChecks,
		pressureResult.ForksStarted,
		pressureResult.ForksRejected,
		pressureResult.MetadataBlocked,
		pressureResult.CPUMaxFinite,
	)
}

func TestKubernetesStage5RegistrationTokenHandoff(t *testing.T) {
	if os.Getenv(kubernetesStage5RuntimeIsolationIntegrationEnv) != "1" {
		t.Skip(kubernetesStage5RuntimeIsolationIntegrationEnv + "=1 is required")
	}
	namespace := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_NAMESPACE"))
	image := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_WORKER_IMAGE"))
	if namespace == "" || image == "" {
		t.Fatal("SYNARA_TEST_KUBERNETES_NAMESPACE and SYNARA_TEST_KUBERNETES_WORKER_IMAGE are required")
	}
	kubernetesContext := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CONTEXT"))
	if kubernetesContext == "" {
		kubernetesContext = "orbstack"
	}
	nodeName := strings.TrimSpace(os.Getenv(kubernetesStage5NodeNameEnv))
	imagePullPolicy := stage5ImagePullPolicy(t)
	podName := "synara-stage5-token-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manifest := stage5RegistrationTokenHandoffManifest(namespace, image, imagePullPolicy, nodeName, podName)
	if output, err := runStage5Kubectl(ctx, manifest, "--context", kubernetesContext, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply Stage 5 registration-token Pod: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = runStage5Kubectl(
			cleanupCtx, nil, "--context", kubernetesContext, "-n", namespace,
			"delete", "pod/"+podName, "--ignore-not-found=true", "--wait=true", "--timeout=25s",
		)
	})
	if output, err := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace,
		"wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+podName, "--timeout=60s",
	); err != nil {
		t.Fatalf(
			"wait for registration-token handoff Pod: %v\n%s\n%s",
			err,
			output,
			stage5PodDiagnostics(kubernetesContext, namespace, podName),
		)
	}
	logs, err := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace, "logs", podName, "-c", "consumer",
	)
	if err != nil {
		t.Fatalf("read registration-token handoff result: %v\n%s", err, logs)
	}
	var result struct {
		ProjectedMounted bool `json:"projectedMounted"`
		StagedInitially  bool `json:"stagedInitially"`
		StagedConsumed   bool `json:"stagedConsumed"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs)), &result); err != nil {
		t.Fatalf("decode registration-token handoff result %q: %v", logs, err)
	}
	if result.ProjectedMounted || !result.StagedInitially || !result.StagedConsumed {
		t.Fatalf("registration-token handoff assertions failed: %#v", result)
	}
	t.Logf(
		"Stage 5 Kubernetes registration-token handoff PASS namespace=%s projectedMounted=%t stagedInitially=%t stagedConsumed=%t",
		namespace,
		result.ProjectedMounted,
		result.StagedInitially,
		result.StagedConsumed,
	)
}

func TestStage5RuntimeIsolationManifestCoLocatesPressureGroup(t *testing.T) {
	manifest := stage5RuntimeIsolationManifest(
		"stage5-test", "worker:test", "IfNotPresent", "worker-a", "run-a",
		"peer-a", "pressure-a", "memory-a", "policy-a",
	)
	podsByRole := map[string]map[string]any{}
	for _, document := range bytes.Split(manifest, []byte("\n---\n")) {
		document = bytes.TrimSpace(document)
		if len(document) == 0 {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal(document, &object); err != nil {
			t.Fatalf("decode Stage 5 manifest document: %v", err)
		}
		if object["kind"] != "Pod" {
			continue
		}
		metadata := object["metadata"].(map[string]any)
		labels := metadata["labels"].(map[string]any)
		role := labels["synara.io/stage5-runtime-role"].(string)
		podsByRole[role] = object
	}
	if len(podsByRole) != 3 {
		t.Fatalf("expected three Stage 5 probe Pods, got %d", len(podsByRole))
	}
	pressureSpec := podsByRole["pressure"]["spec"].(map[string]any)
	if pressureSpec["nodeName"] != "worker-a" {
		t.Fatalf("pressure Pod nodeName = %v, want worker-a", pressureSpec["nodeName"])
	}
	for _, role := range []string{"peer", "memory"} {
		spec := podsByRole[role]["spec"].(map[string]any)
		if spec["nodeName"] != "worker-a" {
			t.Fatalf("%s Pod nodeName = %v, want worker-a", role, spec["nodeName"])
		}
		affinityJSON, err := json.Marshal(spec["affinity"])
		if err != nil {
			t.Fatalf("encode %s Pod affinity: %v", role, err)
		}
		if !strings.Contains(string(affinityJSON), `"synara.io/stage5-runtime-run":"run-a"`) ||
			!strings.Contains(string(affinityJSON), `"synara.io/stage5-runtime-role":"pressure"`) {
			t.Fatalf("%s Pod does not require pressure-Pod co-location: %s", role, affinityJSON)
		}
	}
	for role, pod := range podsByRole {
		spec := pod["spec"].(map[string]any)
		container := spec["containers"].([]any)[0].(map[string]any)
		if container["imagePullPolicy"] != "IfNotPresent" {
			t.Fatalf("%s Pod imagePullPolicy = %v", role, container["imagePullPolicy"])
		}
	}
	registrationManifest := stage5RegistrationTokenHandoffManifest(
		"stage5-test", "worker:test", "Always", "worker-a", "token-a",
	)
	var registrationPod map[string]any
	if err := json.Unmarshal(registrationManifest, &registrationPod); err != nil {
		t.Fatalf("decode Stage 5 registration-token manifest: %v", err)
	}
	registrationSpec := registrationPod["spec"].(map[string]any)
	if registrationSpec["nodeName"] != "worker-a" {
		t.Fatalf("registration-token Pod nodeName = %v, want worker-a", registrationSpec["nodeName"])
	}
	for _, containerKey := range []string{"initContainers", "containers"} {
		container := registrationSpec[containerKey].([]any)[0].(map[string]any)
		if container["imagePullPolicy"] != "Always" {
			t.Fatalf("registration-token %s imagePullPolicy = %v", containerKey, container["imagePullPolicy"])
		}
	}
}

func TestStage5ReadySchedulableNodeNames(t *testing.T) {
	payload := []byte(`{
  "items": [
    {"metadata":{"name":"worker-b"},"spec":{},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
    {"metadata":{"name":"worker-a"},"spec":{},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
    {"metadata":{"name":"not-ready"},"spec":{},"status":{"conditions":[{"type":"Ready","status":"False"}]}},
    {"metadata":{"name":"cordoned"},"spec":{"unschedulable":true},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
  ]
}`)
	nodeNames, err := stage5ReadySchedulableNodeNames(payload)
	if err != nil {
		t.Fatalf("decode Stage 5 node list: %v", err)
	}
	if strings.Join(nodeNames, ",") != "worker-a,worker-b" {
		t.Fatalf("ready schedulable nodes = %v, want [worker-a worker-b]", nodeNames)
	}
	if _, err := stage5ReadySchedulableNodeNames([]byte(`{"items":`)); err == nil {
		t.Fatal("malformed Stage 5 node list was accepted")
	}
}

func stage5RuntimeIsolationNodeNames(t *testing.T, kubernetesContext string) []string {
	t.Helper()
	explicitNodeName := strings.TrimSpace(os.Getenv(kubernetesStage5NodeNameEnv))
	allNodes := strings.TrimSpace(os.Getenv(kubernetesStage5AllNodesEnv))
	nodeSelector := strings.TrimSpace(os.Getenv(kubernetesStage5NodeSelectorEnv))
	if allNodes == "" || allNodes == "0" {
		if nodeSelector != "" {
			t.Fatalf("%s requires %s=1", kubernetesStage5NodeSelectorEnv, kubernetesStage5AllNodesEnv)
		}
		return []string{explicitNodeName}
	}
	if allNodes != "1" {
		t.Fatalf("%s must be 0 or 1", kubernetesStage5AllNodesEnv)
	}
	if explicitNodeName != "" {
		t.Fatalf("%s and %s=1 are mutually exclusive", kubernetesStage5NodeNameEnv, kubernetesStage5AllNodesEnv)
	}
	if nodeSelector == "" {
		t.Fatalf("%s=1 requires a non-empty %s", kubernetesStage5AllNodesEnv, kubernetesStage5NodeSelectorEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext,
		"get", "nodes", "-l", nodeSelector, "-o", "json",
	)
	if err != nil {
		t.Fatalf("list Stage 5 target nodes for selector %q: %v\n%s", nodeSelector, err, output)
	}
	nodeNames, err := stage5ReadySchedulableNodeNames([]byte(output))
	if err != nil {
		t.Fatalf("decode Stage 5 target nodes for selector %q: %v", nodeSelector, err)
	}
	if len(nodeNames) == 0 {
		t.Fatalf("selector %q matched no Ready schedulable Stage 5 target nodes", nodeSelector)
	}
	t.Logf("Stage 5 all-node matrix selector=%q nodes=%s", nodeSelector, strings.Join(nodeNames, ","))
	return nodeNames
}

func stage5ReadySchedulableNodeNames(payload []byte) ([]string, error) {
	var nodeList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &nodeList); err != nil {
		return nil, err
	}
	nodeNames := make([]string, 0, len(nodeList.Items))
	for _, node := range nodeList.Items {
		if node.Spec.Unschedulable || strings.TrimSpace(node.Metadata.Name) == "" {
			continue
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
				break
			}
		}
		if ready {
			nodeNames = append(nodeNames, strings.TrimSpace(node.Metadata.Name))
		}
	}
	sort.Strings(nodeNames)
	return nodeNames, nil
}

func stage5ImagePullPolicy(t *testing.T) string {
	t.Helper()
	policy := strings.TrimSpace(os.Getenv(kubernetesStage5ImagePullPolicyEnv))
	if policy == "" {
		return "IfNotPresent"
	}
	switch policy {
	case "Always", "IfNotPresent", "Never":
		return policy
	default:
		t.Fatalf("%s must be Always, IfNotPresent, or Never", kubernetesStage5ImagePullPolicyEnv)
		return ""
	}
}

func stage5RegistrationTokenHandoffManifest(namespace, image, imagePullPolicy, nodeName, podName string) []byte {
	targetID := uuid.NewString()
	restrictedSecurity := map[string]any{
		"allowPrivilegeEscalation": false,
		"readOnlyRootFilesystem":   true,
		"runAsNonRoot":             true,
		"runAsUser":                10001,
		"runAsGroup":               10001,
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
	resources := map[string]any{
		"requests": map[string]any{"cpu": "10m", "memory": "32Mi", "ephemeral-storage": "16Mi"},
		"limits":   map[string]any{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "64Mi"},
	}
	environment := []any{
		map[string]any{"name": "SYNARA_CONTROL_PLANE_URL", "value": "http://127.0.0.1:9"},
		map[string]any{"name": "SYNARA_WORKER_REGISTRATION_TOKEN_FILE", "value": kubernetesStagedRegistrationTokenPath},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_ID", "value": targetID},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_KIND", "value": "kubernetes"},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_ID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}},
		map[string]any{"name": "SYNARA_AGENTD_NAMESPACE", "value": namespace},
		map[string]any{"name": "SYNARA_AGENTD_RUNNER_COMMAND_JSON", "value": `["provider-host"]`},
		map[string]any{"name": "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL", "value": "v2"},
		map[string]any{"name": "SYNARA_AGENTD_WORKSPACE_ROOT", "value": "/data/workspaces"},
		map[string]any{"name": "SYNARA_AGENTD_GIT_CACHE_ROOT", "value": "/data/git-cache"},
		map[string]any{"name": "SYNARA_AGENTD_PRIVATE_TMP_ROOT", "value": "/tmp"},
		map[string]any{"name": platform.KubernetesPIDsLimitEnvironment, "value": "512"},
	}
	object := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": podName, "namespace": namespace,
			"labels": map[string]any{"synara.io/stage5-token-handoff": podName},
		},
		"spec": map[string]any{
			"automountServiceAccountToken": false,
			"enableServiceLinks":           false,
			"restartPolicy":                "Never",
			"securityContext": map[string]any{
				"runAsNonRoot": true, "fsGroup": 10001,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"initContainers": []any{map[string]any{
				"name": "registration-token-init", "image": image, "imagePullPolicy": imagePullPolicy,
				"command":         []any{"/usr/local/bin/synara-agentd", "--stage-kubernetes-registration-token"},
				"securityContext": restrictedSecurity, "resources": resources,
				"volumeMounts": []any{
					map[string]any{"name": "workload-identity", "mountPath": "/var/run/secrets/synara.io/workload-identity", "readOnly": true},
					map[string]any{"name": "registration-token", "mountPath": "/var/run/secrets/synara.io/registration"},
				},
			}},
			"containers": []any{map[string]any{
				"name": "consumer", "image": image, "imagePullPolicy": imagePullPolicy,
				"command": []any{"/bin/sh", "-ec", stage5RegistrationTokenConsumerScript},
				"env":     environment, "securityContext": restrictedSecurity, "resources": resources,
				"volumeMounts": []any{
					map[string]any{"name": "registration-token", "mountPath": "/var/run/secrets/synara.io/registration"},
					map[string]any{"name": "workspace", "mountPath": "/data"},
					map[string]any{"name": "tmp", "mountPath": "/tmp"},
					map[string]any{"name": "home", "mountPath": "/home/synara"},
				},
			}},
			"volumes": []any{
				map[string]any{"name": "workload-identity", "projected": map[string]any{
					"defaultMode": 0o440,
					"sources": []any{map[string]any{"serviceAccountToken": map[string]any{
						"audience":          "synara.io/stage5-token-handoff/" + targetID,
						"expirationSeconds": 600,
						"path":              "token",
					}}},
				}},
				map[string]any{"name": "registration-token", "emptyDir": map[string]any{}},
				map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
				map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "32Mi"}},
				map[string]any{"name": "home", "emptyDir": map[string]any{"sizeLimit": "32Mi"}},
			},
		},
	}
	if nodeName != "" {
		object["spec"].(map[string]any)["nodeName"] = nodeName
	}
	encoded, _ := json.Marshal(object)
	return encoded
}

const stage5RegistrationTokenConsumerScript = `
projected_mounted=false
if [ -e /var/run/secrets/synara.io/workload-identity/token ]; then projected_mounted=true; fi
staged_initially=false
if [ -s /var/run/secrets/synara.io/registration/one-shot/token ]; then staged_initially=true; fi
/usr/local/bin/synara-agentd >/tmp/agentd.log 2>&1 &
agentd_pid=$!
staged_consumed=false
attempt=0
while [ "$attempt" -lt 50 ]; do
  if [ ! -e /var/run/secrets/synara.io/registration/one-shot/token ]; then staged_consumed=true; break; fi
  sleep 0.1
  attempt=$((attempt + 1))
done
kill "$agentd_pid" 2>/dev/null || true
wait "$agentd_pid" 2>/dev/null || true
printf '{"projectedMounted":%s,"stagedInitially":%s,"stagedConsumed":%s}\n' \
  "$projected_mounted" "$staged_initially" "$staged_consumed"
test "$projected_mounted" = false
test "$staged_initially" = true
test "$staged_consumed" = true
`

func stage5RuntimeIsolationManifest(
	namespace, image, imagePullPolicy, nodeName, suffix, peerName, pressureName, memoryName, policyName string,
) []byte {
	restrictedSecurity := map[string]any{
		"allowPrivilegeEscalation": false,
		"readOnlyRootFilesystem":   true,
		"runAsNonRoot":             true,
		"runAsUser":                10001,
		"runAsGroup":               10001,
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
	pod := func(name, role, script string, resources map[string]any) map[string]any {
		object := map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{
				"name": name, "namespace": namespace,
				"labels": map[string]any{
					"synara.io/stage5-runtime-run":  suffix,
					"synara.io/stage5-runtime-role": role,
				},
			},
			"spec": map[string]any{
				"automountServiceAccountToken": false,
				"enableServiceLinks":           false,
				"restartPolicy":                "Never",
				"securityContext": map[string]any{
					"runAsNonRoot": true, "fsGroup": 10001,
					"seccompProfile": map[string]any{"type": "RuntimeDefault"},
				},
				"containers": []any{map[string]any{
					"name": "probe", "image": image, "imagePullPolicy": imagePullPolicy,
					"command":         []any{"node", "--input-type=module", "-e", script},
					"securityContext": restrictedSecurity, "resources": resources,
					"volumeMounts": []any{map[string]any{"name": "tmp", "mountPath": "/tmp"}},
				}},
				"volumes": []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "64Mi"}}},
			},
		}
		if nodeName != "" {
			object["spec"].(map[string]any)["nodeName"] = nodeName
		}
		if role != "pressure" {
			object["spec"].(map[string]any)["affinity"] = stage5PressurePodAffinity(suffix)
		}
		return object
	}
	objects := []any{
		map[string]any{
			"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": map[string]any{"name": policyName, "namespace": namespace},
			"spec": map[string]any{
				"podSelector": map[string]any{"matchLabels": map[string]any{"synara.io/stage5-runtime-run": suffix}},
				"policyTypes": []any{"Ingress", "Egress"}, "ingress": []any{},
				"egress": []any{map[string]any{
					"to": []any{
						map[string]any{"ipBlock": map[string]any{
							"cidr":   "0.0.0.0/0",
							"except": []any{"169.254.0.0/16", "100.100.100.200/32"},
						}},
						map[string]any{"ipBlock": map[string]any{
							"cidr": "::/0", "except": []any{"fe80::/10", "fd00:ec2::254/128"},
						}},
					},
				}},
			},
		},
		pod(peerName, "peer", `setInterval(() => {}, 1000);`, map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "32Mi", "ephemeral-storage": "16Mi"},
			"limits":   map[string]any{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "64Mi"},
		}),
		pod(pressureName, "pressure", stage5PressureProbeScript, map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "32Mi", "ephemeral-storage": "16Mi"},
			"limits":   map[string]any{"cpu": "100m", "memory": "192Mi", "ephemeral-storage": "64Mi"},
		}),
		pod(memoryName, "memory", `const chunks=[]; setInterval(() => chunks.push(Buffer.alloc(16*1024*1024, 1)), 20);`, map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "16Mi", "ephemeral-storage": "16Mi"},
			"limits":   map[string]any{"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"},
		}),
	}
	encoded := make([]byte, 0)
	for _, object := range objects {
		item, _ := json.Marshal(object)
		encoded = append(encoded, item...)
		encoded = append(encoded, []byte("\n---\n")...)
	}
	return encoded
}

func stage5PressurePodAffinity(suffix string) map[string]any {
	return map[string]any{
		"podAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
				"labelSelector": map[string]any{
					"matchLabels": map[string]any{
						"synara.io/stage5-runtime-run":  suffix,
						"synara.io/stage5-runtime-role": "pressure",
					},
				},
				"topologyKey": "kubernetes.io/hostname",
			}},
		},
	}
}

func runStage5Kubectl(ctx context.Context, input []byte, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "kubectl", arguments...)
	if len(input) > 0 {
		command.Stdin = bytes.NewReader(input)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func stage5PodNodeName(
	t *testing.T,
	ctx context.Context,
	kubernetesContext, namespace, podName string,
) string {
	t.Helper()
	output, err := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace,
		"get", "pod/"+podName, "-o", "jsonpath={.spec.nodeName}",
	)
	if err != nil {
		t.Fatalf("read node for Stage 5 Pod %s: %v\n%s", podName, err, output)
	}
	nodeName := strings.TrimSpace(output)
	if nodeName == "" {
		t.Fatalf("Stage 5 Pod %s has no assigned node", podName)
	}
	return nodeName
}

func stage5PodDiagnostics(kubernetesContext, namespace, podName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, _ := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace, "get", "pod", podName, "-o", "wide",
	)
	logs, _ := runStage5Kubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace, "logs", podName, "--tail=100",
	)
	return fmt.Sprintf("%s\n%s", strings.TrimSpace(status), strings.TrimSpace(logs))
}

const stage5PressureProbeScript = `
import { spawn } from "node:child_process";
import fs from "node:fs/promises";

const endpoints = [
  "http://169.254.169.254/latest/meta-data/",
  "http://100.100.100.200/latest/meta-data/",
  "http://[fd00:ec2::254]/latest/meta-data/",
];
let metadataBlocked = true;
for (const endpoint of endpoints) {
  try {
    await fetch(endpoint, { signal: AbortSignal.timeout(1200) });
    metadataBlocked = false;
  } catch {}
}
const children = [];
let forksStarted = 0;
let forksRejected = 0;
for (let index = 0; index < 256; index += 1) {
  try {
    const child = spawn("sleep", ["5"], { stdio: "ignore" });
    children.push(new Promise((resolve) => {
      let finished = false;
      const finish = () => { if (!finished) { finished = true; resolve(); } };
      child.once("spawn", () => { forksStarted += 1; });
      child.once("error", () => { forksRejected += 1; finish(); });
      child.once("close", finish);
    }));
  } catch {
    forksRejected += 1;
  }
}
const startedAt = Date.now();
let value = 1;
while (Date.now() - startedAt < 5000) value = Math.sqrt(value + 1.000001);
await Promise.all(children);
let cpuMaxFinite = false;
try {
  const cpuMax = (await fs.readFile("/sys/fs/cgroup/cpu.max", "utf8")).trim();
  cpuMaxFinite = !cpuMax.startsWith("max ");
} catch {}
console.log(JSON.stringify({ metadataBlocked, forksStarted, forksRejected, cpuMaxFinite }));
`
