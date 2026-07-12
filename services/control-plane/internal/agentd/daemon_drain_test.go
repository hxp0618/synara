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

func TestDaemonDrainCompletesActiveExecutionBeforeDeadline(t *testing.T) {
	harness := newDaemonDrainHarness(t, 100*time.Millisecond, 2*time.Second)
	harness.startAndCancel(t)
	harness.wait(t)

	harness.state.Lock()
	defer harness.state.Unlock()
	if !harness.state.draining || !harness.state.completed || harness.state.released || harness.state.failed {
		t.Fatalf(
			"graceful Drain did not preserve the active Execution: draining=%t completed=%t released=%t failed=%t",
			harness.state.draining, harness.state.completed, harness.state.released, harness.state.failed,
		)
	}
	if indexOfDrainEvent(harness.state.events, "draining") >= indexOfDrainEvent(harness.state.events, "complete") {
		t.Fatalf("Worker was not marked Draining before completion: %#v", harness.state.events)
	}
}

func TestDaemonDrainReleasesExecutionAfterDeadline(t *testing.T) {
	harness := newDaemonDrainHarness(t, 5*time.Second, 50*time.Millisecond)
	harness.startAndCancel(t)
	harness.wait(t)

	harness.state.Lock()
	defer harness.state.Unlock()
	if !harness.state.draining || !harness.state.released || harness.state.completed || harness.state.failed {
		t.Fatalf(
			"Drain deadline did not release the active Execution: draining=%t completed=%t released=%t failed=%t",
			harness.state.draining, harness.state.completed, harness.state.released, harness.state.failed,
		)
	}
	if indexOfDrainEvent(harness.state.events, "draining") >= indexOfDrainEvent(harness.state.events, "release") {
		t.Fatalf("Worker was not marked Draining before release: %#v", harness.state.events)
	}
}

type daemonDrainHarness struct {
	cancel context.CancelFunc
	done   <-chan error
	state  *daemonDrainState
}

type daemonDrainState struct {
	sync.Mutex
	claimIssued bool
	draining    bool
	completed   bool
	released    bool
	failed      bool
	events      []string
	started     chan struct{}
	startOnce   sync.Once
}

func newDaemonDrainHarness(t *testing.T, runnerDelay, drainTimeout time.Duration) daemonDrainHarness {
	t.Helper()
	t.Setenv("GO_WANT_AGENTD_DRAIN_HELPER", "1")
	t.Setenv("AGENTD_DRAIN_HELPER_DELAY", runnerDelay.String())

	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	targetID := uuid.New()
	turnID := uuid.New()
	state := &daemonDrainState{started: make(chan struct{})}
	execution := executions.Execution{
		ID: executionID, TenantID: tenantID, SessionID: uuid.New(), TurnID: turnID,
		Status: "leased", ExecutionTargetID: targetID, TargetKind: "local", Generation: 1,
	}
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: execution.SessionID,
		TurnID: turnID, Provider: "codex", InputText: "finish safely while the Worker drains",
		RuntimeMode: "approval-required", InteractionMode: "default", DefaultBranch: "main",
	}

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/workers/register":
			if request.Header.Get("Authorization") != "Bearer registration-token" {
				http.Error(response, "invalid registration token", http.StatusUnauthorized)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(executions.RegisteredWorker{
				Worker: executions.Worker{
					ID: workerID, ExecutionTargetID: targetID, TargetKind: "local", Status: "online",
					ProtocolVersion: executions.WorkerProtocolVersion,
				},
				Token: "worker-token",
			})
		case "/v1/workers/heartbeat":
			if request.Header.Get("Authorization") != "Bearer worker-token" {
				http.Error(response, "invalid Worker token", http.StatusUnauthorized)
				return
			}
			var input executions.HeartbeatInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid heartbeat", http.StatusBadRequest)
				return
			}
			if input.Draining != nil && *input.Draining {
				state.Lock()
				if !state.draining {
					state.events = append(state.events, "draining")
				}
				state.draining = true
				state.Unlock()
			}
			response.WriteHeader(http.StatusNoContent)
		case "/v1/workers/executions/claim":
			state.Lock()
			claimed := state.claimIssued
			state.claimIssued = true
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			if claimed {
				_ = json.NewEncoder(response).Encode(executions.ClaimResult{})
				return
			}
			_ = json.NewEncoder(response).Encode(executions.ClaimResult{
				Execution: &execution, Lease: &lease, Workload: &workload,
			})
		case "/v1/workers/executions/" + executionID.String() + "/start":
			state.startOnce.Do(func() { close(state.started) })
			response.WriteHeader(http.StatusNoContent)
		case "/v1/workers/executions/" + executionID.String() + "/complete":
			state.Lock()
			state.completed = true
			state.events = append(state.events, "complete")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case "/v1/workers/executions/" + executionID.String() + "/release":
			state.Lock()
			state.released = true
			state.events = append(state.events, "release")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case "/v1/workers/executions/" + executionID.String() + "/fail":
			state.Lock()
			state.failed = true
			state.events = append(state.events, "fail")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ControlPlaneURL: controlPlaneURL, RegistrationToken: "registration-token",
		ExecutionTargetID: targetID, TargetKind: platform.TargetLocal,
		ClusterID: "test", Namespace: "test", PodName: "drain-worker", Version: "test",
		RunnerCommand:  []string{os.Args[0], "-test.run=TestAgentdDrainRunnerHelperProcess", "--"},
		RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: t.TempDir(), PollInterval: time.Millisecond,
		HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour, DrainTimeout: drainTimeout,
		RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
	daemon := NewDaemon(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	return daemonDrainHarness{cancel: cancel, done: done, state: state}
}

func (h daemonDrainHarness) startAndCancel(t *testing.T) {
	t.Helper()
	select {
	case <-h.state.started:
		h.cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("agentd did not start the claimed Execution")
	}
}

func (h daemonDrainHarness) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("agentd Drain failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agentd did not finish Drain")
	}
}

func indexOfDrainEvent(events []string, target string) int {
	for index, event := range events {
		if event == target {
			return index
		}
	}
	return len(events)
}

func TestAgentdDrainRunnerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_AGENTD_DRAIN_HELPER") != "1" {
		return
	}
	delay, err := time.ParseDuration(os.Getenv("AGENTD_DRAIN_HELPER_DELAY"))
	if err != nil {
		os.Exit(2)
	}
	time.Sleep(delay)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type": "result", "output": map[string]any{"text": "completed during Drain"},
	})
	os.Exit(0)
}
