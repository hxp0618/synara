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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

type recordingWorkerStorageScrubWorkspace struct {
	scrub func(context.Context, executions.WorkerStorageScrub) error
}

func (r recordingWorkerStorageScrubWorkspace) Materialize(
	context.Context,
	executions.Execution,
	executions.Workload,
	*WorkspaceGitCredential,
) (WorkspaceMaterialization, error) {
	return WorkspaceMaterialization{}, errors.New("Materialize was not expected")
}

func (r recordingWorkerStorageScrubWorkspace) ScrubTenantStorage(
	ctx context.Context,
	scrub executions.WorkerStorageScrub,
) error {
	return r.scrub(ctx, scrub)
}

func TestWorkspaceMaterializerScrubsExactTenantStorageWithoutFollowingSymlinks(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	targetID := uuid.New()
	tenantA := uuid.New()
	tenantB := uuid.New()
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, targetID)

	pathsA := []string{
		filepath.Join(workspaceRoot, "v2", targetID.String(), tenantA.String(), "project", "secret-a"),
		filepath.Join(workspaceRoot, "v3", targetID.String(), tenantA.String(), "project", "secret-a"),
		filepath.Join(workspaceRoot, tenantA.String(), "legacy-secret-a"),
		filepath.Join(cacheRoot, "v1", targetID.String(), tenantA.String(), "repo", "secret-a"),
	}
	pathsB := []string{
		filepath.Join(workspaceRoot, "v2", targetID.String(), tenantB.String(), "project", "secret-b"),
		filepath.Join(workspaceRoot, "v3", targetID.String(), tenantB.String(), "project", "secret-b"),
		filepath.Join(workspaceRoot, tenantB.String(), "legacy-secret-b"),
		filepath.Join(cacheRoot, "v1", targetID.String(), tenantB.String(), "repo", "secret-b"),
	}
	for _, name := range append(append([]string(nil), pathsA...), pathsB...) {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("tenant secret"), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range pathsB {
		if err := os.Chmod(name, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	external := filepath.Join(t.TempDir(), "outside-secret")
	if err := os.WriteFile(external, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(workspaceRoot, ".quarantine", "worker-storage-scrub")
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(quarantine, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".locks", "workspace-v3"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cacheRoot, ".locks", "git-cache-v1"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := materializer.ScrubTenantStorage(context.Background(), executions.WorkerStorageScrub{
		ID: uuid.New(), ExecutionTargetID: targetID, TenantID: tenantA,
		ScopeKind: "execution", ScopeID: uuid.New(), ScopeGeneration: 1, ScrubGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range pathsA {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("Tenant A residue remained at %s: %v", name, err)
		}
	}
	for _, name := range pathsB {
		if contents, err := os.ReadFile(name); err != nil || string(contents) != "tenant secret" {
			t.Fatalf("Tenant B storage changed at %s: contents=%q err=%v", name, contents, err)
		}
	}
	if contents, err := os.ReadFile(external); err != nil || string(contents) != "must survive" {
		t.Fatalf("scrub followed quarantine symlink: contents=%q err=%v", contents, err)
	}
	for _, name := range []string{filepath.Join(workspaceRoot, ".quarantine")} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("storage metadata residue remained at %s: %v", name, err)
		}
	}
	for _, name := range []string{filepath.Join(workspaceRoot, ".locks"), filepath.Join(cacheRoot, ".locks")} {
		if info, err := os.Stat(name); err != nil || !info.IsDir() {
			t.Fatalf("shared coordination locks changed at %s: info=%v err=%v", name, info, err)
		}
	}
}

func TestWorkerPrivateTempScrubRequiresExplicitContainerPrivateRoot(t *testing.T) {
	privateTempRoot := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateTempRoot, "secret"), []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(privateTempRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := scrubWorkerPrivateTempStorage(context.Background(), platform.TargetKubernetes, privateTempRoot); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(privateTempRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Worker-private temporary root after scrub = %#v, err=%v", entries, err)
	}
	if contents, err := os.ReadFile(external); err != nil || string(contents) != "outside" {
		t.Fatalf("temporary scrub followed symlink: contents=%q err=%v", contents, err)
	}
	if err := scrubWorkerPrivateTempStorage(context.Background(), platform.TargetKubernetes, ""); err == nil {
		t.Fatal("temporary scrub accepted an implicit root")
	}
	if err := scrubWorkerPrivateTempStorage(context.Background(), platform.TargetSSH, privateTempRoot); err == nil {
		t.Fatal("temporary scrub accepted a host-shared SSH Target")
	}
}

func TestDaemonAcknowledgesStorageScrubOnlyAfterWorkspaceAndTempAreAbsent(t *testing.T) {
	targetID := uuid.New()
	scrub := executions.WorkerStorageScrub{
		ID: uuid.New(), ExecutionTargetID: targetID, TenantID: uuid.New(), ScopeKind: "execution",
		ScopeID: uuid.New(), ScopeGeneration: 7, ScrubGeneration: 3, Status: "pending",
	}
	privateTempRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(privateTempRoot, "provider-secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	actions := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/workers/storage-scrubs/claim":
			_ = json.NewEncoder(response).Encode(executions.WorkerStorageScrubClaimResult{Scrub: &scrub})
		case workerStorageScrubPath(scrub.ID, "acknowledged"):
			if _, err := os.Lstat(filepath.Join(privateTempRoot, "provider-secret")); !os.IsNotExist(err) {
				t.Errorf("storage scrub was acknowledged before temporary residue disappeared: %v", err)
			}
			actions = append(actions, "acknowledged")
			_ = json.NewEncoder(response).Encode(scrub)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		ControlPlaneURL: parsed, ExecutionTargetID: targetID, TargetKind: platform.TargetDocker,
		PrivateTempRoot: privateTempRoot, RequestTimeout: time.Second, ArtifactTimeout: time.Second,
	}
	client := NewClient(config)
	client.workerToken = "worker-token"
	daemon := &Daemon{
		config: config, client: client,
		workspace: recordingWorkerStorageScrubWorkspace{scrub: func(_ context.Context, actual executions.WorkerStorageScrub) error {
			if actual.ID != scrub.ID || actual.ScrubGeneration != scrub.ScrubGeneration {
				t.Fatalf("claimed storage scrub = %#v", actual)
			}
			actions = append(actions, "workspace-scrubbed")
			return nil
		}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	claimed, err := daemon.claimAndRunWorkerStorageScrub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !claimed || !reflect.DeepEqual(actions, []string{"workspace-scrubbed", "acknowledged"}) {
		t.Fatalf("storage scrub actions = %#v, claimed=%v", actions, claimed)
	}
}

func TestWorkerStorageScrubPreventsNextTenantProcessReadingResidue(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	privateTempRoot := t.TempDir()
	targetID, tenantA := uuid.New(), uuid.New()
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, targetID)
	secretPaths := []string{
		filepath.Join(workspaceRoot, "v3", targetID.String(), tenantA.String(), "project", "workspace-secret"),
		filepath.Join(cacheRoot, "v1", targetID.String(), tenantA.String(), "repo", "cache-secret"),
		filepath.Join(privateTempRoot, "provider-temp-secret"),
	}
	for _, name := range secretPaths {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("tenant-a-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scrub := executions.WorkerStorageScrub{
		ID: uuid.New(), ExecutionTargetID: targetID, TenantID: tenantA, ScopeKind: "execution",
		ScopeID: uuid.New(), ScopeGeneration: 1, ScrubGeneration: 1, Status: "pending",
	}
	if err := materializer.ScrubTenantStorage(context.Background(), scrub); err != nil {
		t.Fatal(err)
	}
	if err := scrubWorkerPrivateTempStorage(context.Background(), platform.TargetDocker, privateTempRoot); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestWorkerStorageScrubNextTenantProbeProcess", "--")
	command.Env = append(os.Environ(),
		"SYNARA_STORAGE_SCRUB_PROBE=1",
		"SYNARA_STORAGE_SCRUB_ROOTS="+strings.Join([]string{workspaceRoot, cacheRoot, privateTempRoot}, string(os.PathListSeparator)),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("next-Tenant process found scrubbed residue: %v\n%s", err, output)
	}
}

func TestWorkerStorageScrubNextTenantProbeProcess(t *testing.T) {
	if os.Getenv("SYNARA_STORAGE_SCRUB_PROBE") != "1" {
		return
	}
	for _, root := range filepath.SplitList(os.Getenv("SYNARA_STORAGE_SCRUB_ROOTS")) {
		err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			contents, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			if strings.Contains(string(contents), "tenant-a-secret") {
				return errors.New("Tenant A secret remained readable at " + name)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
