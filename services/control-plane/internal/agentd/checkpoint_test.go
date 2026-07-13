package agentd

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestWorkspaceSnapshotCaptureAndRestorePreservesVerifiedFiles(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "tenant", "project", "session", "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "note.txt"), []byte("durable content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "run.sh"), []byte("#!/bin/sh\necho restored\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	execution := executions.Execution{ID: uuid.New(), Generation: 3}
	materialized := WorkspaceMaterialization{Directory: workspace, Managed: true}
	candidate, err := captureWorkspaceCheckpoint(execution, materialized, WorkspaceInspection{Dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	if candidate.Strategy != "snapshot" || candidate.Artifact == nil || candidate.FileCount != 2 {
		t.Fatalf("unexpected Snapshot candidate: %#v", candidate)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "note.txt"), []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "extra.txt"), []byte("remove me"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactID := uuid.New()
	sha := strings.Repeat("a", 64)
	checkpoint := executions.WorkspaceCheckpoint{
		ID: uuid.New(), Strategy: "snapshot", Status: "ready", ArtifactID: &artifactID,
		SHA256: &sha, Manifest: candidate.Manifest, FileCount: &candidate.FileCount,
		TotalBytes: &candidate.TotalBytes,
	}
	restored, err := NewWorkspaceMaterializer(root).Restore(
		context.Background(), materialized, checkpoint, candidate.ArtifactPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RestoredCheckpointID == nil || *restored.RestoredCheckpointID != checkpoint.ID {
		t.Fatalf("restore did not report the applied Checkpoint: %#v", restored.RestoredCheckpointID)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "nested", "note.txt"))
	if err != nil || string(content) != "durable content\n" {
		t.Fatalf("Snapshot content was not restored: %q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("Snapshot restore retained an unexpected file: %v", err)
	}
	info, err := os.Stat(filepath.Join(workspace, "run.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("Snapshot executable mode was not restored: mode=%v err=%v", info.Mode(), err)
	}
}

func TestWorkspaceSnapshotRestoreRejectsTraversalArchive(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "tenant", "project", "session", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "malicious.tar")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(archive)
	payload := []byte("escape")
	if err := writer.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o600, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	artifactID := uuid.New()
	sha := strings.Repeat("b", 64)
	fileCount := 1
	totalBytes := int64(len(payload))
	checkpoint := executions.WorkspaceCheckpoint{
		ID: uuid.New(), Strategy: "snapshot", Status: "ready", ArtifactID: &artifactID,
		SHA256: &sha, FileCount: &fileCount, TotalBytes: &totalBytes,
		Manifest: map[string]any{
			"format": "synara-workspace-snapshot-v1",
			"files": []map[string]any{{
				"path": "safe.txt", "size": len(payload), "sha256": strings.Repeat("c", 64),
				"executable": false,
			}},
		},
	}
	if _, err := NewWorkspaceMaterializer(root).Restore(
		context.Background(), WorkspaceMaterialization{Directory: workspace, Managed: true}, checkpoint, archivePath,
	); err == nil {
		t.Fatal("Snapshot restore accepted a traversal archive")
	}
	if _, err := os.Stat(filepath.Join(root, "tenant", "project", "session", "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("Traversal archive wrote outside the Workspace: %v", err)
	}
}

func TestWorkspaceInspectionDetectsCommittedHeadChange(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, repository, "init", "-b", "main")
	runGitTestCommand(t, repository, "config", "user.email", "synara@example.com")
	runGitTestCommand(t, repository, "config", "user.name", "Synara Test")
	if err := os.WriteFile(filepath.Join(repository, "file.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, repository, "add", "file.txt")
	runGitTestCommand(t, repository, "commit", "-m", "first")
	initialHead := runGitTestCommand(t, repository, "rev-parse", "HEAD")
	branch := "main"
	if err := os.WriteFile(filepath.Join(repository, "file.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, repository, "add", "file.txt")
	runGitTestCommand(t, repository, "commit", "-m", "second")
	inspection, err := NewWorkspaceMaterializer(root).Inspect(context.Background(), WorkspaceMaterialization{
		Directory: repository, Managed: true, CurrentBranch: &branch, HeadCommit: &initialHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Dirty || inspection.HeadCommit == nil || *inspection.HeadCommit == initialHead {
		t.Fatalf("committed HEAD change was not detected: %#v", inspection)
	}
}

func TestUploadCheckpointArtifactRecoversCompletedArtifactAfterResponseLoss(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	checkpointID := uuid.New()
	artifactID := uuid.New()
	payload := []byte("durable checkpoint payload")
	artifactPath := filepath.Join(t.TempDir(), "workspace.tar")
	if err := os.WriteFile(artifactPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	sha := hex.EncodeToString(digest[:])
	var createRequestIDs []string
	createCalls, uploadCalls, completeCalls := 0, 0, 0
	ready := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		base := "/v1/workers/executions/" + executionID.String() + "/"
		switch request.URL.Path {
		case base + "artifacts":
			createCalls++
			createRequestIDs = append(createRequestIDs, request.Header.Get("X-Request-ID"))
			var input artifacts.WorkerCreateInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.CheckpointID == nil || *input.CheckpointID != checkpointID {
				http.Error(response, "invalid Checkpoint Artifact create", http.StatusBadRequest)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			if ready {
				_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`","status":"ready","sizeBytes":`+
					fmt.Sprint(len(payload))+`,"sha256":"`+sha+`","contentType":"application/x-tar"},"uploadRequired":false}`)
				return
			}
			_, _ = io.WriteString(response, `{"artifact":{"id":"`+artifactID.String()+`","status":"pending"},"uploadRequired":true,"method":"PUT","url":"`+
				server.URL+`/checkpoint-upload","headers":{},"expiresAt":"2030-01-01T00:00:00Z"}`)
		case "/checkpoint-upload":
			uploadCalls++
			uploaded, _ := io.ReadAll(request.Body)
			if string(uploaded) != string(payload) {
				http.Error(response, "payload mismatch", http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		case base + "artifacts/" + artifactID.String() + "/complete":
			completeCalls++
			var input artifacts.WorkerCompleteInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.SHA256 != sha || input.SizeBytes != int64(len(payload)) {
				http.Error(response, "invalid Artifact complete", http.StatusBadRequest)
				return
			}
			ready = true
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"id":"`+artifactID.String()+`"`)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: controlPlaneURL, RequestTimeout: time.Second, ArtifactTimeout: time.Second})
	client.workerToken = "worker-token"
	lease := executions.Lease{
		ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID,
		Generation: 4, LeaseToken: "lease-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	candidate := WorkspaceCheckpointCandidate{
		IdempotencyKey: "response-loss", ArtifactPath: artifactPath,
		Artifact: &RunnerArtifact{Kind: "workspace_snapshot", OriginalName: "workspace.tar", ContentType: "application/x-tar"},
	}
	completed, err := client.UploadCheckpointArtifact(
		context.Background(), executionID, lease, executions.WorkspaceCheckpoint{ID: checkpointID}, candidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ID != artifactID || completed.Status != "ready" {
		t.Fatalf("unexpected recovered Artifact: %#v", completed)
	}
	if createCalls != 2 || uploadCalls != 1 || completeCalls != 1 {
		t.Fatalf("unexpected recovery calls: create=%d upload=%d complete=%d", createCalls, uploadCalls, completeCalls)
	}
	if len(createRequestIDs) != 2 || createRequestIDs[0] == "" || createRequestIDs[0] != createRequestIDs[1] {
		t.Fatalf("Checkpoint Artifact create did not reuse a stable request ID: %#v", createRequestIDs)
	}
}

func TestVerifyReadyArtifactFileRejectsLocalDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace.tar")
	if err := os.WriteFile(path, []byte("new payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	size := int64(len("old payload"))
	sha := strings.Repeat("a", 64)
	contentType := "application/x-tar"
	err := verifyReadyArtifactFile(path, contentType, artifacts.Artifact{
		Status: "ready", SizeBytes: &size, SHA256: &sha, ContentType: &contentType,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ready Artifact local drift was not rejected: %v", err)
	}
}

func runGitTestCommand(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
