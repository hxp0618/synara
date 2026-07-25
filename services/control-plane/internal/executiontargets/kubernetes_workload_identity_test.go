package executiontargets

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

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
			"spec": map[string]any{
				"serviceAccountName": serviceAccountName,
				"containers": []any{map[string]any{
					"name": "agentd",
					"resources": map[string]any{"requests": map[string]any{
						"cpu": "500m", "memory": "256Mi", "ephemeral-storage": "1Gi",
					}},
				}},
			},
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
	if serverState.reviewToken != "workload-bearer-token" {
		t.Fatalf("TokenReview reviewed token %q", serverState.reviewToken)
	}
	if len(serverState.reviewAudiences) != 1 || serverState.reviewAudiences[0] != audience {
		t.Fatalf("TokenReview audiences = %#v", serverState.reviewAudiences)
	}
	if serverState.podAuthHeader != "Bearer kubernetes-api-token" {
		t.Fatalf("Pod lookup used cluster auth header %q", serverState.podAuthHeader)
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
			"spec": map[string]any{"serviceAccountName": serviceAccountName},
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
	reviewToken        string
	reviewAudiences    []string
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
