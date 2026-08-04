package httpapi

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type openAPIContract struct {
	OpenAPI       string                          `yaml:"openapi"`
	Paths         map[string]map[string]operation `yaml:"paths"`
	RouteSurfaces []routeSurface                  `yaml:"x-synara-route-surfaces"`
}

type operation struct {
	OperationID     string               `yaml:"operationId"`
	Exposure        apiExposure          `yaml:"x-synara-exposure"`
	ContractStatus  string               `yaml:"x-synara-contract-status"`
	Responses       map[string]yaml.Node `yaml:"responses"`
	RequestBody     yaml.Node            `yaml:"requestBody"`
	RequestBodyMode string               `yaml:"x-synara-request-body"`
}

type routeSurface struct {
	Method   string      `yaml:"method"`
	Path     string      `yaml:"path"`
	Exposure apiExposure `yaml:"x-synara-exposure"`
}

func TestOpenAPIRouteSurfacesMatchEveryRegisteredRoute(t *testing.T) {
	currentDirectory := httpAPITestDirectory(t)
	specificationBytes, err := os.ReadFile(openAPIContractPath(currentDirectory))
	if err != nil {
		t.Fatal(err)
	}
	var specification openAPIContract
	decoder := yaml.NewDecoder(bytes.NewReader(specificationBytes))
	if err := decoder.Decode(&specification); err != nil {
		t.Fatalf("decode OpenAPI contract: %v", err)
	}
	if specification.OpenAPI != "3.1.0" {
		t.Fatalf("OpenAPI version = %q, want 3.1.0", specification.OpenAPI)
	}

	serverSource, err := os.ReadFile(filepath.Join(currentDirectory, "server.go"))
	if err != nil {
		t.Fatal(err)
	}
	registrations := classifiedRouteRegistrationPattern.FindAllStringSubmatch(string(serverSource), -1)
	declared := make(map[string]apiExposure, len(registrations))
	for _, registration := range registrations {
		key := registration[2] + " " + registration[3]
		declared[key] = exposureFromRegistration(t, registration[1])
	}

	documented := make(map[string]apiExposure, len(specification.RouteSurfaces))
	for _, route := range specification.RouteSurfaces {
		key := route.Method + " " + route.Path
		if _, duplicate := documented[key]; duplicate {
			t.Fatalf("OpenAPI route surface %q is duplicated", key)
		}
		if !validAPIExposure(route.Exposure) {
			t.Fatalf("OpenAPI route surface %q has invalid exposure %q", key, route.Exposure)
		}
		documented[key] = route.Exposure
	}
	if len(documented) != len(declared) {
		t.Fatalf("OpenAPI route surfaces = %d, registered routes = %d", len(documented), len(declared))
	}
	for key, exposure := range declared {
		if documentedExposure, ok := documented[key]; !ok {
			t.Fatalf("registered route %q is absent from OpenAPI x-synara-route-surfaces", key)
		} else if documentedExposure != exposure {
			t.Fatalf("route %q exposure = %q in code, %q in OpenAPI", key, exposure, documentedExposure)
		}
	}
}

func TestEveryPublicRouteHasOneOpenAPIOperation(t *testing.T) {
	currentDirectory := httpAPITestDirectory(t)
	specificationBytes, err := os.ReadFile(openAPIContractPath(currentDirectory))
	if err != nil {
		t.Fatal(err)
	}
	var specification openAPIContract
	if err := yaml.Unmarshal(specificationBytes, &specification); err != nil {
		t.Fatal(err)
	}

	publicRoutes := make(map[string]apiExposure)
	for _, route := range specification.RouteSurfaces {
		if route.Exposure != apiExposureInternal {
			publicRoutes[strings.ToLower(route.Method)+" "+route.Path] = route.Exposure
		}
	}
	operationIDs := make(map[string]string)
	codegenReady := make(map[string]struct{})
	found := 0
	for path, pathItem := range specification.Paths {
		for method, item := range pathItem {
			key := strings.ToLower(method) + " " + path
			exposure, public := publicRoutes[key]
			if !public {
				t.Fatalf("OpenAPI paths exposes undeclared or internal operation %q", key)
			}
			if item.Exposure != exposure {
				t.Fatalf("OpenAPI operation %q exposure = %q, want %q", key, item.Exposure, exposure)
			}
			if item.ContractStatus != "route-only" && item.ContractStatus != "codegen-ready" {
				t.Fatalf("OpenAPI operation %q has unsupported contract status %q", key, item.ContractStatus)
			}
			if item.ContractStatus == "codegen-ready" {
				if !hasSuccessResponse(item.Responses) {
					t.Fatalf("codegen-ready OpenAPI operation %q has no explicit success response", key)
				}
				if item.RequestBodyMode != "" && item.RequestBodyMode != "none" {
					t.Fatalf("codegen-ready operation %q has invalid x-synara-request-body %q", key, item.RequestBodyMode)
				}
				if !strings.EqualFold(method, http.MethodGet) && item.RequestBody.Kind == 0 && item.RequestBodyMode != "none" {
					t.Fatalf("codegen-ready mutation %q has no request body schema", key)
				}
				codegenReady[item.OperationID] = struct{}{}
			}
			if strings.TrimSpace(item.OperationID) == "" {
				t.Fatalf("OpenAPI operation %q has no operationId", key)
			}
			if previous, duplicate := operationIDs[item.OperationID]; duplicate {
				t.Fatalf("OpenAPI operationId %q is shared by %q and %q", item.OperationID, previous, key)
			}
			operationIDs[item.OperationID] = key
			if _, ok := item.Responses["default"]; !ok {
				t.Fatalf("OpenAPI operation %q does not reference the stable error envelope", key)
			}
			found++
		}
	}
	if found != len(publicRoutes) {
		t.Fatalf("OpenAPI operations = %d, public route surfaces = %d", found, len(publicRoutes))
	}
	for _, operationID := range []string{
		"createSession", "createTurn", "listSessionEvents", "streamSessionEvents", "resolveExecutionApproval",
	} {
		if _, ok := codegenReady[operationID]; !ok {
			t.Fatalf("vertical SDK operation %q is not codegen-ready", operationID)
		}
	}
}

func hasSuccessResponse(responses map[string]yaml.Node) bool {
	for status := range responses {
		if len(status) == 3 && status[0] == '2' {
			return true
		}
	}
	return false
}

func httpAPITestDirectory(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate OpenAPI conformance test")
	}
	return filepath.Dir(currentFile)
}

func openAPIContractPath(httpAPIDirectory string) string {
	return filepath.Join(httpAPIDirectory, "..", "..", "..", "..", "docs", "api", "openapi.yaml")
}

func exposureFromRegistration(t *testing.T, registration string) apiExposure {
	t.Helper()
	switch registration {
	case "Internal":
		return apiExposureInternal
	case "PublicBeta":
		return apiExposurePublicBeta
	case "PublicGA":
		return apiExposurePublicGA
	default:
		t.Fatalf("unknown route registration tier %q", registration)
		return ""
	}
}

func validAPIExposure(exposure apiExposure) bool {
	return exposure == apiExposureInternal || exposure == apiExposurePublicBeta || exposure == apiExposurePublicGA
}
