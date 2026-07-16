package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

type daemonDiffArtifactTestState struct {
	sync.Mutex
	artifactCreate     artifacts.WorkerCreateInput
	artifactComplete   artifacts.WorkerCompleteInput
	uploadBody         []byte
	uploadContentType  string
	runtimeEvents      []executions.RuntimeEventInput
	artifactCreates    int
	artifactUploads    int
	artifactCompletes  int
	executionCompleted bool
	failure            *executions.FailExecutionInput
}

func TestDaemonRunExecutionUploadsGuardedDiffAndAppendsArtifactReference(t *testing.T) {
	state, artifactID, err := runDaemonDiffArtifactScenario(
		t,
		"diff-artifact-secret",
		&RunnerCredential{Payload: map[string]any{"apiKey": "provider-secret"}},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	state.Lock()
	defer state.Unlock()
	if state.artifactCreates != 1 || state.artifactUploads != 1 || state.artifactCompletes != 1 {
		t.Fatalf(
			"Diff Artifact lifecycle = create:%d upload:%d complete:%d",
			state.artifactCreates,
			state.artifactUploads,
			state.artifactCompletes,
		)
	}
	if state.artifactCreate.Kind != "diff" || state.artifactCreate.OriginalName == nil ||
		*state.artifactCreate.OriginalName != "turn.diff" {
		t.Fatalf("unexpected Diff Artifact create payload: %#v", state.artifactCreate)
	}
	if state.uploadContentType != "text/x-diff; charset=utf-8" ||
		bytes.Contains(state.uploadBody, []byte("provider-secret")) ||
		!bytes.Contains(state.uploadBody, []byte(secretguard.RedactionMarker)) {
		t.Fatalf("Diff Artifact was not safely staged before upload: contentType=%q body=%q",
			state.uploadContentType, state.uploadBody)
	}
	digest := sha256.Sum256(state.uploadBody)
	if state.artifactComplete.SizeBytes != int64(len(state.uploadBody)) ||
		state.artifactComplete.SHA256 != hex.EncodeToString(digest[:]) ||
		state.artifactComplete.ContentType != state.uploadContentType {
		t.Fatalf("unexpected Diff Artifact complete payload: %#v", state.artifactComplete)
	}
	if len(state.runtimeEvents) != 1 {
		t.Fatalf("expected one Diff reference Event, got %d", len(state.runtimeEvents))
	}
	event := state.runtimeEvents[0]
	if event.EventVersion != executions.RuntimeEventVersionV2 || event.EventType != "turn.diff.updated" ||
		event.EventID != uuid.NewSHA1(turnDiffArtifactEventNamespace, artifactID[:]) {
		t.Fatalf("unexpected Diff reference Event envelope: %#v", event)
	}
	reference, ok := event.Payload["artifact"].(map[string]any)
	if !ok || reference["artifactId"] != artifactID.String() ||
		reference["contentType"] != state.uploadContentType || reference["sha256"] != state.artifactComplete.SHA256 {
		t.Fatalf("unexpected Diff Artifact reference: %#v", event.Payload)
	}
	for key, want := range map[string]int{
		"sizeBytes": len(state.uploadBody), "fileCount": providerHostTestDiffFileCount,
		"additions": providerHostTestDiffAdditions, "deletions": providerHostTestDiffDeletions,
	} {
		value, valid := integerField(reference, key)
		if !valid || value != want {
			t.Fatalf("Diff Artifact reference %s = %d, %t; want %d", key, value, valid, want)
		}
	}
	encodedEvent, marshalErr := json.Marshal(event.Payload)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if bytes.Contains(encodedEvent, []byte("provider-diffs")) || bytes.Contains(encodedEvent, []byte("provider-secret")) {
		t.Fatalf("Diff path or credential leaked into Runtime Event: %s", encodedEvent)
	}
	if !state.executionCompleted || state.failure != nil {
		t.Fatalf("execution did not complete after Diff reference persistence: completed=%t failure=%#v",
			state.executionCompleted, state.failure)
	}
}

func TestDaemonRunExecutionRejectsSymlinkDiffSource(t *testing.T) {
	state, _, err := runDaemonDiffArtifactScenario(t, "diff-artifact-symlink", nil, "")
	if err == nil || !strings.Contains(err.Error(), "runner artifact must be a regular file") {
		t.Fatalf("symlink Diff error = %v", err)
	}
	state.Lock()
	defer state.Unlock()
	if state.artifactCreates != 0 || state.artifactUploads != 0 || state.artifactCompletes != 0 ||
		len(state.runtimeEvents) != 0 || state.executionCompleted || state.failure == nil {
		t.Fatalf("unsafe Diff crossed the bound Runtime Output boundary: %#v", state)
	}
}

func TestDaemonRunExecutionRejectsIncompleteReadyDiffArtifact(t *testing.T) {
	state, _, err := runDaemonDiffArtifactScenario(t, "diff-artifact", nil, "missing-sha")
	if err == nil || !strings.Contains(err.Error(), "ready Diff Artifact is missing reference metadata") {
		t.Fatalf("incomplete ready Diff error = %v", err)
	}
	state.Lock()
	defer state.Unlock()
	if state.artifactCreates != 1 || state.artifactUploads != 1 || state.artifactCompletes != 1 ||
		len(state.runtimeEvents) != 0 || state.executionCompleted || state.failure == nil {
		t.Fatalf("incomplete ready Diff was projected into the Session: %#v", state)
	}
}

func TestDaemonRunExecutionRejectsMismatchedReadyDiffArtifact(t *testing.T) {
	state, _, err := runDaemonDiffArtifactScenario(t, "diff-artifact", nil, "mismatched-sha")
	if err == nil || !strings.Contains(err.Error(), "ready Artifact does not match the current local payload") {
		t.Fatalf("mismatched ready Diff error = %v", err)
	}
	state.Lock()
	defer state.Unlock()
	if state.artifactCreates != 1 || state.artifactUploads != 1 || state.artifactCompletes != 1 ||
		len(state.runtimeEvents) != 0 || state.executionCompleted || state.failure == nil {
		t.Fatalf("mismatched ready Diff was projected into the Session: %#v", state)
	}
}

func runDaemonDiffArtifactScenario(
	t *testing.T,
	mode string,
	credential *RunnerCredential,
	readyMode string,
) (*daemonDiffArtifactTestState, uuid.UUID, error) {
	t.Helper()
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", mode)
	executionID := uuid.New()
	tenantID := uuid.New()
	turnID := uuid.New()
	artifactID := uuid.New()
	providerCredentialID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: uuid.New(),
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	state := &daemonDiffArtifactTestState{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/diff-upload" {
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(response, "read upload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.artifactUploads++
			state.uploadBody = append([]byte(nil), body...)
			state.uploadContentType = request.Header.Get("Content-Type")
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Header.Get("Authorization") != "Bearer worker-token" {
			http.Error(response, "missing Worker token", http.StatusUnauthorized)
			return
		}
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch request.URL.Path {
		case base + "start":
			response.WriteHeader(http.StatusNoContent)
		case base + "credentials/" + providerCredentialID.String() + "/resolve":
			if credential == nil {
				http.Error(response, "unexpected Provider Credential resolution", http.StatusNotFound)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(credential)
		case base + "control-commands/pull", base + "interaction-resolutions/pull":
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{"items": []any{}})
		case base + "artifacts":
			var input artifacts.WorkerCreateInput
			if decodeErr := json.NewDecoder(request.Body).Decode(&input); decodeErr != nil {
				http.Error(response, "invalid Artifact create payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.artifactCreates++
			state.artifactCreate = input
			state.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(artifacts.UploadGrant{
				Artifact:       artifacts.Artifact{ID: artifactID, Kind: "diff", Status: "pending"},
				UploadRequired: true, Method: http.MethodPut, URL: server.URL + "/diff-upload",
			})
		case base + "artifacts/" + artifactID.String() + "/complete":
			var input artifacts.WorkerCompleteInput
			if decodeErr := json.NewDecoder(request.Body).Decode(&input); decodeErr != nil {
				http.Error(response, "invalid Artifact complete payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.artifactCompletes++
			state.artifactComplete = input
			state.Unlock()
			originalName := "turn.diff"
			contentType, sizeBytes, digest := input.ContentType, input.SizeBytes, input.SHA256
			ready := artifacts.Artifact{
				ID: artifactID, Kind: "diff", Status: "ready", OriginalName: &originalName,
				ContentType: &contentType, SizeBytes: &sizeBytes, SHA256: &digest,
			}
			if readyMode == "missing-sha" {
				ready.SHA256 = nil
			} else if readyMode == "mismatched-sha" {
				mismatched := strings.Repeat("f", 64)
				ready.SHA256 = &mismatched
			}
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(ready)
		case base + "events":
			var input executions.RuntimeEventInput
			if decodeErr := json.NewDecoder(request.Body).Decode(&input); decodeErr != nil {
				http.Error(response, "invalid Runtime Event", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.runtimeEvents = append(state.runtimeEvents, input)
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "complete":
			state.Lock()
			state.executionCompleted = true
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case base + "fail":
			var input executions.FailExecutionInput
			if decodeErr := json.NewDecoder(request.Body).Decode(&input); decodeErr != nil {
				http.Error(response, "invalid failure payload", http.StatusBadRequest)
				return
			}
			state.Lock()
			state.failure = &input
			state.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	controlPlaneURL, parseErr := url.Parse(server.URL)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	cfg := Config{
		ControlPlaneURL: controlPlaneURL, TargetKind: platform.TargetLocal,
		RunnerProtocol: RunnerProtocolV2, WorkspaceRoot: t.TempDir(), PollInterval: time.Millisecond,
		HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour, DrainTimeout: time.Second,
		RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
		ExperimentalProviders: []string{"codex"},
	}
	cfg.RunnerCommand = providerHostV2TestCommand()
	client := NewClient(cfg)
	client.workerToken = "worker-token"
	daemon := &Daemon{
		config: cfg, client: client, runner: NewRunner(cfg),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	execution := executions.Execution{ID: executionID, TurnID: turnID, Generation: lease.Generation, Status: "leased"}
	workload := executions.Workload{
		TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(),
		TurnID: turnID, Provider: "codex", InputText: "produce a large Diff",
	}
	if credential != nil {
		workload.ProviderCredentialID = &providerCredentialID
	}
	err := daemon.runExecution(context.Background(), execution, lease, workload, nil)
	return state, artifactID, err
}
