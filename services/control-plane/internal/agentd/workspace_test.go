package agentd

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

type workspaceResolver map[string][]net.IPAddr

func (r workspaceResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return r[host], nil
}

func TestWorkspaceMaterializerClonesThenFetchesStableSessionCheckout(t *testing.T) {
	root := t.TempDir()
	cacheRoot := t.TempDir()
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	materializer := NewWorkspaceMaterializerWithCache(root, cacheRoot, targetID)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	commands := make([]string, 0)
	configureWorkspaceTestNetwork(t, materializer, createWorkspaceTestSource(t), func(directory string, _ []string, arguments []string) {
		commands = append(commands, strings.Join(append([]string{directory}, arguments...), " "))
	})

	first, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if !first.Managed || first.Directory != second.Directory || first.RepositoryFingerprint == nil ||
		first.CurrentBranch == nil || *first.CurrentBranch != sessionBranch(workload.SessionID.String()) ||
		first.BaseCommit == nil || first.HeadCommit == nil {
		t.Fatalf("unexpected materialized Workspace: first=%#v second=%#v", first, second)
	}
	encodedCommands := strings.Join(commands, "\n")
	if !strings.Contains(encodedCommands, "http.followRedirects=false") ||
		!strings.Contains(encodedCommands, "http.curloptResolve=+git.example.com:443:93.184.216.34") {
		t.Fatalf("Git network commands were not DNS-pinned with redirects disabled: %s", encodedCommands)
	}
}

func TestWorkspaceMaterializerRecoversInterruptedGitGenerationInstall(t *testing.T) {
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
	writeWorkspaceTestFile(t, filepath.Join(first.Directory, "preserved-untracked.txt"), "durable git workspace\n")
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(
		filepath.Dir(first.LogicalRoot), "."+filepath.Base(first.LogicalRoot)+".backup-"+uuid.NewString(),
	)
	if err := os.Rename(first.LogicalRoot, backup); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(
		filepath.Dir(first.LogicalRoot), "."+filepath.Base(first.LogicalRoot)+".staging-"+uuid.NewString(),
	)
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(staging, "partial.txt"), "partial git install\n")

	recovered, err := materializer.Materialize(context.Background(), execution, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Release()
	assertWorkspaceTestFile(
		t,
		filepath.Join(recovered.Directory, "preserved-untracked.txt"),
		"durable git workspace\n",
	)
	for _, path := range []string{backup, staging} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("interrupted Git Workspace residue survived at %s: %v", path, err)
		}
	}
}

func gitCommandArguments(arguments []string) []string {
	for len(arguments) >= 2 && arguments[0] == "-c" {
		arguments = arguments[2:]
	}
	return arguments
}

func flattenGitCommands(commands [][]string) []string {
	flattened := make([]string, 0)
	for _, command := range commands {
		flattened = append(flattened, command...)
	}
	return flattened
}

func TestWorkspaceMaterializerRejectsSSRFAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workspaceID := uuid.New()
	execution := executions.Execution{ID: uuid.New()}
	workload := executions.Workload{
		TenantID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(), RemoteWorkspaceID: &workspaceID,
		DefaultBranch: "main", RepositoryURL: stringPointer("https://internal.example/repository.git"),
	}
	materializer := NewWorkspaceMaterializer(root)
	materializer.resolver = workspaceResolver{"internal.example": {{IP: net.ParseIP("10.0.0.9")}}}
	if _, err := materializer.Materialize(context.Background(), execution, workload, nil); err == nil {
		t.Fatal("Workspace materializer accepted an SSRF target")
	}

	root = t.TempDir()
	workload.TenantID = uuid.New()
	materializer = NewWorkspaceMaterializer(root)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(root, "v2", uuid.Nil.String())
	if err := os.Symlink(outside, targetPath); err != nil {
		t.Fatal(err)
	}
	workload.RepositoryURL = nil
	if _, err := materializer.Materialize(context.Background(), execution, workload, nil); err == nil {
		t.Fatal("Workspace materializer followed a symlink outside the Workspace root")
	}
}

func TestWorkspaceMaterializerRejectsRepositoryRebinding(t *testing.T) {
	workspaceID := uuid.New()
	previousFingerprint := strings.Repeat("f", 64)
	workload := executions.Workload{
		TenantID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(), RemoteWorkspaceID: &workspaceID,
		WorkspaceRepositoryFingerprint: &previousFingerprint, DefaultBranch: "main",
		RepositoryURL: stringPointer("https://git.example.com/team/repository.git"),
	}
	materializer := NewWorkspaceMaterializer(t.TempDir())
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	materializer.runGit = func(context.Context, string, []string, ...string) (string, error) {
		t.Fatal("Git ran before the repository fingerprint mismatch was rejected")
		return "", nil
	}
	if _, err := materializer.Materialize(context.Background(), executions.Execution{ID: uuid.New()}, workload, nil); err == nil {
		t.Fatal("logical Workspace was rebound to a different Repository")
	}
}

func TestWorkspaceInspectionDetectsGitAndGeneratedFileChanges(t *testing.T) {
	directory := t.TempDir()
	runTestGit(t, directory, "init", "-b", "main")
	runTestGit(t, directory, "config", "user.email", "worker@example.com")
	runTestGit(t, directory, "config", "user.name", "Synara Worker")
	tracked := filepath.Join(directory, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, directory, "add", "tracked.txt")
	runTestGit(t, directory, "commit", "-m", "baseline")

	materializer := NewWorkspaceMaterializer(t.TempDir())
	clean, err := materializer.Inspect(context.Background(), WorkspaceMaterialization{
		Directory: directory, Managed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if clean.Dirty || clean.CurrentBranch == nil || *clean.CurrentBranch != "main" || clean.HeadCommit == nil {
		t.Fatalf("unexpected clean Workspace inspection: %#v", clean)
	}
	if err := os.WriteFile(tracked, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, err := materializer.Inspect(context.Background(), WorkspaceMaterialization{
		Directory: directory, Managed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty || dirty.CurrentBranch == nil || dirty.HeadCommit == nil {
		t.Fatalf("tracked changes were not reported as dirty: %#v", dirty)
	}

	generatedDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(generatedDirectory, "generated.txt"), []byte("output"), 0o600); err != nil {
		t.Fatal(err)
	}
	generated, err := materializer.Inspect(context.Background(), WorkspaceMaterialization{
		Directory: generatedDirectory, Managed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !generated.Dirty || generated.CurrentBranch != nil || generated.HeadCommit != nil {
		t.Fatalf("generated files in a non-Git Workspace were not reported as dirty: %#v", generated)
	}
}

func TestWorkspaceMaterializerUsesAskPassWithoutLeakingCredential(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	workload.RepositoryURL = stringPointer("https://git.example.com/team/private.git")
	materializer := NewWorkspaceMaterializerWithCache(root, t.TempDir(), targetID)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	materializer.executable = func() (string, error) { return "/usr/local/bin/synara-agentd", nil }
	secret := "private-git-token"
	var environments [][]string
	var commands [][]string
	var socketPath string
	configureWorkspaceTestNetwork(t, materializer, createWorkspaceTestSource(t), func(_ string, environment, arguments []string) {
		environments = append(environments, append([]string(nil), environment...))
		commands = append(commands, append([]string(nil), arguments...))
		for _, value := range environment {
			if strings.HasPrefix(value, GitAskPassSocketEnvironment+"=") {
				socketPath = strings.TrimPrefix(value, GitAskPassSocketEnvironment+"=")
			}
		}
	})
	credential := &WorkspaceGitCredential{HTTPS: &GitHTTPSCredential{Host: "git.example.com", Username: "git-user", Token: secret}}
	materialized, err := materializer.Materialize(context.Background(), execution, workload, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer materialized.Release()
	if socketPath == "" {
		t.Fatal("authenticated Git commands omitted the AskPass socket")
	}
	if pathContainedBy(root, socketPath) {
		t.Fatalf("AskPass socket was created inside the Workspace root: %s", socketPath)
	}
	encodedEnvironment := strings.Join(flattenGitCommands(environments), "\n")
	encodedCommands := strings.Join(flattenGitCommands(commands), "\n")
	if strings.Contains(encodedEnvironment, secret) || strings.Contains(encodedCommands, secret) {
		t.Fatalf("Git Credential leaked into environment or argv: env=%s argv=%s", encodedEnvironment, encodedCommands)
	}
	if !strings.Contains(encodedEnvironment, "GIT_ASKPASS=/usr/local/bin/synara-agentd") ||
		!strings.Contains(encodedEnvironment, "GIT_ASKPASS_REQUIRE=force") {
		t.Fatalf("authenticated Git commands omitted AskPass configuration: %s", encodedEnvironment)
	}
	for index, command := range commands {
		arguments := gitCommandArguments(command)
		if len(arguments) == 0 {
			continue
		}
		environment := strings.Join(environments[index], "\n")
		usesAskPass := strings.Contains(environment, GitAskPassSocketEnvironment+"=")
		isNetworkFetch := false
		for _, argument := range command {
			if strings.HasPrefix(argument, "http.curloptResolve=") {
				isNetworkFetch = true
			}
		}
		if isNetworkFetch && !usesAskPass {
			t.Fatal("authenticated cache Fetch did not receive AskPass")
		}
		if !isNetworkFetch && usesAskPass {
			t.Fatalf("local Git command %q received the Git Credential environment", arguments[0])
		}
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("AskPass socket survived Workspace preparation: %v", err)
	}
}

func TestWorkspaceMaterializerRejectsDangerousLocalGitConfiguration(t *testing.T) {
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	workload.RepositoryURL = stringPointer("https://git.example.com/team/private.git")
	materializer := NewWorkspaceMaterializerWithCache(t.TempDir(), t.TempDir(), targetID)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	configureWorkspaceTestNetwork(t, materializer, createWorkspaceTestSource(t), nil)
	credential := &WorkspaceGitCredential{HTTPS: &GitHTTPSCredential{Host: "git.example.com", Username: "git-user", Token: "private-token"}}
	materialized, err := materializer.Materialize(context.Background(), execution, workload, credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialized.Release(); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, materialized.Directory, "config", "credential.helper", "store")
	_, err = materializer.Materialize(context.Background(), execution, workload, credential)
	if err == nil {
		t.Fatal("Workspace accepted dangerous repository-local Git configuration")
	}
	value := strings.TrimSpace(runTestGitOutput(t, materialized.Directory, "config", "--local", "credential.helper"))
	if value != "store" {
		t.Fatalf("invalid Workspace generation was modified instead of preserved: %q", value)
	}
}

func TestWorkspaceMaterializerRedactsCredentialFromGitError(t *testing.T) {
	secret := "private-token-in-stderr"
	targetID := uuid.New()
	execution, workload := workspaceTestWorkload(targetID)
	workload.RepositoryURL = stringPointer("https://git.example.com/team/private.git")
	materializer := NewWorkspaceMaterializerWithCache(t.TempDir(), t.TempDir(), targetID)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	materializer.executable = func() (string, error) { return "/usr/local/bin/synara-agentd", nil }
	realRun := materializer.runGitCommand
	materializer.runGit = func(ctx context.Context, directory string, environment []string, arguments ...string) (string, error) {
		for _, argument := range arguments {
			if strings.HasPrefix(argument, "http.curloptResolve=") {
				return "", errors.New("remote rejected token " + secret)
			}
		}
		if commandArguments := gitCommandArguments(arguments); len(commandArguments) > 0 && commandArguments[0] == "fetch" {
			return "", errors.New("remote rejected token " + secret)
		}
		return realRun(ctx, directory, environment, arguments...)
	}
	_, err := materializer.Materialize(
		context.Background(), execution, workload,
		&WorkspaceGitCredential{HTTPS: &GitHTTPSCredential{Host: "git.example.com", Username: "git-user", Token: secret}},
	)
	if err == nil {
		t.Fatal("Workspace Clone failure was not returned")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Git Credential leaked through Workspace failure: %v", err)
	}
}

func TestGitEnvironmentDropsAmbientCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	encoded := strings.Join(gitEnvironment(nil), "\n")
	for _, secret := range []string{"github-secret", "/tmp/agent.sock", "aws-secret"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("ambient Credential leaked into Git environment: %s", encoded)
		}
	}
	for _, required := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ASKPASS=/bin/false"} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("Git isolation environment omitted %s: %s", required, encoded)
		}
	}
}

func runTestGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "GIT_CONFIG_NOSYSTEM=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", arguments, err, output)
	}
}

var _ gitpolicy.Resolver = workspaceResolver{}
