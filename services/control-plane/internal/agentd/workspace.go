package agentd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

type workspaceMaterializer interface {
	Materialize(context.Context, executions.Execution, executions.Workload) (WorkspaceMaterialization, error)
}

type WorkspaceMaterialization struct {
	Directory             string
	Managed               bool
	RepositoryFingerprint *string
	CurrentBranch         *string
	BaseCommit            *string
	HeadCommit            *string
}

type WorkspaceMaterializer struct {
	root     string
	resolver gitpolicy.Resolver
	runGit   func(context.Context, string, ...string) (string, error)
}

func NewWorkspaceMaterializer(root string) *WorkspaceMaterializer {
	materializer := &WorkspaceMaterializer{root: root, resolver: net.DefaultResolver}
	materializer.runGit = materializer.runGitCommand
	return materializer
}

func (m *WorkspaceMaterializer) Materialize(
	ctx context.Context,
	execution executions.Execution,
	workload executions.Workload,
) (WorkspaceMaterialization, error) {
	directory, managed, err := m.workspaceDirectory(execution, workload)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", err.Error(), true, false)
	}
	if workload.RepositoryURL == nil || strings.TrimSpace(*workload.RepositoryURL) == "" {
		return WorkspaceMaterialization{Directory: directory, Managed: managed}, nil
	}
	remote, err := gitpolicy.ResolveRemoteHTTPS(ctx, m.resolver, *workload.RepositoryURL)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "Repository URL is not allowed for a remote Workspace.", true, false,
		)
	}
	defaultBranch, err := gitpolicy.NormalizeBranch(workload.DefaultBranch, "main")
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The default Git branch is invalid.", true, false,
		)
	}
	fingerprint := gitpolicy.Fingerprint(remote.URL)
	if workload.WorkspaceRepositoryFingerprint != nil &&
		strings.TrimSpace(*workload.WorkspaceRepositoryFingerprint) != fingerprint {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Project repository no longer matches the logical Workspace.", true, false,
		)
	}
	gitDirectory := filepath.Join(directory, ".git")
	gitInfo, statErr := os.Lstat(gitDirectory)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		empty, emptyErr := directoryEmpty(directory)
		if emptyErr != nil || !empty {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Workspace path is not an empty Git checkout.", true, false,
			)
		}
		if _, err := m.runGit(ctx, m.root, networkGitArguments(
			remote, "clone", "--no-tags", "--single-branch", "--branch", defaultBranch, "--", remote.URL, directory,
		)...); err != nil {
			_ = clearDirectory(directory)
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Git repository could not be cloned.", true, true,
			)
		}
		branch := sessionBranch(workload.SessionID.String())
		if _, err := m.runGit(ctx, directory, "switch", "-c", branch); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The isolated Session branch could not be created.", true, true,
			)
		}
	case statErr != nil:
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout is unavailable.", true, true)
	case gitInfo.Mode()&os.ModeSymlink != 0 || !gitInfo.IsDir():
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git metadata path is unsafe.", true, false)
	default:
		topLevel, err := m.runGit(ctx, directory, "rev-parse", "--show-toplevel")
		if err != nil || filepath.Clean(topLevel) != filepath.Clean(directory) {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Workspace is not the expected Git checkout.", true, false)
		}
		if _, err := m.runGit(ctx, directory, "remote", "set-url", "origin", remote.URL); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git remote could not be secured.", true, true)
		}
		refspec := "+refs/heads/" + defaultBranch + ":refs/remotes/origin/" + defaultBranch
		if _, err := m.runGit(ctx, directory, networkGitArguments(
			remote, "fetch", "--prune", "--no-tags", "origin", refspec,
		)...); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git repository could not be fetched.", true, true)
		}
		currentBranch, branchErr := m.runGit(ctx, directory, "branch", "--show-current")
		if branchErr != nil || currentBranch == "" {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout does not have a current branch.", true, false)
		}
		if currentBranch == defaultBranch {
			expectedBranch := sessionBranch(workload.SessionID.String())
			existingBranch, listErr := m.runGit(ctx, directory, "branch", "--list", expectedBranch)
			if listErr != nil {
				return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The isolated Session branch could not be inspected.", true, true)
			}
			arguments := []string{"switch", expectedBranch}
			if strings.TrimSpace(existingBranch) == "" {
				arguments = []string{"switch", "-c", expectedBranch}
			}
			if _, err := m.runGit(ctx, directory, arguments...); err != nil {
				return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The isolated Session branch could not be restored.", true, true)
			}
		}
	}
	currentBranch, err := m.runGit(ctx, directory, "branch", "--show-current")
	if err != nil || currentBranch == "" {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout does not have a current branch.", true, false)
	}
	currentBranch, err = gitpolicy.NormalizeBranch(currentBranch, "")
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout branch is invalid.", true, false)
	}
	headCommit, err := m.runGit(ctx, directory, "rev-parse", "HEAD")
	if err != nil || !validGitObjectID(headCommit) {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout HEAD is invalid.", true, false)
	}
	baseCommit, err := m.runGit(ctx, directory, "merge-base", "HEAD", "refs/remotes/origin/"+defaultBranch)
	if err != nil || !validGitObjectID(baseCommit) {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout base commit is invalid.", true, false)
	}
	return WorkspaceMaterialization{
		Directory: directory, Managed: managed, RepositoryFingerprint: &fingerprint,
		CurrentBranch: &currentBranch, BaseCommit: &baseCommit, HeadCommit: &headCommit,
	}, nil
}

func (m *WorkspaceMaterializer) workspaceDirectory(
	execution executions.Execution,
	workload executions.Workload,
) (string, bool, error) {
	root, err := filepath.Abs(strings.TrimSpace(m.root))
	if err != nil || strings.TrimSpace(m.root) == "" {
		return "", false, errors.New("Workspace root is invalid")
	}
	segments := []string{
		workload.TenantID.String(), workload.ProjectID.String(), workload.SessionID.String(), execution.ID.String(),
	}
	managed := workload.RemoteWorkspaceID != nil
	if managed {
		segments[len(segments)-1] = workload.RemoteWorkspaceID.String()
	}
	directory := filepath.Join(append([]string{root}, segments...)...)
	if err := ensureContainedDirectory(root, directory); err != nil {
		return "", false, err
	}
	return directory, managed, nil
}

func (m *WorkspaceMaterializer) runGitCommand(ctx context.Context, directory string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	command.Env = gitEnvironment()
	var stdout bytes.Buffer
	stderr := &boundedBuffer{maximum: 32 << 10}
	command.Stdout = &stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git command failed: %s", message)
	}
	if stdout.Len() > 8<<10 {
		return "", errors.New("git command output exceeded the safe limit")
	}
	return strings.TrimSpace(stdout.String()), nil
}

func gitEnvironment() []string {
	environment := []string{
		"LC_ALL=C", "LANG=C", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ASKPASS=/bin/false",
		"GIT_LFS_SKIP_SMUDGE=1",
		"SSH_ASKPASS=/bin/false", "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityFile=/dev/null -o StrictHostKeyChecking=yes",
	}
	for _, name := range []string{"PATH", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func networkGitArguments(remote gitpolicy.Remote, arguments ...string) []string {
	address := remote.PinnedIP
	if strings.Contains(address, ":") {
		address = "[" + address + "]"
	}
	resolve := "+" + remote.Hostname + ":" + remote.Port + ":" + address
	prefix := []string{"-c", "http.followRedirects=false", "-c", "http.curloptResolve=" + resolve}
	return append(prefix, arguments...)
}

func ensureContainedDirectory(root, directory string) error {
	root = filepath.Clean(root)
	directory = filepath.Clean(directory)
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("Workspace path escapes the configured root")
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return err
	}
	if err := ensureRealDirectory(root); err != nil {
		return err
	}
	current := root
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("Workspace path contains an invalid segment")
		}
		current = filepath.Join(current, segment)
		if err := ensureRealDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func ensureRealDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(directory, 0o700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Workspace path contains a symlink or non-directory component")
	}
	return nil
}

func directoryEmpty(directory string) (bool, error) {
	entries, err := os.ReadDir(directory)
	return len(entries) == 0, err
}

func clearDirectory(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func sessionBranch(sessionID string) string {
	return "synara/session-" + strings.ReplaceAll(sessionID, "-", "")
}

func validGitObjectID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 7 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func workspaceFailure(code, message string, userAction, moveWorker bool) *runnerFailure {
	return &runnerFailure{
		code: code, message: message, retryable: moveWorker,
		requiresNewExecution: true, requiresUserAction: userAction,
		canReconstructFromHistory: true, canMoveWorker: moveWorker,
	}
}
