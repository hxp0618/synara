package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
)

func TestExecutionPlacementRoutesEnforceTenantReadManageAndCAS(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	now := time.Now().UTC()
	kubernetesTargetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: kubernetesTargetID, TenantID: &fixture.tenantID, Kind: "kubernetes",
		Name: "placement-kubernetes", Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.targetID = kubernetesTargetID
	basePath := fixture.ownerPlacementPath()

	for _, tc := range []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{name: "unauthenticated list", status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "active tenant mismatch list", token: fixture.crossTenantToken, status: http.StatusNotFound, code: "tenant_not_found"},
		{name: "missing worker read list", token: fixture.memberToken, status: http.StatusForbidden, code: "tenant_forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertProblemResponse(
				t,
				fixture.placementRequest(t, http.MethodGet, basePath, tc.token, ""),
				tc.status,
				tc.code,
			)
		})
	}

	readable := fixture.placementRequest(t, http.MethodGet, basePath, fixture.readOnlyToken, "")
	if readable.Code != http.StatusOK {
		t.Fatalf("read-only list status = %d, body = %s", readable.Code, readable.Body.String())
	}
	var initial placement.State
	decodePlacementHTTPResponse(t, readable, &initial)
	if len(initial.Pools) != 1 || initial.Policy.Version != 1 || initial.Pools[0].Name != "default" {
		t.Fatalf("initial placement state = %#v", initial)
	}

	createBody := placementHTTPJSON(t, map[string]any{
		"name":               "interactive-warm",
		"mode":               "warm",
		"capacityClass":      "interactive",
		"clusterId":          "",
		"region":             "",
		"namespace":          "",
		"desiredIdleUnits":   2,
		"minIdleUnits":       1,
		"maxActiveUnits":     6,
		"schedulingTemplate": map[string]any{},
		"status":             "active",
	})
	assertProblemResponse(
		t,
		fixture.placementRequest(t, http.MethodPost, basePath, fixture.readOnlyToken, createBody),
		http.StatusForbidden,
		"tenant_forbidden",
	)

	createdResponse := fixture.placementRequest(t, http.MethodPost, basePath, fixture.ownerToken, createBody)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createdResponse.Code, createdResponse.Body.String())
	}
	var created placement.Pool
	decodePlacementHTTPResponse(t, createdResponse, &created)
	if created.Name != "interactive-warm" || created.Mode != placement.PoolModeWarm || created.Version != 1 || created.MinIdleUnits != 1 {
		t.Fatalf("created pool = %#v", created)
	}

	patchPath := basePath + "/" + created.ID.String()
	patchBody := placementHTTPJSON(t, map[string]any{
		"expectedVersion":    1,
		"name":               "interactive-warm-primary",
		"mode":               "warm",
		"capacityClass":      "interactive",
		"clusterId":          "cluster-a",
		"region":             "cn-east-1",
		"namespace":          "synara-workers",
		"desiredIdleUnits":   3,
		"minIdleUnits":       2,
		"maxActiveUnits":     8,
		"schedulingTemplate": map[string]any{"priorityClassName": "interactive"},
		"status":             "draining",
	})
	assertProblemResponse(
		t,
		fixture.placementRequest(t, http.MethodPatch, patchPath, fixture.readOnlyToken, patchBody),
		http.StatusForbidden,
		"tenant_forbidden",
	)
	updatedResponse := fixture.placementRequest(t, http.MethodPatch, patchPath, fixture.ownerToken, patchBody)
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", updatedResponse.Code, updatedResponse.Body.String())
	}
	var updated placement.Pool
	decodePlacementHTTPResponse(t, updatedResponse, &updated)
	if updated.Name != "interactive-warm-primary" || updated.Version != 2 || updated.Status != placement.PoolStatusDraining || updated.MinIdleUnits != 2 {
		t.Fatalf("updated pool = %#v", updated)
	}

	policyPath := basePath[:len(basePath)-len("/worker-pools")] + "/placement-policy"
	assertProblemResponse(
		t,
		fixture.placementRequest(t, http.MethodPut, policyPath, fixture.readOnlyToken, placementHTTPJSON(t, map[string]any{
			"expectedVersion": 1,
			"defaultPoolId":   initial.Policy.DefaultPoolID,
		})),
		http.StatusForbidden,
		"tenant_forbidden",
	)
	assertProblemResponse(
		t,
		fixture.placementRequest(t, http.MethodPut, policyPath, fixture.ownerToken, placementHTTPJSON(t, map[string]any{
			"expectedVersion": 1,
			"defaultPoolId":   uuid.NewString(),
		})),
		http.StatusConflict,
		"execution_placement_pool_not_found",
	)

	updatePolicyBody := placementHTTPJSON(t, map[string]any{
		"expectedVersion":  initial.Policy.Version,
		"defaultPoolId":    initial.Policy.DefaultPoolID,
		"balancedPoolId":   updated.ID,
		"lowLatencyPoolId": updated.ID,
	})
	policyResponse := fixture.placementRequest(t, http.MethodPut, policyPath, fixture.ownerToken, updatePolicyBody)
	if policyResponse.Code != http.StatusOK {
		t.Fatalf("policy update status = %d, body = %s", policyResponse.Code, policyResponse.Body.String())
	}
	var updatedState placement.State
	decodePlacementHTTPResponse(t, policyResponse, &updatedState)
	if updatedState.Policy.Version != 2 || updatedState.Policy.BalancedPoolID == nil ||
		updatedState.Policy.LowLatencyPoolID == nil || *updatedState.Policy.LowLatencyPoolID != updated.ID {
		t.Fatalf("updated placement state = %#v", updatedState)
	}

	assertProblemResponse(
		t,
		fixture.placementRequest(t, http.MethodPut, policyPath, fixture.ownerToken, updatePolicyBody),
		http.StatusConflict,
		"execution_placement_policy_version_conflict",
	)
	assertProblemResponse(
		t,
		fixture.placementRequest(t, http.MethodPatch, patchPath, fixture.ownerToken, placementHTTPJSON(t, map[string]any{
			"expectedVersion":    1,
			"name":               "stale-update",
			"mode":               "warm",
			"capacityClass":      "interactive",
			"clusterId":          "cluster-a",
			"region":             "cn-east-1",
			"namespace":          "synara-workers",
			"desiredIdleUnits":   1,
			"minIdleUnits":       1,
			"maxActiveUnits":     4,
			"schedulingTemplate": map[string]any{},
			"status":             "active",
		})),
		http.StatusConflict,
		"worker_pool_version_conflict",
	)
}

func (f workerManifestHTTPFixture) ownerPlacementPath() string {
	return "/v1/tenants/" + f.tenantID.String() + "/execution-targets/" + f.targetID.String() + "/worker-pools"
}

func (f workerManifestHTTPFixture) placementRequest(
	t *testing.T,
	method, path, token, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func placementHTTPJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func decodePlacementHTTPResponse(t *testing.T, response *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
		t.Fatalf("decode body %s: %v", response.Body.String(), err)
	}
}
