package executiontargets

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
)

const orbStackKubernetesPriorityIntegrationEnv = "SYNARA_ORBSTACK_KUBERNETES_PRIORITY_TEST"

func TestKubernetesWorkerPriorityOrbStackIntegration(t *testing.T) {
	if os.Getenv(orbStackKubernetesPriorityIntegrationEnv) != "1" {
		t.Skip(orbStackKubernetesPriorityIntegrationEnv + "=1 is required")
	}
	apiServer := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_API_SERVER"))
	bearerToken := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_BEARER_TOKEN"))
	caData := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CA_DATA"))
	namespace := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_NAMESPACE"))
	serviceAccountName := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_SERVICE_ACCOUNT"))
	priorityClassName := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_PRIORITY_CLASS"))
	preemptingPriorityClassName := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_PREEMPTING_PRIORITY_CLASS"))
	priorityValueText := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_PRIORITY_VALUE"))
	image := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_WORKER_IMAGE"))
	if apiServer == "" || bearerToken == "" || caData == "" || namespace == "" || serviceAccountName == "" ||
		priorityClassName == "" || preemptingPriorityClassName == "" || priorityValueText == "" || image == "" {
		t.Fatal("the OrbStack Kubernetes priority acceptance environment is incomplete")
	}
	priorityValue, err := strconv.ParseInt(priorityValueText, 10, 32)
	if err != nil {
		t.Fatalf("SYNARA_TEST_KUBERNETES_PRIORITY_VALUE is invalid: %v", err)
	}
	caCertificate, err := base64.StdEncoding.DecodeString(caData)
	if err != nil || len(caCertificate) == 0 {
		t.Fatal("SYNARA_TEST_KUBERNETES_CA_DATA is not valid base64 CA data")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	configuration := kubernetesTargetConfiguration{
		APIServer: apiServer, BearerToken: bearerToken, CACertificate: string(caCertificate),
		Namespace: namespace, ServiceAccountName: serviceAccountName,
		Image: image, ImagePullPolicy: "IfNotPresent",
		ControlPlaneURL: "http://control-plane.invalid:3780", AllowInsecureControlPlane: true,
		RunnerCommand: []string{"true"}, CPURequest: "10m", CPULimit: "100m",
		MemoryRequest: "16Mi", MemoryLimit: "64Mi", WorkspaceSizeLimit: "64Mi",
	}
	client, err := newKubernetesHTTPClient(configuration)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, organizationID, targetID := uuid.New(), uuid.New(), uuid.New()
	target := persistence.ExecutionTarget{
		ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID,
		Kind: "kubernetes", Capabilities: map[string]any{"workspaceModes": []string{"local"}},
	}
	reconciler := &KubernetesReconciler{config: KubernetesReconcilerConfig{WorkerLeaseTTL: 12 * time.Second}}
	coldExecution := kubernetesExecution{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID,
		ProjectID: uuid.New(), SessionID: uuid.New(), Status: "queued", QueuedAt: time.Now().UTC(),
		WorkerPool: &kubernetesExecutionPoolSnapshot{
			ID: uuid.New(), Version: 1, CapacityClass: placement.CapacityClassInteractive,
			Mode: placement.PoolModePerExecution, ClusterID: kubernetesLocalClusterID, Namespace: namespace,
			SchedulingTemplate: map[string]any{"priorityClassName": priorityClassName},
		},
	}
	coldPod, err := reconciler.executionPod(target, configuration, strings.Repeat("a", 64), coldExecution, nil)
	if err != nil {
		t.Fatal(err)
	}
	warmPlan := kubernetesWarmPodPlan{
		Pool: kubernetesWarmPool{
			ID: uuid.New(), Version: 1, CapacityClass: placement.CapacityClassInteractive,
			ClusterID: kubernetesLocalClusterID, Namespace: namespace,
			DesiredIdleUnits: 1, MaxActiveUnits: 1,
			SchedulingTemplate: map[string]any{
				"priorityClassName": priorityClassName,
				"preemptionPolicy":  placement.KubernetesPreemptionPolicyNever,
			},
			Status: placement.PoolStatusActive,
		},
		Slot: 0, ConfigHash: strings.Repeat("b", 64),
	}
	warmPod, err := reconciler.warmPoolPod(target, configuration, warmPlan, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []map[string]any{coldPod, warmPod} {
		container := object["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		// Keep the production-generated PodSpec and replace only the acceptance
		// process so the real kubelet can hold the Pod Running without agentd.
		container["command"] = []any{"/bin/sh", "-c", "sleep 300"}
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		pods, listErr := client.ListPods(cleanupCtx, namespace, targetID)
		if listErr != nil {
			t.Errorf("list exact priority acceptance Pods for cleanup: %v", listErr)
			return
		}
		for _, pod := range pods {
			if deleteErr := client.DeletePod(cleanupCtx, namespace, pod.Name, pod.UID); deleteErr != nil {
				t.Errorf("delete exact priority acceptance Pod %s/%s: %v", pod.Name, pod.UID, deleteErr)
			}
		}
	})

	priorityClassValidation := map[string]error{}
	for _, object := range []map[string]any{coldPod, warmPod} {
		metadata := object["metadata"].(map[string]any)
		name := metadata["name"].(string)
		if err := validateKubernetesWorkerPodPriorityClass(ctx, client, object, priorityClassValidation); err != nil {
			t.Fatalf("validate production-generated priority Pod %s: %v", name, err)
		}
		if err := client.Apply(ctx, kubernetesNamespacedPath(namespace, "pods", name), object); err != nil {
			t.Fatalf("apply production-generated priority Pod %s: %v", name, err)
		}
	}
	running := waitForOrbStackKubernetesTargetPods(t, ctx, client, namespace, targetID, "cold and warm non-preempting priority Pods", func(pods []kubernetesPod) bool {
		if len(pods) != 2 {
			return false
		}
		for _, pod := range pods {
			if pod.Phase != "Running" {
				return false
			}
		}
		return true
	})
	if len(running) != 2 {
		t.Fatalf("running priority Pods = %#v", running)
	}
	admittedUIDs := make(map[string]string, 2)
	for _, object := range []map[string]any{coldPod, warmPod} {
		name := object["metadata"].(map[string]any)["name"].(string)
		var observed struct {
			Metadata struct {
				UID string `json:"uid"`
			} `json:"metadata"`
			Spec struct {
				PriorityClassName string `json:"priorityClassName"`
				PreemptionPolicy  string `json:"preemptionPolicy"`
				Priority          int32  `json:"priority"`
			} `json:"spec"`
		}
		if err := client.do(ctx, http.MethodGet, kubernetesNamespacedPath(namespace, "pods", name), nil, &observed, http.StatusOK); err != nil {
			t.Fatalf("get admitted priority Pod %s: %v", name, err)
		}
		if observed.Metadata.UID == "" || observed.Spec.PriorityClassName != priorityClassName ||
			observed.Spec.PreemptionPolicy != placement.KubernetesPreemptionPolicyNever ||
			observed.Spec.Priority != int32(priorityValue) {
			t.Fatalf("admitted priority Pod %s = %#v", name, observed)
		}
		admittedUIDs[name] = observed.Metadata.UID
	}

	unsafe := coldExecution
	unsafe.ID = uuid.New()
	unsafe.WorkerPool = &kubernetesExecutionPoolSnapshot{
		ID: uuid.New(), Version: 1, CapacityClass: placement.CapacityClassInteractive,
		Mode: placement.PoolModePerExecution, ClusterID: kubernetesLocalClusterID, Namespace: namespace,
		SchedulingTemplate: map[string]any{
			"priorityClassName": preemptingPriorityClassName,
			"preemptionPolicy":  placement.KubernetesPreemptionPolicyNever,
		},
	}
	unsafePod, err := reconciler.executionPod(target, configuration, strings.Repeat("c", 64), unsafe, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = validateKubernetesWorkerPodPriorityClass(ctx, client, unsafePod, priorityClassValidation)
	assertProblemCode(t, err, 409, "worker_pool_preemption_unsupported")

	rejected := coldExecution
	rejected.ID = uuid.New()
	rejected.WorkerPool = &kubernetesExecutionPoolSnapshot{
		ID: uuid.New(), Version: 1, CapacityClass: placement.CapacityClassInteractive,
		Mode: placement.PoolModePerExecution, ClusterID: kubernetesLocalClusterID, Namespace: namespace,
		SchedulingTemplate: map[string]any{
			"priorityClassName": preemptingPriorityClassName,
			"preemptionPolicy":  "PreemptLowerPriority",
		},
	}
	_, err = reconciler.executionPod(target, configuration, strings.Repeat("d", 64), rejected, nil)
	assertProblemCode(t, err, 409, "worker_pool_preemption_unsupported")
	coldName := coldPod["metadata"].(map[string]any)["name"].(string)
	warmName := warmPod["metadata"].(map[string]any)["name"].(string)
	t.Logf(
		"OrbStack non-preempting priority accepted target=%s cold=%s/%s warm=%s/%s class=%s value=%d; live class rejection=%s",
		targetID,
		coldName,
		admittedUIDs[coldName],
		warmName,
		admittedUIDs[warmName],
		priorityClassName,
		priorityValue,
		preemptingPriorityClassName,
	)
}
