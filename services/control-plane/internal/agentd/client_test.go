package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestClientAdvertisesWorkerProtocolV2OnRegisterAndHeartbeat(t *testing.T) {
	registerInputs := make(chan executions.RegisterWorkerInput, 1)
	heartbeatInputs := make(chan executions.HeartbeatInput, 1)

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/workers/register":
			var registerInput executions.RegisterWorkerInput
			if err := json.NewDecoder(request.Body).Decode(&registerInput); err != nil {
				t.Errorf("decode register input: %v", err)
				http.Error(response, "invalid register input", http.StatusBadRequest)
				return
			}
			registerInputs <- registerInput
			response.Header().Set(artifacts.WorkerIdempotencyFeatureHeader, artifacts.WorkerIdempotencyFeatureHeaderValue)
			_ = json.NewEncoder(response).Encode(executions.RegisteredWorker{Token: "worker-token"})
		case "/v1/workers/heartbeat":
			if request.Header.Get("Authorization") != "Bearer worker-token" {
				t.Errorf("heartbeat used unexpected authorization header %q", request.Header.Get("Authorization"))
			}
			var heartbeatInput executions.HeartbeatInput
			if err := json.NewDecoder(request.Body).Decode(&heartbeatInput); err != nil {
				t.Errorf("decode heartbeat input: %v", err)
				http.Error(response, "invalid heartbeat input", http.StatusBadRequest)
				return
			}
			heartbeatInputs <- heartbeatInput
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected request path", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ControlPlaneURL:   controlPlaneURL,
		RegistrationToken: "registration-token",
		ExecutionTargetID: uuid.New(),
		TargetKind:        platform.TargetLocal,
		ClusterID:         "test-cluster",
		Namespace:         "test-namespace",
		PodName:           "test-worker",
		InstanceUID:       uuid.NewString(),
		Version:           "test-version",
		Capabilities:      map[string]any{"workspace": true},
		RequestTimeout:    time.Second,
		ArtifactTimeout:   time.Second,
	}
	client := NewClient(cfg)
	if _, err := client.Register(context.Background(), cfg); err != nil {
		t.Fatalf("register Worker: %v", err)
	}
	if err := client.Heartbeat(context.Background(), cfg, false); err != nil {
		t.Fatalf("heartbeat Worker: %v", err)
	}
	if !client.artifactIdempotencySupported {
		t.Fatal("Client did not negotiate the Worker Artifact idempotency feature")
	}

	registerInput := <-registerInputs
	heartbeatInput := <-heartbeatInputs
	if registerInput.ProtocolVersion != 2 {
		t.Fatalf("register protocolVersion = %d, want Worker Protocol v2", registerInput.ProtocolVersion)
	}
	if heartbeatInput.ProtocolVersion != 2 {
		t.Fatalf("heartbeat protocolVersion = %d, want Worker Protocol v2", heartbeatInput.ProtocolVersion)
	}
}

func TestClientRegisterTreatsMissingArtifactIdempotencyHeaderAsLegacy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(executions.RegisteredWorker{Token: "worker-token"})
	}))
	t.Cleanup(server.Close)
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ControlPlaneURL: controlPlaneURL, RegistrationToken: "registration-token",
		ExecutionTargetID: uuid.New(), TargetKind: platform.TargetLocal, InstanceUID: uuid.NewString(),
		RequestTimeout: time.Second, ArtifactTimeout: time.Second,
	}
	client := NewClient(cfg)
	if _, err := client.Register(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if client.artifactIdempotencySupported {
		t.Fatal("Client enabled Artifact retries against a legacy Control Plane")
	}
}

func TestClientAppendEventCarriesNegotiatedVersionAndDefaultsLegacyToV1(t *testing.T) {
	versions := make(chan int, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var input executions.RuntimeEventInput
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Errorf("decode Runtime Event: %v", err)
			http.Error(response, "invalid Runtime Event", http.StatusBadRequest)
			return
		}
		versions <- input.EventVersion
		response.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: controlPlaneURL, RequestTimeout: time.Second, ArtifactTimeout: time.Second})
	client.workerToken = "worker-token"
	lease := executions.Lease{TenantID: uuid.New(), Generation: 1, LeaseToken: "lease-token"}
	executionID := uuid.New()

	if err := client.AppendEvent(context.Background(), executionID, lease, RunnerMessage{
		Type: "event", EventVersion: executions.RuntimeEventVersionV2, EventType: "content.delta",
		Payload: map[string]any{"streamKind": "assistant_text", "delta": "hello"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.AppendEvent(context.Background(), executionID, lease, RunnerMessage{
		Type: "event", EventType: "runtime.output.delta", Payload: map[string]any{"text": "legacy"},
	}); err != nil {
		t.Fatal(err)
	}
	if first, second := <-versions, <-versions; first != executions.RuntimeEventVersionV2 || second != executions.RuntimeEventVersionV1 {
		t.Fatalf("Runtime Event versions = %d, %d; want 2, 1", first, second)
	}
	if err := client.AppendEvent(context.Background(), executionID, lease, RunnerMessage{
		Type: "event", EventVersion: 3, EventType: "future.event", Payload: map[string]any{},
	}); err == nil {
		t.Fatal("Client accepted an unsupported Runtime Event version")
	}
}
