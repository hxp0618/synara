package agentd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func newControlUpdateTestDaemon(t *testing.T, serverURL string) *Daemon {
	t.Helper()
	controlPlaneURL, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ControlPlaneURL: controlPlaneURL,
		RequestTimeout:  5 * time.Second,
	}
	return &Daemon{
		config: cfg,
		client: NewClient(cfg),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func controlUpdateTestLease() executions.Lease {
	return executions.Lease{TenantID: uuid.New(), Generation: 3, LeaseToken: "lease-token"}
}

// When the Control Plane exposes the combined pull, one request must serve
// both delivery kinds and the legacy endpoints must not be touched.
func TestPullRunnerControlUpdatesUsesCombinedEndpoint(t *testing.T) {
	executionID := uuid.New()
	commandID := uuid.New()
	interactionID := uuid.New()

	var state struct {
		sync.Mutex
		combined int
		legacy   int
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch r.URL.Path {
		case base + "control-updates/pull":
			state.Lock()
			state.combined++
			state.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(executions.ControlUpdates{
				ControlCommands: []executions.ControlCommandDelivery{{
					ControlCommandID: commandID, CommandType: "Interrupt", CommandID: "interrupt:1", Provider: "codex",
				}},
				InteractionResolutions: []executions.InteractionResolutionDelivery{{
					InteractionID: interactionID, CommandType: "ResolveApproval",
					CommandID: "approval:1", ResolutionKind: "approved",
				}},
			})
		case base + "control-commands/pull", base + "interaction-resolutions/pull":
			state.Lock()
			state.legacy++
			state.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	daemon := newControlUpdateTestDaemon(t, server.URL)
	legacyPulls := false
	commands, resolutions, err := daemon.pullRunnerControlUpdates(
		context.Background(), executionID, controlUpdateTestLease(), &legacyPulls,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].CommandID != "interrupt:1" {
		t.Fatalf("combined pull did not surface the Control command: %#v", commands)
	}
	if len(resolutions) != 1 || resolutions[0].CommandID != "approval:1" {
		t.Fatalf("combined pull did not surface the Interaction resolution: %#v", resolutions)
	}
	if legacyPulls {
		t.Fatal("a working combined endpoint switched the loop to legacy pulls")
	}
	state.Lock()
	defer state.Unlock()
	if state.combined != 1 {
		t.Fatalf("combined endpoint requests = %d, want 1", state.combined)
	}
	if state.legacy != 0 {
		t.Fatalf("legacy endpoints were called %d times despite a working combined pull", state.legacy)
	}
}

// A Control Plane predating the endpoint answers a bare 404. The loop must
// fall back once and stay on the legacy pulls without retrying the combined
// endpoint every cycle.
func TestPullRunnerControlUpdatesFallsBackAndLatches(t *testing.T) {
	executionID := uuid.New()

	var state struct {
		sync.Mutex
		combined int
		commands int
		items    int
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch r.URL.Path {
		case base + "control-commands/pull":
			state.Lock()
			state.commands++
			state.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case base + "interaction-resolutions/pull":
			state.Lock()
			state.items++
			state.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		default:
			// Includes control-updates/pull on an older Control Plane.
			state.Lock()
			if r.URL.Path == base+"control-updates/pull" {
				state.combined++
			}
			state.Unlock()
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	daemon := newControlUpdateTestDaemon(t, server.URL)
	legacyPulls := false
	for cycle := 0; cycle < 3; cycle++ {
		if _, _, err := daemon.pullRunnerControlUpdates(
			context.Background(), executionID, controlUpdateTestLease(), &legacyPulls,
		); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	if !legacyPulls {
		t.Fatal("an unsupported combined endpoint did not latch the legacy fallback")
	}
	state.Lock()
	defer state.Unlock()
	if state.combined != 1 {
		t.Fatalf("combined endpoint was probed %d times, want 1 before latching", state.combined)
	}
	if state.commands != 3 || state.items != 3 {
		t.Fatalf("legacy pulls = (%d commands, %d resolutions), want 3 each", state.commands, state.items)
	}
}

// A fencing or authorization rejection is a real answer. It must surface as an
// error rather than silently downgrading the runner onto the legacy path.
func TestPullRunnerControlUpdatesDoesNotFallBackOnRejection(t *testing.T) {
	executionID := uuid.New()
	legacyCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch r.URL.Path {
		case base + "control-updates/pull":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "generation_fenced", "message": "superseded"},
			})
		default:
			legacyCalled = true
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	daemon := newControlUpdateTestDaemon(t, server.URL)
	legacyPulls := false
	if _, _, err := daemon.pullRunnerControlUpdates(
		context.Background(), executionID, controlUpdateTestLease(), &legacyPulls,
	); err == nil {
		t.Fatal("a fenced combined pull did not surface an error")
	}
	if legacyPulls || legacyCalled {
		t.Fatal("a fencing rejection was mistaken for an unsupported endpoint")
	}
}

// A handled 404 carries a problem envelope and is a real answer, unlike the
// bare 404 produced by an unrouted path.
func TestControlUpdatesUnsupportedDistinguishesHandledNotFound(t *testing.T) {
	if errControlUpdatesUnsupported(&controlPlaneProblem{
		Status: http.StatusNotFound, Code: "execution_not_found", Message: "Execution not found.",
	}) {
		t.Fatal("execution_not_found was treated as an unsupported endpoint")
	}
	if !errControlUpdatesUnsupported(&controlPlaneProblem{Status: http.StatusNotFound}) {
		t.Fatal("a bare 404 was not treated as an unsupported endpoint")
	}
	if errControlUpdatesUnsupported(&controlPlaneProblem{Status: http.StatusConflict}) {
		t.Fatal("a non-404 status was treated as an unsupported endpoint")
	}
}
