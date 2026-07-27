package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const orbStackKubernetesTenantIsolationIntegrationEnv = "SYNARA_ORBSTACK_KUBERNETES_TENANT_ISOLATION_TEST"

func TestOrbStackKubernetesGeneralWorkerTenantIsolation(t *testing.T) {
	if os.Getenv(orbStackKubernetesTenantIsolationIntegrationEnv) != "1" {
		t.Skip(orbStackKubernetesTenantIsolationIntegrationEnv + "=1 is required")
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	primaryDB, _ := isolatedPostgresTestDBs(t)
	service := integrationService(t, primaryDB)
	registrationToken := "orbstack-tenant-isolation-" + uuid.NewString()
	workerAPI := newOrbStackTenantIsolationWorkerAPI(service, registrationToken)
	server := httptest.NewServer(workerAPI)
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	controlPlaneHost := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CONTROL_PLANE_HOST"))
	if controlPlaneHost == "" {
		controlPlaneHost = "host.docker.internal"
	}
	controlPlaneURL := "http://" + controlPlaneHost + ":" + serverURL.Port()

	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	pinnedTarget := seedWorkerTenantIsolationTargetKind(t, primaryDB, placement.TenantIsolationPinned, "kubernetes")
	pinnedAFirst := seedWorkerTenantIsolationExecution(t, primaryDB, pinnedTarget, "orbstack-pinned-a-first", base)
	pinnedB := seedWorkerTenantIsolationExecution(t, primaryDB, pinnedTarget, "orbstack-pinned-b", base.Add(time.Second))
	pinnedASecond := seedWorkerTenantIsolationExecutionForTenant(
		t, primaryDB, pinnedTarget, pinnedAFirst, "orbstack-pinned-a-second", base.Add(2*time.Second),
	)

	sharedTarget := seedWorkerTenantIsolationTargetKind(t, primaryDB, placement.TenantIsolationShared, "kubernetes")
	sharedA := seedWorkerTenantIsolationExecution(t, primaryDB, sharedTarget, "orbstack-shared-a", base)
	sharedB := seedWorkerTenantIsolationExecution(t, primaryDB, sharedTarget, "orbstack-shared-b", base.Add(time.Second))

	type podExpectation struct {
		name              string
		target            persistence.ExecutionTarget
		firstExecutionID  uuid.UUID
		firstTenantID     uuid.UUID
		secondExecutionID uuid.UUID
		secondTenantID    uuid.UUID
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	expectations := []podExpectation{
		{
			name: "synara-tenant-isolation-pinned-" + suffix, target: pinnedTarget,
			firstExecutionID: pinnedAFirst.ExecutionID, firstTenantID: pinnedAFirst.TenantID,
			secondExecutionID: pinnedASecond.ExecutionID, secondTenantID: pinnedASecond.TenantID,
		},
		{
			name: "synara-tenant-isolation-shared-" + suffix, target: sharedTarget,
			firstExecutionID: sharedA.ExecutionID, firstTenantID: sharedA.TenantID,
			secondExecutionID: sharedB.ExecutionID, secondTenantID: sharedB.TenantID,
		},
	}
	observedPodUIDs := make(map[string]string, len(expectations))
	for _, expectation := range expectations {
		expectation := expectation
		applyOrbStackTenantIsolationPod(
			t, ctx, kubernetesContext, namespace, image, controlPlaneURL, registrationToken,
			expectation.name, expectation.target.ID, expectation.firstExecutionID,
		)
		t.Cleanup(func() {
			cleanupOrbStackTenantIsolationPod(
				t, kubernetesContext, namespace, expectation.name, observedPodUIDs[expectation.name],
			)
		})
	}

	results := make(map[uuid.UUID]orbStackTenantIsolationResult, len(expectations))
	for len(results) < len(expectations) {
		select {
		case result := <-workerAPI.results:
			if result.Error != "" {
				t.Fatalf("OrbStack Worker Pod %s failed: %s", result.PodName, result.Error)
			}
			if _, duplicate := results[result.TargetID]; duplicate {
				t.Fatalf("duplicate OrbStack result for Target %s", result.TargetID)
			}
			results[result.TargetID] = result
			observedPodUIDs[result.PodName] = result.InstanceUID
		case <-ctx.Done():
			for _, expectation := range expectations {
				t.Logf("Pod %s status/logs:\n%s", expectation.name, orbStackTenantIsolationPodDiagnostics(kubernetesContext, namespace, expectation.name))
			}
			t.Fatalf("wait for OrbStack Tenant isolation results: %v", ctx.Err())
		}
	}

	for _, expectation := range expectations {
		result, ok := results[expectation.target.ID]
		if !ok {
			t.Fatalf("missing OrbStack result for Target %s", expectation.target.ID)
		}
		if result.FirstExecutionID != expectation.firstExecutionID || result.FirstTenantID != expectation.firstTenantID ||
			result.SecondExecutionID != expectation.secondExecutionID || result.SecondTenantID != expectation.secondTenantID {
			t.Fatalf("unexpected OrbStack claim sequence for Target %s: %#v", expectation.target.ID, result)
		}
		if result.PodName != expectation.name || result.InstanceUID == "" || result.WorkerID == uuid.Nil {
			t.Fatalf("invalid physical OrbStack Worker identity: %#v", result)
		}
		var worker persistence.WorkerInstance
		if err := primaryDB.Where("id = ?", result.WorkerID).Take(&worker).Error; err != nil {
			t.Fatal(err)
		}
		if worker.ExecutionTargetID != expectation.target.ID || worker.PodName != expectation.name ||
			worker.InstanceUID != result.InstanceUID || worker.ClusterID != "kubernetes" || worker.Namespace != namespace {
			t.Fatalf("persisted Worker does not match the physical OrbStack Pod: %#v result=%#v", worker, result)
		}
		if expectation.target.ID == pinnedTarget.ID {
			if worker.TenantBindingID == nil || *worker.TenantBindingID != pinnedAFirst.TenantID {
				t.Fatalf("pinned OrbStack Worker binding = %#v, want %s", worker.TenantBindingID, pinnedAFirst.TenantID)
			}
		} else if worker.TenantBindingID != nil {
			t.Fatalf("shared OrbStack Worker unexpectedly bound to Tenant %s", *worker.TenantBindingID)
		}
		for _, executionID := range []uuid.UUID{result.FirstExecutionID, result.SecondExecutionID} {
			var execution persistence.AgentExecution
			if err := primaryDB.Where("id = ?", executionID).Take(&execution).Error; err != nil {
				t.Fatal(err)
			}
			if execution.Status != "completed" || execution.WorkerID == nil || *execution.WorkerID != worker.ID {
				t.Fatalf("OrbStack Execution was not completed by its physical Worker: %#v", execution)
			}
		}
	}

	var pinnedOther persistence.AgentExecution
	if err := primaryDB.Where("id = ?", pinnedB.ExecutionID).Take(&pinnedOther).Error; err != nil {
		t.Fatal(err)
	}
	if pinnedOther.Status != "queued" || pinnedOther.WorkerID != nil {
		t.Fatalf("pinned OrbStack Worker crossed into the other Tenant: %#v", pinnedOther)
	}
	t.Logf(
		"OrbStack Kubernetes Tenant isolation PASS namespace=%s pinnedWorker=%s pinnedUID=%s sharedWorker=%s sharedUID=%s pinnedOther=%s",
		namespace,
		results[pinnedTarget.ID].WorkerID, results[pinnedTarget.ID].InstanceUID,
		results[sharedTarget.ID].WorkerID, results[sharedTarget.ID].InstanceUID,
		pinnedOther.Status,
	)
}

type orbStackTenantIsolationResult struct {
	TargetID          uuid.UUID `json:"targetId"`
	WorkerID          uuid.UUID `json:"workerId"`
	PodName           string    `json:"podName"`
	InstanceUID       string    `json:"instanceUid"`
	FirstExecutionID  uuid.UUID `json:"firstExecutionId"`
	FirstTenantID     uuid.UUID `json:"firstTenantId"`
	SecondExecutionID uuid.UUID `json:"secondExecutionId"`
	SecondTenantID    uuid.UUID `json:"secondTenantId"`
	Error             string    `json:"error,omitempty"`
}

type orbStackTenantIsolationWorkerAPI struct {
	service           *Service
	registrationToken string
	results           chan orbStackTenantIsolationResult
	mu                sync.Mutex
	errors            []string
}

func newOrbStackTenantIsolationWorkerAPI(service *Service, registrationToken string) *orbStackTenantIsolationWorkerAPI {
	return &orbStackTenantIsolationWorkerAPI{
		service: service, registrationToken: registrationToken,
		results: make(chan orbStackTenantIsolationResult, 2),
	}
}

func (a *orbStackTenantIsolationWorkerAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/v1/workers/register":
		if orbStackTenantIsolationBearer(request) != a.registrationToken {
			a.writeError(writer, problem.New(401, "invalid_worker_registration_token", "The Worker registration token is invalid."))
			return
		}
		var input RegisterWorkerInput
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			a.writeError(writer, err)
			return
		}
		input.RegistrationTrustMode = WorkerRegistrationTrustSharedToken
		input.Capabilities = workerManifestTestCapabilitiesForVersion(input.Version)
		addWorkerManifestTestContainmentEvidence(input.Capabilities)
		if err := signWorkerManifestTestContainmentValue(input.Capabilities, workerManifestRegistrationContext{
			ExecutionTargetID: input.ExecutionTargetID, TargetKind: platform.TargetKubernetes,
			InstanceUID: input.InstanceUID, ClusterID: input.ClusterID, Namespace: input.Namespace, PodName: input.PodName,
		}); err != nil {
			a.writeError(writer, err)
			return
		}
		registered, err := a.service.Register(request.Context(), input)
		if err != nil {
			a.writeError(writer, err)
			return
		}
		a.writeJSON(writer, http.StatusCreated, registered)
	case request.URL.Path == "/v1/workers/executions/claim":
		worker, err := a.authenticate(request)
		if err != nil {
			a.writeError(writer, err)
			return
		}
		var input ClaimExecutionInput
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			a.writeError(writer, err)
			return
		}
		result, err := a.service.Claim(request.Context(), worker, input, request.Header.Get("X-Request-ID"))
		if err != nil {
			a.writeError(writer, err)
			return
		}
		a.writeJSON(writer, http.StatusOK, result.Value)
	case strings.HasPrefix(request.URL.Path, "/v1/workers/executions/") && strings.HasSuffix(request.URL.Path, "/complete"):
		worker, err := a.authenticate(request)
		if err != nil {
			a.writeError(writer, err)
			return
		}
		rawID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1/workers/executions/"), "/complete")
		executionID, err := uuid.Parse(rawID)
		if err != nil {
			a.writeError(writer, problem.New(400, "invalid_execution_id", "Execution ID is invalid."))
			return
		}
		var input CompleteExecutionInput
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			a.writeError(writer, err)
			return
		}
		result, err := a.service.Complete(request.Context(), worker, executionID, input, request.Header.Get("X-Request-ID"))
		if err != nil {
			a.writeError(writer, err)
			return
		}
		a.writeJSON(writer, http.StatusOK, result.Value)
	case request.URL.Path == "/acceptance/result":
		if orbStackTenantIsolationBearer(request) != a.registrationToken {
			a.writeError(writer, problem.New(401, "invalid_acceptance_token", "The acceptance token is invalid."))
			return
		}
		var result orbStackTenantIsolationResult
		if err := json.NewDecoder(request.Body).Decode(&result); err != nil {
			a.writeError(writer, err)
			return
		}
		select {
		case a.results <- result:
			a.writeJSON(writer, http.StatusAccepted, map[string]any{"accepted": true})
		default:
			a.writeError(writer, problem.New(409, "acceptance_result_duplicate", "The acceptance result was already submitted."))
		}
	default:
		http.NotFound(writer, request)
	}
}

func (a *orbStackTenantIsolationWorkerAPI) authenticate(request *http.Request) (persistence.WorkerInstance, error) {
	return a.service.Authenticate(request.Context(), orbStackTenantIsolationBearer(request))
}

func (a *orbStackTenantIsolationWorkerAPI) writeError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	message := err.Error()
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		status = apiError.Status
		code = apiError.Code
		message = apiError.Message
	}
	a.mu.Lock()
	a.errors = append(a.errors, code+":"+message)
	a.mu.Unlock()
	a.writeJSON(writer, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func (a *orbStackTenantIsolationWorkerAPI) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func orbStackTenantIsolationBearer(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(value) < len("Bearer ") || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(value[len("Bearer "):])
}

func applyOrbStackTenantIsolationPod(
	t *testing.T,
	ctx context.Context,
	kubernetesContext, namespace, image, controlPlaneURL, registrationToken, podName string,
	targetID, firstExecutionID uuid.UUID,
) {
	t.Helper()
	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": podName, "namespace": namespace,
			"labels": map[string]any{
				"synara.io/acceptance":          "stage4-tenant-isolation",
				"synara.io/execution-target-id": targetID.String(),
			},
		},
		"spec": map[string]any{
			"automountServiceAccountToken": false,
			"enableServiceLinks":           false,
			"restartPolicy":                "Never",
			"securityContext": map[string]any{
				"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001, "fsGroup": 10001,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{map[string]any{
				"name": "worker-probe", "image": image, "imagePullPolicy": "Never",
				"command": []any{"/usr/local/bin/node", "--input-type=module", "-e", orbStackTenantIsolationPodScript},
				"env": []any{
					map[string]any{"name": "CONTROL_PLANE_URL", "value": controlPlaneURL},
					map[string]any{"name": "REGISTRATION_TOKEN", "value": registrationToken},
					map[string]any{"name": "EXECUTION_TARGET_ID", "value": targetID.String()},
					map[string]any{"name": "FIRST_EXECUTION_ID", "value": firstExecutionID.String()},
					map[string]any{"name": "POD_NAME", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
					map[string]any{"name": "POD_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}},
					map[string]any{"name": "POD_NAMESPACE", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.namespace"}}},
				},
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
					"capabilities": map[string]any{"drop": []any{"ALL"}},
				},
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "10m", "memory": "32Mi"},
					"limits":   map[string]any{"cpu": "250m", "memory": "256Mi"},
				},
				"volumeMounts": []any{
					map[string]any{"name": "tmp", "mountPath": "/tmp"},
					map[string]any{"name": "home", "mountPath": "/home/synara"},
				},
			}},
			"volumes": []any{
				map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
				map[string]any{"name": "home", "emptyDir": map[string]any{}},
			},
		},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := runOrbStackTenantIsolationKubectl(ctx, encoded, "--context", kubernetesContext, "apply", "-f", "-"); err != nil {
		t.Fatalf("create OrbStack Worker Pod %s: %v\n%s", podName, err, output)
	}
}

func cleanupOrbStackTenantIsolationPod(
	t *testing.T,
	kubernetesContext, namespace, podName, expectedUID string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	uidOutput, err := runOrbStackTenantIsolationKubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace,
		"get", "pod", podName, "-o", "jsonpath={.metadata.uid}",
	)
	if err != nil {
		if strings.Contains(uidOutput, "NotFound") || strings.Contains(uidOutput, "not found") {
			return
		}
		t.Errorf("resolve exact OrbStack Worker Pod %s before cleanup: %v\n%s", podName, err, uidOutput)
		return
	}
	currentUID := strings.TrimSpace(uidOutput)
	if expectedUID != "" && currentUID != expectedUID {
		t.Errorf("refuse to delete OrbStack Worker Pod %s: UID changed from %s to %s", podName, expectedUID, currentUID)
		return
	}
	if output, err := runOrbStackTenantIsolationKubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace,
		"delete", "pod", podName, "--wait=true", "--timeout=25s",
	); err != nil {
		t.Errorf("delete exact OrbStack Worker Pod %s/%s: %v\n%s", podName, currentUID, err, output)
	}
}

func orbStackTenantIsolationPodDiagnostics(kubernetesContext, namespace, podName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, _ := runOrbStackTenantIsolationKubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace, "get", "pod", podName, "-o", "wide",
	)
	logs, _ := runOrbStackTenantIsolationKubectl(
		ctx, nil, "--context", kubernetesContext, "-n", namespace, "logs", podName, "--tail=120",
	)
	return strings.TrimSpace(status) + "\n" + strings.TrimSpace(logs)
}

func runOrbStackTenantIsolationKubectl(ctx context.Context, input []byte, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "kubectl", arguments...)
	if len(input) > 0 {
		command.Stdin = bytes.NewReader(input)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

const orbStackTenantIsolationPodScript = `
const base = process.env.CONTROL_PLANE_URL;
const registrationToken = process.env.REGISTRATION_TOKEN;
const targetId = process.env.EXECUTION_TARGET_ID;
const podName = process.env.POD_NAME;
const instanceUid = process.env.POD_UID;
const namespace = process.env.POD_NAMESPACE;

async function post(path, body, token, requestId) {
  const response = await fetch(base + path, {
    method: "POST",
    headers: {
      "Authorization": "Bearer " + token,
      "Content-Type": "application/json",
      "X-Request-ID": requestId,
    },
    body: JSON.stringify(body),
  });
  const text = await response.text();
  let decoded = {};
  if (text) {
    try { decoded = JSON.parse(text); } catch { decoded = { raw: text }; }
  }
  if (!response.ok) {
    throw new Error(path + " returned " + response.status + ":" + JSON.stringify(decoded));
  }
  return decoded;
}

async function complete(claim, workerToken, step) {
  await post(
    "/v1/workers/executions/" + claim.execution.id + "/complete",
    {
      tenantId: claim.execution.tenantId,
      generation: claim.lease.generation,
      leaseToken: claim.lease.leaseToken,
      output: { acceptance: "stage4-tenant-isolation", step },
    },
    workerToken,
    targetId + "-complete-" + step,
  );
}

let result = { targetId, podName, instanceUid };
try {
  const registration = await post(
    "/v1/workers/register",
    {
      executionTargetId: targetId,
      targetKind: "kubernetes",
      workerMode: "general-pool",
      instanceUid,
      clusterId: "kubernetes",
      namespace,
      podName,
      version: "orbstack-tenant-isolation",
      protocolVersion: 2,
      capabilities: {},
      leaseSupported: true,
      fencingSupported: true,
    },
    registrationToken,
    targetId + "-register",
  );
  result.workerId = registration.worker.id;
  const first = await post(
    "/v1/workers/executions/claim",
    { executionTargetId: targetId, targetKind: "kubernetes", executionId: process.env.FIRST_EXECUTION_ID },
    registration.token,
    targetId + "-claim-first",
  );
  if (!first.execution || !first.lease) throw new Error("first claim returned no Execution");
  result.firstExecutionId = first.execution.id;
  result.firstTenantId = first.execution.tenantId;
  await complete(first, registration.token, "first");

  const second = await post(
    "/v1/workers/executions/claim",
    { executionTargetId: targetId, targetKind: "kubernetes" },
    registration.token,
    targetId + "-claim-second",
  );
  if (!second.execution || !second.lease) throw new Error("second claim returned no Execution");
  result.secondExecutionId = second.execution.id;
  result.secondTenantId = second.execution.tenantId;
  await complete(second, registration.token, "second");
} catch (error) {
  result.error = String(error && error.stack ? error.stack : error);
}

await post("/acceptance/result", result, registrationToken, targetId + "-result");
console.log(JSON.stringify(result));
if (result.error) process.exit(1);
`
