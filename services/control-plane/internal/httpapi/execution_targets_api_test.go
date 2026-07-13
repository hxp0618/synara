package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
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
