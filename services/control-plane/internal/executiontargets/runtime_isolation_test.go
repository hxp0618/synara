package executiontargets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestNormalizeKubernetesRuntimeIsolationConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		backend     string
		policy      *runtimeIsolationConfiguration
		wantMode    string
		wantRuntime string
		wantProfile platform.ExecutionTargetIsolationProfile
		wantClass   string
		wantErr     bool
	}{
		{
			name: "legacy native", backend: "native-pod", wantMode: runtimeIsolationModeLegacy,
			wantRuntime: runtimeIsolationRunc, wantProfile: platform.IsolationKubernetesRestricted,
		},
		{
			name: "explicit gvisor", backend: "native-pod",
			policy: &runtimeIsolationConfiguration{
				Mode: runtimeIsolationModeExplicit, Runtime: runtimeIsolationGVisor,
				MinimumProfile: platform.IsolationGVisorSandboxed,
			},
			wantMode: runtimeIsolationModeExplicit, wantRuntime: runtimeIsolationGVisor,
			wantProfile: platform.IsolationGVisorSandboxed, wantClass: kubernetesDefaultGVisorRuntimeClassName,
		},
		{
			name: "auto native", backend: "native-pod",
			policy:   &runtimeIsolationConfiguration{Mode: runtimeIsolationModeAuto},
			wantMode: runtimeIsolationModeAuto, wantProfile: platform.IsolationKubernetesRestricted,
			wantClass: kubernetesDefaultGVisorRuntimeClassName,
		},
		{
			name: "gvisor cocoon rejected", backend: "sandbox-operator-cocoon",
			policy: &runtimeIsolationConfiguration{
				Mode: runtimeIsolationModeExplicit, Runtime: runtimeIsolationGVisor,
				MinimumProfile: platform.IsolationGVisorSandboxed,
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configuration := kubernetesTargetConfiguration{
				AllocationBackend: test.backend, RuntimeIsolation: test.policy,
			}
			err := normalizeKubernetesRuntimeIsolationConfiguration(&configuration)
			if test.wantErr {
				if err == nil {
					t.Fatal("runtime isolation configuration was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if configuration.RuntimeIsolation.Mode != test.wantMode ||
				configuration.RuntimeIsolation.Runtime != test.wantRuntime ||
				configuration.RuntimeIsolation.MinimumProfile != test.wantProfile ||
				configuration.RuntimeIsolation.RuntimeClassName != test.wantClass {
				t.Fatalf("normalized runtime isolation = %#v", configuration.RuntimeIsolation)
			}
		})
	}
}

func TestNewDockerRuntimeIsolationDefaultsFollowDeploymentProfile(t *testing.T) {
	personal := defaultRuntimeIsolationForNewTarget(platform.TargetDocker, platform.ProfilePersonal, map[string]any{})
	personalPolicy := personal["runtimeIsolation"].(map[string]any)
	if !slices.Equal(personalPolicy["preferred"].([]string), []string{runtimeIsolationRunc}) {
		t.Fatalf("Personal Docker runtime default = %#v", personalPolicy)
	}
	enterprise := defaultRuntimeIsolationForNewTarget(platform.TargetDocker, platform.ProfileEnterprise, map[string]any{})
	enterprisePolicy := enterprise["runtimeIsolation"].(map[string]any)
	if !slices.Equal(enterprisePolicy["preferred"].([]string), []string{runtimeIsolationGVisor, runtimeIsolationRunc}) {
		t.Fatalf("Enterprise Docker runtime default = %#v", enterprisePolicy)
	}
}

func TestDockerRuntimeDetectionRequiresExactEngineRuntimeName(t *testing.T) {
	capabilities := detectDockerRuntimeIsolationCapabilities([]string{
		"runc", "io.containerd.runsc.v1", "not-runsc", "runsc",
	})
	if len(capabilities) != 2 || capabilities[0].Runtime != runtimeIsolationGVisor ||
		capabilities[1].Runtime != runtimeIsolationRunc {
		t.Fatalf("Docker runtime capabilities = %#v", capabilities)
	}
	capabilities = detectDockerRuntimeIsolationCapabilities([]string{
		"runc", "io.containerd.runsc.v1", "not-runsc",
	})
	if len(capabilities) != 1 || capabilities[0].Runtime != runtimeIsolationRunc {
		t.Fatalf("Docker shim type was accepted as an Engine runtime name: %#v", capabilities)
	}
}

func TestResolveKubernetesRuntimeIsolationUsesAttestedPreferredRuntime(t *testing.T) {
	now := time.Now().UTC()
	expiresAt := now.Add(time.Minute)
	configuration := kubernetesTargetConfiguration{
		AllocationBackend: "native-pod",
		RuntimeIsolation: &runtimeIsolationConfiguration{
			Mode: runtimeIsolationModeAuto, Preferred: []string{runtimeIsolationGVisor, runtimeIsolationRunc},
			MinimumProfile:   platform.IsolationKubernetesRestricted,
			FallbackPolicy:   runtimeIsolationFallbackAllowLower,
			RuntimeClassName: kubernetesDefaultGVisorRuntimeClassName,
		},
	}
	decision, err := resolveKubernetesRuntimeIsolation(configuration, []runtimeIsolationCapability{
		{
			Runtime: runtimeIsolationGVisor, Profile: platform.IsolationGVisorSandboxed,
			RuntimeClassName:  kubernetesDefaultGVisorRuntimeClassName,
			AttestationDigest: strings.Repeat("a", 64), AttestedAt: &now, AttestationExpiresAt: &expiresAt,
		},
		{Runtime: runtimeIsolationRunc, Profile: platform.IsolationKubernetesRestricted},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.EffectiveRuntime != runtimeIsolationGVisor ||
		decision.EffectiveProfile != platform.IsolationGVisorSandboxed || decision.Decision != "selected" {
		t.Fatalf("runtime decision = %#v", decision)
	}

	decision, err = resolveKubernetesRuntimeIsolation(configuration, []runtimeIsolationCapability{
		{Runtime: runtimeIsolationRunc, Profile: platform.IsolationKubernetesRestricted},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.EffectiveRuntime != runtimeIsolationRunc || decision.Decision != "fallback" {
		t.Fatalf("runtime fallback decision = %#v", decision)
	}

	configuration.RuntimeIsolation.FallbackPolicy = runtimeIsolationFallbackFailClosed
	if rejected, err := resolveKubernetesRuntimeIsolation(configuration, []runtimeIsolationCapability{
		{Runtime: runtimeIsolationRunc, Profile: platform.IsolationKubernetesRestricted},
	}); err == nil || rejected.DecisionReasonCode != "runtime_isolation_fallback_forbidden" {
		t.Fatalf("fail-closed lower-runtime decision = %#v, %v", rejected, err)
	}

	configuration.RuntimeIsolation.FallbackPolicy = runtimeIsolationFallbackAllowLower
	configuration.RuntimeIsolation.MinimumProfile = platform.IsolationGVisorSandboxed
	if _, err := resolveKubernetesRuntimeIsolation(configuration, []runtimeIsolationCapability{
		{Runtime: runtimeIsolationRunc, Profile: platform.IsolationKubernetesRestricted},
	}); err == nil {
		t.Fatal("runc satisfied a gVisor minimum profile")
	}
}

func TestKubernetesHTTPClientAttestsGVisorRuntime(t *testing.T) {
	now := time.Date(2026, time.July, 30, 6, 0, 0, 0, time.UTC)
	instanceID := uuid.NewString()
	binaryHash := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apis/node.k8s.io/v1/runtimeclasses/synara-gvisor":
			_, _ = writer.Write([]byte(`{"handler":"runsc","scheduling":{"nodeSelector":{"synara.io/gvisor-ready":"true"}}}`))
		case "/api/v1/nodes":
			if request.URL.Query().Get("labelSelector") != "synara.io/gvisor-ready=true" {
				t.Fatalf("node selector = %q", request.URL.Query().Get("labelSelector"))
			}
			response := map[string]any{"items": []any{map[string]any{"metadata": map[string]any{
				"name": "worker-a", "annotations": map[string]string{
					kubernetesGVisorRuntimeHandlerAnnotation:   "runsc",
					kubernetesGVisorRuntimeVersionAnnotation:   "runsc version release-20260729.0",
					kubernetesGVisorRuntimeBinaryAnnotation:    binaryHash,
					kubernetesGVisorRuntimeConfigAnnotation:    strings.Repeat("b", 64),
					kubernetesGVisorAttestorInstanceAnnotation: instanceID,
					kubernetesGVisorAttestedAtAnnotation:       now.Add(-time.Second).Format(time.RFC3339Nano),
				},
			}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}}
			_ = json.NewEncoder(writer).Encode(response)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test", client: server.Client()}
	configuration := kubernetesTargetConfiguration{
		AllocationBackend: "native-pod",
		RuntimeIsolation: &runtimeIsolationConfiguration{
			Mode: runtimeIsolationModeExplicit, Runtime: runtimeIsolationGVisor,
			MinimumProfile:   platform.IsolationGVisorSandboxed,
			FallbackPolicy:   runtimeIsolationFallbackFailClosed,
			RuntimeClassName: kubernetesDefaultGVisorRuntimeClassName,
		},
	}
	capabilities, err := client.RuntimeIsolationCapabilities(context.Background(), configuration, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 2 || capabilities[0].Runtime != runtimeIsolationGVisor ||
		capabilities[0].Profile != platform.IsolationGVisorSandboxed ||
		!isLowerHexSHA256(capabilities[0].AttestationDigest) || capabilities[0].AttestedAt == nil ||
		capabilities[0].AttestationExpiresAt == nil {
		t.Fatalf("gVisor capabilities = %#v", capabilities)
	}
	firstDigest := capabilities[0].AttestationDigest
	binaryHash = strings.Repeat("c", 64)
	capabilities, err = client.RuntimeIsolationCapabilities(context.Background(), configuration, now)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities[0].AttestationDigest == firstDigest {
		t.Fatal("gVisor runtime binary replacement did not invalidate the attestation digest")
	}
}

func TestKubernetesHTTPClientRejectsStaleGVisorAttestation(t *testing.T) {
	now := time.Date(2026, time.July, 30, 6, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apis/node.k8s.io/v1/runtimeclasses/synara-gvisor":
			_, _ = writer.Write([]byte(`{"handler":"runsc","scheduling":{"nodeSelector":{"synara.io/gvisor-ready":"true"}}}`))
		case "/api/v1/nodes":
			_ = json.NewEncoder(writer).Encode(map[string]any{"items": []any{map[string]any{"metadata": map[string]any{
				"name": "worker-a", "annotations": map[string]string{
					kubernetesGVisorRuntimeHandlerAnnotation:   "runsc",
					kubernetesGVisorRuntimeVersionAnnotation:   "runsc version release-test",
					kubernetesGVisorRuntimeBinaryAnnotation:    strings.Repeat("a", 64),
					kubernetesGVisorRuntimeConfigAnnotation:    strings.Repeat("b", 64),
					kubernetesGVisorAttestorInstanceAnnotation: uuid.NewString(),
					kubernetesGVisorAttestedAtAnnotation:       now.Add(-time.Minute).Format(time.RFC3339Nano),
				},
			}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test", client: server.Client()}
	configuration := kubernetesTargetConfiguration{
		AllocationBackend: "native-pod",
		RuntimeIsolation: &runtimeIsolationConfiguration{
			Mode: runtimeIsolationModeExplicit, Runtime: runtimeIsolationGVisor,
			MinimumProfile: platform.IsolationGVisorSandboxed, FallbackPolicy: runtimeIsolationFallbackFailClosed,
			RuntimeClassName: kubernetesDefaultGVisorRuntimeClassName,
		},
	}
	_, err := client.RuntimeIsolationCapabilities(context.Background(), configuration, now)
	assertProblemCode(t, err, 503, "gvisor_attestation_stale")
}

func TestKubernetesGVisorEligibleNodeTaintsRequireMatchingToleration(t *testing.T) {
	taints := []kubernetesNodeTaint{{Key: "sandbox", Value: "gvisor", Effect: "NoSchedule"}}
	if kubernetesNodeTaintsTolerated(taints, nil) {
		t.Fatal("untolerated gVisor Node taint was accepted")
	}
	if kubernetesNodeTaintsTolerated(taints, []any{map[string]any{
		"key": "sandbox", "operator": "Equal", "value": "native", "effect": "NoSchedule",
	}}) {
		t.Fatal("wrong gVisor Node toleration value was accepted")
	}
	if !kubernetesNodeTaintsTolerated(taints, []any{map[string]any{
		"key": "sandbox", "operator": "Equal", "value": "gvisor", "effect": "NoSchedule",
	}}) {
		t.Fatal("matching gVisor Node toleration was rejected")
	}
	if !kubernetesNodeTaintsTolerated(taints, []any{map[string]any{
		"operator": "Exists", "effect": "NoSchedule",
	}}) {
		t.Fatal("wildcard Exists gVisor Node toleration was rejected")
	}
}

func TestKubernetesHTTPClientRunsGVisorCanaryBeforeAcceptance(t *testing.T) {
	phase := ""
	staleDeleted := false
	var applied map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodGet:
			if request.URL.Query().Has("labelSelector") {
				if staleDeleted {
					_, _ = writer.Write([]byte(`{"items":[]}`))
				} else {
					_, _ = writer.Write([]byte(`{"items":[{"metadata":{"name":"stale-canary","uid":"stale-uid"}}]}`))
				}
				return
			}
			if phase == "" {
				writer.WriteHeader(http.StatusNotFound)
				_, _ = writer.Write([]byte(`{"message":"not found"}`))
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"status": map[string]any{"phase": phase}})
		case http.MethodPatch:
			if err := json.NewDecoder(request.Body).Decode(&applied); err != nil {
				t.Fatal(err)
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{}`))
		case http.MethodDelete:
			var deletion struct {
				Preconditions struct {
					UID string `json:"uid"`
				} `json:"preconditions"`
			}
			if err := json.NewDecoder(request.Body).Decode(&deletion); err != nil {
				t.Fatal(err)
			}
			if request.URL.Path != "/api/v1/namespaces/synara-test/pods/stale-canary" || deletion.Preconditions.UID != "stale-uid" {
				t.Fatalf("stale canary deletion = %s %#v", request.URL.Path, deletion)
			}
			staleDeleted = true
			_, _ = writer.Write([]byte(`{}`))
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test", client: server.Client()}
	configuration := kubernetesTargetConfiguration{
		Namespace: "synara-test", ServiceAccountName: "synara-worker", Image: "synara-agentd:test",
		ImagePullPolicy: "IfNotPresent",
		RuntimeIsolationDecision: &runtimeIsolationDecision{
			EffectiveRuntime: runtimeIsolationGVisor, EffectiveProfile: platform.IsolationGVisorSandboxed,
			RuntimeClassName:  kubernetesDefaultGVisorRuntimeClassName,
			AttestationDigest: strings.Repeat("c", 64),
		},
	}
	target := persistence.ExecutionTarget{ID: uuid.New()}
	if err := client.EnsureRuntimeIsolationCanary(context.Background(), target, configuration, nil); err == nil {
		t.Fatal("new gVisor canary was treated as complete")
	}
	if !staleDeleted {
		t.Fatal("stale gVisor canary was not deleted with an exact identity")
	}
	spec := applied["spec"].(map[string]any)
	if spec["runtimeClassName"] != kubernetesDefaultGVisorRuntimeClassName {
		t.Fatalf("canary RuntimeClass = %#v", spec["runtimeClassName"])
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	command := container["command"].([]any)
	if len(command) != 2 || command[1] != platform.GVisorRuntimeVerifyArgument {
		t.Fatalf("canary command = %#v", command)
	}
	phase = "Succeeded"
	if err := client.EnsureRuntimeIsolationCanary(context.Background(), target, configuration, nil); err != nil {
		t.Fatalf("completed gVisor canary: %v", err)
	}
}

func TestKubernetesReconcilerFreezesAttestedGVisorDecisionBeforePodApply(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["runtimeIsolation"] = map[string]any{
		"mode": "explicit", "runtime": "gvisor",
		"minimumProfile": "gvisor-sandboxed-v1", "fallbackPolicy": "fail-closed",
		"runtimeClassName":          kubernetesDefaultGVisorRuntimeClassName,
		"gvisorCompatibleProviders": []string{"codex"},
	}
	fixture.updateConfiguration(t, configuration)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Minute)
	client := newFakeKubernetesClient()
	client.runtimeIsolationCapabilities = []runtimeIsolationCapability{{
		Runtime: runtimeIsolationGVisor, Profile: platform.IsolationGVisorSandboxed,
		RuntimeClassName:  kubernetesDefaultGVisorRuntimeClassName,
		AttestationDigest: strings.Repeat("d", 64), AttestedAt: &now, AttestationExpiresAt: &expiresAt,
	}}
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var decisions []persistence.ExecutionRuntimeIsolationDecision
	if err := fixture.db.Order("execution_id").Find(&decisions).Error; err != nil {
		t.Fatal(err)
	}
	if len(decisions) != len(fixture.executionIDs) {
		t.Fatalf("persisted runtime isolation decisions = %d", len(decisions))
	}
	for _, decision := range decisions {
		if decision.EffectiveRuntime == nil || *decision.EffectiveRuntime != runtimeIsolationGVisor ||
			decision.EffectiveProfile == nil || *decision.EffectiveProfile != string(platform.IsolationGVisorSandboxed) ||
			decision.RuntimeClassName == nil || *decision.RuntimeClassName != kubernetesDefaultGVisorRuntimeClassName ||
			decision.AttestationDigest == nil || *decision.AttestationDigest != strings.Repeat("d", 64) {
			t.Fatalf("persisted gVisor decision = %#v", decision)
		}
	}
	var audits []persistence.AuditLog
	if err := fixture.db.Where("action = ?", "execution.runtime_isolation_decided").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != len(decisions) {
		t.Fatalf("runtime isolation Audit rows = %d, want %d", len(audits), len(decisions))
	}
	encodedAudits, err := json.Marshal(audits)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedAudits), strings.Repeat("d", 64)) {
		t.Fatalf("runtime isolation attestation digest leaked into Audit metadata: %s", encodedAudits)
	}
	pod := client.lastKind("Pod")
	spec := pod["spec"].(map[string]any)
	if spec["runtimeClassName"] != kubernetesDefaultGVisorRuntimeClassName {
		t.Fatalf("gVisor Pod RuntimeClass = %#v", spec["runtimeClassName"])
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	environment := container["env"].([]any)
	foundProfile := false
	for _, raw := range environment {
		item := raw.(map[string]any)
		if item["name"] == platform.KubernetesRuntimeIsolationProfileEnvironment &&
			item["value"] == string(platform.IsolationGVisorSandboxed) {
			foundProfile = true
		}
	}
	if !foundProfile {
		t.Fatalf("gVisor Pod environment = %#v", environment)
	}

	initialPodIdentities := make(map[string]string, len(client.pods))
	initialPodHashes := make(map[string]string, len(client.pods))
	for name, pod := range client.pods {
		initialPodIdentities[name] = pod.UID
		initialPodHashes[name] = pod.Annotations[kubernetesConfigAnnotation]
	}
	refreshedAt := now.Add(5 * time.Second)
	refreshedExpiry := expiresAt.Add(5 * time.Second)
	client.runtimeIsolationCapabilities[0].AttestedAt = &refreshedAt
	client.runtimeIsolationCapabilities[0].AttestationExpiresAt = &refreshedExpiry
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 0 || len(client.pods) != len(initialPodIdentities) {
		t.Fatalf("fresh attestation window recycled gVisor Pods: deleted=%#v active=%d", client.deletedPods, len(client.pods))
	}
	for name, initialUID := range initialPodIdentities {
		pod, found := client.pods[name]
		if !found || pod.UID != initialUID || pod.Annotations[kubernetesConfigAnnotation] != initialPodHashes[name] {
			t.Fatalf("fresh attestation window changed gVisor Pod %s: beforeUID=%s after=%#v", name, initialUID, pod)
		}
	}
}

func TestKubernetesReconcilerFiltersUnacceptedGVisorAndUsesAllowedFallback(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["runtimeIsolation"] = map[string]any{
		"mode": "auto", "preferred": []string{"gvisor", "runc"},
		"minimumProfile": "kubernetes-restricted-v1", "fallbackPolicy": "allow-lower",
		"runtimeClassName": kubernetesDefaultGVisorRuntimeClassName,
	}
	fixture.updateConfiguration(t, configuration)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Minute)
	client := newFakeKubernetesClient()
	client.runtimeIsolationCapabilities = []runtimeIsolationCapability{
		{
			Runtime: runtimeIsolationGVisor, Profile: platform.IsolationGVisorSandboxed,
			RuntimeClassName:  kubernetesDefaultGVisorRuntimeClassName,
			AttestationDigest: strings.Repeat("e", 64), AttestedAt: &now, AttestationExpiresAt: &expiresAt,
		},
		{Runtime: runtimeIsolationRunc, Profile: platform.IsolationKubernetesRestricted},
	}
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var decisions []persistence.ExecutionRuntimeIsolationDecision
	if err := fixture.db.Order("execution_id").Find(&decisions).Error; err != nil {
		t.Fatal(err)
	}
	if len(decisions) != len(fixture.executionIDs) {
		t.Fatalf("persisted runtime isolation decisions = %d", len(decisions))
	}
	for _, decision := range decisions {
		if decision.Decision != "fallback" || decision.EffectiveRuntime == nil ||
			*decision.EffectiveRuntime != runtimeIsolationRunc || decision.RuntimeClassName != nil {
			t.Fatalf("unaccepted gVisor fallback decision = %#v", decision)
		}
	}
	pod := client.lastKind("Pod")
	if _, found := pod["spec"].(map[string]any)["runtimeClassName"]; found {
		t.Fatalf("fallback Pod unexpectedly selected a RuntimeClass: %#v", pod)
	}
}

func TestKubernetesReconcilerRejectsExplicitGVisorWithoutCompatibilityAcceptance(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["runtimeIsolation"] = map[string]any{
		"mode": "explicit", "runtime": "gvisor",
		"minimumProfile": "gvisor-sandboxed-v1", "fallbackPolicy": "fail-closed",
		"runtimeClassName": kubernetesDefaultGVisorRuntimeClassName,
	}
	fixture.updateConfiguration(t, configuration)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Minute)
	client := newFakeKubernetesClient()
	client.runtimeIsolationCapabilities = []runtimeIsolationCapability{{
		Runtime: runtimeIsolationGVisor, Profile: platform.IsolationGVisorSandboxed,
		RuntimeClassName:  kubernetesDefaultGVisorRuntimeClassName,
		AttestationDigest: strings.Repeat("f", 64), AttestedAt: &now, AttestationExpiresAt: &expiresAt,
	}}
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertProblemCode(t, err, 409, "gvisor_provider_compatibility_unaccepted")
	var decisions int64
	if err := fixture.db.Model(&persistence.ExecutionRuntimeIsolationDecision{}).Count(&decisions).Error; err != nil {
		t.Fatal(err)
	}
	if decisions != 0 {
		t.Fatalf("rejected compatibility persisted %d accepted decisions", decisions)
	}
}

func TestGVisorCompatibilityRequiresEveryActiveWorkerReleaseDeclaration(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	revisionID := fixture.seedReleaseRevision(t, 1, "sha256:"+strings.Repeat("7", 64))
	if err := fixture.db.Create(&persistence.WorkerReleasePolicy{
		TenantID: fixture.tenantID, ExecutionTargetID: fixture.targetID, PolicyVersion: 1,
		PromotedRevisionID: revisionID, UpdatedBy: fixture.userID, UpdatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	var target persistence.ExecutionTarget
	if err := fixture.db.First(&target, "id = ?", fixture.targetID).Error; err != nil {
		t.Fatal(err)
	}
	err := gvisorCompatibilityAccepted(
		context.Background(), fixture.db, target,
		&runtimeIsolationConfiguration{
			Mode: runtimeIsolationModeExplicit, Runtime: runtimeIsolationGVisor,
			GVisorCompatibleProviders: []string{"codex"},
		},
	)
	assertProblemCode(t, err, 409, "gvisor_provider_compatibility_unaccepted")
}

func TestUpdateRuntimeIsolationPolicyRequiresDrainAndExitsLegacyMode(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	fixture.updateConfiguration(t, kubernetesTestConfiguration(""))
	principal := identity.Principal{UserID: fixture.userID, ActiveTenantID: &fixture.tenantID}
	policy := map[string]any{
		"mode": "explicit", "runtime": "gvisor",
		"minimumProfile": "gvisor-sandboxed-v1", "fallbackPolicy": "fail-closed",
		"runtimeClassName":          kubernetesDefaultGVisorRuntimeClassName,
		"gvisorCompatibleProviders": []any{"CODEX"},
	}
	_, err := fixture.reconciler.targets.UpdateRuntimeIsolationPolicy(
		context.Background(), principal, fixture.tenantID, fixture.targetID, policy, "runtime-policy-active", "127.0.0.1",
	)
	assertProblemCode(t, err, 409, "runtime_isolation_policy_execution_active")
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	updated, err := fixture.reconciler.targets.UpdateRuntimeIsolationPolicy(
		context.Background(), principal, fixture.tenantID, fixture.targetID, policy, "runtime-policy-drained", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "offline" || updated.RuntimeIsolationPolicy == nil ||
		updated.RuntimeIsolationPolicy.Mode != runtimeIsolationModeExplicit ||
		!slices.Equal(updated.RuntimeIsolationPolicy.GVisorCompatibleProviders, []string{"codex"}) {
		t.Fatalf("updated runtime isolation Target = %#v", updated)
	}
	var auditRow persistence.AuditLog
	if err := fixture.db.Where("action = ? AND resource_id = ?", "execution_target.runtime_isolation_policy_updated", fixture.targetID).
		Take(&auditRow).Error; err != nil {
		t.Fatal(err)
	}
	encodedAudit, err := json.Marshal(auditRow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedAudit), "gvisorCompatibleProviders") ||
		strings.Contains(string(encodedAudit), kubernetesDefaultGVisorRuntimeClassName) {
		t.Fatalf("runtime policy leaked into Audit metadata: %s", encodedAudit)
	}
}

func TestUpdateRuntimeIsolationPolicyRejectsUnacceptedActiveReleaseBeforeGoingOffline(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	revisionID := fixture.seedReleaseRevision(t, 1, "sha256:"+strings.Repeat("8", 64))
	if err := fixture.db.Create(&persistence.WorkerReleasePolicy{
		TenantID: fixture.tenantID, ExecutionTargetID: fixture.targetID, PolicyVersion: 1,
		PromotedRevisionID: revisionID, UpdatedBy: fixture.userID, UpdatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: fixture.userID, ActiveTenantID: &fixture.tenantID}
	_, err := fixture.reconciler.targets.UpdateRuntimeIsolationPolicy(
		context.Background(), principal, fixture.tenantID, fixture.targetID,
		map[string]any{
			"mode": "explicit", "runtime": "gvisor",
			"minimumProfile": "gvisor-sandboxed-v1", "fallbackPolicy": "fail-closed",
			"runtimeClassName":          kubernetesDefaultGVisorRuntimeClassName,
			"gvisorCompatibleProviders": []string{"codex"},
		},
		"runtime-policy-release-unaccepted", "127.0.0.1",
	)
	assertProblemCode(t, err, 409, "gvisor_provider_compatibility_unaccepted")
	var target persistence.ExecutionTarget
	if err := fixture.db.First(&target, "id = ?", fixture.targetID).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != "active" {
		t.Fatalf("rejected runtime policy changed Target status to %q", target.Status)
	}
}
