package agentd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

type workspaceCleanupTestFixture struct {
	materializer  *WorkspaceMaterializer
	request       WorkspaceCleanupRequest
	layout        workspaceCleanupLayout
	workspaceRoot string
	cacheRoot     string
	source        string
}

type workspaceCleanupSyncCall struct {
	root     string
	relative string
}

func TestWorkspaceCleanupDeletesV3WithoutFollowingCheckoutSymlinkOrTouchingCache(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	cacheSentinel := filepath.Join(fixture.cacheRoot, "cache-sentinel")
	if err := os.WriteFile(cacheSentinel, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	externalSentinel := filepath.Join(external, "external-sentinel")
	if err := os.WriteFile(externalSentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(fixture.source, "checkout", "external-link")
	if err := os.Symlink(externalSentinel, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}

	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("CleanupWorkspace() = %#v, %v", result, err)
	}
	assertPathAbsent(t, fixture.source)
	assertFileContents(t, cacheSentinel, "cache")
	assertFileContents(t, externalSentinel, "preserve")

	replayed, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || replayed.Status != WorkspaceCleanupAlreadyAbsent {
		t.Fatalf("idempotent CleanupWorkspace() = %#v, %v", replayed, err)
	}
}

func TestWorkspaceCleanupPersistsEveryNamespaceTransitionBeforeSuccess(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	calls := make([]workspaceCleanupSyncCall, 0, 24)
	fixture.materializer.cleanupDirectorySync = func(root *os.Root, relative string) error {
		calls = append(calls, workspaceCleanupSyncCall{
			root: filepath.Clean(root.Name()), relative: filepath.Clean(relative),
		})
		return syncWorkspaceCleanupDirectory(root, relative)
	}

	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("durable CleanupWorkspace() = %#v, %v", result, err)
	}
	mainRoot := filepath.Clean(fixture.workspaceRoot)
	required := []workspaceCleanupSyncCall{
		{root: mainRoot, relative: "."},
		{root: mainRoot, relative: ".quarantine"},
		{root: mainRoot, relative: filepath.Join(".quarantine", "workspace-v1")},
		{root: mainRoot, relative: fixture.layout.ClaimRelative},
		{root: mainRoot, relative: filepath.Dir(fixture.layout.SourceRelative)},
		{
			root:     filepath.Join(mainRoot, fixture.layout.PayloadRelative),
			relative: ".",
		},
	}
	for _, expected := range required {
		found := false
		for _, call := range calls {
			if call == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("cleanup durability calls omitted %#v: %#v", expected, calls)
		}
	}
	last := calls[len(calls)-1]
	if last.root != mainRoot || last.relative != filepath.Dir(fixture.layout.ClaimRelative) {
		t.Fatalf("last durability boundary = %#v, want quarantine parent after claim removal", last)
	}
}

func TestWorkspaceCleanupBatchesSiblingUnlinksIntoOneDirectorySync(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	checkout := filepath.Join(fixture.source, "checkout")
	for index := 0; index < 64; index++ {
		path := filepath.Join(checkout, uuid.NewString()+".txt")
		if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checkoutSyncs := 0
	fixture.materializer.cleanupDirectorySync = func(root *os.Root, relative string) error {
		if filepath.Clean(root.Name()) == filepath.Clean(filepath.Join(
			fixture.workspaceRoot, fixture.layout.PayloadRelative, "checkout",
		)) && filepath.Clean(relative) == "." {
			checkoutSyncs++
		}
		return syncWorkspaceCleanupDirectory(root, relative)
	}

	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("batched durable CleanupWorkspace() = %#v, %v", result, err)
	}
	if checkoutSyncs != 1 {
		t.Fatalf("checkout directory syncs = %d, want one batched sync for 64 sibling unlinks", checkoutSyncs)
	}
}

func TestWorkspaceCleanupRenameDurabilityFailureBlocksSuccessAndResumes(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	injected := errors.New("injected source-parent sync failure")
	fail := true
	fixture.materializer.cleanupDirectorySync = func(root *os.Root, relative string) error {
		if fail && filepath.Clean(root.Name()) == filepath.Clean(fixture.workspaceRoot) &&
			filepath.Clean(relative) == filepath.Dir(fixture.layout.SourceRelative) {
			fail = false
			return injected
		}
		return syncWorkspaceCleanupDirectory(root, relative)
	}

	_, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	var cleanupErr *WorkspaceCleanupError
	if !errors.As(err, &cleanupErr) || !cleanupErr.Retryable || cleanupErr.Code != "workspace_cleanup_durability_failed" ||
		!errors.Is(err, injected) {
		t.Fatalf("rename durability error = %#v, %v", cleanupErr, err)
	}
	assertPathAbsent(t, fixture.source)
	assertPathPresent(t, filepath.Join(fixture.workspaceRoot, fixture.layout.PayloadRelative))

	fixture.materializer.cleanupDirectorySync = nil
	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("cleanup retry after rename durability failure = %#v, %v", result, err)
	}
}

func TestWorkspaceCleanupFinalizationDurabilityFailureBlocksSuccessAndResumes(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	injected := errors.New("injected claim-parent sync failure")
	fail := true
	fixture.materializer.cleanupDirectorySync = func(root *os.Root, relative string) error {
		if fail && filepath.Clean(root.Name()) == filepath.Clean(fixture.workspaceRoot) &&
			filepath.Clean(relative) == filepath.Dir(fixture.layout.ClaimRelative) {
			claimExists, inspectErr := inspectRootRelativeDirectory(root, fixture.layout.ClaimRelative)
			if inspectErr != nil {
				return inspectErr
			}
			if !claimExists {
				fail = false
				return injected
			}
		}
		return syncWorkspaceCleanupDirectory(root, relative)
	}

	_, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	var cleanupErr *WorkspaceCleanupError
	if !errors.As(err, &cleanupErr) || !cleanupErr.Retryable || cleanupErr.Code != "workspace_cleanup_retryable" ||
		!errors.Is(err, injected) {
		t.Fatalf("finalization durability error = %#v, %v", cleanupErr, err)
	}
	assertPathAbsent(t, fixture.source)
	assertPathAbsent(t, filepath.Join(fixture.workspaceRoot, fixture.layout.ClaimRelative))

	fixture.materializer.cleanupDirectorySync = nil
	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupAlreadyAbsent {
		t.Fatalf("cleanup retry after finalization durability failure = %#v, %v", result, err)
	}
}

func TestWorkspaceCleanupRejectsEveryManifestIdentityMismatchWithoutDeletion(t *testing.T) {
	tests := map[string]func(*workspaceGenerationManifest){
		"target":          func(m *workspaceGenerationManifest) { m.ExecutionTargetID = uuid.NewString() },
		"tenant":          func(m *workspaceGenerationManifest) { m.TenantID = uuid.NewString() },
		"project":         func(m *workspaceGenerationManifest) { m.ProjectID = uuid.NewString() },
		"session":         func(m *workspaceGenerationManifest) { m.SessionID = uuid.NewString() },
		"workspace":       func(m *workspaceGenerationManifest) { m.LogicalWorkspaceID = uuid.NewString() },
		"materialization": func(m *workspaceGenerationManifest) { m.MaterializationID = uuid.NewString() },
		"incarnation":     func(m *workspaceGenerationManifest) { m.IncarnationID = uuid.NewString() },
		"layout":          func(m *workspaceGenerationManifest) { m.LayoutVersion = workspaceLayoutV2 },
		"managed":         func(m *workspaceGenerationManifest) { m.Managed = false },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
			manifest, err := readWorkspaceManifest(filepath.Join(fixture.source, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			mutate(&manifest)
			if err := writeWorkspaceManifest(fixture.source, manifest); err != nil {
				t.Fatal(err)
			}
			_, err = fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
			assertPermanentWorkspaceCleanupError(t, err, "workspace_cleanup_identity_mismatch")
			assertPathPresent(t, fixture.source)
			assertPathAbsent(t, filepath.Join(fixture.workspaceRoot, fixture.layout.PayloadRelative))
		})
	}
}

func TestWorkspaceCleanupRejectsQuarantineManifestMismatchWithoutDeletion(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	root, err := openVerifiedWorkspaceRoot(fixture.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRootRelativeDirectory(root, fixture.layout.ClaimRelative, syncWorkspaceCleanupDirectory); err != nil {
		t.Fatal(err)
	}
	marker := workspaceCleanupMarkerForRequest(fixture.request, workspaceCleanupQuarantined)
	if err := writeWorkspaceCleanupMarker(root, fixture.layout, marker, syncWorkspaceCleanupDirectory); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(fixture.layout.SourceRelative, fixture.layout.PayloadRelative); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(fixture.workspaceRoot, fixture.layout.PayloadRelative)
	manifest, err := readWorkspaceManifest(filepath.Join(payload, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest.MaterializationID = uuid.NewString()
	if err := writeWorkspaceManifest(payload, manifest); err != nil {
		t.Fatal(err)
	}

	_, err = fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	assertPermanentWorkspaceCleanupError(t, err, "workspace_cleanup_identity_mismatch")
	assertPathPresent(t, payload)
	assertPathPresent(t, filepath.Join(payload, "checkout"))
}

func TestWorkspaceCleanupRejectsUnleasedOrPodScopedRequestBeforeMutation(t *testing.T) {
	tests := map[string]func(*WorkspaceCleanupRequest){
		"unleased generation": func(request *WorkspaceCleanupRequest) { request.DispatchGeneration = 0 },
		"pod storage":         func(request *WorkspaceCleanupRequest) { request.StorageScope = "pod" },
		"unknown target":      func(request *WorkspaceCleanupRequest) { request.TargetKind = "unknown" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
			request := fixture.request
			mutate(&request)
			_, err := fixture.materializer.CleanupWorkspace(context.Background(), request)
			assertPermanentWorkspaceCleanupError(t, err, "workspace_cleanup_invalid")
			assertPathPresent(t, fixture.source)
			assertPathAbsent(t, filepath.Join(fixture.workspaceRoot, ".quarantine"))
		})
	}
}

func TestWorkspaceCleanupRejectsRootAncestorAndQuarantineSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires Developer Mode on Windows")
	}
	t.Run("configured root", func(t *testing.T) {
		realRoot := t.TempDir()
		linkedRoot := filepath.Join(t.TempDir(), "workspace-root")
		if err := os.Symlink(realRoot, linkedRoot); err != nil {
			t.Fatal(err)
		}
		request := newWorkspaceCleanupRequest(workspaceLayoutV3)
		materializer := NewWorkspaceMaterializerWithCache(linkedRoot, t.TempDir(), request.ExecutionTargetID)
		layout, err := materializer.resolveWorkspaceCleanupLayout(request)
		if err != nil {
			t.Fatal(err)
		}
		createWorkspaceCleanupGeneration(t, filepath.Join(realRoot, layout.SourceRelative), cleanupManifestForRequest(request))
		_, err = materializer.CleanupWorkspace(context.Background(), request)
		assertPermanentWorkspaceCleanupError(t, err, "workspace_cleanup_unsafe_path")
		assertPathPresent(t, filepath.Join(realRoot, layout.SourceRelative))
	})

	t.Run("source ancestor", func(t *testing.T) {
		workspaceRoot := t.TempDir()
		request := newWorkspaceCleanupRequest(workspaceLayoutV3)
		materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, t.TempDir(), request.ExecutionTargetID)
		layout, err := materializer.resolveWorkspaceCleanupLayout(request)
		if err != nil {
			t.Fatal(err)
		}
		targetParent := filepath.Join(workspaceRoot, "v3", request.ExecutionTargetID.String())
		if err := os.MkdirAll(targetParent, 0o700); err != nil {
			t.Fatal(err)
		}
		externalTenant := t.TempDir()
		tenantLink := filepath.Join(targetParent, request.TenantID.String())
		if err := os.Symlink(externalTenant, tenantLink); err != nil {
			t.Fatal(err)
		}
		externalSource := filepath.Join(
			externalTenant, request.ProjectID.String(), request.SessionID.String(), request.LogicalWorkspaceID.String(),
			request.IncarnationID.String(),
		)
		createWorkspaceCleanupGeneration(t, externalSource, cleanupManifestForRequest(request))
		sentinel := filepath.Join(externalSource, "checkout", "sentinel")
		if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = materializer.CleanupWorkspace(context.Background(), request)
		assertPermanentWorkspaceCleanupError(t, err, "workspace_cleanup_unsafe_path")
		assertFileContents(t, sentinel, "preserve")
		assertPathAbsent(t, filepath.Join(workspaceRoot, layout.PayloadRelative))
	})

	t.Run("quarantine ancestor", func(t *testing.T) {
		fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
		external := t.TempDir()
		sentinel := filepath.Join(external, "sentinel")
		if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(fixture.workspaceRoot, ".quarantine")); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
		assertPermanentWorkspaceCleanupError(t, err, "workspace_cleanup_unsafe_quarantine")
		assertFileContents(t, sentinel, "preserve")
		assertPathPresent(t, fixture.source)
	})
}

func TestWorkspaceCleanupContendsWithMaterializationLogicalLock(t *testing.T) {
	request := newWorkspaceCleanupRequest(workspaceLayoutV3)
	workspaceRoot := t.TempDir()
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, t.TempDir(), request.ExecutionTargetID)
	workspaceID := request.LogicalWorkspaceID
	materializationID := request.MaterializationID
	incarnationID := request.IncarnationID
	workload := executions.Workload{
		TenantID: request.TenantID, OrganizationID: request.OrganizationID, ProjectID: request.ProjectID,
		SessionID: request.SessionID, RemoteWorkspaceID: &workspaceID,
		WorkspaceMaterializationID:            &materializationID,
		WorkspaceMaterializationIncarnationID: &incarnationID,
		WorkspaceLayoutVersion:                workspaceLayoutV3,
	}
	materialized, err := materializer.Materialize(context.Background(), executions.Execution{
		ID: uuid.New(), ExecutionTargetID: request.ExecutionTargetID,
	}, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectedRoot := filepath.Join(
		workspaceRoot, "v3", request.ExecutionTargetID.String(), request.TenantID.String(), request.ProjectID.String(),
		request.SessionID.String(), request.LogicalWorkspaceID.String(), request.IncarnationID.String(),
	)
	if materialized.LogicalRoot != expectedRoot {
		t.Fatalf("v3 materialization root = %q, want %q", materialized.LogicalRoot, expectedRoot)
	}
	manifest, err := readWorkspaceManifest(filepath.Join(materialized.LogicalRoot, "manifest.json"))
	if err != nil || manifest.Format != workspaceLayoutV3Format || manifest.LayoutVersion != workspaceLayoutV3 ||
		manifest.MaterializationID != request.MaterializationID.String() || manifest.IncarnationID != request.IncarnationID.String() {
		t.Fatalf("v3 materialization Manifest = %#v, %v", manifest, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = materializer.CleanupWorkspace(ctx, request)
	var cleanupErr *WorkspaceCleanupError
	if !errors.As(err, &cleanupErr) || !cleanupErr.Retryable || cleanupErr.Code != "workspace_cleanup_lock_busy" {
		t.Fatalf("lock contention error = %#v, %v", cleanupErr, err)
	}
	if err := materialized.Release(); err != nil {
		t.Fatal(err)
	}
	result, err := materializer.CleanupWorkspace(context.Background(), request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("cleanup after materialization release = %#v, %v", result, err)
	}
}

func TestWorkspaceCleanupResumesAfterRenameCrash(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	root, err := openVerifiedWorkspaceRoot(fixture.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRootRelativeDirectory(root, fixture.layout.ClaimRelative, syncWorkspaceCleanupDirectory); err != nil {
		t.Fatal(err)
	}
	marker := workspaceCleanupMarkerForRequest(fixture.request, workspaceCleanupPrepared)
	if err := writeWorkspaceCleanupMarker(root, fixture.layout, marker, syncWorkspaceCleanupDirectory); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(fixture.layout.SourceRelative, fixture.layout.PayloadRelative); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("rename-crash retry = %#v, %v", result, err)
	}
	assertPathAbsent(t, fixture.source)
	assertPathAbsent(t, filepath.Join(fixture.workspaceRoot, fixture.layout.ClaimRelative))
}

func TestWorkspaceCleanupResumesPartialDeleteWithoutManifest(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	root, err := openVerifiedWorkspaceRoot(fixture.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRootRelativeDirectory(root, fixture.layout.ClaimRelative, syncWorkspaceCleanupDirectory); err != nil {
		t.Fatal(err)
	}
	marker := workspaceCleanupMarkerForRequest(fixture.request, workspaceCleanupDeleting)
	if err := writeWorkspaceCleanupMarker(root, fixture.layout, marker, syncWorkspaceCleanupDirectory); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(fixture.layout.SourceRelative, fixture.layout.PayloadRelative); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(filepath.Join(fixture.layout.PayloadRelative, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("partial-delete retry = %#v, %v", result, err)
	}
	assertPathAbsent(t, filepath.Join(fixture.workspaceRoot, fixture.layout.ClaimRelative))
}

func TestWorkspaceCleanupDeletesOldQuarantineButPreservesNewV2Incarnation(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV2, false)
	root, err := openVerifiedWorkspaceRoot(fixture.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRootRelativeDirectory(root, fixture.layout.ClaimRelative, syncWorkspaceCleanupDirectory); err != nil {
		t.Fatal(err)
	}
	marker := workspaceCleanupMarkerForRequest(fixture.request, workspaceCleanupQuarantined)
	if err := writeWorkspaceCleanupMarker(root, fixture.layout, marker, syncWorkspaceCleanupDirectory); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(fixture.layout.SourceRelative, fixture.layout.PayloadRelative); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	newMaterializationID := uuid.New()
	newIncarnationID := uuid.New()
	newManifest := cleanupManifestForRequest(fixture.request)
	newManifest.MaterializationID = newMaterializationID.String()
	newManifest.IncarnationID = newIncarnationID.String()
	createWorkspaceCleanupGeneration(t, fixture.source, newManifest)
	newSentinel := filepath.Join(fixture.source, "checkout", "new-incarnation")
	if err := os.WriteFile(newSentinel, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("stale-incarnation cleanup = %#v, %v", result, err)
	}
	assertFileContents(t, newSentinel, "new")
	manifest, err := readWorkspaceManifest(filepath.Join(fixture.source, "manifest.json"))
	if err != nil || manifest.MaterializationID != newMaterializationID.String() || manifest.IncarnationID != newIncarnationID.String() {
		t.Fatalf("new incarnation changed: %#v, %v", manifest, err)
	}
	assertPathAbsent(t, filepath.Join(fixture.workspaceRoot, fixture.layout.ClaimRelative))
}

func TestWorkspaceMaterializerAdoptsExactLegacyV2ManifestBeforeCleanup(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV2, true)
	workspaceID := fixture.request.LogicalWorkspaceID
	materializationID := fixture.request.MaterializationID
	incarnationID := fixture.request.IncarnationID
	workload := executions.Workload{
		TenantID: fixture.request.TenantID, OrganizationID: fixture.request.OrganizationID,
		ProjectID: fixture.request.ProjectID, SessionID: fixture.request.SessionID,
		RemoteWorkspaceID: &workspaceID, WorkspaceMaterializationID: &materializationID,
		WorkspaceMaterializationIncarnationID: &incarnationID, WorkspaceLayoutVersion: workspaceLayoutV2,
	}
	materialized, err := fixture.materializer.Materialize(context.Background(), executions.Execution{
		ID: uuid.New(), ExecutionTargetID: fixture.request.ExecutionTargetID,
	}, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := readWorkspaceManifest(filepath.Join(fixture.source, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != workspaceLayoutV3Format || manifest.LayoutVersion != workspaceLayoutV2 ||
		manifest.MaterializationID != materializationID.String() || manifest.IncarnationID != incarnationID.String() {
		t.Fatalf("legacy Manifest was not adopted: %#v", manifest)
	}
	if err := materialized.Release(); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || result.Status != WorkspaceCleanupDeleted {
		t.Fatalf("cleanup adopted v2 generation = %#v, %v", result, err)
	}
}

func TestWorkspaceCleanupCancelledBeforeMutationIsRetryable(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fixture.materializer.CleanupWorkspace(ctx, fixture.request)
	var cleanupErr *WorkspaceCleanupError
	if !errors.As(err, &cleanupErr) || !cleanupErr.Retryable || cleanupErr.Code != "workspace_cleanup_cancelled" {
		t.Fatalf("cancelled cleanup error = %#v, %v", cleanupErr, err)
	}
	assertPathPresent(t, fixture.source)
	assertPathAbsent(t, filepath.Join(fixture.workspaceRoot, ".quarantine"))
}

func TestWorkspaceCleanupCancellationDuringWalkerResumesSafely(t *testing.T) {
	fixture := newWorkspaceCleanupTestFixture(t, workspaceLayoutV3, false)
	for index := 0; index < 8; index++ {
		path := filepath.Join(fixture.source, "checkout", "nested", uuid.NewString(), "payload")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &cancelAfterChecksContext{Context: context.Background(), cancelAt: 7}
	_, err := fixture.materializer.CleanupWorkspace(ctx, fixture.request)
	var cleanupErr *WorkspaceCleanupError
	if !errors.As(err, &cleanupErr) || !cleanupErr.Retryable || cleanupErr.Code != "workspace_cleanup_cancelled" ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("walker cancellation error = %#v, %v", cleanupErr, err)
	}
	assertPathPresent(t, filepath.Join(fixture.workspaceRoot, fixture.layout.ClaimRelative))

	result, err := fixture.materializer.CleanupWorkspace(context.Background(), fixture.request)
	if err != nil || (result.Status != WorkspaceCleanupDeleted && result.Status != WorkspaceCleanupAlreadyAbsent) {
		t.Fatalf("walker cancellation retry = %#v, %v", result, err)
	}
	assertPathAbsent(t, fixture.source)
	assertPathAbsent(t, filepath.Join(fixture.workspaceRoot, fixture.layout.ClaimRelative))
}

type cancelAfterChecksContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (c *cancelAfterChecksContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func newWorkspaceCleanupTestFixture(
	t *testing.T,
	layoutVersion int,
	legacyManifest bool,
) workspaceCleanupTestFixture {
	t.Helper()
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	request := newWorkspaceCleanupRequest(layoutVersion)
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, request.ExecutionTargetID)
	layout, err := materializer.resolveWorkspaceCleanupLayout(request)
	if err != nil {
		t.Fatal(err)
	}
	manifest := cleanupManifestForRequest(request)
	if legacyManifest {
		manifest.Format = workspaceLayoutVersion
		manifest.LayoutVersion = 0
		manifest.MaterializationID = ""
		manifest.IncarnationID = ""
	}
	source := filepath.Join(workspaceRoot, layout.SourceRelative)
	createWorkspaceCleanupGeneration(t, source, manifest)
	return workspaceCleanupTestFixture{
		materializer: materializer, request: request, layout: layout,
		workspaceRoot: workspaceRoot, cacheRoot: cacheRoot, source: source,
	}
}

func newWorkspaceCleanupRequest(layoutVersion int) WorkspaceCleanupRequest {
	return WorkspaceCleanupRequest{
		CleanupID: uuid.New(), TenantID: uuid.New(), OrganizationID: uuid.New(), ProjectID: uuid.New(),
		SessionID: uuid.New(), LogicalWorkspaceID: uuid.New(), MaterializationID: uuid.New(),
		IncarnationID: uuid.New(), ExecutionTargetID: uuid.New(), TargetKind: "local",
		StorageScope: "target", LayoutVersion: layoutVersion, DispatchGeneration: 1,
	}
}

func cleanupManifestForRequest(request WorkspaceCleanupRequest) workspaceGenerationManifest {
	return workspaceGenerationManifest{
		Format: workspaceLayoutV3Format, ExecutionTargetID: request.ExecutionTargetID.String(),
		TenantID: request.TenantID.String(), ProjectID: request.ProjectID.String(), SessionID: request.SessionID.String(),
		LogicalWorkspaceID: request.LogicalWorkspaceID.String(), MaterializationID: request.MaterializationID.String(),
		IncarnationID: request.IncarnationID.String(), LayoutVersion: request.LayoutVersion, Managed: true,
	}
}

func createWorkspaceCleanupGeneration(t *testing.T, root string, manifest workspaceGenerationManifest) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "checkout"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkspaceManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
}

func assertPermanentWorkspaceCleanupError(t *testing.T, err error, code string) {
	t.Helper()
	var cleanupErr *WorkspaceCleanupError
	if !errors.As(err, &cleanupErr) || cleanupErr.Retryable || cleanupErr.Code != code {
		t.Fatalf("cleanup error = %#v, %v; want permanent %q", cleanupErr, err, code)
	}
}

func assertPathPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent: %v", path, err)
	}
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != expected {
		t.Fatalf("file %s = %q, %v; want %q", path, contents, err, expected)
	}
}
