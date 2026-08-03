package kmsworker

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPServerRequiresVerifiedRoleAndServesLifecycleAndCrypto(t *testing.T) {
	fixture := newServiceFixture(t, 0x61)
	authorizer, err := NewAuthorizer(map[string][]Role{
		"spiffe://synara.test/control-plane": {RoleControlPlaneCryptor},
		"spiffe://synara.test/manager":       {RoleKeyManager},
		"spiffe://synara.test/disabler":      {RoleKeyDisabler},
		"spiffe://synara.test/approver":      {RoleDeletionApprover},
		"spiffe://synara.test/auditor":       {RoleAuditor},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewHTTPServer(fixture.service, authorizer)
	if err != nil {
		t.Fatal(err)
	}

	request := authenticatedRequest(t, http.MethodPost, "/v1/keys", `{"name":"HTTP key"}`, "spiffe://synara.test/manager")
	request.Header.Set("Idempotency-Key", "http-create")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create status = %d, headers = %#v, body = %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var created KeyDescription
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	dataKey := bytes.Repeat([]byte{0x71}, 32)
	aad := []byte("http-aad")
	encoded, _ := json.Marshal(map[string]any{"dataKey": dataKey, "aad": aad})
	request = authenticatedRequest(t, http.MethodPost, "/v1/keys/"+created.KeyID+"/versions/1:wrap", string(encoded), "spiffe://synara.test/control-plane")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("wrap status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var wrapped WrapResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &wrapped); err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(map[string]any{"wrappedDataKey": wrapped.WrappedDataKey, "aad": aad})
	request = authenticatedRequest(t, http.MethodPost, "/v1/keys/"+created.KeyID+"/versions/1:unwrap", string(encoded), "spiffe://synara.test/control-plane")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unwrap status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var unwrapped struct {
		DataKey []byte `json:"dataKey"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &unwrapped); err != nil || !bytes.Equal(unwrapped.DataKey, dataKey) {
		t.Fatalf("unexpected unwrap response: %x, %v", unwrapped.DataKey, err)
	}

	request = authenticatedRequest(t, http.MethodGet, "/v1/audit-events?limit=20", "", "spiffe://synara.test/auditor")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"operation":"unwrap"`) {
		t.Fatalf("audit response = %d, %s", recorder.Code, recorder.Body.String())
	}
	request = authenticatedRequest(t, http.MethodGet, "/metrics", "", "spiffe://synara.test/auditor")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `synara_kms_requests_total{operation="wrap",status="200"} 1`) ||
		!strings.Contains(recorder.Body.String(), "synara_kms_seal_key_info") || strings.Contains(recorder.Body.String(), "http-aad") {
		t.Fatalf("metrics response = %d, %s", recorder.Code, recorder.Body.String())
	}
}

func TestReadinessRequiresActiveAuthenticatedCanary(t *testing.T) {
	fixture := newServiceFixture(t, 0x64)
	authorizer, _ := NewAuthorizer(map[string][]Role{"manager": {RoleKeyManager}})
	server, _ := NewHTTPServer(fixture.service, authorizer)
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty KMS readiness = %d, %s", recorder.Code, recorder.Body.String())
	}
	fixture.createKey("ready-create")
	request = httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("active KMS readiness = %d, %s", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPServerRejectsUnverifiedWrongRoleUnknownFieldsAndOversizedBodies(t *testing.T) {
	fixture := newServiceFixture(t, 0x62)
	authorizer, err := NewAuthorizer(map[string][]Role{
		"manager": {RoleKeyManager}, "cryptor": {RoleControlPlaneCryptor},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, _ := NewHTTPServer(fixture.service, authorizer)

	tests := []struct {
		name     string
		request  *http.Request
		expected int
	}{
		{name: "no certificate", request: httptest.NewRequest(http.MethodPost, "/v1/keys", strings.NewReader(`{"name":"x"}`)), expected: http.StatusUnauthorized},
		{name: "wrong role", request: authenticatedRequest(t, http.MethodPost, "/v1/keys", `{"name":"x"}`, "cryptor"), expected: http.StatusForbidden},
		{name: "unknown field", request: authenticatedRequest(t, http.MethodPost, "/v1/keys", `{"name":"x","keyMaterial":"do-not-import"}`, "manager"), expected: http.StatusBadRequest},
		{name: "oversized", request: authenticatedRequest(t, http.MethodPost, "/v1/keys", `{"name":"`+strings.Repeat("x", maximumRequestBytes)+`"}`, "manager"), expected: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.request.Header.Set("Idempotency-Key", "test-idempotency")
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, test.request)
			if recorder.Code != test.expected {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "do-not-import") || strings.Contains(recorder.Body.String(), "certificate") {
				t.Fatalf("error leaked request or authentication detail: %s", recorder.Body.String())
			}
		})
	}
}

func TestHTTPPatchCanClearExpiryWithoutChangingMaterial(t *testing.T) {
	fixture := newServiceFixture(t, 0x63)
	created := fixture.createKey("http-update-create")
	authorizer, _ := NewAuthorizer(map[string][]Role{"manager": {RoleKeyManager}})
	server, _ := NewHTTPServer(fixture.service, authorizer)
	request := authenticatedRequest(t, http.MethodPatch, "/v1/keys/"+created.KeyID,
		`{"expectedRevision":1,"encryptNotAfter":"2026-08-04T10:00:00Z","decryptNotAfter":null}`, "manager")
	request.Header.Set("Idempotency-Key", "http-update")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var updated KeyDescription
	if err := json.Unmarshal(recorder.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Versions[0].EncryptNotAfter == nil || updated.Versions[0].DecryptNotAfter != nil {
		t.Fatalf("unexpected lifecycle update: %#v", updated.Versions[0])
	}
}

func authenticatedRequest(t *testing.T, method, path, body, identity string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	parsedURI, err := url.Parse(identity)
	certificate := &x509.Certificate{}
	if err == nil && parsedURI.Scheme != "" {
		certificate.URIs = []*url.URL{parsedURI}
	} else {
		certificate.Subject.CommonName = identity
	}
	request.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certificate},
		VerifiedChains:   [][]*x509.Certificate{{certificate}},
	}
	return request
}
