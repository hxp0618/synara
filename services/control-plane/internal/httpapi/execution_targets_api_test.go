package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

func TestUpdateExecutionTargetProviderPolicyRoute(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	request := httptest.NewRequest(
		http.MethodPatch,
		"/v1/tenants/"+fixture.tenantID.String()+"/execution-targets/"+fixture.targetID.String()+"/provider-policy",
		strings.NewReader(`{"experimentalProviders":[" OpenCode "]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: fixture.ownerToken})
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var target executiontargets.Target
	if err := json.Unmarshal(recorder.Body.Bytes(), &target); err != nil {
		t.Fatal(err)
	}
	policy, err := executiontargets.ParseProviderPolicy(target.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.ExperimentalProviders) != 1 || policy.ExperimentalProviders[0] != "opencode" {
		t.Fatalf("updated Provider Policy = %#v", policy.ExperimentalProviders)
	}
}

func TestUpdateExecutionTargetProcessContainmentPolicyRoute(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	request := httptest.NewRequest(
		http.MethodPatch,
		"/v1/tenants/"+fixture.tenantID.String()+"/execution-targets/"+fixture.targetID.String()+"/process-containment-policy",
		strings.NewReader(`{"trustMode":"signed-v1","keyId":"test-key","ed25519PublicKey":"AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA="}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: fixture.ownerToken})
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var target executiontargets.Target
	if err := json.Unmarshal(recorder.Body.Bytes(), &target); err != nil {
		t.Fatal(err)
	}
	policy, err := executiontargets.ParseProcessContainmentPolicy(target.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if policy.TrustMode != executiontargets.ProcessContainmentTrustSignedV1 || policy.KeyID != "test-key" {
		t.Fatalf("updated process containment policy = %#v", policy)
	}
}

func TestUpdateExecutionTargetRuntimeIsolationPolicyRoute(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	var organization persistence.Organization
	if err := fixture.db.Where("tenant_id = ?", fixture.tenantID).First(&organization).Error; err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest(
		http.MethodPost,
		"/v1/tenants/"+fixture.tenantID.String()+"/execution-targets",
		strings.NewReader(`{"organizationId":"`+organization.ID.String()+`","kind":"kubernetes","name":"runtime-policy-route","configuration":{"image":"synara-agentd:test"},"capabilities":{}}`),
	)
	create.Header.Set("Content-Type", "application/json")
	create.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: fixture.ownerToken})
	createdRecorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(createdRecorder, create)
	if createdRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createdRecorder.Code, createdRecorder.Body.String())
	}
	var created executiontargets.Target
	if err := json.Unmarshal(createdRecorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := "/v1/tenants/" + fixture.tenantID.String() + "/execution-targets/" + created.ID.String() + "/runtime-isolation-policy"
	body := `{"mode":"explicit","runtime":"gvisor","minimumProfile":"gvisor-sandboxed-v1","fallbackPolicy":"fail-closed","runtimeClassName":"synara-gvisor","gvisorCompatibleProviders":["CODEX"]}`
	request := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: token})
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, req)
		return recorder
	}
	assertProblemResponse(t, request(fixture.memberToken), http.StatusForbidden, "tenant_forbidden")
	updatedRecorder := request(fixture.ownerToken)
	if updatedRecorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", updatedRecorder.Code, updatedRecorder.Body.String())
	}
	var updated executiontargets.Target
	if err := json.Unmarshal(updatedRecorder.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status != "offline" || updated.RuntimeIsolationPolicy == nil ||
		len(updated.RuntimeIsolationPolicy.GVisorCompatibleProviders) != 1 ||
		updated.RuntimeIsolationPolicy.GVisorCompatibleProviders[0] != "codex" {
		t.Fatalf("updated runtime isolation Target = %#v", updated)
	}
}

func TestDisableManagedKubernetesExecutionTargetRoute(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	targetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &fixture.tenantID, Kind: "kubernetes",
		Name: "http-managed-kubernetes", Status: "active", Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	available := 8
	if _, err := routing.NewService(fixture.db).ObserveHealth(context.Background(), routing.HealthObservation{
		ExecutionTargetID: targetID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: &available,
		ReservationAuthority:   &routing.ReservationAuthorityObservation{Mode: routing.ReservationAuthorityExactActiveV1},
		Source:                 "managed-kubernetes-routing-publisher:http-test",
		ObservedAt:             now, TTL: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/tenants/" + fixture.tenantID.String() + "/execution-targets/" + targetID.String() + "/kubernetes/disable"
	request := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: token})
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, req)
		return recorder
	}

	denied := request(fixture.memberToken)
	assertProblemResponse(t, denied, http.StatusForbidden, "tenant_forbidden")

	first := request(fixture.ownerToken)
	if first.Code != http.StatusOK || first.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("first disable status=%d replay=%q body=%s", first.Code, first.Header().Get("Idempotency-Replayed"), first.Body.String())
	}
	var disabled executiontargets.Target
	if err := json.Unmarshal(first.Body.Bytes(), &disabled); err != nil {
		t.Fatal(err)
	}
	if disabled.ID != targetID || disabled.Status != "disabled" {
		t.Fatalf("disabled Target = %#v", disabled)
	}

	replay := request(fixture.ownerToken)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d replay=%q body=%s", replay.Code, replay.Header().Get("Idempotency-Replayed"), replay.Body.String())
	}
}
