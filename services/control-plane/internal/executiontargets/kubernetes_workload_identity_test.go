package executiontargets

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestVerifyKubernetesWorkloadIdentity(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	executionID := uuid.New()
	audience := KubernetesWorkloadIdentityAudience(targetID)
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	serverState := &kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": expectedUsername,
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{claims.PodName},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{audience},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{
				"name": claims.PodName,
				"uid":  claims.PodUID,
				"labels": map[string]any{
					kubernetesManagedLabel:   "true",
					kubernetesTargetLabel:    targetID.String(),
					kubernetesExecutionLabel: executionID.String(),
				},
			},
			"spec": hardenedKubernetesWorkloadIdentityPodSpec(serviceAccountName, targetID),
		},
	}
	server := httptest.NewTLSServer(http.HandlerFunc(serverState.serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	verified, err := service.VerifyKubernetesWorkloadIdentity(context.Background(), targetID, claims, "workload-bearer-token")
	if err != nil {
		t.Fatal(err)
	}
	if verified.TargetID != targetID || verified.Audience != audience || verified.Namespace != claims.Namespace ||
		verified.PodName != claims.PodName || verified.PodUID != claims.PodUID ||
		verified.ClusterID != kubernetesLocalClusterID ||
		verified.WorkerMode != kubernetesWorkerModeExecutionPinned ||
		verified.ServiceAccountName != serviceAccountName || verified.ServiceAccountUsername != expectedUsername {
		t.Fatalf("verified Kubernetes workload identity = %#v", verified)
	}
	if verified.RequestedCPUMillicores == nil || *verified.RequestedCPUMillicores != 500 ||
		verified.RequestedMemoryBytes == nil || *verified.RequestedMemoryBytes != 256<<20 ||
		verified.RequestedEphemeralStorageBytes == nil || *verified.RequestedEphemeralStorageBytes != 1<<30 {
		t.Fatalf("verified Kubernetes resource requests = %#v", verified)
	}
	if serverState.clusterAuthHeader != "Bearer kubernetes-api-token" {
		t.Fatalf("TokenReview used cluster auth header %q", serverState.clusterAuthHeader)
	}
	if serverState.pidsAuthHeader != "Bearer kubernetes-api-token" {
		t.Fatalf("PID-limit attestation used cluster auth header %q", serverState.pidsAuthHeader)
	}
	if serverState.reviewToken != "workload-bearer-token" {
		t.Fatalf("TokenReview reviewed token %q", serverState.reviewToken)
	}
	if len(serverState.reviewAudiences) != 1 || serverState.reviewAudiences[0] != audience {
		t.Fatalf("TokenReview audiences = %#v", serverState.reviewAudiences)
	}
	unbounded := int64(-1)
	serverState.podPIDsLimit = &unbounded
	_, unboundedErr := service.VerifyKubernetesWorkloadIdentity(
		context.Background(),
		targetID,
		claims,
		"workload-bearer-token",
	)
	assertProblemCode(
		t,
		unboundedErr,
		401,
		"kubernetes_workload_identity_pids_limit_invalid",
	)
	if serverState.podAuthHeader != "Bearer kubernetes-api-token" {
		t.Fatalf("Pod lookup used cluster auth header %q", serverState.podAuthHeader)
	}
}

func TestVerifyKubernetesWorkloadIdentityRejectsWeakenedOuterSandbox(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	executionID := uuid.New()
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	state := &kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{"status": map[string]any{
			"authenticated": true,
			"user": map[string]any{
				"username": expectedUsername,
				"extra": map[string]any{
					kubernetesPodNameExtraKey: []string{claims.PodName},
					kubernetesPodUIDExtraKey:  []string{claims.PodUID},
				},
			},
			"audiences": []string{KubernetesWorkloadIdentityAudience(targetID)},
		}},
		podStatusCode: http.StatusOK,
	}
	server := httptest.NewTLSServer(http.HandlerFunc(state.serveHTTP))
	defer server.Close()
	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)

	tests := []struct {
		name   string
		weaken func(map[string]any)
	}{
		{name: "ambient service account token", weaken: func(spec map[string]any) {
			spec["automountServiceAccountToken"] = true
		}},
		{name: "host pid namespace", weaken: func(spec map[string]any) { spec["hostPID"] = true }},
		{name: "sidecar", weaken: func(spec map[string]any) {
			spec["containers"] = append(spec["containers"].([]any), map[string]any{"name": "sidecar"})
		}},
		{name: "missing registration token init container", weaken: func(spec map[string]any) {
			spec["initContainers"] = spec["initContainers"].([]any)[:1]
		}},
		{name: "missing network boundary init container", weaken: func(spec map[string]any) {
			spec["initContainers"] = spec["initContainers"].([]any)[1:]
		}},
		{name: "network boundary init after registration", weaken: func(spec map[string]any) {
			initContainers := spec["initContainers"].([]any)
			initContainers[0], initContainers[1] = initContainers[1], initContainers[0]
		}},
		{name: "network boundary init has a volume mount", weaken: func(spec map[string]any) {
			initContainer := spec["initContainers"].([]any)[0].(map[string]any)
			initContainer["volumeMounts"] = []any{map[string]any{"name": "tmp", "mountPath": "/tmp"}}
		}},
		{name: "projected token exposed to agentd and Provider", weaken: func(spec map[string]any) {
			container := spec["containers"].([]any)[0].(map[string]any)
			container["volumeMounts"] = append(container["volumeMounts"].([]any), map[string]any{
				"name": kubernetesWorkloadIdentityVolume, "mountPath": "/var/run/secrets/synara.io/workload-identity", "readOnly": true,
			})
		}},
		{name: "privilege escalation", weaken: func(spec map[string]any) {
			container := spec["containers"].([]any)[0].(map[string]any)
			container["securityContext"].(map[string]any)["allowPrivilegeEscalation"] = true
		}},
		{name: "missing ephemeral storage limit", weaken: func(spec map[string]any) {
			container := spec["containers"].([]any)[0].(map[string]any)
			limits := container["resources"].(map[string]any)["limits"].(map[string]any)
			delete(limits, "ephemeral-storage")
		}},
		{name: "host path volume", weaken: func(spec map[string]any) {
			spec["volumes"] = append(spec["volumes"].([]any), map[string]any{
				"name": "host", "hostPath": map[string]any{"path": "/"},
			})
		}},
		{name: "private temp mismatch", weaken: func(spec map[string]any) {
			container := spec["containers"].([]any)[0].(map[string]any)
			for _, raw := range container["env"].([]any) {
				item := raw.(map[string]any)
				if item["name"] == "SYNARA_AGENTD_PRIVATE_TMP_ROOT" {
					item["value"] = "/data/tmp"
				}
			}
		}},
		{name: "missing PID limit declaration", weaken: func(spec map[string]any) {
			container := spec["containers"].([]any)[0].(map[string]any)
			environment := container["env"].([]any)
			container["env"] = environment[:len(environment)-1]
		}},
		{name: "excessive PID limit declaration", weaken: func(spec map[string]any) {
			container := spec["containers"].([]any)[0].(map[string]any)
			for _, raw := range container["env"].([]any) {
				item := raw.(map[string]any)
				if item["name"] == platform.KubernetesPIDsLimitEnvironment {
					item["value"] = "1048577"
				}
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := cloneKubernetesWorkloadIdentityPodSpec(t, hardenedKubernetesWorkloadIdentityPodSpec(serviceAccountName, targetID))
			test.weaken(spec)
			state.podResponse = map[string]any{
				"metadata": map[string]any{
					"name": claims.PodName,
					"uid":  claims.PodUID,
					"labels": map[string]any{
						kubernetesManagedLabel:   "true",
						kubernetesTargetLabel:    targetID.String(),
						kubernetesExecutionLabel: executionID.String(),
					},
				},
				"spec": spec,
			}
			err := service.VerifyWorkerRegistration(
				context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token",
			)
			assertProblemCode(t, err, 401, "kubernetes_workload_identity_outer_sandbox_invalid")
		})
	}
}

func TestLoadKubernetesWorkloadIdentityTargetAcceptsOfflineButRejectsDisabled(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).
		Where("id = ?", fixture.targetID).Update("status", "offline").Error; err != nil {
		t.Fatal(err)
	}
	model, err := fixture.reconciler.targets.loadKubernetesWorkloadIdentityTarget(context.Background(), fixture.targetID)
	if err != nil || model.ID != fixture.targetID || model.Status != "offline" {
		t.Fatalf("load offline Kubernetes Target = %#v, %v", model, err)
	}
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).
		Where("id = ?", fixture.targetID).Update("status", "disabled").Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.reconciler.targets.loadKubernetesWorkloadIdentityTarget(context.Background(), fixture.targetID)
	assertProblemCode(t, err, 404, "execution_target_not_found")
}

func TestResolveWorkerRegistrationAllowsOnlyVerifiedPodBoundKubernetesTargetOffline(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).
		Where("id = ?", fixture.targetID).Update("status", "offline").Error; err != nil {
		t.Fatal(err)
	}
	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, _, resolveErr := fixture.reconciler.targets.ResolveWorkerRegistrationTargetInTransaction(
			context.Background(), tx, fixture.targetID, "kubernetes", uuid.NewString(), nil, false,
		)
		return resolveErr
	})
	assertProblemCode(t, err, 404, "execution_target_not_found")
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		model, kind, resolveErr := fixture.reconciler.targets.ResolveWorkerRegistrationTargetInTransaction(
			context.Background(), tx, fixture.targetID, "kubernetes", uuid.NewString(), nil, true,
		)
		if resolveErr != nil {
			return resolveErr
		}
		if model.ID != fixture.targetID || kind != "kubernetes" {
			t.Fatalf("resolved offline Pod-bound target = %#v/%q", model, kind)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyKubernetesWorkloadIdentityUsesExplicitWorkerModeLabel(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	poolID := uuid.New()
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": expectedUsername,
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{claims.PodName},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{KubernetesWorkloadIdentityAudience(targetID)},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{"uid": claims.PodUID, "labels": map[string]any{
				kubernetesManagedLabel:           "true",
				kubernetesTargetLabel:            targetID.String(),
				kubernetesWorkerModeLabel:        kubernetesWorkerModeWarmPool,
				kubernetesWorkerPoolIDLabel:      poolID.String(),
				kubernetesWorkerPoolVersionLabel: "1",
				kubernetesCapacityClassLabel:     "interactive",
			}},
			"spec": hardenedKubernetesWorkloadIdentityPodSpec(serviceAccountName, targetID),
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	verified, err := service.VerifyKubernetesWorkerRegistration(
		context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	if verified.WorkerMode != kubernetesWorkerModeWarmPool {
		t.Fatalf("verified worker mode = %q, want %q", verified.WorkerMode, kubernetesWorkerModeWarmPool)
	}
	if verified.WorkerPoolID == nil || *verified.WorkerPoolID != poolID ||
		verified.WorkerPoolVersion == nil || *verified.WorkerPoolVersion != 1 ||
		verified.CapacityClass == nil || *verified.CapacityClass != "interactive" {
		t.Fatalf("verified warm Pool identity = %#v", verified)
	}
}

func TestVerifyKubernetesWorkloadIdentityRejectsMissingWorkerModeLabels(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": expectedUsername,
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{claims.PodName},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{KubernetesWorkloadIdentityAudience(targetID)},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{"uid": claims.PodUID, "labels": map[string]any{
				kubernetesManagedLabel: "true", kubernetesTargetLabel: targetID.String(),
			}},
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	assertProblemCode(
		t,
		service.VerifyWorkerRegistration(context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token"),
		401,
		"kubernetes_workload_identity_worker_mode_missing",
	)
}

func TestVerifyKubernetesWorkloadIdentityRejectsWrongAudience(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": expectedUsername,
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{claims.PodName},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{"synara.execution-target.other"},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{"uid": claims.PodUID, "labels": map[string]any{
				kubernetesManagedLabel: "true", kubernetesTargetLabel: targetID.String(),
			}},
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	assertProblemCode(t, service.VerifyWorkerRegistration(context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token"), 401, "kubernetes_workload_identity_audience_mismatch")
}

func TestVerifyKubernetesWorkloadIdentityRejectsWrongServiceAccount(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": "system:serviceaccount:" + claims.Namespace + ":different-worker",
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{claims.PodName},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{KubernetesWorkloadIdentityAudience(targetID)},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{"uid": claims.PodUID, "labels": map[string]any{
				kubernetesManagedLabel: "true", kubernetesTargetLabel: targetID.String(),
			}},
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	assertProblemCode(t, service.VerifyWorkerRegistration(context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token"), 401, "kubernetes_workload_identity_service_account_mismatch")
}

func TestVerifyKubernetesWorkloadIdentityRejectsWrongPodClaims(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": expectedUsername,
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{"different-pod"},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{KubernetesWorkloadIdentityAudience(targetID)},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{"uid": claims.PodUID, "labels": map[string]any{
				kubernetesManagedLabel: "true", kubernetesTargetLabel: targetID.String(),
			}},
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	assertProblemCode(t, service.VerifyWorkerRegistration(context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token"), 401, "kubernetes_workload_identity_pod_claim_mismatch")
}

func TestVerifyKubernetesWorkloadIdentityRejectsReplacedPodUID(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": expectedUsername,
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{claims.PodName},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{KubernetesWorkloadIdentityAudience(targetID)},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{"uid": "replacement-pod-uid", "labels": map[string]any{
				kubernetesManagedLabel: "true", kubernetesTargetLabel: targetID.String(),
			}},
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	assertProblemCode(t, service.VerifyWorkerRegistration(context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token"), 401, "kubernetes_workload_identity_pod_uid_mismatch")
}

func TestVerifyKubernetesWorkloadIdentityRejectsTerminatingPod(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": expectedUsername,
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{claims.PodName},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{KubernetesWorkloadIdentityAudience(targetID)},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{
				"uid":               claims.PodUID,
				"deletionTimestamp": time.Now().UTC().Format(time.RFC3339),
				"labels": map[string]any{
					kubernetesManagedLabel: "true", kubernetesTargetLabel: targetID.String(),
				},
			},
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	assertProblemCode(
		t,
		service.VerifyWorkerRegistration(
			context.Background(),
			targetID,
			claims.Namespace,
			claims.PodName,
			claims.PodUID,
			"workload-bearer-token",
		),
		409,
		"kubernetes_workload_identity_pod_terminating",
	)
}

func TestVerifyKubernetesWorkloadIdentityRejectsWrongPodLabels(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	expectedUsername := "system:serviceaccount:" + claims.Namespace + ":" + serviceAccountName
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusCreated,
		reviewResponse: map[string]any{
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": expectedUsername,
					"extra": map[string]any{
						kubernetesPodNameExtraKey: []string{claims.PodName},
						kubernetesPodUIDExtraKey:  []string{claims.PodUID},
					},
				},
				"audiences": []string{KubernetesWorkloadIdentityAudience(targetID)},
			},
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{"uid": claims.PodUID, "labels": map[string]any{
				kubernetesManagedLabel: "false", kubernetesTargetLabel: uuid.NewString(),
			}},
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	assertProblemCode(t, service.VerifyWorkerRegistration(context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token"), 401, "kubernetes_workload_identity_target_mismatch")
}

func TestVerifyKubernetesWorkloadIdentitySurfacesTokenReviewAPIFailure(t *testing.T) {
	claims := KubernetesWorkloadIdentityClaims{
		Namespace: "synara-target", PodName: "synara-worker-0", PodUID: "pod-uid-1",
	}
	serviceAccountName := "synara-worker"
	fixture := newKubernetesReconcileFixture(t, "")
	targetID := fixture.targetID
	server := httptest.NewTLSServer(http.HandlerFunc((&kubernetesWorkloadIdentityServerState{
		t:                  t,
		targetID:           targetID,
		serviceAccountName: serviceAccountName,
		reviewStatusCode:   http.StatusInternalServerError,
		reviewResponse: map[string]any{
			"reason":  "InternalError",
			"message": "token review backend unavailable",
		},
		podStatusCode: http.StatusOK,
		podResponse: map[string]any{
			"metadata": map[string]any{"uid": claims.PodUID, "labels": map[string]any{
				kubernetesManagedLabel: "true", kubernetesTargetLabel: targetID.String(),
			}},
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
		},
	}).serveHTTP))
	defer server.Close()

	service := configureKubernetesWorkloadIdentityService(t, fixture, claims.Namespace, serviceAccountName, server)
	assertProblemCode(t, service.VerifyWorkerRegistration(context.Background(), targetID, claims.Namespace, claims.PodName, claims.PodUID, "workload-bearer-token"), 502, "kubernetes_workload_identity_review_failed")
}

type kubernetesWorkloadIdentityServerState struct {
	t                  *testing.T
	targetID           uuid.UUID
	serviceAccountName string
	reviewStatusCode   int
	reviewResponse     any
	podStatusCode      int
	podResponse        any
	clusterAuthHeader  string
	podAuthHeader      string
	pidsAuthHeader     string
	podPIDsLimit       *int64
	reviewToken        string
	reviewAudiences    []string
}

func hardenedKubernetesWorkloadIdentityPodSpec(serviceAccountName string, targetID uuid.UUID) map[string]any {
	return map[string]any{
		"nodeName":                     "worker-a",
		"serviceAccountName":           serviceAccountName,
		"automountServiceAccountToken": false,
		"hostNetwork":                  false,
		"hostPID":                      false,
		"hostIPC":                      false,
		"securityContext": map[string]any{
			"runAsNonRoot": true,
			"fsGroup":      10001,
			"seccompProfile": map[string]any{
				"type": "RuntimeDefault",
			},
		},
		"containers": []any{map[string]any{
			"name": "agentd", "image": "synara/worker:test", "imagePullPolicy": "IfNotPresent",
			"command": []any{"/usr/local/bin/synara-agentd"},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"runAsNonRoot":             true,
				"runAsUser":                10001,
				"runAsGroup":               10001,
				"capabilities":             map[string]any{"drop": []any{"ALL"}},
				"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
			},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "500m", "memory": "256Mi", "ephemeral-storage": "1Gi"},
				"limits":   map[string]any{"cpu": "1", "memory": "512Mi", "ephemeral-storage": "2Gi"},
			},
			"env": []any{
				map[string]any{"name": "SYNARA_EXECUTION_TARGET_KIND", "value": "kubernetes"},
				map[string]any{"name": "SYNARA_WORKER_REGISTRATION_TOKEN_FILE", "value": kubernetesStagedRegistrationTokenPath},
				map[string]any{"name": "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL", "value": "v2"},
				map[string]any{"name": "SYNARA_AGENTD_PRIVATE_TMP_ROOT", "value": "/tmp"},
				map[string]any{"name": platform.KubernetesPIDsLimitEnvironment, "value": "512"},
			},
			"volumeMounts": []any{
				map[string]any{"name": "workspace", "mountPath": "/data"},
				map[string]any{"name": "tmp", "mountPath": "/tmp"},
				map[string]any{"name": "home", "mountPath": "/home/synara"},
				map[string]any{
					"name": kubernetesRegistrationTokenVolume, "mountPath": "/var/run/secrets/synara.io/registration",
				},
			},
		}},
		"initContainers": []any{map[string]any{
			"name": kubernetesNetworkBoundaryInitName, "image": "synara/worker:test", "imagePullPolicy": "IfNotPresent",
			"command": []any{"/usr/local/bin/synara-agentd", platform.KubernetesNetworkBoundaryVerifyArgument},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"runAsNonRoot":             true,
				"runAsUser":                10001,
				"runAsGroup":               10001,
				"capabilities":             map[string]any{"drop": []any{"ALL"}},
				"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
			},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "500m", "memory": "256Mi", "ephemeral-storage": "1Gi"},
				"limits":   map[string]any{"cpu": "1", "memory": "512Mi", "ephemeral-storage": "2Gi"},
			},
		}, map[string]any{
			"name": kubernetesRegistrationTokenInitName, "image": "synara/worker:test", "imagePullPolicy": "IfNotPresent",
			"command": []any{"/usr/local/bin/synara-agentd", platform.KubernetesRegistrationTokenStageArgument},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"runAsNonRoot":             true,
				"runAsUser":                10001,
				"runAsGroup":               10001,
				"capabilities":             map[string]any{"drop": []any{"ALL"}},
				"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
			},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "500m", "memory": "256Mi", "ephemeral-storage": "1Gi"},
				"limits":   map[string]any{"cpu": "1", "memory": "512Mi", "ephemeral-storage": "2Gi"},
			},
			"volumeMounts": []any{
				map[string]any{
					"name": kubernetesWorkloadIdentityVolume, "mountPath": "/var/run/secrets/synara.io/workload-identity", "readOnly": true,
				},
				map[string]any{
					"name": kubernetesRegistrationTokenVolume, "mountPath": "/var/run/secrets/synara.io/registration",
				},
			},
		}},
		"volumes": []any{
			map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
			map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
			map[string]any{"name": "home", "emptyDir": map[string]any{}},
			map[string]any{
				"name": kubernetesWorkloadIdentityVolume,
				"projected": map[string]any{"defaultMode": 0o440, "sources": []any{map[string]any{
					"serviceAccountToken": map[string]any{
						"audience": KubernetesWorkerRegistrationAudience(targetID), "expirationSeconds": 600, "path": "token",
					},
				}}},
			},
			map[string]any{"name": kubernetesRegistrationTokenVolume, "emptyDir": map[string]any{}},
		},
	}
}

func cloneKubernetesWorkloadIdentityPodSpec(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (s *kubernetesWorkloadIdentityServerState) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews":
		s.clusterAuthHeader = r.Header.Get("Authorization")
		var input struct {
			Spec struct {
				Token     string   `json:"token"`
				Audiences []string `json:"audiences"`
			} `json:"spec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			s.t.Fatal(err)
		}
		s.reviewToken = input.Spec.Token
		s.reviewAudiences = append([]string(nil), input.Spec.Audiences...)
		writeTestJSON(w, s.reviewStatusCode, s.reviewResponse)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/synara-target/pods/synara-worker-0":
		s.podAuthHeader = r.Header.Get("Authorization")
		writeTestJSON(w, s.podStatusCode, s.podResponse)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes/worker-a/proxy/configz":
		s.pidsAuthHeader = r.Header.Get("Authorization")
		limit := int64(512)
		if s.podPIDsLimit != nil {
			limit = *s.podPIDsLimit
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"kubeletconfig": map[string]any{"podPidsLimit": limit},
		})
	default:
		s.t.Fatalf("unexpected Kubernetes workload identity request: %s %s", r.Method, r.URL.Path)
	}
}

func configureKubernetesWorkloadIdentityService(
	t *testing.T,
	fixture kubernetesReconcileFixture,
	namespace, serviceAccountName string,
	server *httptest.Server,
) *Service {
	t.Helper()
	pemCertificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	configuration := kubernetesTestConfiguration("")
	configuration["apiServer"] = server.URL
	configuration["caCertificate"] = string(pemCertificate)
	configuration["namespace"] = namespace
	configuration["serviceAccountName"] = serviceAccountName
	fixture.updateConfiguration(t, configuration)
	return fixture.reconciler.targets
}

func writeTestJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(err)
	}
}

func assertProblemCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) {
		t.Fatalf("expected problem error, got %v", err)
	}
	if apiError.Status != status || apiError.Code != code {
		t.Fatalf("problem = (%d, %s), want (%d, %s)", apiError.Status, apiError.Code, status, code)
	}
}
