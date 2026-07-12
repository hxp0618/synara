package agentd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
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
	workspaceID := uuid.New()
	execution := executions.Execution{ID: uuid.New()}
	workload := executions.Workload{
		TenantID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(), RemoteWorkspaceID: &workspaceID,
		DefaultBranch: "main", RepositoryURL: stringPointer("https://git.example.com/team/repository.git"),
	}
	materializer := NewWorkspaceMaterializer(root)
	materializer.resolver = workspaceResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	commands := make([][]string, 0)
	materializer.runGit = func(_ context.Context, directory string, arguments ...string) (string, error) {
		commands = append(commands, append([]string{directory}, arguments...))
		commandArguments := gitCommandArguments(arguments)
		switch {
		case len(commandArguments) > 0 && commandArguments[0] == "clone":
			checkout := commandArguments[len(commandArguments)-1]
			if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
				return "", err
			}
		case reflect.DeepEqual(commandArguments, []string{"rev-parse", "--show-toplevel"}):
			return directory, nil
		case reflect.DeepEqual(commandArguments, []string{"branch", "--show-current"}):
			return sessionBranch(workload.SessionID.String()), nil
		case reflect.DeepEqual(commandArguments, []string{"rev-parse", "HEAD"}):
			return strings.Repeat("a", 40), nil
		case len(commandArguments) > 0 && commandArguments[0] == "merge-base":
			return strings.Repeat("b", 40), nil
		}
		return "", nil
	}

	first, err := materializer.Materialize(context.Background(), execution, workload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := materializer.Materialize(context.Background(), execution, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Managed || first.Directory != second.Directory || first.RepositoryFingerprint == nil ||
		first.CurrentBranch == nil || *first.CurrentBranch != sessionBranch(workload.SessionID.String()) ||
		first.BaseCommit == nil || first.HeadCommit == nil {
		t.Fatalf("unexpected materialized Workspace: first=%#v second=%#v", first, second)
	}
	cloneCount, fetchCount := 0, 0
	for _, command := range commands {
		arguments := gitCommandArguments(command[1:])
		if len(arguments) > 0 && arguments[0] == "clone" {
			cloneCount++
		}
		if len(arguments) > 0 && arguments[0] == "fetch" {
			fetchCount++
		}
	}
	if cloneCount != 1 || fetchCount != 1 {
		t.Fatalf("Workspace was not cloned once then fetched: commands=%#v", commands)
	}
	encodedCommands := strings.Join(flattenGitCommands(commands), "\n")
	if !strings.Contains(encodedCommands, "http.followRedirects=false") ||
		!strings.Contains(encodedCommands, "http.curloptResolve=+git.example.com:443:93.184.216.34") {
		t.Fatalf("Git network commands were not DNS-pinned with redirects disabled: %s", encodedCommands)
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
	if _, err := materializer.Materialize(context.Background(), execution, workload); err == nil {
		t.Fatal("Workspace materializer accepted an SSRF target")
	}

	root = t.TempDir()
	workload.TenantID = uuid.New()
	materializer = NewWorkspaceMaterializer(root)
	outside := t.TempDir()
	tenantPath := filepath.Join(root, workload.TenantID.String())
	if err := os.Symlink(outside, tenantPath); err != nil {
		t.Fatal(err)
	}
	workload.RepositoryURL = nil
	if _, err := materializer.Materialize(context.Background(), execution, workload); err == nil {
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
	materializer.runGit = func(context.Context, string, ...string) (string, error) {
		t.Fatal("Git ran before the repository fingerprint mismatch was rejected")
		return "", nil
	}
	if _, err := materializer.Materialize(context.Background(), executions.Execution{ID: uuid.New()}, workload); err == nil {
		t.Fatal("logical Workspace was rebound to a different Repository")
	}
}

func TestGitEnvironmentDropsAmbientCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	encoded := strings.Join(gitEnvironment(), "\n")
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

var _ gitpolicy.Resolver = workspaceResolver{}
