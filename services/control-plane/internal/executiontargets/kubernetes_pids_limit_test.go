package executiontargets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestKubernetesHTTPClientAttestsEveryEligibleNodePodPIDsLimit(t *testing.T) {
	seenSelector := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(response, "missing bearer token", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v1/nodes":
			seenSelector = request.URL.Query().Get("labelSelector")
			_ = json.NewEncoder(response).Encode(map[string]any{"items": []any{
				map[string]any{"metadata": map[string]any{"name": "worker-b"}},
				map[string]any{"metadata": map[string]any{"name": "worker-a"}},
			}})
		case "/api/v1/nodes/worker-a/proxy/configz":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"kubeletconfig": map[string]any{"podPidsLimit": 256},
			})
		case "/api/v1/nodes/worker-b/proxy/configz":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"kubeletconfig": map[string]any{"podPidsLimit": 512},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
	if err := client.AttestPodPIDsLimit(
		context.Background(),
		map[string]string{"synara.io/pool": "provider", "kubernetes.io/os": "linux"},
		512,
	); err != nil {
		t.Fatal(err)
	}
	if seenSelector != "kubernetes.io/os=linux,synara.io/pool=provider" {
		t.Fatalf("node label selector = %q", seenSelector)
	}
}

func TestKubernetesHTTPClientRejectsUnboundedOrExcessivePodPIDsLimit(t *testing.T) {
	for _, limit := range []int64{-1, 0, 513} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v1/nodes":
					_ = json.NewEncoder(response).Encode(map[string]any{"items": []any{
						map[string]any{"metadata": map[string]any{"name": "worker-a"}},
					}})
				case "/api/v1/nodes/worker-a/proxy/configz":
					_ = json.NewEncoder(response).Encode(map[string]any{
						"kubeletconfig": map[string]any{"podPidsLimit": limit},
					})
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)
			client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
			err := client.AttestPodPIDsLimit(context.Background(), nil, 512)
			if err == nil || !strings.Contains(err.Error(), "not within") {
				t.Fatalf("podPidsLimit %d was accepted: %v", limit, err)
			}
		})
	}
}

func TestKubernetesHTTPClientRejectsMissingEligiblePIDAttestationNodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{"items": []any{}})
	}))
	t.Cleanup(server.Close)
	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
	if err := client.AttestPodPIDsLimit(context.Background(), nil, 512); err == nil ||
		!strings.Contains(err.Error(), "no eligible nodes") {
		t.Fatalf("empty eligible-node set was accepted: %v", err)
	}
}
