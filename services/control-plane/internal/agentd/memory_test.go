package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestDownloadMemoryArtifactVerifiesFrozenIdentityHashAndContent(t *testing.T) {
	payload := []byte("Prefer concise answers across rebuilt Pods.")
	digest := sha256.Sum256(payload)
	sha := hex.EncodeToString(digest[:])
	executionID := uuid.New()
	reference := executions.RecoveryMemoryReference{
		Scope: "session", ScopeID: uuid.New(), HeadID: uuid.New(), MemoryKey: "instructions",
		RevisionID: uuid.New(), ArtifactID: uuid.New(), SHA256: sha,
		MediaType: "text/markdown", SizeBytes: int64(len(payload)),
	}
	lease := executions.Lease{
		TenantID: uuid.New(), Generation: 4, LeaseToken: "lease-token",
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/artifact/download"):
			size := int64(len(payload))
			contentType := "text/markdown; charset=utf-8"
			_ = json.NewEncoder(response).Encode(artifacts.DownloadGrant{
				Artifact: artifacts.Artifact{
					ID: reference.ArtifactID, SizeBytes: &size, SHA256: &sha, ContentType: &contentType,
				},
				URL: server.URL + "/memory-content", ExpiresAt: time.Now().Add(time.Minute),
			})
		case request.Method == http.MethodGet && request.URL.Path == "/memory-content":
			_, _ = response.Write(payload)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{
		baseURL: baseURL, http: server.Client(), uploadHTTP: server.Client(), workerToken: "worker-token",
	}
	document, err := client.DownloadMemoryArtifact(context.Background(), executionID, lease, reference)
	if err != nil {
		t.Fatal(err)
	}
	if document.Content != string(payload) || document.ContentType != "text/markdown" ||
		document.RevisionID != reference.RevisionID || document.SHA256 != reference.SHA256 {
		t.Fatalf("unexpected verified Memory document: %#v", document)
	}

	tampered := reference
	tampered.SHA256 = strings.Repeat("f", 64)
	if _, err := client.DownloadMemoryArtifact(context.Background(), executionID, lease, tampered); err == nil {
		t.Fatal("tampered Recovery Bundle Memory SHA accepted the Artifact grant")
	}

	tampered = reference
	tampered.SizeBytes++
	if _, err := client.DownloadMemoryArtifact(context.Background(), executionID, lease, tampered); err == nil {
		t.Fatal("tampered Recovery Bundle Memory size accepted the Artifact grant")
	}

	tampered = reference
	tampered.MediaType = "text/plain"
	if _, err := client.DownloadMemoryArtifact(context.Background(), executionID, lease, tampered); err == nil {
		t.Fatal("tampered Recovery Bundle Memory media type accepted the Artifact grant")
	}
}
