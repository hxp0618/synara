package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestTenantWorkerRoutesEnforceReadManageActiveTenantAndIdempotency(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)

	for _, test := range []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{name: "unauthenticated", status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "active tenant mismatch", token: fixture.crossTenantToken, status: http.StatusNotFound, code: "tenant_not_found"},
		{name: "missing worker read", token: fixture.memberToken, status: http.StatusForbidden, code: "tenant_forbidden"},
	} {
		t.Run("list "+test.name, func(t *testing.T) {
			response := fixture.workerRequest(t, http.MethodGet, fixture.ownerWorkerPath(), test.token, "", "")
			assertProblemResponse(t, response, test.status, test.code)
		})
	}

	readable := fixture.workerRequest(t, http.MethodGet, fixture.ownerWorkerPath(), fixture.readOnlyToken, "", "")
	if readable.Code != http.StatusOK {
		t.Fatalf("Worker reader status = %d, body = %s", readable.Code, readable.Body.String())
	}

	body := `{"expectedIncarnation":1,"reason":"operator response"}`
	readOnlyRevoke := fixture.workerRequest(
		t, http.MethodPost, fixture.ownerWorkerPath()+"/"+fixture.workerID.String()+"/revoke",
		fixture.readOnlyToken, body, "worker-read-only-revoke",
	)
	assertProblemResponse(t, readOnlyRevoke, http.StatusForbidden, "tenant_forbidden")

	missingKey := fixture.workerRequest(
		t, http.MethodPost, fixture.ownerWorkerPath()+"/"+fixture.workerID.String()+"/revoke",
		fixture.ownerToken, body, "",
	)
	assertProblemResponse(t, missingKey, http.StatusBadRequest, "idempotency_key_required")
}

func TestTenantWorkerRoutesProjectSafeFieldsAndReplayRevocation(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)

	listed := fixture.workerRequest(t, http.MethodGet, fixture.ownerWorkerPath(), fixture.ownerToken, "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listed.Code, listed.Body.String())
	}
	assertSafeManagedWorkerEnvelope(t, listed, fixture.workerID.String(), "active")

	path := fixture.ownerWorkerPath() + "/" + fixture.workerID.String() + "/revoke"
	body := `{"expectedIncarnation":1,"reason":"operator security response"}`
	first := fixture.workerRequest(t, http.MethodPost, path, fixture.ownerToken, body, "worker-revoke-http")
	if first.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body = %s", first.Code, first.Body.String())
	}
	assertSafeWorkerRevocation(t, first, fixture.workerID.String(), "revoked")

	replayed := fixture.workerRequest(t, http.MethodPost, path, fixture.ownerToken, body, "worker-revoke-http")
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay status/header = %d/%q, body = %s", replayed.Code, replayed.Header().Get("Idempotent-Replayed"), replayed.Body.String())
	}
	var firstJSON, replayedJSON any
	if err := json.Unmarshal(first.Body.Bytes(), &firstJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replayed.Body.Bytes(), &replayedJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstJSON, replayedJSON) {
		t.Fatalf("replayed body changed: first=%s replayed=%s", first.Body.String(), replayed.Body.String())
	}

	conflict := fixture.workerRequest(
		t, http.MethodPost, path, fixture.ownerToken,
		`{"expectedIncarnation":1,"reason":"different reason"}`, "worker-revoke-http",
	)
	assertProblemResponse(t, conflict, http.StatusConflict, "idempotency_conflict")

	listed = fixture.workerRequest(t, http.MethodGet, fixture.ownerWorkerPath(), fixture.ownerToken, "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("post-revoke list status = %d, body = %s", listed.Code, listed.Body.String())
	}
	assertSafeManagedWorkerEnvelope(t, listed, fixture.workerID.String(), "revoked")
}

func (f workerManifestHTTPFixture) ownerWorkerPath() string {
	return "/v1/tenants/" + f.tenantID.String() + "/workers"
}

func (f workerManifestHTTPFixture) workerRequest(
	t *testing.T,
	method, path, token, body, idempotencyKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func assertSafeManagedWorkerEnvelope(
	t *testing.T,
	response *httptest.ResponseRecorder,
	workerID, administrativeStatus string,
) {
	t.Helper()
	var envelope struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Items) != 1 {
		t.Fatalf("Worker items = %#v", envelope.Items)
	}
	assertSafeManagedWorker(t, envelope.Items[0], workerID, administrativeStatus, response.Body.String())
}

func assertSafeWorkerRevocation(
	t *testing.T,
	response *httptest.ResponseRecorder,
	workerID, administrativeStatus string,
) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	var worker map[string]json.RawMessage
	if err := json.Unmarshal(payload["worker"], &worker); err != nil {
		t.Fatal(err)
	}
	assertSafeManagedWorker(t, worker, workerID, administrativeStatus, response.Body.String())
}

func assertSafeManagedWorker(
	t *testing.T,
	worker map[string]json.RawMessage,
	wantWorkerID, wantAdministrativeStatus, body string,
) {
	t.Helper()
	for _, forbiddenKey := range []string{"capabilities", "authTokenHash", "token", "tokenHash"} {
		if _, exists := worker[forbiddenKey]; exists {
			t.Fatalf("managed Worker leaked %q: %s", forbiddenKey, body)
		}
	}
	for _, forbiddenValue := range []string{"RAW-WORKER-CAPABILITY-SECRET", "WORKER-TOKEN-HASH-SECRET"} {
		if strings.Contains(body, forbiddenValue) {
			t.Fatalf("managed Worker response leaked %q: %s", forbiddenValue, body)
		}
	}
	var workerID, administrativeStatus string
	if err := json.Unmarshal(worker["id"], &workerID); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(worker["administrativeStatus"], &administrativeStatus); err != nil {
		t.Fatal(err)
	}
	if workerID != wantWorkerID || administrativeStatus != wantAdministrativeStatus {
		t.Fatalf("managed Worker identity/status = %q/%q, want %q/%q", workerID, administrativeStatus, wantWorkerID, wantAdministrativeStatus)
	}
}
