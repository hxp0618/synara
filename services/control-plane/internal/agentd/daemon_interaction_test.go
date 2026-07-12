package agentd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestDaemonRunExecutionDeliversInteractionResolutionEndToEnd(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "interaction")

	executionID := uuid.New()
	interactionID := uuid.New()
	tenantID := uuid.New()
	turnID := uuid.New()
	workerID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	delivery := executions.InteractionResolutionDelivery{
		InteractionID: interactionID, RequestID: "approval-1", Provider: "codex",
		CommandType: "ResolveApproval", CommandID: "approval-1:resolution",
		ResolutionKind: "approved", Resolution: map[string]any{"decision": "accept"},
		DeliveryStatus: "pending", DeliveryAvailableAt: time.Now().UTC(),
	}

	var state struct {
		sync.Mutex
		interactionRequested bool
		delivered            bool
		acknowledged         bool
		completed            bool
		failed               bool
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer worker-token" {
			http.Error(response, "missing Worker token", http.StatusUnauthorized)
			return
		}
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch request.URL.Path {
		case base + "start":
			response.WriteHeader(http.StatusNoContent)
		case base + "events":
			var input executions.RuntimeEventInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil ||
				input.EventType != "approval.requested" || input.Payload["requestId"] != "approval-1" {
				http.Error(response, "invalid interaction event", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.interactionRequested = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "interaction-resolutions/pull":
			state.Lock()
			available := state.interactionRequested && !state.acknowledged
			state.Unlock()
			items := []executions.InteractionResolutionDelivery{}
			if available {
				items = append(items, delivery)
			}
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{"items": items})
		case base + "interaction-resolutions/" + interactionID.String() + "/delivered":
			state.Lock()
			state.delivered = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "interaction-resolutions/" + interactionID.String() + "/acknowledged":
			state.Lock()
			state.acknowledged = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "complete":
			state.Lock()
			state.completed = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "fail":
			state.Lock()
			state.failed = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ControlPlaneURL: controlPlaneURL, TargetKind: platform.TargetLocal,
		RunnerProtocol: RunnerProtocolV2, WorkspaceRoot: t.TempDir(), PollInterval: time.Millisecond,
		HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour, RequestTimeout: time.Second,
		ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
	// The helper is the current Go test binary. Passing -test.run keeps the child
	// isolated to TestProviderHostV2HelperProcess.
	cfg.RunnerCommand = []string{os.Args[0], "-test.run=TestProviderHostV2HelperProcess", "--"}
	client := NewClient(cfg)
	client.workerToken = "worker-token"
	daemon := &Daemon{
		config: cfg, client: client, runner: NewRunner(cfg),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	execution := executions.Execution{ID: executionID, TurnID: turnID, Generation: lease.Generation, Status: "leased"}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: turnID, Provider: "codex", InputText: "run an approval-gated command",
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}
	state.Lock()
	defer state.Unlock()
	if !state.interactionRequested || !state.delivered || !state.acknowledged || !state.completed || state.failed {
		t.Fatalf("incomplete interaction lifecycle: requested=%t delivered=%t acknowledged=%t completed=%t failed=%t",
			state.interactionRequested, state.delivered, state.acknowledged, state.completed, state.failed)
	}
}
