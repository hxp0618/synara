package agentd

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func createWorkspaceTestSource(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	runTestGit(t, source, "init", "-b", "main")
	runTestGit(t, source, "config", "user.email", "worker@example.com")
	runTestGit(t, source, "config", "user.name", "Synara Worker")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "README.md")
	runTestGit(t, source, "commit", "-m", "baseline")
	return source
}

func configureWorkspaceTestNetwork(
	t *testing.T,
	materializer *WorkspaceMaterializer,
	source string,
	observe func(directory string, environment, arguments []string),
) *atomic.Int32 {
	t.Helper()
	realRun := materializer.runGitCommand
	sourceURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(source)}).String()
	var networkFetches atomic.Int32
	materializer.runGit = func(
		ctx context.Context,
		directory string,
		environment []string,
		arguments ...string,
	) (string, error) {
		if observe != nil {
			observe(directory, append([]string(nil), environment...), append([]string(nil), arguments...))
		}
		networkCommand := false
		for _, argument := range arguments {
			if strings.HasPrefix(argument, "http.curloptResolve=") {
				networkCommand = true
				break
			}
		}
		commandArguments := gitCommandArguments(arguments)
		if networkCommand && len(commandArguments) > 0 && commandArguments[0] == "fetch" {
			networkFetches.Add(1)
			refspec := commandArguments[len(commandArguments)-1]
			return realRun(
				ctx, directory, environment,
				"-c", "protocol.file.allow=always", "fetch", "--prune", "--no-tags", "--", sourceURL, refspec,
			)
		}
		return realRun(ctx, directory, environment, arguments...)
	}
	return &networkFetches
}

func workspaceTestWorkload(targetID uuid.UUID) (executions.Execution, executions.Workload) {
	workspaceID := uuid.New()
	execution := executions.Execution{ID: uuid.New(), ExecutionTargetID: targetID}
	workload := executions.Workload{
		TenantID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(), RemoteWorkspaceID: &workspaceID,
		DefaultBranch: "main", RepositoryURL: stringPointer("https://git.example.com/team/repository.git"),
	}
	return execution, workload
}

func TestWorkspaceCacheMaterializesPrivateWorktreeAndFetchesEveryTurn(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, targetID)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	var observedCommands []string
	networkFetches := configureWorkspaceTestNetwork(t, materializer, createWorkspaceTestSource(t), func(_ string, _ []string, arguments []string) {
		observedCommands = append(observedCommands, strings.Join(arguments, " "))
	})

	first, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Managed || first.LogicalRoot == "" || first.GitDir == "" || first.Directory != filepath.Join(first.LogicalRoot, "checkout") {
		t.Fatalf("unexpected private Workspace materialization: %#v", first)
	}
	gitFileInfo, err := os.Lstat(filepath.Join(first.Directory, ".git"))
	if err != nil || !gitFileInfo.Mode().IsRegular() || gitFileInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("checkout .git is not a regular gitfile: info=%v err=%v", gitFileInfo, err)
	}
	commonDir := strings.TrimSpace(runTestGitOutput(t, first.Directory, "rev-parse", "--git-common-dir"))
	if !sameExistingPath(commonDir, first.GitDir) {
		t.Fatalf("checkout common-dir is not private: %s != %s", commonDir, first.GitDir)
	}
	firstDirectory := first.Directory
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Directory != firstDirectory || networkFetches.Load() != 2 {
		t.Fatalf("warm Workspace was not reused with a fresh Fetch: directory=%s fetches=%d", second.Directory, networkFetches.Load())
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	encodedCommands := strings.Join(observedCommands, "\n")
	for _, forbidden := range []string{"clone --local", "clone --shared", "clone --reference"} {
		if strings.Contains(encodedCommands, forbidden) {
			t.Fatalf("private Git materialization used forbidden local sharing flag %s: %s", forbidden, encodedCommands)
		}
	}
	if !strings.Contains(encodedCommands, "protocol.file.allow=always") || !strings.Contains(encodedCommands, "file://") {
		t.Fatalf("private Git materialization did not use controlled file transport: %s", encodedCommands)
	}
	if err := rejectGitObjectAlternates(second.GitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cacheRoot); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, second.Directory, "cat-file", "-e", "HEAD^{commit}")
}

func TestWorkspaceLockCoversConcurrentTurns(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, targetID)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	configureWorkspaceTestNetwork(t, materializer, createWorkspaceTestSource(t), nil)
	first, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		materialized WorkspaceMaterialization
		err          error
	}
	resultChannel := make(chan result, 1)
	go func() {
		materialized, err := materializer.Materialize(context.Background(), execution, workload, nil)
		resultChannel <- result{materialized: materialized, err: err}
	}()
	select {
	case outcome := <-resultChannel:
		t.Fatalf("second Turn entered the Workspace before release: %v", outcome.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-resultChannel:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		_ = outcome.materialized.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("second Turn did not acquire the released Workspace lock")
	}
}

func TestWorkspaceCacheRebuildPreservesPrivateDirtyWorkspace(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	materializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, targetID)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	configureWorkspaceTestNetwork(t, materializer, createWorkspaceTestSource(t), nil)
	first, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	dirtyPath := filepath.Join(first.Directory, "local.txt")
	if err := os.WriteFile(dirtyPath, []byte("preserve me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheRepository := first.cache.RepoGit
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheRepository, "config"), []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	content, err := os.ReadFile(dirtyPath)
	if err != nil || string(content) != "preserve me\n" {
		t.Fatalf("cache rebuild damaged the private Workspace: %q err=%v", content, err)
	}
	if err := materializer.validateBareRepository(context.Background(), second.cache.RepoGit, *workload.RepositoryURL); err != nil {
		t.Fatalf("cache was not rebuilt: %v", err)
	}
}

func TestWorkspaceLayoutSeparatesExecutionTargetsAndPreservesLegacyData(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	execution, workload := workspaceTestWorkload(uuid.New())
	firstTarget := execution.ExecutionTargetID
	secondTarget := uuid.New()
	firstMaterializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, firstTarget)
	secondMaterializer := NewWorkspaceMaterializerWithCache(workspaceRoot, cacheRoot, secondTarget)
	firstLayout, err := firstMaterializer.resolveWorkspaceLayout(execution, workload)
	if err != nil {
		t.Fatal(err)
	}
	execution.ExecutionTargetID = secondTarget
	secondLayout, err := secondMaterializer.resolveWorkspaceLayout(execution, workload)
	if err != nil {
		t.Fatal(err)
	}
	if firstLayout.Root == secondLayout.Root {
		t.Fatal("different Execution Targets reused one Workspace path")
	}
	legacyFile := filepath.Join(firstLayout.LegacyRoot, "uncheckpointed.txt")
	if err := os.MkdirAll(filepath.Dir(legacyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	workload.RepositoryURL = nil
	execution.ExecutionTargetID = firstTarget
	if _, err := firstMaterializer.Materialize(context.Background(), execution, workload, nil); err == nil {
		t.Fatal("legacy Workspace with uncheckpointed data was silently replaced")
	}
	if content, err := os.ReadFile(legacyFile); err != nil || string(content) != "keep" {
		t.Fatalf("legacy Workspace data was not preserved: %q err=%v", content, err)
	}
}

func runTestGitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", arguments, err)
	}
	return string(output)
}
