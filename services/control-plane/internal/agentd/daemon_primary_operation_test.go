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
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestDaemonAcknowledgesPrimaryOperationOnlyAfterProviderTerminalAndSkipsComplete(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "primary-operation")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	executionID := uuid.New()
	controlCommandID := uuid.New()
	tenantID := uuid.New()
	turnID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: uuid.New(),
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	commandID := "compact:" + controlCommandID.String()

	var state struct {
		sync.Mutex
		order           []string
		acknowledgement executions.ControlCommandDeliveryInput
		completed       bool
		failed          bool
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
		case base + "control-commands/pull", base + "interaction-resolutions/pull":
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{"items": []any{}})
		case base + "control-commands/" + controlCommandID.String() + "/delivered":
			commands, err := os.ReadFile(commandLog)
			if err != nil || string(commands) != "Describe\nStartSession\n" {
				http.Error(response, "primary command was written before durable delivery", http.StatusConflict)
				return
			}
			state.Lock()
			state.order = append(state.order, "delivered-before-write")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "control-commands/" + controlCommandID.String() + "/acknowledged":
			var input executions.ControlCommandDeliveryInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid acknowledgement", http.StatusBadRequest)
				return
			}
			commands, err := os.ReadFile(commandLog)
			output, _ := input.Result["output"].(map[string]any)
			if err != nil || string(commands) != "Describe\nStartSession\nCompactSession\n" ||
				input.CommandID != commandID || input.ProviderResumeCursor == nil ||
				*input.ProviderResumeCursor != "cursor-compacted" || input.Result["supportMode"] != "native" ||
				output["operation"] != "compact" {
				http.Error(response, "acknowledgement arrived before the Provider terminal Result", http.StatusConflict)
				return
			}
			if _, leaked := input.Result["providerResumeCursor"]; leaked {
				http.Error(response, "Provider Cursor leaked into the ordinary operation Result", http.StatusConflict)
				return
			}
			state.Lock()
			state.order = append(state.order, "terminal-before-ack")
			state.acknowledgement = input
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
		ExperimentalProviders: []string{"codex"}, RunnerCommand: providerHostV2TestCommand(),
	}
	client := NewClient(cfg)
	client.workerToken = "worker-token"
	daemon := &Daemon{
		config: cfg, client: client, runner: NewRunner(cfg),
		workspace: workspaceMaterializerFunc(func(
			context.Context, executions.Execution, executions.Workload, *WorkspaceGitCredential,
		) (WorkspaceMaterialization, error) {
			return WorkspaceMaterialization{Directory: t.TempDir()}, nil
		}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	execution := executions.Execution{
		ID: executionID, TenantID: tenantID, SessionID: uuid.New(), TurnID: turnID,
		Generation: lease.Generation, Status: "leased",
	}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: execution.SessionID,
		TurnID: turnID, Provider: "codex", TurnKind: "compact", RuntimeMode: "full-access",
		InteractionMode: "default", DefaultBranch: "main",
		PrimaryOperation: &executions.PrimaryOperation{
			ControlCommandID: controlCommandID, Provider: "codex", CommandType: "CompactSession",
			CommandID: commandID, Payload: map[string]any{"turnId": turnID.String()},
		},
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}

	state.Lock()
	defer state.Unlock()
	if len(state.order) != 2 || state.order[0] != "delivered-before-write" || state.order[1] != "terminal-before-ack" ||
		state.acknowledgement.CommandID != commandID || state.completed || state.failed {
		t.Fatalf(
			"primary operation lifecycle order=%#v acknowledgement=%#v completed=%t failed=%t",
			state.order, state.acknowledgement, state.completed, state.failed,
		)
	}
}
