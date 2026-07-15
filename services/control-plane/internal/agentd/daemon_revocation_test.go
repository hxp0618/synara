package agentd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestDaemonStopsAfterWorkerTokenRevokedDuringClaim(t *testing.T) {
	var claimCalls atomic.Int64
	targetID := uuid.New()
	workerID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/workers/register":
			writeRevocationRegisteredWorker(t, response, workerID, targetID)
		case "/v1/workers/heartbeat":
			response.WriteHeader(http.StatusNoContent)
		case "/v1/workers/executions/claim":
			claimCalls.Add(1)
			writeWorkerRevocationProblem(response, http.StatusUnauthorized, "worker_token_revoked")
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	err := NewDaemon(
		revocationDaemonConfig(t, server.URL, targetID, time.Hour),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).Run(context.Background())
	if !isWorkerRevocationError(err) {
		t.Fatalf("Daemon.Run() error = %T %v, want Worker revocation", err, err)
	}
	if got := claimCalls.Load(); got != 1 {
		t.Fatalf("revoked Worker made %d Claim requests, want 1", got)
	}
}

func TestDaemonStopsAfterWorkerIdentityRevokedDuringHeartbeat(t *testing.T) {
	var heartbeatCalls atomic.Int64
	var claimCalls atomic.Int64
	targetID := uuid.New()
	workerID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/workers/register":
			writeRevocationRegisteredWorker(t, response, workerID, targetID)
		case "/v1/workers/heartbeat":
			heartbeatCalls.Add(1)
			writeWorkerRevocationProblem(response, http.StatusForbidden, "worker_identity_revoked")
		case "/v1/workers/executions/claim":
			claimCalls.Add(1)
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(executions.ClaimResult{})
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := NewDaemon(
		revocationDaemonConfig(t, server.URL, targetID, time.Millisecond),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).Run(ctx)
	if !isWorkerRevocationError(err) {
		t.Fatalf("Daemon.Run() error = %T %v, want Worker revocation", err, err)
	}
	if got := heartbeatCalls.Load(); got != 1 {
		t.Fatalf("revoked Worker made %d Heartbeat requests, want 1", got)
	}
	if got := claimCalls.Load(); got > 2 {
		t.Fatalf("Heartbeat revocation allowed %d extra Claim requests", got)
	}
}

func revocationDaemonConfig(
	t *testing.T,
	serverURL string,
	targetID uuid.UUID,
	heartbeatInterval time.Duration,
) Config {
	t.Helper()
	controlPlaneURL, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		ControlPlaneURL: controlPlaneURL, RegistrationToken: "registration-token",
		ExecutionTargetID: targetID, TargetKind: platform.TargetLocal,
		RunnerProtocol: RunnerProtocolV1, WorkspaceRoot: t.TempDir(),
		GitCacheRoot: filepath.Join(t.TempDir(), "git-cache"), PollInterval: time.Second,
		HeartbeatInterval: heartbeatInterval, LeaseRenewInterval: time.Hour,
		DrainTimeout: 10 * time.Millisecond, RequestTimeout: time.Second,
		ArtifactTimeout: time.Second, RunnerMessageBytes: 1 << 20,
	}
}

func writeRevocationRegisteredWorker(
	t *testing.T,
	response http.ResponseWriter,
	workerID uuid.UUID,
	targetID uuid.UUID,
) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(executions.RegisteredWorker{
		Worker: executions.Worker{
			ID: workerID, ExecutionTargetID: targetID, TargetKind: "local", Status: "online",
			ProtocolVersion: executions.WorkerProtocolVersion,
		},
		Token: "worker-token",
	}); err != nil {
		t.Errorf("encode registered Worker: %v", err)
	}
}

func writeWorkerRevocationProblem(response http.ResponseWriter, status int, code string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": "Worker authorization was revoked."},
	})
}
