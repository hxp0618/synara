package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var classifiedRouteRegistrationPattern = regexp.MustCompile(
	`(?m)^\s*routes\.(Internal|PublicBeta|PublicGA)(?:Func)?\("([A-Z]+) ([^"]+)",`,
)

func TestEveryServerRouteDeclaresItsAPISurface(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate API route guard source")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "server.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if strings.Contains(text, "http.NewServeMux()") || regexp.MustCompile(`(?m)^\s*(?:mux|routes\.mux)\.(?:Handle|HandleFunc)\(`).MatchString(text) {
		t.Fatal("server routes must be registered through classifiedServeMux")
	}

	matches := classifiedRouteRegistrationPattern.FindAllStringSubmatch(text, -1)
	if len(matches) < 300 {
		t.Fatalf("classified route inventory = %d, want at least 300", len(matches))
	}
	publicDeveloperAuth := regexp.MustCompile(
		`(?m)^\s*routes\.PublicBeta(?:Func)?\("[A-Z]+ [^"]+", server\.requireDeveloperAuth\(`,
	).FindAllString(text, -1)

	seen := make(map[string]string, len(matches))
	publicBeta := make(map[string]struct{})
	for _, match := range matches {
		exposure, method, path := match[1], match[2], match[3]
		key := method + " " + path
		if previous, exists := seen[key]; exists {
			t.Fatalf("route %q is classified twice as %s and %s", key, previous, exposure)
		}
		seen[key] = exposure
		if exposure == "PublicBeta" {
			publicBeta[key] = struct{}{}
		}
		if exposure != "Internal" && isPermanentlyInternalDeveloperRoute(path) {
			t.Fatalf("permanently internal route %q was classified as %s", key, exposure)
		}
	}

	for _, required := range []string{
		"POST /v1/projects/{projectID}/sessions",
		"POST /v1/sessions/{sessionID}/turns",
		"GET /v1/sessions/{sessionID}/events/stream",
		"GET /v1/executions/{executionID}/interactions",
		"POST /v1/executions/{executionID}/approvals/{requestID}/resolve",
	} {
		if _, ok := publicBeta[required]; !ok {
			t.Fatalf("developer quickstart route %q is not public-beta", required)
		}
	}
	if strings.Contains(text, "routes.PublicGA(") || strings.Contains(text, "routes.PublicGAFunc(") {
		t.Fatal("Stage 7 routes must not be promoted to public-ga before the GA gates pass")
	}
	if len(publicDeveloperAuth) != len(publicBeta) {
		t.Fatalf("public-beta routes using developer authentication = %d, public-beta inventory = %d", len(publicDeveloperAuth), len(publicBeta))
	}
}

func TestClassifiedServeMuxRecordsAndServesRoute(t *testing.T) {
	routes := newClassifiedServeMux()
	routes.PublicBetaFunc("GET /v1/example/{exampleID}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("exampleID") != "example-1" {
			t.Fatalf("path value = %q", r.PathValue("exampleID"))
		}
		w.WriteHeader(http.StatusNoContent)
	})

	manifest := routes.manifest()
	if len(manifest) != 1 || manifest[0] != (apiRoute{Method: http.MethodGet, Path: "/v1/example/{exampleID}", Exposure: apiExposurePublicBeta}) {
		t.Fatalf("route manifest = %#v", manifest)
	}

	request, err := http.NewRequest(http.MethodGet, "/v1/example/example-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := &statusRecorder{header: make(http.Header)}
	routes.ServeHTTP(response, request)
	if response.status != http.StatusNoContent {
		t.Fatalf("route status = %d", response.status)
	}
}

func isPermanentlyInternalDeveloperRoute(path string) bool {
	return strings.HasPrefix(path, "/v1/workers/") ||
		strings.HasPrefix(path, "/v1/platform/") ||
		path == "/v1/auth/dev-login" ||
		strings.HasPrefix(path, "/v1/artifact-content/") ||
		strings.HasPrefix(path, "/scim/")
}

type statusRecorder struct {
	header http.Header
	status int
}

func (r *statusRecorder) Header() http.Header { return r.header }

func (r *statusRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return len(body), nil
}

func (r *statusRecorder) WriteHeader(status int) { r.status = status }
