package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var tenantRouteRegistrationPattern = regexp.MustCompile(`(?m)^\s*routes\.(?:Internal|PublicBeta|PublicGA)(?:Func)?\("([A-Z]+) ([^"]*\/v1\/tenants\/\{tenantID\}[^"]*)",`)
var routeParameterPattern = regexp.MustCompile(`\{[^}]+\}`)

type explicitTenantRoute struct {
	method   string
	template string
}

func explicitTenantRouteInventory(t *testing.T) []explicitTenantRoute {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate Tenant route test source")
	}
	source, err := os.ReadFile(strings.TrimSuffix(currentFile, "tenant_route_active_context_test.go") + "server.go")
	if err != nil {
		t.Fatal(err)
	}
	matches := tenantRouteRegistrationPattern.FindAllStringSubmatch(string(source), -1)
	if len(matches) < 100 {
		t.Fatalf("explicit Tenant route inventory = %d, want at least 100", len(matches))
	}
	routes := make([]explicitTenantRoute, 0, len(matches))
	for _, match := range matches {
		routes = append(routes, explicitTenantRoute{method: match[1], template: match[2]})
	}
	return routes
}

func TestEveryExplicitTenantRouteRejectsInactiveTenantContextBeforeHandling(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	routes := explicitTenantRouteInventory(t)
	for _, route := range routes {
		method, template := route.method, route.template
		if method == http.MethodPost && template == "/v1/tenants/{tenantID}/restore" {
			continue
		}
		path := strings.ReplaceAll(template, "{tenantID}", fixture.tenantID.String())
		path = routeParameterPattern.ReplaceAllString(path, fixture.targetID.String())
		t.Run(method+" "+template, func(t *testing.T) {
			request := httptest.NewRequest(method, path, nil)
			request.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: fixture.crossTenantToken})
			response := httptest.NewRecorder()
			fixture.handler.ServeHTTP(response, request)
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			if response.Code != http.StatusNotFound || body.Error.Code != "tenant_not_found" {
				t.Fatalf(
					"inactive Tenant context reached handler: status=%d code=%q body=%s",
					response.Code, body.Error.Code, response.Body.String(),
				)
			}
		})
	}

	t.Run("deletion recovery remains an explicit exception", func(t *testing.T) {
		reached := false
		handler := fixture.server.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusNoContent)
		}))
		request := httptest.NewRequest(http.MethodPost, "/v1/tenants/"+fixture.tenantID.String()+"/restore", nil)
		request.Pattern = "POST /v1/tenants/{tenantID}/restore"
		request.SetPathValue("tenantID", fixture.tenantID.String())
		request.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: fixture.crossTenantToken})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if !reached || response.Code != http.StatusNoContent {
			t.Fatalf("inactive Tenant recovery did not reach its explicit exception handler: status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("HEAD inherits the GET Tenant context guard", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodHead, "/v1/tenants/"+fixture.tenantID.String(), nil)
		request.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: fixture.crossTenantToken})
		response := httptest.NewRecorder()
		fixture.handler.ServeHTTP(response, request)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		if response.Code != http.StatusNotFound || body.Error.Code != "tenant_not_found" {
			t.Fatalf("HEAD Tenant context guard = status %d code %q", response.Code, body.Error.Code)
		}
	})
}
