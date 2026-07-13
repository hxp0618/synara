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

	registerInput := <-registerInputs
	heartbeatInput := <-heartbeatInputs
	if registerInput.ProtocolVersion != 2 {
		t.Fatalf("register protocolVersion = %d, want Worker Protocol v2", registerInput.ProtocolVersion)
	}
	if heartbeatInput.ProtocolVersion != 2 {
		t.Fatalf("heartbeat protocolVersion = %d, want Worker Protocol v2", heartbeatInput.ProtocolVersion)
	}
}
