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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestDaemonRunExecutionActiveTurnSuspendWaitsForDurableCheckpointReceipt(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "suspend")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	exitMarker := filepath.Join(t.TempDir(), "provider-stopped")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)
	t.Setenv("PROVIDER_HOST_TEST_EXIT_MARKER", exitMarker)

	executionID := uuid.New()
	controlCommandID := uuid.New()
	tenantID := uuid.New()
	turnID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: uuid.New(),
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	directive := executions.ResourceDirective{
		Action:               "suspend",
		Reason:               resourceSuspendReasonActiveIdleTimeout,
		SuspendAttemptID:     uuid.New(),
		RequestedAt:          time.Now().UTC(),
		CheckpointDeadlineAt: time.Now().UTC().Add(2 * time.Second),
	}
	delivery := executions.ControlCommandDelivery{
		ControlCommandID: controlCommandID,
		Provider:         "codex",
		CommandType:      "SuspendTurn",
		CommandID:        "suspend:" + directive.SuspendAttemptID.String(),
		Payload: map[string]any{
			"turnId": turnID.String(), "suspendAttemptId": directive.SuspendAttemptID.String(),
		},
		DeliveryStatus: "pending", DeliveryAvailableAt: time.Now().UTC(),
	}

	var state struct {
		sync.Mutex
		delivered          bool
		acknowledged       bool
		acknowledgedCursor string
		acknowledgedTarget string
		quiesceBeforeAck   bool
		quiesceBeforeStop  bool
		quiesceCalls       int
		suspendCompleted   bool
		executionCompleted bool
		executionFailed    bool
		released           bool
		aborted            bool
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
		case base + "control-commands/pull":
			state.Lock()
			available := !state.acknowledged
			state.Unlock()
			items := []executions.ControlCommandDelivery{}
			if available {
				items = append(items, delivery)
			}
			writeJSON(response, map[string]any{"items": items})
		case base + "control-commands/" + controlCommandID.String() + "/delivered":
			state.Lock()
			state.delivered = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "control-commands/" + controlCommandID.String() + "/acknowledged":
			var input executions.ControlCommandDeliveryInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid acknowledgement", http.StatusBadRequest)
				return
			}
			quiesced, _ := input.Result["quiesced"].(bool)
			checkpointProtocol, _ := input.Result["checkpointProtocol"].(string)
			targetCommandID, _ := input.Result["targetCommandId"].(string)
			if !quiesced || checkpointProtocol != runnerSuspendCheckpointProtocol ||
				targetCommandID == "" || input.ProviderResumeCursor == nil ||
				*input.ProviderResumeCursor != "cursor-suspended" {
				http.Error(response, "invalid suspend checkpoint receipt", http.StatusBadRequest)
				return
			}
			if _, leaked := input.Result["providerResumeCursor"]; leaked {
				http.Error(response, "cursor leaked into result", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.acknowledged = true
			state.acknowledgedCursor = *input.ProviderResumeCursor
			state.acknowledgedTarget = targetCommandID
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "resource-directives/pull":
			state.Lock()
			eligible := state.acknowledged && !state.suspendCompleted
			state.Unlock()
			var selected *executions.ResourceDirective
			if eligible {
				candidate := directive
				selected = &candidate
			}
			writeJSON(response, map[string]any{"directive": selected})
		case base + "interaction-resolutions/pull":
			writeJSON(response, map[string]any{"items": []any{}})
		case base + "resource-suspend/quiesced":
			var input executions.MarkResourceSuspendQuiescedInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil ||
				input.SuspendAttemptID != directive.SuspendAttemptID {
				http.Error(response, "invalid quiesce receipt", http.StatusBadRequest)
				return
			}
			_, markerErr := os.Stat(exitMarker)
			state.Lock()
			if !state.acknowledged {
				state.quiesceBeforeAck = true
			}
			if markerErr != nil {
				state.quiesceBeforeStop = true
			}
			state.quiesceCalls++
			state.Unlock()
			writeJSON(response, executions.ResourceSuspendQuiesceReceipt{
				SuspendAttemptID: directive.SuspendAttemptID, ProviderQuiescedAt: time.Now().UTC(),
			})
		case base + "resource-suspend/complete":
			var input executions.CompleteResourceSuspendInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil ||
				input.SuspendAttemptID != directive.SuspendAttemptID || input.CheckpointStatus != "unchanged" {
				http.Error(response, "invalid suspend completion", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.suspendCompleted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "complete":
			state.Lock()
			state.executionCompleted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "fail":
			state.Lock()
			state.executionFailed = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "release":
			state.Lock()
			state.released = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "resource-suspend/abort":
			state.Lock()
			state.aborted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	daemon := newProviderHostTestDaemon(t, server.URL, time.Second)
	execution := executions.Execution{ID: executionID, TurnID: turnID, Generation: lease.Generation, Status: "leased"}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: turnID, Provider: "codex", InputText: "suspend this active turn",
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}
	state.Lock()
	defer state.Unlock()
	if !state.delivered || !state.acknowledged || state.acknowledgedCursor != "cursor-suspended" ||
		state.acknowledgedTarget == "" || state.quiesceCalls != 1 || state.quiesceBeforeAck ||
		state.quiesceBeforeStop || !state.suspendCompleted || state.executionCompleted ||
		state.executionFailed || state.released || state.aborted {
		t.Fatalf(
			"active-turn suspend handshake incomplete: delivered=%t acknowledged=%t cursor=%q target=%q quiesceCalls=%d quiesceBeforeAck=%t quiesceBeforeStop=%t suspendCompleted=%t executionCompleted=%t executionFailed=%t released=%t aborted=%t",
			state.delivered,
			state.acknowledged,
			state.acknowledgedCursor,
			state.acknowledgedTarget,
			state.quiesceCalls,
			state.quiesceBeforeAck,
			state.quiesceBeforeStop,
			state.suspendCompleted,
			state.executionCompleted,
			state.executionFailed,
			state.released,
			state.aborted,
		)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nStartSession\nSendTurn\nSuspendTurn\n" {
		t.Fatalf("unexpected active-turn suspend command sequence %q", commands)
	}
}

func TestDaemonRunExecutionActiveTurnSuspendTimeoutFencesGeneration(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "suspend-hang")
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
	directive := executions.ResourceDirective{
		Action:               "suspend",
		Reason:               resourceSuspendReasonActiveIdleTimeout,
		SuspendAttemptID:     uuid.New(),
		RequestedAt:          time.Now().UTC(),
		CheckpointDeadlineAt: time.Now().UTC().Add(2 * time.Second),
	}
	delivery := executions.ControlCommandDelivery{
		ControlCommandID: controlCommandID,
		Provider:         "codex",
		CommandType:      "SuspendTurn",
		CommandID:        "suspend:" + directive.SuspendAttemptID.String(),
		Payload: map[string]any{
			"turnId": turnID.String(), "suspendAttemptId": directive.SuspendAttemptID.String(),
		},
		DeliveryStatus: "pending", DeliveryAvailableAt: time.Now().UTC(),
	}

	var state struct {
		sync.Mutex
		delivered       bool
		acknowledged    bool
		quiesceCalls    int
		released        bool
		aborted         bool
		executionFailed bool
		executionDone   bool
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
		case base + "resource-directives/pull":
			commands, err := os.ReadFile(commandLog)
			if err != nil || !strings.Contains(string(commands), "SendTurn\n") {
				writeJSON(response, map[string]any{"directive": nil})
				return
			}
			candidate := directive
			writeJSON(response, map[string]any{"directive": &candidate})
		case base + "control-commands/pull":
			state.Lock()
			available := !state.delivered
			state.Unlock()
			items := []executions.ControlCommandDelivery{}
			if available {
				items = append(items, delivery)
			}
			writeJSON(response, map[string]any{"items": items})
		case base + "control-commands/" + controlCommandID.String() + "/delivered":
			state.Lock()
			state.delivered = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "control-commands/" + controlCommandID.String() + "/acknowledged":
			state.Lock()
			state.acknowledged = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "interaction-resolutions/pull":
			writeJSON(response, map[string]any{"items": []any{}})
		case base + "resource-suspend/quiesced":
			state.Lock()
			state.quiesceCalls++
			state.Unlock()
			writeProblem(response, http.StatusConflict, "active_suspend_receipt_required", "missing receipt")
		case base + "release":
			var input executions.ReleaseLeaseInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || !input.PreserveInteractionResolutions {
				http.Error(response, "invalid release", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.released = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "resource-suspend/abort":
			state.Lock()
			state.aborted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "complete":
			state.Lock()
			state.executionDone = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "fail":
			state.Lock()
			state.executionFailed = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	daemon := newProviderHostTestDaemon(t, server.URL, time.Second)
	execution := executions.Execution{ID: executionID, TurnID: turnID, Generation: lease.Generation, Status: "leased"}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: turnID, Provider: "codex", InputText: "hang on suspend",
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}
	state.Lock()
	defer state.Unlock()
	if !state.delivered || state.acknowledged || state.quiesceCalls != 1 || !state.released ||
		state.aborted || state.executionDone || state.executionFailed {
		t.Fatalf(
			"active-turn suspend timeout did not fence correctly: delivered=%t acknowledged=%t quiesceCalls=%d released=%t aborted=%t executionDone=%t executionFailed=%t",
			state.delivered,
			state.acknowledged,
			state.quiesceCalls,
			state.released,
			state.aborted,
			state.executionDone,
			state.executionFailed,
		)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nStartSession\nSendTurn\nSuspendTurn\n" {
		t.Fatalf("unexpected suspend-timeout command sequence %q", commands)
	}
}

func TestDaemonRunExecutionActiveTurnSuspendNaturalCompletionWins(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "progress-complete")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	executionID := uuid.New()
	tenantID := uuid.New()
	turnID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: uuid.New(),
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	directive := executions.ResourceDirective{
		Action:               "suspend",
		Reason:               resourceSuspendReasonActiveIdleTimeout,
		SuspendAttemptID:     uuid.New(),
		RequestedAt:          time.Now().UTC(),
		CheckpointDeadlineAt: time.Now().UTC().Add(2 * time.Second),
	}

	var state struct {
		sync.Mutex
		executionCompleted bool
		executionFailed    bool
		quiesceCalls       int
		suspendCompleted   bool
		released           bool
		aborted            bool
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
		case base + "resource-directives/pull":
			candidate := directive
			writeJSON(response, map[string]any{"directive": &candidate})
		case base + "control-commands/pull", base + "interaction-resolutions/pull":
			writeJSON(response, map[string]any{"items": []any{}})
		case base + "complete":
			state.Lock()
			state.executionCompleted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "fail":
			state.Lock()
			state.executionFailed = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "resource-suspend/quiesced":
			state.Lock()
			state.quiesceCalls++
			state.Unlock()
			writeJSON(response, executions.ResourceSuspendQuiesceReceipt{
				SuspendAttemptID: directive.SuspendAttemptID, ProviderQuiescedAt: time.Now().UTC(),
			})
		case base + "resource-suspend/complete":
			state.Lock()
			state.suspendCompleted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "release":
			state.Lock()
			state.released = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "resource-suspend/abort":
			state.Lock()
			state.aborted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	daemon := newProviderHostTestDaemon(t, server.URL, time.Second)
	execution := executions.Execution{ID: executionID, TurnID: turnID, Generation: lease.Generation, Status: "leased"}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: turnID, Provider: "codex", InputText: "finish naturally",
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}
	state.Lock()
	defer state.Unlock()
	if !state.executionCompleted || state.executionFailed || state.quiesceCalls != 0 ||
		state.suspendCompleted || state.released || state.aborted {
		t.Fatalf(
			"natural completion did not win active suspend race: executionCompleted=%t executionFailed=%t quiesceCalls=%d suspendCompleted=%t released=%t aborted=%t",
			state.executionCompleted,
			state.executionFailed,
			state.quiesceCalls,
			state.suspendCompleted,
			state.released,
			state.aborted,
		)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nStartSession\nSendTurn\n" {
		t.Fatalf("unexpected natural-completion command sequence %q", commands)
	}
}

func TestDaemonRunExecutionWaitingSuspendStillCancelsImmediately(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "interrupt")
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
	directive := executions.ResourceDirective{
		Action:               "suspend",
		Reason:               "waiting-keepalive",
		SuspendAttemptID:     uuid.New(),
		RequestedAt:          time.Now().UTC(),
		CheckpointDeadlineAt: time.Now().UTC().Add(2 * time.Second),
	}
	delivery := executions.ControlCommandDelivery{
		ControlCommandID: controlCommandID,
		Provider:         "codex",
		CommandType:      "SuspendTurn",
		CommandID:        "suspend:" + directive.SuspendAttemptID.String(),
		Payload: map[string]any{
			"turnId": turnID.String(), "suspendAttemptId": directive.SuspendAttemptID.String(),
		},
		DeliveryStatus: "pending", DeliveryAvailableAt: time.Now().UTC(),
	}

	var state struct {
		sync.Mutex
		delivered         bool
		acknowledged      bool
		quiesceCalls      int
		suspendCompleted  bool
		executionFailed   bool
		executionDone     bool
		controlPulls      int
		controlPullCancel bool
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
		case base + "resource-directives/pull":
			candidate := directive
			writeJSON(response, map[string]any{"directive": &candidate})
		case base + "control-commands/pull":
			state.Lock()
			state.controlPulls++
			state.Unlock()
			select {
			case <-request.Context().Done():
				state.Lock()
				state.controlPullCancel = true
				state.Unlock()
				return
			case <-time.After(200 * time.Millisecond):
			}
			writeJSON(response, map[string]any{"items": []executions.ControlCommandDelivery{delivery}})
		case base + "control-commands/" + controlCommandID.String() + "/delivered":
			state.Lock()
			state.delivered = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "control-commands/" + controlCommandID.String() + "/acknowledged":
			state.Lock()
			state.acknowledged = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "interaction-resolutions/pull":
			writeJSON(response, map[string]any{"items": []any{}})
		case base + "resource-suspend/quiesced":
			state.Lock()
			state.quiesceCalls++
			state.Unlock()
			writeJSON(response, executions.ResourceSuspendQuiesceReceipt{
				SuspendAttemptID: directive.SuspendAttemptID, ProviderQuiescedAt: time.Now().UTC(),
			})
		case base + "resource-suspend/complete":
			state.Lock()
			state.suspendCompleted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "complete":
			state.Lock()
			state.executionDone = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "fail":
			state.Lock()
			state.executionFailed = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	daemon := newProviderHostTestDaemon(t, server.URL, time.Second)
	execution := executions.Execution{ID: executionID, TurnID: turnID, Generation: lease.Generation, Status: "leased"}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: turnID, Provider: "codex", InputText: "waiting suspend",
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}
	state.Lock()
	defer state.Unlock()
	if state.delivered || state.acknowledged || state.quiesceCalls != 1 || !state.suspendCompleted ||
		state.executionDone || state.executionFailed {
		t.Fatalf(
			"waiting suspend path changed unexpectedly: delivered=%t acknowledged=%t quiesceCalls=%d suspendCompleted=%t executionDone=%t executionFailed=%t",
			state.delivered,
			state.acknowledged,
			state.quiesceCalls,
			state.suspendCompleted,
			state.executionDone,
			state.executionFailed,
		)
	}
	commands, err := os.ReadFile(commandLog)
	if err == nil {
		if strings.Contains(string(commands), "SuspendTurn\n") {
			t.Fatalf("unexpected waiting-suspend command sequence %q", commands)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func newProviderHostTestDaemon(t *testing.T, serverURL string, requestTimeout time.Duration) *Daemon {
	t.Helper()
	controlPlaneURL, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ControlPlaneURL:       controlPlaneURL,
		TargetKind:            platform.TargetKubernetes,
		RunnerProtocol:        RunnerProtocolV2,
		WorkspaceRoot:         t.TempDir(),
		PollInterval:          time.Millisecond,
		HeartbeatInterval:     time.Hour,
		LeaseRenewInterval:    time.Hour,
		RequestTimeout:        requestTimeout,
		ArtifactTimeout:       time.Second,
		RunnerMessageBytes:    1 << 20,
		ExperimentalProviders: []string{"codex"},
	}
	cfg.RunnerCommand = providerHostV2TestCommand()
	client := NewClient(cfg)
	client.workerToken = "worker-token"
	return &Daemon{
		config: cfg,
		client: client,
		runner: NewRunner(cfg),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}

func writeProblem(response http.ResponseWriter, status int, code, message string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]any{
		"error": map[string]any{
			"code": code, "message": message,
		},
	})
}
