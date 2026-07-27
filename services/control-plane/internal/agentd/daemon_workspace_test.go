package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestDaemonRestoresSnapshotBeforeProviderAndReusesUnchangedCheckpoint(t *testing.T) {
	t.Setenv("GO_WANT_AGENTD_DRAIN_HELPER", "1")
	t.Setenv("AGENTD_DRAIN_HELPER_DELAY", "1ms")
	executionID := uuid.New()
	tenantID := uuid.New()
	organizationID := uuid.New()
	projectID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	workspaceID := uuid.New()
	workerID := uuid.New()
	checkpointID := uuid.New()
	artifactID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 2, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "restored.txt"), []byte("checkpoint payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := captureWorkspaceCheckpoint(
		context.Background(),
		executions.Execution{ID: uuid.New(), Generation: 1},
		WorkspaceMaterialization{Directory: source, Managed: true},
		WorkspaceInspection{Dirty: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	archive, err := os.ReadFile(candidate.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	sha := hex.EncodeToString(digest[:])
	archiveSize := int64(len(archive))
	checkpoint := executions.WorkspaceCheckpoint{
		ID: checkpointID, WorkspaceID: workspaceID, SessionID: sessionID, TurnID: &turnID,
		ExecutionID: uuid.New(), Generation: 1, IdempotencyKey: "previous-snapshot",
		Strategy: "snapshot", Status: "ready", ArtifactID: &artifactID,
		Manifest: candidate.Manifest, FileCount: &candidate.FileCount,
		TotalBytes: &candidate.TotalBytes, SHA256: &sha,
	}
	var state struct {
		sync.Mutex
		order []string
		ready executions.WorkspaceReadyInput
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch request.URL.Path {
		case base + "workspace/checkpoints/" + checkpointID.String() + "/artifact/download":
			state.Lock()
			state.order = append(state.order, "checkpoint.download.grant")
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`","sizeBytes":`+fmt.Sprint(archiveSize)+`,"sha256":"`+sha+`"},"url":"`+server.URL+`/checkpoint-content","expiresAt":"2030-01-01T00:00:00Z"}`)
		case "/checkpoint-content":
			state.Lock()
			state.order = append(state.order, "checkpoint.download.content")
			state.Unlock()
			response.Header().Set("Content-Type", "application/x-tar")
			_, _ = response.Write(archive)
		case base + "workspace/ready":
			if err := json.NewDecoder(request.Body).Decode(&state.ready); err != nil {
				http.Error(response, "invalid Workspace ready payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.order = append(state.order, "workspace.ready")
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
	root := t.TempDir()
	cfg := Config{
		ControlPlaneURL: controlPlaneURL, TargetKind: platform.TargetLocal,
		RunnerCommand:  agentdDrainRunnerTestCommand(),
		RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: root, PollInterval: time.Millisecond,
		HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour, DrainTimeout: time.Second,
		RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
	client := NewClient(cfg)
	client.workerToken = "worker-token"
	daemon := &Daemon{
		config: cfg, client: client, runner: NewRunner(cfg),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	execution := executions.Execution{ID: executionID, TenantID: tenantID, TurnID: turnID, Generation: lease.Generation}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID,
		SessionID: sessionID, TurnID: turnID, RemoteWorkspaceID: &workspaceID,
		RestoreCheckpointID: &checkpointID, RestoreCheckpoint: &checkpoint,
		Provider: "codex", InputText: "continue", DefaultBranch: "main",
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(
		root, "v2", uuid.Nil.String(), tenantID.String(), projectID.String(), sessionID.String(), workspaceID.String(), "checkout",
	)
	content, err := os.ReadFile(filepath.Join(workspace, "restored.txt"))
	if err != nil || string(content) != "checkpoint payload\n" {
		t.Fatalf("Provider did not receive the restored Workspace: %q err=%v", content, err)
	}
	state.Lock()
	defer state.Unlock()
	expectedOrder := []string{
		"checkpoint.download.grant", "checkpoint.download.content", "workspace.ready",
		"execution.start", "execution.complete",
	}
	if !reflect.DeepEqual(state.order, expectedOrder) {
		t.Fatalf("unexpected restore lifecycle order: %#v", state.order)
	}
	if state.ready.RestoredCheckpointID == nil || *state.ready.RestoredCheckpointID != checkpointID {
		t.Fatalf("Workspace restore was not reported: %#v", state.ready)
	}
}

func TestDaemonPreparesManagedWorkspaceBeforeStartingProvider(t *testing.T) {
	t.Setenv("GO_WANT_AGENTD_DRAIN_HELPER", "1")
	t.Setenv("AGENTD_DRAIN_HELPER_DELAY", "1ms")
	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	workspaceID := uuid.New()
	gitGrantID := uuid.New()
	checkpointID := uuid.New()
	artifactID := uuid.New()
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
		checkpoint    executions.CreateWorkspaceCheckpointInput
		requestBodies []string
	}
	executionStarted := make(chan struct{})
	backgroundRefreshResult := make(chan error, 1)
	var logs bytes.Buffer
	controlPlaneURL, err := url.Parse("http://control-plane.invalid")
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		requestBody, _ := io.ReadAll(request.Body)
		request.Body = io.NopCloser(strings.NewReader(string(requestBody)))
		state.Lock()
		state.requestBodies = append(state.requestBodies, string(requestBody))
		state.Unlock()
		switch request.URL.Path {
		case "/checkpoint-upload":
			state.Lock()
			state.order = append(state.order, "checkpoint.upload")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "credential-grants/" + gitGrantID.String() + "/resolve":
			state.Lock()
			state.order = append(state.order, "git.resolve")
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"grantId":"`+gitGrantID.String()+`","bindingKind":"git_fetch","purpose":"git","provider":"git","credentialType":"https_token","selector":"https://git.example.com/team/repository.git","payload":{"host":"git.example.com","username":"git-user","token":"git-secret-token"}}`)
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
		case base + "workspace/checkpoints":
			var input executions.CreateWorkspaceCheckpointInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid Checkpoint payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.checkpoint = input
			state.order = append(state.order, "checkpoint.create")
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"id":"`+checkpointID.String()+`","status":"pending"}`)
		case base + "artifacts":
			state.Lock()
			state.order = append(state.order, "checkpoint.artifact.create")
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`"},"method":"PUT","url":"`+controlPlaneURL.String()+`/checkpoint-upload","headers":{},"expiresAt":"2030-01-01T00:00:00Z"}`)
		case base + "artifacts/" + artifactID.String() + "/complete":
			var input struct {
				SHA256 string `json:"sha256"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				http.Error(response, "invalid Artifact complete payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.order = append(state.order, "checkpoint.artifact.ready")
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"id":"`+artifactID.String()+`","sha256":"`+input.SHA256+`"}`)
		case base + "workspace/checkpoints/" + checkpointID.String() + "/ready":
			state.Lock()
			state.order = append(state.order, "checkpoint.ready")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "start":
			state.Lock()
			state.order = append(state.order, "execution.start")
			state.Unlock()
			close(executionStarted)
			response.WriteHeader(http.StatusNoContent)
		case base + "complete":
			state.Lock()
			state.order = append(state.order, "execution.complete")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	})
	transport := workspaceRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		result := response.Result()
		result.Request = request
		return result, nil
	})
	cfg := Config{
		ControlPlaneURL: controlPlaneURL, TargetKind: platform.TargetLocal,
		RunnerCommand:  agentdDrainRunnerTestCommand(),
		RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: t.TempDir(), PollInterval: time.Millisecond,
		HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour, DrainTimeout: time.Second,
		RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
	client := NewClient(cfg)
	client.http.Transport = transport
	client.uploadHTTP.Transport = transport
	client.workerToken = "worker-token"
	materializedDirectory := t.TempDir()
	daemon := &Daemon{
		config: cfg, client: client, runner: NewRunner(cfg),
		workspace: workspaceMaterializerInspector{
			materialize: func(_ context.Context, _ executions.Execution, _ executions.Workload, credential *WorkspaceGitCredential) (WorkspaceMaterialization, error) {
				if credential == nil || credential.HTTPS == nil || credential.SSH != nil ||
					credential.HTTPS.Host != "git.example.com" || credential.HTTPS.Username != "git-user" ||
					credential.HTTPS.Token != "git-secret-token" {
					t.Fatalf("Workspace materializer received an invalid Git Credential: %#v", credential)
				}
				state.Lock()
				state.order = append(state.order, "workspace.materialize")
				state.Unlock()
				return WorkspaceMaterialization{
					Directory: materializedDirectory, Managed: true, RepositoryFingerprint: fingerprint,
					CurrentBranch: branch, BaseCommit: baseCommit, HeadCommit: headCommit,
					cacheFetchOutcome: workspaceCacheFetchOutcomeFreshSkip,
					backgroundCacheRefresh: func(_ context.Context, refreshCredential *WorkspaceGitCredential) error {
						state.Lock()
						readyBeforeRefresh := state.ready.RepositoryFingerprint != nil
						state.Unlock()
						if !readyBeforeRefresh {
							backgroundRefreshResult <- errors.New("background refresh started before workspace.ready")
							return errors.New("background refresh ordering failed")
						}
						if refreshCredential == nil || refreshCredential.HTTPS == nil ||
							refreshCredential.HTTPS.Token != "git-secret-token" {
							backgroundRefreshResult <- errors.New("background refresh did not reuse the resolved Claim credential")
							return errors.New("background refresh credential failed")
						}
						<-executionStarted
						backgroundRefreshResult <- nil
						return errors.New("bounded background refresh failure")
					},
				}, nil
			},
			inspect: func(_ context.Context, _ WorkspaceMaterialization) (WorkspaceInspection, error) {
				state.Lock()
				state.order = append(state.order, "workspace.inspect")
				state.Unlock()
				return WorkspaceInspection{Dirty: false}, nil
			},
		},
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	execution := executions.Execution{ID: executionID, TenantID: tenantID, TurnID: uuid.New(), Generation: 1}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: execution.TurnID, RemoteWorkspaceID: &workspaceID, Provider: "codex", InputText: "run",
		CredentialGrants: []executions.CredentialGrantDescriptor{{
			GrantID: gitGrantID, BindingKind: "git_fetch", Purpose: "git", Provider: "git",
			CredentialType: "https_token", Selector: "https://git.example.com/team/repository.git",
		}},
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-backgroundRefreshResult; err != nil {
		t.Fatal(err)
	}
	state.Lock()
	defer state.Unlock()
	expectedOrder := []string{
		"git.resolve", "workspace.materialize", "workspace.ready", "execution.start",
		"workspace.inspect", "workspace.dirty", "checkpoint.create", "checkpoint.artifact.create",
		"checkpoint.upload", "checkpoint.artifact.ready", "checkpoint.ready", "execution.complete",
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
	if state.dirty.CurrentBranch != nil || state.dirty.HeadCommit != nil {
		t.Fatalf("empty non-Git Workspace reported Git metadata: %#v", state.dirty)
	}
	if state.checkpoint.Strategy != "snapshot" {
		t.Fatalf("empty non-Git Workspace did not use a Snapshot Checkpoint: %#v", state.checkpoint)
	}
	if strings.Contains(strings.Join(state.requestBodies, "\n"), "git-secret-token") {
		t.Fatal("Git Credential leaked into an agentd request after resolution")
	}
	encodedLogs := logs.String()
	if strings.Count(encodedLogs, "Workspace cache materialization completed") != 1 ||
		!strings.Contains(encodedLogs, "outcome=cache-fresh-skip") ||
		!strings.Contains(encodedLogs, "Workspace cache background refresh failed") {
		t.Fatalf("Workspace cache visibility logs are incomplete: %s", encodedLogs)
	}
}

func TestDaemonGrantResolveFailureRejectsFreshWorkspaceCache(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	workspaceID := uuid.New()
	gitGrantID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	var state struct {
		sync.Mutex
		order []string
	}
	transport := workspaceRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		state.Lock()
		defer state.Unlock()
		status := http.StatusNoContent
		body := ""
		switch request.URL.Path {
		case base + "credential-grants/" + gitGrantID.String() + "/resolve":
			state.order = append(state.order, "git.resolve.rejected")
			status = http.StatusForbidden
			body = `{"error":"credential revoked"}`
		case base + "workspace/failed":
			state.order = append(state.order, "workspace.failed")
		case base + "fail":
			state.order = append(state.order, "execution.fail")
		default:
			status = http.StatusNotFound
			body = "unexpected path"
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})
	controlPlaneURL, err := url.Parse("http://control-plane.invalid")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ControlPlaneURL: controlPlaneURL, TargetKind: platform.TargetLocal,
		RunnerCommand: []string{"unused"}, RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: t.TempDir(),
		PollInterval: time.Millisecond, HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour,
		DrainTimeout: time.Second, RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
		WorkspaceFetchWindow: time.Minute,
	}
	client := NewClient(cfg)
	client.http.Transport = transport
	client.workerToken = "worker-token"
	materializeCalled := false
	daemon := &Daemon{
		config: cfg, client: client, runner: NewRunner(cfg),
		workspace: workspaceMaterializerFunc(func(context.Context, executions.Execution, executions.Workload, *WorkspaceGitCredential) (WorkspaceMaterialization, error) {
			materializeCalled = true
			return WorkspaceMaterialization{cacheFetchOutcome: workspaceCacheFetchOutcomeFreshSkip}, nil
		}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	execution := executions.Execution{ID: executionID, TenantID: tenantID, TurnID: uuid.New(), Generation: 1}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: execution.TurnID, RemoteWorkspaceID: &workspaceID, Provider: "codex", InputText: "run",
		CredentialGrants: []executions.CredentialGrantDescriptor{{
			GrantID: gitGrantID, BindingKind: "git_fetch", Purpose: "git", Provider: "git",
			CredentialType: "https_token", Selector: "https://git.example.com/team/repository.git",
		}},
	}
	if err := daemon.runExecution(context.Background(), execution, lease, workload, nil); err == nil {
		t.Fatal("revoked git_fetch Grant did not fail the Execution")
	}
	if materializeCalled {
		t.Fatal("fresh Workspace cache bypassed the per-Claim git_fetch Grant resolve")
	}
	state.Lock()
	defer state.Unlock()
	want := []string{"git.resolve.rejected", "workspace.failed", "execution.fail"}
	if !reflect.DeepEqual(state.order, want) {
		t.Fatalf("unexpected revoked Grant lifecycle: %#v", state.order)
	}
}

func TestDaemonBackgroundCacheRefreshFailureIsBoundedAndClearsCredential(t *testing.T) {
	var logs bytes.Buffer
	daemon := &Daemon{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	credential := &WorkspaceGitCredential{HTTPS: &GitHTTPSCredential{
		Host: "git.example.com", Username: "git-user", Token: "short-lived-token",
	}}
	done := daemon.startWorkspaceBackgroundCacheRefresh(
		context.Background(),
		executions.Execution{ID: uuid.New()},
		executions.Lease{Generation: 7},
		stringPointer(strings.Repeat("a", 64)),
		func(_ context.Context, received *WorkspaceGitCredential) error {
			if received != credential || received.HTTPS == nil || received.HTTPS.Token != "short-lived-token" {
				t.Errorf("background refresh did not receive the resolved Credential: %#v", received)
			}
			return errors.New(strings.Repeat("x", 2_000))
		},
		credential,
	)
	<-done
	if credential.HTTPS != nil || credential.SSH != nil {
		t.Fatalf("background refresh retained the resolved Credential: %#v", credential)
	}
	encoded := logs.String()
	if !strings.Contains(encoded, "Workspace cache background refresh failed") ||
		strings.Contains(encoded, strings.Repeat("x", 1_001)) {
		t.Fatalf("background refresh failure log was missing or unbounded: %s", encoded)
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
		workspace: workspaceMaterializerFunc(func(context.Context, executions.Execution, executions.Workload, *WorkspaceGitCredential) (WorkspaceMaterialization, error) {
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

type workspaceMaterializerFunc func(context.Context, executions.Execution, executions.Workload, *WorkspaceGitCredential) (WorkspaceMaterialization, error)

type workspaceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f workspaceRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (f workspaceMaterializerFunc) Materialize(
	ctx context.Context,
	execution executions.Execution,
	workload executions.Workload,
	credential *WorkspaceGitCredential,
) (WorkspaceMaterialization, error) {
	return f(ctx, execution, workload, credential)
}

type workspaceMaterializerInspector struct {
	materialize func(context.Context, executions.Execution, executions.Workload, *WorkspaceGitCredential) (WorkspaceMaterialization, error)
	inspect     func(context.Context, WorkspaceMaterialization) (WorkspaceInspection, error)
}

func (m workspaceMaterializerInspector) Materialize(
	ctx context.Context,
	execution executions.Execution,
	workload executions.Workload,
	credential *WorkspaceGitCredential,
) (WorkspaceMaterialization, error) {
	return m.materialize(ctx, execution, workload, credential)
}

func (m workspaceMaterializerInspector) Inspect(
	ctx context.Context,
	materialized WorkspaceMaterialization,
) (WorkspaceInspection, error) {
	return m.inspect(ctx, materialized)
}
