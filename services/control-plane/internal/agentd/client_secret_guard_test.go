package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

type artifactRoundTripFunc func(*http.Request) (*http.Response, error)

func (f artifactRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestUploadCheckpointArtifactBlocksRegisteredSecretBeforeCreate(t *testing.T) {
	secret := "checkpoint-secret-123456"
	guard := executionGuardForSecretTest(t, secret)
	ctx := withExecutionSecretGuard(context.Background(), guard)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		http.Error(response, "Checkpoint Artifact must be blocked before create", http.StatusInternalServerError)
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workspace.tar")
	if err := os.WriteFile(path, []byte("tar-prefix\x00"+secret+"\x00tar-suffix"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: time.Second, ArtifactTimeout: time.Second})
	lease := secretGuardClientLease()
	_, err = client.UploadCheckpointArtifact(ctx, lease.ExecutionID, lease, executions.WorkspaceCheckpoint{
		ID: uuid.New(),
	}, WorkspaceCheckpointCandidate{
		IdempotencyKey: "secret-checkpoint", ArtifactPath: path,
		Artifact: &RunnerArtifact{
			Kind: "workspace_snapshot", OriginalName: "workspace.tar", ContentType: "application/x-tar",
		},
	})
	if !secretguard.IsExposure(err) {
		t.Fatalf("Checkpoint secret error = %T %v", err, err)
	}
	if requests != 0 {
		t.Fatalf("blocked Checkpoint Artifact made %d control-plane requests", requests)
	}
}

func TestMarkWorkspaceCheckpointFailedSanitizesFailure(t *testing.T) {
	secret := "checkpoint-error-secret-123456"
	guard := executionGuardForSecretTest(t, secret)
	ctx := withExecutionSecretGuard(context.Background(), guard)
	var persisted executions.WorkspaceCheckpointFailedInput
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&persisted); err != nil {
			http.Error(response, "invalid failure", http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: time.Second})
	lease := secretGuardClientLease()
	err = client.MarkWorkspaceCheckpointFailed(
		ctx,
		lease.ExecutionID,
		lease,
		WorkspaceCheckpointCandidate{IdempotencyKey: "failed-secret-checkpoint"},
		executions.WorkspaceCheckpoint{ID: uuid.New()},
		errors.New("upload failed with "+secret),
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted.FailureMessage, secret) ||
		!strings.Contains(persisted.FailureMessage, secretguard.RedactionMarker) {
		t.Fatalf("persisted Checkpoint failure was not sanitized: %q", persisted.FailureMessage)
	}
}

func TestArtifactUploadErrorsStripPresignedCredentials(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		transport artifactRoundTripFunc
	}{
		{
			name: "transport",
			transport: func(request *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("dial failed for %s", request.URL.String())
			},
		},
		{
			name: "status-body",
			transport: func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						"denied https://upload-user:upload-password@upload.example.test/object?X-Amz-Signature=query-secret",
					)),
					Request: request,
				}, nil
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			artifactID := uuid.New()
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(response).Encode(map[string]any{
					"artifact":       map[string]any{"id": artifactID.String(), "status": "pending"},
					"uploadRequired": true,
					"method":         http.MethodPut,
					"url": "https://upload-user:upload-password@upload.example.test/object" +
						"?X-Amz-Credential=query-secret&X-Amz-Signature=signature-secret",
					"headers":   map[string]string{},
					"expiresAt": "2030-01-01T00:00:00Z",
				})
			}))
			defer server.Close()
			baseURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: time.Second, ArtifactTimeout: time.Second})
			client.uploadHTTP = &http.Client{Transport: testCase.transport, Timeout: time.Second}
			path := filepath.Join(t.TempDir(), "report.txt")
			if err := os.WriteFile(path, []byte("safe Artifact"), 0o600); err != nil {
				t.Fatal(err)
			}
			lease := secretGuardClientLease()
			_, err = client.UploadArtifact(context.Background(), lease.ExecutionID, lease, RunnerArtifact{
				Path: "report.txt", Kind: "generated_file", ContentType: "text/plain",
			}, path)
			if err == nil {
				t.Fatal("Artifact upload unexpectedly succeeded")
			}
			message := err.Error()
			for _, unsafe := range []string{
				"upload-user", "upload-password", "query-secret", "signature-secret", "X-Amz-", "?",
			} {
				if strings.Contains(message, unsafe) {
					t.Fatalf("Artifact error retained %q: %s", unsafe, message)
				}
			}
		})
	}
}

func secretGuardClientLease() executions.Lease {
	return executions.Lease{
		ExecutionID: uuid.New(), TenantID: uuid.New(), WorkerID: uuid.New(),
		Generation: 7, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
}
