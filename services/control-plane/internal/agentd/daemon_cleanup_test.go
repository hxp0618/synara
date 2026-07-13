package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

type daemonCleanupWorkspace struct {
	cleanup func(context.Context, WorkspaceCleanupRequest) (WorkspaceCleanupResult, error)
}

func (d daemonCleanupWorkspace) Materialize(
	context.Context,
	executions.Execution,
	executions.Workload,
	*GitHTTPSCredential,
) (WorkspaceMaterialization, error) {
	return WorkspaceMaterialization{}, errors.New("Materialize was not expected")
}

func (d daemonCleanupWorkspace) CleanupWorkspace(
	ctx context.Context,
	request WorkspaceCleanupRequest,
) (WorkspaceCleanupResult, error) {
	return d.cleanup(ctx, request)
}

func TestDaemonWorkspaceCleanupStartsBeforeDeletingAndAcknowledgesAfterAbsence(t *testing.T) {
	claim := testDaemonWorkspaceCleanupClaim()
	var mu sync.Mutex
	actions := make([]string, 0, 3)
	record := func(action string) {
		mu.Lock()
		defer mu.Unlock()
		actions = append(actions, action)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case workspaceCleanupPath(claim.CleanupID, "started"):
			record("started")
		case workspaceCleanupPath(claim.CleanupID, "acknowledged"):
			record("acknowledged")
		default:
			t.Fatalf("unexpected Workspace cleanup request %s", request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{}`))
	}))
	defer server.Close()

	daemon := testDaemonForWorkspaceCleanup(t, server.URL, daemonCleanupWorkspace{
		cleanup: func(_ context.Context, request WorkspaceCleanupRequest) (WorkspaceCleanupResult, error) {
			record("cleanup")
			if request.MaterializationID != claim.MaterializationID || request.IncarnationID != claim.IncarnationID ||
				request.DispatchGeneration != claim.DispatchGeneration {
				t.Fatalf("cleanup request = %#v, claim = %#v", request, claim)
			}
			return WorkspaceCleanupResult{Status: WorkspaceCleanupDeleted}, nil
		},
	})

	if err := daemon.runWorkspaceCleanup(context.Background(), claim); err != nil {
		t.Fatalf("run Workspace cleanup: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if expected := []string{"started", "cleanup", "acknowledged"}; !reflect.DeepEqual(actions, expected) {
		t.Fatalf("actions = %v, want %v", actions, expected)
	}
}

func TestDaemonWorkspaceCleanupReportsRetryableFilesystemFailure(t *testing.T) {
	claim := testDaemonWorkspaceCleanupClaim()
	var failure executions.WorkspaceCleanupFailedInput
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case workspaceCleanupPath(claim.CleanupID, "started"):
		case workspaceCleanupPath(claim.CleanupID, "failed"):
			if err := json.NewDecoder(request.Body).Decode(&failure); err != nil {
				t.Fatalf("decode failure: %v", err)
			}
		default:
			t.Fatalf("unexpected Workspace cleanup request %s", request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{}`))
	}))
	defer server.Close()

	daemon := testDaemonForWorkspaceCleanup(t, server.URL, daemonCleanupWorkspace{
		cleanup: func(context.Context, WorkspaceCleanupRequest) (WorkspaceCleanupResult, error) {
			return WorkspaceCleanupResult{}, &WorkspaceCleanupError{
				Code: "workspace_cleanup_lock_busy", Message: "The Workspace lock is busy.", Retryable: true,
			}
		},
	})

	if err := daemon.runWorkspaceCleanup(context.Background(), claim); err == nil {
		t.Fatal("Workspace cleanup unexpectedly succeeded")
	}
	if failure.ErrorCode != "workspace_cleanup_lock_busy" || failure.ErrorMessage != "The Workspace lock is busy." || !failure.Retryable {
		t.Fatalf("failure = %#v", failure)
	}
	if failure.DispatchGeneration != claim.DispatchGeneration || failure.LeaseToken != claim.Lease.LeaseToken {
		t.Fatalf("failure lease = %#v, claim lease = %#v", failure.WorkspaceCleanupLeaseInput, claim.Lease)
	}
}

func TestWorkspaceCleanupRenewalsUseDistinctRequestReceipts(t *testing.T) {
	claim := testDaemonWorkspaceCleanupClaim()
	requestIDs := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestIDs = append(requestIDs, request.Header.Get("X-Request-ID"))
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{}`))
	}))
	defer server.Close()

	daemon := testDaemonForWorkspaceCleanup(t, server.URL, daemonCleanupWorkspace{
		cleanup: func(context.Context, WorkspaceCleanupRequest) (WorkspaceCleanupResult, error) {
			return WorkspaceCleanupResult{}, nil
		},
	})
	if err := daemon.client.RenewWorkspaceCleanup(context.Background(), claim); err != nil {
		t.Fatalf("first renewal: %v", err)
	}
	if err := daemon.client.RenewWorkspaceCleanup(context.Background(), claim); err != nil {
		t.Fatalf("second renewal: %v", err)
	}
	if len(requestIDs) != 2 || requestIDs[0] == "" || requestIDs[0] == requestIDs[1] {
		t.Fatalf("renew request IDs = %v", requestIDs)
	}
}

func TestWorkspaceCleanupClaimScheduleProvidesBoundedFairness(t *testing.T) {
	startedAt := time.Now().UTC()
	schedule := newWorkspaceCleanupClaimSchedule(startedAt)
	if schedule.due(startedAt) {
		t.Fatal("Workspace cleanup fairness probe was due before any Execution completed")
	}
	for completed := 1; completed < workspaceCleanupProbeExecutionInterval; completed++ {
		schedule.recordExecution()
		if schedule.due(startedAt.Add(time.Duration(completed) * time.Second)) {
			t.Fatalf("Workspace cleanup fairness probe was due after only %d Executions", completed)
		}
	}
	schedule.recordExecution()
	if !schedule.due(startedAt.Add(4 * time.Second)) {
		t.Fatalf("Workspace cleanup fairness probe was not due after %d Executions", workspaceCleanupProbeExecutionInterval)
	}

	probeAt := startedAt.Add(4 * time.Second)
	schedule.recordProbe(probeAt)
	if schedule.due(probeAt) {
		t.Fatal("a completed cleanup probe did not restore Execution priority")
	}
	if schedule.due(probeAt.Add(workspaceCleanupProbeMaximumInterval - time.Nanosecond)) {
		t.Fatal("Workspace cleanup fairness probe ran before the maximum interval")
	}
	if !schedule.due(probeAt.Add(workspaceCleanupProbeMaximumInterval)) {
		t.Fatal("Workspace cleanup fairness probe was not due at the maximum interval")
	}
}

func TestAssignedExecutionDaemonNeverClaimsWorkspaceCleanup(t *testing.T) {
	assignedExecutionID := uuid.New()
	if workspaceCleanupClaimsEnabled(Config{AssignedExecutionID: &assignedExecutionID}) {
		t.Fatal("an assigned-execution Worker was allowed to claim Workspace cleanup")
	}
	if !workspaceCleanupClaimsEnabled(Config{}) {
		t.Fatal("a general Worker was prevented from claiming Workspace cleanup")
	}
}

func TestDaemonProbesWorkspaceCleanupAfterBoundedContinuousExecutions(t *testing.T) {
	workerID := uuid.New()
	targetID := uuid.New()
	tenantID := uuid.New()
	executionID := uuid.New()
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 1, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	execution := executions.Execution{
		ID: executionID, TenantID: tenantID, SessionID: uuid.New(), TurnID: uuid.New(),
		Status: "leased", ExecutionTargetID: targetID, TargetKind: "local", Generation: 1,
	}
	var mu sync.Mutex
	executionClaims := 0
	cleanupProbeAt := -1
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/workers/register":
			_ = json.NewEncoder(response).Encode(executions.RegisteredWorker{
				Worker: executions.Worker{
					ID: workerID, ExecutionTargetID: targetID, TargetKind: "local", Status: "online",
					ProtocolVersion: executions.WorkerProtocolVersion,
				},
				Token: "worker-token",
			})
		case "/v1/workers/heartbeat":
			response.WriteHeader(http.StatusNoContent)
		case "/v1/workers/executions/claim":
			mu.Lock()
			executionClaims++
			claimNumber := executionClaims
			mu.Unlock()
			if claimNumber <= workspaceCleanupProbeExecutionInterval {
				_ = json.NewEncoder(response).Encode(executions.ClaimResult{Execution: &execution, Lease: &lease})
				return
			}
			_ = json.NewEncoder(response).Encode(executions.ClaimResult{})
		case "/v1/workers/executions/" + executionID.String() + "/release":
			response.WriteHeader(http.StatusNoContent)
		case "/v1/workers/workspace-cleanups/claim":
			mu.Lock()
			if cleanupProbeAt < 0 {
				cleanupProbeAt = executionClaims
			}
			mu.Unlock()
			_ = json.NewEncoder(response).Encode(executions.WorkspaceCleanupClaimResult{})
			cancel()
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	defer cancel()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	config := Config{
		ControlPlaneURL: parsed, RegistrationToken: "registration-token",
		ExecutionTargetID: targetID, TargetKind: platform.TargetLocal,
		RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: workspaceRoot,
		GitCacheRoot: filepath.Join(t.TempDir(), "git-cache"), PollInterval: time.Millisecond,
		HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour, DrainTimeout: 10 * time.Millisecond,
		RequestTimeout: time.Second, ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
	done := make(chan error, 1)
	go func() {
		done <- NewDaemon(config, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon fairness run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop after the fairness probe")
	}
	mu.Lock()
	defer mu.Unlock()
	if cleanupProbeAt != workspaceCleanupProbeExecutionInterval {
		t.Fatalf("Workspace cleanup probe ran after %d Execution claims, want %d", cleanupProbeAt, workspaceCleanupProbeExecutionInterval)
	}
}

func testDaemonForWorkspaceCleanup(
	t *testing.T,
	serverURL string,
	workspace workspaceMaterializer,
) *Daemon {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	config := Config{
		ControlPlaneURL: parsed, RequestTimeout: time.Second, ArtifactTimeout: time.Second,
		LeaseRenewInterval: time.Hour,
	}
	client := NewClient(config)
	client.workerToken = "worker-token"
	return &Daemon{
		config: config, client: client, workspace: workspace,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func testDaemonWorkspaceCleanupClaim() executions.WorkspaceCleanupClaim {
	now := time.Now().UTC()
	cleanupID := uuid.New()
	return executions.WorkspaceCleanupClaim{
		CleanupID: cleanupID, TenantID: uuid.New(), OrganizationID: uuid.New(), ProjectID: uuid.New(),
		SessionID: uuid.New(), LogicalWorkspaceID: uuid.New(), MaterializationID: uuid.New(), IncarnationID: uuid.New(),
		ExecutionTargetID: uuid.New(), TargetKind: "docker", StorageScope: "target", LayoutVersion: workspaceLayoutV3,
		Reason: "retention-expired", DispatchGeneration: 3,
		Lease: executions.WorkspaceCleanupLease{
			CleanupID: cleanupID, DispatchGeneration: 3, LeaseToken: "cleanup-token", ExpiresAt: now.Add(time.Minute),
		},
	}
}
