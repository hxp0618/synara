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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestDaemonPreparesManagedWorkspaceBeforeStartingProvider(t *testing.T) {
	t.Setenv("GO_WANT_AGENTD_DRAIN_HELPER", "1")
	t.Setenv("AGENTD_DRAIN_HELPER_DELAY", "1ms")
	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	workspaceID := uuid.New()
	gitCredentialID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	fingerprint, branch := stringPointer("a"+strings.Repeat("b", 63)), stringPointer("synara/session-test")
	baseCommit, headCommit := stringPointer(strings.Repeat("c", 40)), stringPointer(strings.Repeat("d", 40))
	var state struct {
		sync.Mutex
		order         []string
		ready         executions.WorkspaceReadyInput
		dirty         executions.WorkspaceDirtyInput
		requestBodies []string
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		requestBody, _ := io.ReadAll(request.Body)
		request.Body = io.NopCloser(strings.NewReader(string(requestBody)))
		state.Lock()
		state.requestBodies = append(state.requestBodies, string(requestBody))
		state.Unlock()
		switch request.URL.Path {
		case base + "git-credentials/" + gitCredentialID.String() + "/resolve":
			state.Lock()
			state.order = append(state.order, "git.resolve")
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"payload":{"host":"git.example.com","username":"git-user","token":"git-secret-token"}}`)
		case base + "workspace/ready":
			var input executions.WorkspaceReadyInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid Workspace ready payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.ready = input
			state.order = append(state.order, "workspace.ready")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "workspace/dirty":
			var input executions.WorkspaceDirtyInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid Workspace dirty payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.dirty = input
			state.order = append(state.order, "workspace.dirty")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "start":
			state.Lock()
			state.order = append(state.order, "execution.start")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "complete":
			state.Lock()
			state.order = append(state.order, "execution.complete")
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
		RunnerCommand:  []string{os.Args[0], "-test.run=TestAgentdDrainRunnerHelperProcess", "--"},
		RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: t.TempDir(), PollInterval: time.Millisecond,
		HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour, DrainTimeout: time.Second,
		RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
	client := NewClient(cfg)
	client.workerToken = "worker-token"
	materializedDirectory := t.TempDir()
	daemon := &Daemon{
		config: cfg, client: client, runner: NewRunner(cfg),
		workspace: workspaceMaterializerInspector{
			materialize: func(_ context.Context, _ executions.Execution, _ executions.Workload, credential *GitHTTPSCredential) (WorkspaceMaterialization, error) {
				if credential == nil || credential.Host != "git.example.com" || credential.Username != "git-user" || credential.Token != "git-secret-token" {
					t.Fatalf("Workspace materializer received an invalid Git Credential: %#v", credential)
				}
				state.Lock()
				state.order = append(state.order, "workspace.materialize")
				state.Unlock()
				return WorkspaceMaterialization{
					Directory: materializedDirectory, Managed: true, RepositoryFingerprint: fingerprint,
					CurrentBranch: branch, BaseCommit: baseCommit, HeadCommit: headCommit,
				}, nil
			},
			inspect: func(_ context.Context, _ WorkspaceMaterialization) (WorkspaceInspection, error) {
				state.Lock()
				state.order = append(state.order, "workspace.inspect")
				state.Unlock()
				return WorkspaceInspection{Dirty: true, CurrentBranch: branch, HeadCommit: headCommit}, nil
			},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	execution := executions.Execution{ID: executionID, TenantID: tenantID, TurnID: uuid.New(), Generation: 1}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: execution.TurnID, RemoteWorkspaceID: &workspaceID, Provider: "codex", InputText: "run",
		GitCredentialID: &gitCredentialID,
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}
	state.Lock()
	defer state.Unlock()
	expectedOrder := []string{
		"git.resolve", "workspace.materialize", "workspace.ready", "execution.start",
		"workspace.inspect", "workspace.dirty", "execution.complete",
	}
	if len(state.order) != len(expectedOrder) {
		t.Fatalf("unexpected Workspace/Execution lifecycle order: %#v", state.order)
	}
	for index := range expectedOrder {
		if state.order[index] != expectedOrder[index] {
			t.Fatalf("unexpected Workspace/Execution lifecycle order: %#v", state.order)
		}
	}
	if state.ready.RepositoryFingerprint == nil || *state.ready.RepositoryFingerprint != *fingerprint ||
		state.ready.CurrentBranch == nil || *state.ready.CurrentBranch != *branch {
		t.Fatalf("Workspace metadata was not reported: %#v", state.ready)
	}
	if state.dirty.CurrentBranch == nil || *state.dirty.CurrentBranch != *branch ||
		state.dirty.HeadCommit == nil || *state.dirty.HeadCommit != *headCommit {
		t.Fatalf("dirty Workspace metadata was not reported: %#v", state.dirty)
	}
	if strings.Contains(strings.Join(state.requestBodies, "\n"), "git-secret-token") {
		t.Fatal("Git Credential leaked into an agentd request after resolution")
	}
}

func TestDaemonReportsManagedWorkspaceFailureBeforeFailingExecution(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	workspaceID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	var state struct {
		sync.Mutex
		order  []string
		failed executions.WorkspaceFailedInput
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch request.URL.Path {
		case base + "workspace/failed":
			var input executions.WorkspaceFailedInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid Workspace failure payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.failed = input
			state.order = append(state.order, "workspace.failed")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "fail":
			state.Lock()
			state.order = append(state.order, "execution.fail")
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
		RunnerCommand: []string{"unused"}, RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: t.TempDir(),
		PollInterval: time.Millisecond, HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour,
		DrainTimeout: time.Second, RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
	client := NewClient(cfg)
	client.workerToken = "worker-token"
	daemon := &Daemon{
		config: cfg, client: client, runner: NewRunner(cfg),
		workspace: workspaceMaterializerFunc(func(context.Context, executions.Execution, executions.Workload, *GitHTTPSCredential) (WorkspaceMaterialization, error) {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "Repository URL is not allowed for a remote Workspace.", true, false,
			)
		}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	execution := executions.Execution{ID: executionID, TenantID: tenantID, TurnID: uuid.New(), Generation: 1}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: execution.TurnID, RemoteWorkspaceID: &workspaceID, Provider: "codex", InputText: "run",
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err == nil {
		t.Fatal("Workspace preparation failure did not fail the Execution")
	}
	state.Lock()
	defer state.Unlock()
	if len(state.order) != 2 || state.order[0] != "workspace.failed" || state.order[1] != "execution.fail" {
		t.Fatalf("unexpected Workspace failure order: %#v", state.order)
	}
	if state.failed.FailureCode != "workspace_invalid" || state.failed.FailureMessage == "" {
		t.Fatalf("Workspace failure was not safely classified: %#v", state.failed)
	}
}

type workspaceMaterializerFunc func(context.Context, executions.Execution, executions.Workload, *GitHTTPSCredential) (WorkspaceMaterialization, error)

func (f workspaceMaterializerFunc) Materialize(
	ctx context.Context,
	execution executions.Execution,
	workload executions.Workload,
	credential *GitHTTPSCredential,
) (WorkspaceMaterialization, error) {
	return f(ctx, execution, workload, credential)
}

type workspaceMaterializerInspector struct {
	materialize func(context.Context, executions.Execution, executions.Workload, *GitHTTPSCredential) (WorkspaceMaterialization, error)
	inspect     func(context.Context, WorkspaceMaterialization) (WorkspaceInspection, error)
}

func (m workspaceMaterializerInspector) Materialize(
	ctx context.Context,
	execution executions.Execution,
	workload executions.Workload,
	credential *GitHTTPSCredential,
) (WorkspaceMaterialization, error) {
	return m.materialize(ctx, execution, workload, credential)
}

func (m workspaceMaterializerInspector) Inspect(
	ctx context.Context,
	materialized WorkspaceMaterialization,
) (WorkspaceInspection, error) {
	return m.inspect(ctx, materialized)
}
