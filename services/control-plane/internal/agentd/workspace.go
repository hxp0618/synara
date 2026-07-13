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
	Materialize(context.Context, executions.Execution, executions.Workload, *GitHTTPSCredential) (WorkspaceMaterialization, error)
}

type workspaceInspector interface {
	Inspect(context.Context, WorkspaceMaterialization) (WorkspaceInspection, error)
}

type WorkspaceMaterialization struct {
	Directory             string
	Managed               bool
	RepositoryFingerprint *string
	CurrentBranch         *string
	BaseCommit            *string
	HeadCommit            *string
}

type WorkspaceInspection struct {
	Dirty         bool
	CurrentBranch *string
	HeadCommit    *string
}

type WorkspaceMaterializer struct {
	root       string
	resolver   gitpolicy.Resolver
	runGit     func(context.Context, string, []string, ...string) (string, error)
	executable func() (string, error)
}

func NewWorkspaceMaterializer(root string) *WorkspaceMaterializer {
	materializer := &WorkspaceMaterializer{root: root, resolver: net.DefaultResolver, executable: os.Executable}
	materializer.runGit = materializer.runGitCommand
	return materializer
}

func (m *WorkspaceMaterializer) Materialize(
	ctx context.Context,
	execution executions.Execution,
	workload executions.Workload,
	credential *GitHTTPSCredential,
) (WorkspaceMaterialization, error) {
	directory, managed, err := m.workspaceDirectory(execution, workload)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", err.Error(), true, false)
	}
	if workload.RepositoryURL == nil || strings.TrimSpace(*workload.RepositoryURL) == "" {
		if credential != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "A Git Credential cannot be used without a Project repository.", true, false,
			)
		}
		return WorkspaceMaterialization{Directory: directory, Managed: managed}, nil
	}
	remote, err := gitpolicy.ResolveRemoteHTTPS(ctx, m.resolver, *workload.RepositoryURL)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "Repository URL is not allowed for a remote Workspace.", true, false,
		)
	}
	environment := gitEnvironment(nil)
	if credential != nil {
		credentialHost, hostErr := gitpolicy.NormalizeHostname(credential.Host)
		if hostErr != nil || credentialHost != remote.Hostname || remote.Port != "443" ||
			strings.TrimSpace(credential.Username) == "" || credential.Token == "" {
			return WorkspaceMaterialization{}, workspaceFailure(
				"credential_invalid", "The Git Credential does not match the Project repository.", true, false,
			)
		}
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
		if _, err := m.runNetworkGit(
			ctx, m.root, remote, credential,
			"clone", "--no-tags", "--single-branch", "--branch", defaultBranch, "--", remote.URL, directory,
		); err != nil {
			_ = clearDirectory(directory)
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Git repository could not be cloned.", true, true,
			)
		}
		branch := sessionBranch(workload.SessionID.String())
		if _, err := m.runGit(ctx, directory, environment, "switch", "-c", branch); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The isolated Session branch could not be created.", true, true,
			)
		}
	case statErr != nil:
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout is unavailable.", true, true)
	case gitInfo.Mode()&os.ModeSymlink != 0 || !gitInfo.IsDir():
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git metadata path is unsafe.", true, false)
	default:
		topLevel, err := m.runGit(ctx, directory, environment, "rev-parse", "--show-toplevel")
		if err != nil || filepath.Clean(topLevel) != filepath.Clean(directory) {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Workspace is not the expected Git checkout.", true, false)
		}
		if err := m.rejectDangerousLocalGitConfig(ctx, directory, environment); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "The Git checkout contains unsafe local configuration.", true, false,
			)
		}
		if _, err := m.runGit(ctx, directory, environment, "remote", "set-url", "origin", remote.URL); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git remote could not be secured.", true, true)
		}
		refspec := "+refs/heads/" + defaultBranch + ":refs/remotes/origin/" + defaultBranch
		if _, err := m.runNetworkGit(
			ctx, directory, remote, credential, "fetch", "--prune", "--no-tags", "origin", refspec,
		); err != nil {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git repository could not be fetched.", true, true)
		}
		currentBranch, branchErr := m.runGit(ctx, directory, environment, "branch", "--show-current")
		if branchErr != nil || currentBranch == "" {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout does not have a current branch.", true, false)
		}
		if currentBranch == defaultBranch {
			expectedBranch := sessionBranch(workload.SessionID.String())
			existingBranch, listErr := m.runGit(ctx, directory, environment, "branch", "--list", expectedBranch)
			if listErr != nil {
				return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The isolated Session branch could not be inspected.", true, true)
			}
			arguments := []string{"switch", expectedBranch}
			if strings.TrimSpace(existingBranch) == "" {
				arguments = []string{"switch", "-c", expectedBranch}
			}
			if _, err := m.runGit(ctx, directory, environment, arguments...); err != nil {
				return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The isolated Session branch could not be restored.", true, true)
			}
		}
	}
	currentBranch, err := m.runGit(ctx, directory, environment, "branch", "--show-current")
	if err != nil || currentBranch == "" {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout does not have a current branch.", true, false)
	}
	currentBranch, err = gitpolicy.NormalizeBranch(currentBranch, "")
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout branch is invalid.", true, false)
	}
	headCommit, err := m.runGit(ctx, directory, environment, "rev-parse", "HEAD")
	if err != nil || !validGitObjectID(headCommit) {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout HEAD is invalid.", true, false)
	}
	baseCommit, err := m.runGit(ctx, directory, environment, "merge-base", "HEAD", "refs/remotes/origin/"+defaultBranch)
	if err != nil || !validGitObjectID(baseCommit) {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout base commit is invalid.", true, false)
	}
	return WorkspaceMaterialization{
		Directory: directory, Managed: managed, RepositoryFingerprint: &fingerprint,
		CurrentBranch: &currentBranch, BaseCommit: &baseCommit, HeadCommit: &headCommit,
	}, nil
}

func (m *WorkspaceMaterializer) Inspect(
	ctx context.Context,
	materialized WorkspaceMaterialization,
) (WorkspaceInspection, error) {
	if !materialized.Managed {
		return WorkspaceInspection{}, nil
	}
	gitDirectory := filepath.Join(materialized.Directory, ".git")
	gitInfo, err := os.Lstat(gitDirectory)
	if errors.Is(err, os.ErrNotExist) {
		dirty, inspectErr := directoryContainsEntries(materialized.Directory)
		return WorkspaceInspection{Dirty: dirty}, inspectErr
	}
	if err != nil || gitInfo.Mode()&os.ModeSymlink != 0 || !gitInfo.IsDir() {
		return WorkspaceInspection{}, errors.New("Workspace Git metadata is unavailable")
	}
	environment := gitEnvironment(nil)
	if err := m.rejectDangerousLocalGitConfig(ctx, materialized.Directory, environment); err != nil {
		return WorkspaceInspection{}, errors.New("Workspace Git configuration became unsafe")
	}
	command := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "--untracked-files=normal")
	command.Dir = materialized.Directory
	command.Env = environment
	stdout := &boundedBuffer{maximum: 1}
	stderr := &boundedBuffer{maximum: 32 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return WorkspaceInspection{}, errors.New("Workspace Git status could not be inspected")
	}
	currentBranch, err := m.runGit(ctx, materialized.Directory, environment, "branch", "--show-current")
	if err != nil || currentBranch == "" {
		return WorkspaceInspection{}, errors.New("Workspace Git branch could not be inspected")
	}
	currentBranch, err = gitpolicy.NormalizeBranch(currentBranch, "")
	if err != nil {
		return WorkspaceInspection{}, errors.New("Workspace Git branch is invalid")
	}
	headCommit, err := m.runGit(ctx, materialized.Directory, environment, "rev-parse", "HEAD")
	if err != nil || !validGitObjectID(headCommit) {
		return WorkspaceInspection{}, errors.New("Workspace Git HEAD could not be inspected")
	}
	return WorkspaceInspection{
		Dirty: stdout.buffer.Len() > 0, CurrentBranch: &currentBranch, HeadCommit: &headCommit,
	}, nil
}

func (m *WorkspaceMaterializer) runNetworkGit(
	ctx context.Context,
	directory string,
	remote gitpolicy.Remote,
	credential *GitHTTPSCredential,
	arguments ...string,
) (string, error) {
	environment := gitEnvironment(nil)
	if credential == nil {
		return m.runGit(ctx, directory, environment, networkGitArguments(remote, arguments...)...)
	}
	askPass, err := m.newWorkspaceGitAskPassServer(remote.Hostname, credential.Username, credential.Token)
	if err != nil {
		return "", errors.New("Git Credential helper could not be initialized")
	}
	executable, executableErr := m.executable()
	if executableErr != nil {
		_ = askPass.Close()
		return "", errors.New("Git Credential helper executable is unavailable")
	}
	askPassEnvironment, environmentErr := askPass.Environment(executable)
	if environmentErr != nil {
		_ = askPass.Close()
		return "", errors.New("Git Credential helper environment is invalid")
	}
	output, runErr := m.runGit(
		ctx, directory, gitEnvironment(&askPassEnvironment), networkGitArguments(remote, arguments...)...,
	)
	closeErr := askPass.Close()
	if runErr != nil {
		return "", redactGitCredentialError(runErr, credential.Token)
	}
	if closeErr != nil {
		return "", errors.New("Git Credential helper cleanup failed")
	}
	return output, nil
}

func (m *WorkspaceMaterializer) newWorkspaceGitAskPassServer(
	expectedHost, username, token string,
) (*gitAskPassServer, error) {
	workspaceRoot, err := filepath.Abs(strings.TrimSpace(m.root))
	if err != nil {
		return nil, err
	}
	candidates := []string{os.TempDir(), "/tmp", filepath.Dir(workspaceRoot)}
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		candidate, err = filepath.Abs(strings.TrimSpace(candidate))
		if err != nil || candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		server, createErr := newGitAskPassServer(candidate, expectedHost, username, token)
		if createErr != nil {
			continue
		}
		if pathContainedBy(workspaceRoot, server.directory) {
			_ = server.Close()
			continue
		}
		return server, nil
	}
	return nil, errors.New("no safe temporary directory is available for Git AskPass")
}

func (m *WorkspaceMaterializer) rejectDangerousLocalGitConfig(
	ctx context.Context,
	directory string,
	environment []string,
) error {
	output, err := m.runGit(ctx, directory, environment, "config", "--local", "--no-includes", "--null", "--list")
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(output, "\x00") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key := entry
		if separator := strings.IndexAny(key, "=\n"); separator >= 0 {
			key = key[:separator]
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if dangerousLocalGitConfigKey(key) {
			return errors.New("unsafe local Git configuration")
		}
	}
	return nil
}

func dangerousLocalGitConfigKey(key string) bool {
	return strings.HasPrefix(key, "credential.") || key == "core.askpass" || key == "core.hookspath" ||
		key == "core.sshcommand" || key == "http.proxy" || key == "http.extraheader" ||
		(strings.HasPrefix(key, "http.") && (strings.HasSuffix(key, ".proxy") || strings.HasSuffix(key, ".extraheader"))) ||
		(strings.HasPrefix(key, "remote.") && (strings.HasSuffix(key, ".proxy") || strings.HasSuffix(key, ".receivepack") || strings.HasSuffix(key, ".uploadpack"))) ||
		(strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof"))) ||
		key == "include.path" || strings.HasPrefix(key, "includeif.") ||
		(strings.HasPrefix(key, "filter.") && (strings.HasSuffix(key, ".process") || strings.HasSuffix(key, ".clean") || strings.HasSuffix(key, ".smudge")))
}

func pathContainedBy(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)))
}

func redactGitCredentialError(err error, token string) error {
	message := err.Error()
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	return errors.New(message)
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

func (m *WorkspaceMaterializer) runGitCommand(ctx context.Context, directory string, environment []string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	command.Env = environment
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

func gitEnvironment(askPass *gitAskPassEnvironment) []string {
	environment := []string{
		"LC_ALL=C", "LANG=C", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_LFS_SKIP_SMUDGE=1",
		"SSH_ASKPASS=/bin/false", "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityFile=/dev/null -o StrictHostKeyChecking=yes",
	}
	if askPass == nil {
		environment = append(environment, "GIT_ASKPASS=/bin/false")
	} else {
		environment = append(environment,
			"GIT_ASKPASS="+askPass.Executable,
			"GIT_ASKPASS_REQUIRE=force",
			GitAskPassSocketEnvironment+"="+askPass.SocketPath,
		)
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
	prefix := []string{
		"-c", "http.followRedirects=false",
		"-c", "http.curloptResolve=" + resolve,
		"-c", "credential.helper=",
		"-c", "core.hooksPath=/dev/null",
		"-c", "http.proxy=",
		"-c", "http.extraHeader=",
	}
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

func directoryContainsEntries(directory string) (bool, error) {
	entries, err := os.ReadDir(directory)
	return len(entries) > 0, err
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
