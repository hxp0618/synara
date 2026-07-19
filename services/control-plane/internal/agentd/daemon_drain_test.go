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

func TestDaemonDrainCheckpointsManagedWorkspaceBeforeRelease(t *testing.T) {
	harness := newDaemonDrainHarnessWithOptions(t, daemonDrainHarnessOptions{
		runnerDelay:  5 * time.Second,
		drainTimeout: 50 * time.Millisecond,
		managed:      true,
	})
	harness.startAndCancel(t)
	harness.wait(t)

	harness.state.Lock()
	defer harness.state.Unlock()
	if !harness.state.released || !harness.state.checkpointReady || harness.state.failed {
		t.Fatalf(
			"managed Drain did not checkpoint before release: released=%t checkpointReady=%t failed=%t events=%#v",
			harness.state.released, harness.state.checkpointReady, harness.state.failed, harness.state.events,
		)
	}
	if !strings.Contains(harness.state.releaseReason, "workspace-checkpoint=ready") ||
		strings.Contains(harness.state.releaseReason, "data-loss-risk") {
		t.Fatalf("managed Drain release reason did not prove a ready Checkpoint: %q", harness.state.releaseReason)
	}
	renewIndex := indexOfDrainEvent(harness.state.events, "lease.renew")
	inspectIndex := indexOfDrainEvent(harness.state.events, "workspace.inspect")
	checkpointIndex := indexOfDrainEvent(harness.state.events, "checkpoint.ready")
	releaseIndex := indexOfDrainEvent(harness.state.events, "release")
	workspaceReleaseIndex := indexOfDrainEvent(harness.state.events, "workspace.release")
	if renewIndex >= inspectIndex || inspectIndex >= checkpointIndex || checkpointIndex >= releaseIndex ||
		releaseIndex >= workspaceReleaseIndex {
		t.Fatalf("managed Drain did not retain the Workspace lock through Checkpoint and release: %#v", harness.state.events)
	}
}

func TestDaemonDrainRetriesCommittedLeaseProbeWithStableRequestIDAfterResponseLoss(t *testing.T) {
	harness := newDaemonDrainHarnessWithOptions(t, daemonDrainHarnessOptions{
		runnerDelay:                 5 * time.Second,
		drainTimeout:                50 * time.Millisecond,
		managed:                     true,
		drainRenewLoseFirstResponse: true,
	})
	harness.startAndCancel(t)
	harness.wait(t)

	harness.state.Lock()
	defer harness.state.Unlock()
	if !harness.state.released || !harness.state.checkpointReady || harness.state.failed {
		t.Fatalf(
			"Drain did not continue through Checkpoint and Release after Renew response loss: %#v",
			harness.state.events,
		)
	}
	if harness.state.drainRenewCommits != 1 || harness.state.drainRenewReplays != 1 {
		t.Fatalf(
			"Drain Renew did not commit once and replay one receipt: commits=%d replays=%d events=%#v",
			harness.state.drainRenewCommits,
			harness.state.drainRenewReplays,
			harness.state.events,
		)
	}
	requestIDs := harness.state.drainRenewRequestIDs
	if len(requestIDs) != 2 || requestIDs[0] == "" || requestIDs[0] != requestIDs[1] ||
		!strings.HasPrefix(requestIDs[0], drainCheckpointRenewRequestIDPrefix) {
		t.Fatalf("Drain Renew did not retry with one stable attempt ID: %#v", requestIDs)
	}
	commitIndex := indexOfDrainEvent(harness.state.events, "lease.renew.commit")
	replayIndex := indexOfDrainEvent(harness.state.events, "lease.renew.replay")
	checkpointIndex := indexOfDrainEvent(harness.state.events, "checkpoint.ready")
	releaseIndex := indexOfDrainEvent(harness.state.events, "release")
	if commitIndex >= replayIndex || replayIndex >= checkpointIndex || checkpointIndex >= releaseIndex {
		t.Fatalf("Drain Renew replay did not fence Checkpoint and Release: %#v", harness.state.events)
	}
}

func TestDaemonDrainMarksDataLossRiskWhenManagedCheckpointFails(t *testing.T) {
	harness := newDaemonDrainHarnessWithOptions(t, daemonDrainHarnessOptions{
		runnerDelay:          5 * time.Second,
		drainTimeout:         50 * time.Millisecond,
		managed:              true,
		checkpointCreateFail: true,
	})
	harness.startAndCancel(t)
	harness.wait(t)

	harness.state.Lock()
	defer harness.state.Unlock()
	if !harness.state.released || harness.state.checkpointReady || harness.state.checkpointCreateCalls != 3 {
		t.Fatalf(
			"failed managed Drain Checkpoint had an unexpected lifecycle: released=%t checkpointReady=%t createCalls=%d events=%#v",
			harness.state.released, harness.state.checkpointReady, harness.state.checkpointCreateCalls, harness.state.events,
		)
	}
	if !strings.Contains(harness.state.releaseReason, "data-loss-risk=workspace-checkpoint-failed") ||
		strings.Contains(harness.state.releaseReason, "workspace-checkpoint=ready") {
		t.Fatalf("failed managed Drain Checkpoint was not explicit in the recovery Event reason: %q", harness.state.releaseReason)
	}
	checkpointIndex := indexOfDrainEvent(harness.state.events, "checkpoint.create")
	releaseIndex := indexOfDrainEvent(harness.state.events, "release")
	workspaceReleaseIndex := indexOfDrainEvent(harness.state.events, "workspace.release")
	if checkpointIndex >= releaseIndex || releaseIndex >= workspaceReleaseIndex {
		t.Fatalf("failed managed Drain did not retain the Workspace lock through release: %#v", harness.state.events)
	}
}

func TestDaemonDrainDoesNotInspectWorkspaceAfterLeaseProbeFails(t *testing.T) {
	harness := newDaemonDrainHarnessWithOptions(t, daemonDrainHarnessOptions{
		runnerDelay:  5 * time.Second,
		drainTimeout: 50 * time.Millisecond,
		managed:      true,
		renewFail:    true,
	})
	harness.startAndCancel(t)
	harness.wait(t)

	harness.state.Lock()
	defer harness.state.Unlock()
	if !harness.state.released || harness.state.checkpointCreateCalls != 0 ||
		indexOfDrainEvent(harness.state.events, "workspace.inspect") != len(harness.state.events) {
		t.Fatalf("fenced Drain inspected or checkpointed the Workspace: %#v", harness.state.events)
	}
	if !strings.Contains(harness.state.releaseReason, "data-loss-risk=workspace-checkpoint-failed") {
		t.Fatalf("fenced Drain release did not preserve the data-loss warning: %q", harness.state.releaseReason)
	}
	if len(harness.state.drainRenewRequestIDs) != 1 {
		t.Fatalf(
			"deterministic 409 Drain Renew was retried: requestIDs=%#v events=%#v",
			harness.state.drainRenewRequestIDs,
			harness.state.events,
		)
	}
}

func TestDaemonDrainReleasesAfterNormalRenewalIsCanceled(t *testing.T) {
	harness := newDaemonDrainHarnessWithOptions(t, daemonDrainHarnessOptions{
		runnerDelay:        5 * time.Second,
		drainTimeout:       50 * time.Millisecond,
		managed:            true,
		blockFirstRenew:    true,
		leaseRenewInterval: 10 * time.Millisecond,
	})
	select {
	case <-harness.state.started:
	case <-time.After(3 * time.Second):
		t.Fatal("agentd did not start the claimed Execution")
	}
	select {
	case <-harness.state.renewStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("agentd did not begin normal Lease renewal")
	}
	harness.cancel()
	time.Sleep(100 * time.Millisecond)
	close(harness.state.unblockRenew)
	harness.wait(t)

	harness.state.Lock()
	defer harness.state.Unlock()
	if !harness.state.released || !harness.state.checkpointReady {
		t.Fatalf("cancelled normal renewal skipped Drain Checkpoint or Release: %#v", harness.state.events)
	}
	if !strings.Contains(harness.state.releaseReason, "workspace-checkpoint=ready") {
		t.Fatalf("cancelled normal renewal lost the safe Drain reason: %q", harness.state.releaseReason)
	}
	normalRequestIDs := make(map[string]struct{}, len(harness.state.normalRenewRequestIDs))
	for _, requestID := range harness.state.normalRenewRequestIDs {
		if requestID == "" {
			t.Fatal("normal Renew omitted its request identity")
		}
		normalRequestIDs[requestID] = struct{}{}
	}
	drainIDReused := false
	if len(harness.state.drainRenewRequestIDs) == 1 {
		_, drainIDReused = normalRequestIDs[harness.state.drainRenewRequestIDs[0]]
	}
	if len(harness.state.normalRenewRequestIDs) < 1 ||
		len(normalRequestIDs) != len(harness.state.normalRenewRequestIDs) ||
		len(harness.state.drainRenewRequestIDs) != 1 || drainIDReused {
		t.Fatalf(
			"normal and Drain Renew did not use isolated request identities: normal=%#v drain=%#v",
			harness.state.normalRenewRequestIDs,
			harness.state.drainRenewRequestIDs,
		)
	}
}

func TestClientReleaseRetriesWithStableRequestIDAfterResponseLoss(t *testing.T) {
	executionID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID,
		TenantID:    uuid.New(),
		WorkerID:    uuid.New(),
		Generation:  7,
		LeaseToken:  "lease-token",
	}
	var requestIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestIDs = append(requestIDs, request.Header.Get("X-Request-ID"))
		if len(requestIDs) == 1 {
			connection, _, err := response.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = connection.Close()
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: controlPlaneURL, RequestTimeout: time.Second})
	client.workerToken = "worker-token"
	if err := client.Release(context.Background(), executionID, lease, "worker-drain; data-loss-risk=workspace-checkpoint-failed"); err != nil {
		t.Fatal(err)
	}
	if len(requestIDs) != 2 || requestIDs[0] == "" || requestIDs[0] != requestIDs[1] {
		t.Fatalf("Execution release did not retry with one stable request ID: %#v", requestIDs)
	}
}

func TestRenewRequestIDsAreFreshAcrossOrdinaryCallsAndDrainAttempts(t *testing.T) {
	requestIDs := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestIDs <- request.Header.Get("X-Request-ID")
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: controlPlaneURL, RequestTimeout: time.Second})
	client.workerToken = "worker-token"
	executionID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID,
		TenantID:    uuid.New(),
		WorkerID:    uuid.New(),
		Generation:  7,
		LeaseToken:  "lease-token",
	}
	if err := client.Renew(context.Background(), executionID, lease); err != nil {
		t.Fatal(err)
	}
	if err := client.Renew(context.Background(), executionID, lease); err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{client: client}
	if err := daemon.renewLeaseForDrainCheckpoint(context.Background(), executionID, lease); err != nil {
		t.Fatal(err)
	}
	if err := daemon.renewLeaseForDrainCheckpoint(context.Background(), executionID, lease); err != nil {
		t.Fatal(err)
	}
	ordinaryFirst, ordinarySecond := <-requestIDs, <-requestIDs
	drainFirst, drainSecond := <-requestIDs, <-requestIDs
	if ordinaryFirst == "" || ordinarySecond == "" || ordinaryFirst == ordinarySecond ||
		strings.HasPrefix(ordinaryFirst, drainCheckpointRenewRequestIDPrefix) ||
		strings.HasPrefix(ordinarySecond, drainCheckpointRenewRequestIDPrefix) {
		t.Fatalf(
			"ordinary Renew did not use a fresh request ID per call: %q, %q",
			ordinaryFirst,
			ordinarySecond,
		)
	}
	if drainFirst == "" || drainSecond == "" || drainFirst == drainSecond ||
		!strings.HasPrefix(drainFirst, drainCheckpointRenewRequestIDPrefix) ||
		!strings.HasPrefix(drainSecond, drainCheckpointRenewRequestIDPrefix) {
		t.Fatalf(
			"Drain Renew attempts did not use unique attempt IDs: %q, %q",
			drainFirst,
			drainSecond,
		)
	}
}

type daemonDrainHarness struct {
	cancel context.CancelFunc
	done   <-chan error
	state  *daemonDrainState
}

type daemonDrainState struct {
	sync.Mutex
	claimIssued           bool
	draining              bool
	completed             bool
	released              bool
	failed                bool
	checkpointReady       bool
	checkpointCreateCalls int
	releaseReason         string
	events                []string
	started               chan struct{}
	startOnce             sync.Once
	renewStarted          chan struct{}
	unblockRenew          chan struct{}
	renewStartOnce        sync.Once
	renewCalls            int
	normalRenewRequestIDs []string
	drainRenewRequestIDs  []string
	drainRenewReceiptID   string
	drainRenewCommits     int
	drainRenewReplays     int
}

func newDaemonDrainHarness(t *testing.T, runnerDelay, drainTimeout time.Duration) daemonDrainHarness {
	return newDaemonDrainHarnessWithOptions(t, daemonDrainHarnessOptions{
		runnerDelay:  runnerDelay,
		drainTimeout: drainTimeout,
	})
}

type daemonDrainHarnessOptions struct {
	runnerDelay                 time.Duration
	drainTimeout                time.Duration
	managed                     bool
	checkpointCreateFail        bool
	renewFail                   bool
	blockFirstRenew             bool
	leaseRenewInterval          time.Duration
	drainRenewLoseFirstResponse bool
}

func newDaemonDrainHarnessWithOptions(t *testing.T, options daemonDrainHarnessOptions) daemonDrainHarness {
	t.Helper()
	t.Setenv("GO_WANT_AGENTD_DRAIN_HELPER", "1")
	t.Setenv("AGENTD_DRAIN_HELPER_DELAY", options.runnerDelay.String())

	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	targetID := uuid.New()
	turnID := uuid.New()
	workspaceID := uuid.New()
	checkpointID := uuid.New()
	artifactID := uuid.New()
	state := &daemonDrainState{
		started: make(chan struct{}), renewStarted: make(chan struct{}), unblockRenew: make(chan struct{}),
	}
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
	if options.managed {
		workload.RemoteWorkspaceID = &workspaceID
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
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
		case base + "workspace/ready":
			state.Lock()
			state.events = append(state.events, "workspace.ready")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "workspace/dirty":
			state.Lock()
			state.events = append(state.events, "workspace.dirty")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "renew":
			requestID := request.Header.Get("X-Request-ID")
			drainRenew := strings.HasPrefix(requestID, drainCheckpointRenewRequestIDPrefix)
			loseResponse := false
			state.Lock()
			state.renewCalls++
			renewCall := state.renewCalls
			state.events = append(state.events, "lease.renew")
			if drainRenew {
				state.drainRenewRequestIDs = append(state.drainRenewRequestIDs, requestID)
			} else {
				state.normalRenewRequestIDs = append(state.normalRenewRequestIDs, requestID)
			}
			if options.drainRenewLoseFirstResponse && drainRenew {
				switch {
				case state.drainRenewReceiptID == "":
					state.drainRenewReceiptID = requestID
					state.drainRenewCommits++
					state.events = append(state.events, "lease.renew.commit")
					loseResponse = true
				case state.drainRenewReceiptID == requestID:
					state.drainRenewReplays++
					state.events = append(state.events, "lease.renew.replay")
				default:
					state.drainRenewCommits++
					state.events = append(state.events, "lease.renew.commit")
				}
			}
			state.renewStartOnce.Do(func() { close(state.renewStarted) })
			state.Unlock()
			if loseResponse {
				connection, _, err := response.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack committed Drain Renew response: %v", err)
					return
				}
				_ = connection.Close()
				return
			}
			if options.blockFirstRenew && renewCall == 1 {
				<-state.unblockRenew
				http.Error(response, "normal renewal canceled", http.StatusServiceUnavailable)
				return
			}
			if options.renewFail {
				http.Error(response, "generation fenced", http.StatusConflict)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		case base + "workspace/checkpoints":
			state.Lock()
			state.checkpointCreateCalls++
			state.events = append(state.events, "checkpoint.create")
			state.Unlock()
			if options.checkpointCreateFail {
				http.Error(response, "checkpoint unavailable", http.StatusServiceUnavailable)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"id":"`+checkpointID.String()+`","status":"pending"}`)
		case base + "artifacts":
			state.Lock()
			state.events = append(state.events, "checkpoint.artifact.create")
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`","status":"pending"},"method":"PUT","url":"`+
				server.URL+`/checkpoint-upload","headers":{},"expiresAt":"2030-01-01T00:00:00Z"}`)
		case "/checkpoint-upload":
			state.Lock()
			state.events = append(state.events, "checkpoint.upload")
			state.Unlock()
			_, _ = io.Copy(io.Discard, request.Body)
			response.WriteHeader(http.StatusNoContent)
		case base + "artifacts/" + artifactID.String() + "/complete":
			var input struct {
				SHA256 string `json:"sha256"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.SHA256 == "" {
				http.Error(response, "invalid Artifact completion", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.events = append(state.events, "checkpoint.artifact.ready")
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"id":"`+artifactID.String()+`","status":"ready","sha256":"`+input.SHA256+`"}`)
		case base + "workspace/checkpoints/" + checkpointID.String() + "/ready":
			state.Lock()
			state.checkpointReady = true
			state.events = append(state.events, "checkpoint.ready")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "start":
			state.startOnce.Do(func() { close(state.started) })
			response.WriteHeader(http.StatusNoContent)
		case base + "complete":
			state.Lock()
			state.completed = true
			state.events = append(state.events, "complete")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "release":
			var input executions.ReleaseLeaseInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid release", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.released = true
			state.releaseReason = input.Reason
			state.events = append(state.events, "release")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "fail":
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
	leaseRenewInterval := time.Hour
	if options.leaseRenewInterval > 0 {
		leaseRenewInterval = options.leaseRenewInterval
	}
	cfg := Config{
		ControlPlaneURL: controlPlaneURL, RegistrationToken: "registration-token",
		ExecutionTargetID: targetID, TargetKind: platform.TargetLocal,
		ClusterID: "test", Namespace: "test", PodName: "drain-worker", Version: "test",
		RunnerCommand:  agentdDrainRunnerTestCommand(),
		RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: t.TempDir(), PollInterval: time.Millisecond,
		HeartbeatInterval: time.Hour, LeaseRenewInterval: leaseRenewInterval, DrainTimeout: options.drainTimeout,
		RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
	daemon := NewDaemon(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if options.managed {
		workspaceDirectory := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspaceDirectory, "drain-result.txt"), []byte("preserve me\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		daemon.workspace = workspaceMaterializerInspector{
			materialize: func(context.Context, executions.Execution, executions.Workload, *WorkspaceGitCredential) (WorkspaceMaterialization, error) {
				return WorkspaceMaterialization{
					Directory: workspaceDirectory,
					Managed:   true,
					release: func() error {
						state.Lock()
						state.events = append(state.events, "workspace.release")
						state.Unlock()
						return nil
					},
				}, nil
			},
			inspect: func(context.Context, WorkspaceMaterialization) (WorkspaceInspection, error) {
				state.Lock()
				state.events = append(state.events, "workspace.inspect")
				state.Unlock()
				return WorkspaceInspection{Dirty: true}, nil
			},
		}
	}
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
	if !containsString(os.Args, "--synara-agentd-drain-test-helper") {
		return
	}
	if os.Getenv("GO_WANT_AGENTD_DRAIN_HELPER") != "" || os.Getenv("AGENTD_DRAIN_HELPER_DELAY") != "" {
		os.Exit(2)
	}
	delay, err := time.ParseDuration(agentdDrainTestArgument("--synara-agentd-drain-test-delay"))
	if err != nil {
		os.Exit(2)
	}
	time.Sleep(delay)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type": "result", "output": map[string]any{"text": "completed during Drain"},
	})
	os.Exit(0)
}

func agentdDrainRunnerTestCommand() []string {
	return []string{
		os.Args[0], "-test.run=^TestAgentdDrainRunnerHelperProcess$", "--",
		"--synara-agentd-drain-test-helper",
		"--synara-agentd-drain-test-delay", os.Getenv("AGENTD_DRAIN_HELPER_DELAY"),
	}
}

func agentdDrainTestArgument(name string) string {
	for index := 0; index+1 < len(os.Args); index++ {
		if os.Args[index] == name {
			return os.Args[index+1]
		}
	}
	return ""
}
