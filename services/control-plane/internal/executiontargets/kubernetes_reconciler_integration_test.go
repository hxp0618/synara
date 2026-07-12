package executiontargets

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestKubernetesReconcilerAgainstRealAPIServer(t *testing.T) {
	apiServer := strings.TrimSpace(os.Getenv("SYNARA_KUBERNETES_INTEGRATION_API_SERVER"))
	bearerToken := strings.TrimSpace(os.Getenv("SYNARA_KUBERNETES_INTEGRATION_TOKEN"))
	caCertificate := strings.TrimSpace(os.Getenv("SYNARA_KUBERNETES_INTEGRATION_CA"))
	if apiServer == "" || bearerToken == "" || caCertificate == "" {
		t.Skip("set SYNARA_KUBERNETES_INTEGRATION_API_SERVER, SYNARA_KUBERNETES_INTEGRATION_TOKEN, and SYNARA_KUBERNETES_INTEGRATION_CA")
	}

	fixture := newKubernetesReconcileFixture(t)
	namespace := "synara-it-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	configuration := map[string]any{
		"apiServer": apiServer, "bearerToken": bearerToken, "caCertificate": caCertificate,
		"namespace": namespace, "manageNamespace": true,
		"image": "busybox:1.37", "imagePullPolicy": "IfNotPresent",
		"controlPlaneUrl": "http://127.0.0.1:3780", "allowInsecureControlPlane": true,
		"runnerCommand": []string{"/bin/true"}, "maxActivePods": 1,
		"egressCidrs": []string{"0.0.0.0/0"}, "cpuRequest": "10m", "cpuLimit": "100m",
		"memoryRequest": "16Mi", "memoryLimit": "64Mi", "workspaceSizeLimit": "64Mi",
		"quotaCpuRequests": "100m", "quotaCpuLimits": "1", "quotaMemoryRequests": "128Mi",
		"quotaMemoryLimits": "1Gi", "quotaEphemeralStorage": "1Gi",
	}
	encrypted, err := encryptConfiguration(fixture.reconciler.targets.cipher, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Updates(map[string]any{"configuration_encrypted": encrypted, "status": "offline"}).Error; err != nil {
		t.Fatal(err)
	}

	client, err := kubernetesHTTPFactory{}.Open(kubernetesTargetConfiguration{
		APIServer: apiServer, BearerToken: bearerToken, CACertificate: caCertificate,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpClient := client.(*kubernetesHTTPClient)
	t.Cleanup(func() {
		_ = httpClient.do(context.Background(), "DELETE", "/api/v1/namespaces/"+namespace, nil, nil, 200, 202, 404)
	})

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pods, err := client.ListPods(context.Background(), namespace, fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Labels[kubernetesExecutionLabel] != fixture.executionIDs[0].String() {
		t.Fatalf("real Kubernetes API returned unexpected managed Pods: %#v", pods)
	}
	for _, resource := range []string{"serviceaccounts", "secrets", "resourcequotas"} {
		path := kubernetesNamespacedPath(namespace, resource, kubernetesSecretName(fixture.targetID))
		if resource == "resourcequotas" {
			path = kubernetesNamespacedPath(namespace, resource, kubernetesSecretName(fixture.targetID))
		}
		if err := httpClient.do(context.Background(), "GET", path, nil, &map[string]any{}, 200); err != nil {
			t.Fatalf("real Kubernetes API did not persist %s: %v", resource, err)
		}
	}
}
