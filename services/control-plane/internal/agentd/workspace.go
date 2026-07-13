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

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

type workspaceMaterializer interface {
	Materialize(context.Context, executions.Execution, executions.Workload, *GitHTTPSCredential) (WorkspaceMaterialization, error)
}

type workspaceInspector interface {
	Inspect(context.Context, WorkspaceMaterialization) (WorkspaceInspection, error)
}

type workspaceRestorer interface {
	Restore(context.Context, WorkspaceMaterialization, executions.WorkspaceCheckpoint, string) (WorkspaceMaterialization, error)
}

type WorkspaceMaterialization struct {
	Directory             string
	LogicalRoot           string
	GitDir                string
	Managed               bool
	RestoredCheckpointID  *uuid.UUID
	RepositoryFingerprint *string
	CurrentBranch         *string
	BaseCommit            *string
	HeadCommit            *string
	cache                 workspaceCacheLayout
	manifest              workspaceGenerationManifest
	release               func() error
}

type WorkspaceInspection struct {
	Dirty         bool
	CurrentBranch *string
	HeadCommit    *string
}

type WorkspaceMaterializer struct {
	root       string
	cacheRoot  string
	targetID   uuid.UUID
	resolver   gitpolicy.Resolver
	runGit     func(context.Context, string, []string, ...string) (string, error)
	executable func() (string, error)
}

func NewWorkspaceMaterializer(root string) *WorkspaceMaterializer {
	return NewWorkspaceMaterializerWithCache(root, filepath.Join(filepath.Dir(root), filepath.Base(root)+"-git-cache"), uuid.Nil)
}

func NewWorkspaceMaterializerWithCache(root, cacheRoot string, targetID uuid.UUID) *WorkspaceMaterializer {
	if strings.TrimSpace(cacheRoot) == "" {
		cacheRoot = filepath.Join(filepath.Dir(root), filepath.Base(root)+"-git-cache")
	}
	materializer := &WorkspaceMaterializer{
		root: root, cacheRoot: cacheRoot, targetID: targetID,
		resolver: net.DefaultResolver, executable: os.Executable,
	}
	materializer.runGit = materializer.runGitCommand
	return materializer
}

func (m WorkspaceMaterialization) Release() error {
	if m.release == nil {
		return nil
	}
	return m.release()
}

func (m *WorkspaceMaterializer) Materialize(
	ctx context.Context,
	execution executions.Execution,
	workload executions.Workload,
	credential *GitHTTPSCredential,
) (WorkspaceMaterialization, error) {
	layout, err := m.resolveWorkspaceLayout(execution, workload)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", err.Error(), true, false)
	}
	managed := workload.RemoteWorkspaceID != nil
	workspaceLock, err := acquireWorkspaceFileLock(ctx, m.root, layout.LockPath)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Workspace is already in use or could not be locked.", false, true,
		)
	}
	release := workspaceLock.Release
	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			_ = release()
		}
	}()
	baseMaterialization := WorkspaceMaterialization{
		Directory: layout.Checkout, LogicalRoot: layout.Root,
		Managed: managed, release: release,
	}
	if workload.RepositoryURL == nil || strings.TrimSpace(*workload.RepositoryURL) == "" {
		if credential != nil {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "A Git Credential cannot be used without a Project repository.", true, false,
			)
		}
		expected := expectedWorkspaceManifest(layout, workload, managed, "", "", "")
		baseMaterialization.manifest = expected
		exists, existsErr := pathExists(layout.Root)
		if existsErr != nil {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Workspace generation is unavailable.", true, true)
		}
		if exists {
			if err := validateNonGitGeneration(layout, expected); err == nil {
				releaseOnFailure = false
				return baseMaterialization, nil
			} else if workload.RestoreCheckpoint == nil {
				return WorkspaceMaterialization{}, workspaceFailure(
					"workspace_invalid", "The existing Workspace generation is invalid and has no Ready Checkpoint.", true, false,
				)
			}
		} else if legacyData, legacyErr := legacyWorkspaceContainsData(layout.LegacyRoot); legacyErr != nil || (legacyData && workload.RestoreCheckpoint == nil) {
			return WorkspaceMaterialization{}, workspaceFailure(
				"workspace_invalid", "A legacy Workspace may contain unrecoverable local data and was preserved.", true, false,
			)
		}
		staging, err := createWorkspaceStagingRoot(layout.Root)
		if err != nil {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Workspace generation could not be staged.", false, true)
		}
		defer os.RemoveAll(staging)
		if err := buildNonGitWorkspaceGeneration(staging, expected); err != nil || replaceWorkspaceGeneration(layout.Root, staging) != nil {
			return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Workspace generation could not be installed.", false, true)
		}
		releaseOnFailure = false
		return baseMaterialization, nil
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
	expected := expectedWorkspaceManifest(layout, workload, managed, fingerprint, remote.URL, defaultBranch)
	cache, err := m.resolveCacheLayout(layout, workload.TenantID, workload.ProjectID, fingerprint)
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", err.Error(), true, false)
	}
	baseMaterialization.cache = cache
	baseMaterialization.GitDir = layout.GitDir
	baseMaterialization.manifest = expected
	baseMaterialization.RepositoryFingerprint = &fingerprint
	if err := m.withPreparedCache(ctx, cache, remote, defaultBranch, credential, func(cacheRepository string) error {
		exists, err := pathExists(layout.Root)
		if err != nil {
			return err
		}
		if exists {
			if err := m.validatePrivateGitGeneration(ctx, layout, expected); err == nil {
				if err := m.fetchPrivateRepositoryFromCache(ctx, layout.Root, layout.GitDir, cacheRepository, defaultBranch); err != nil {
					return err
				}
				return m.validatePrivateGitGeneration(ctx, layout, expected)
			} else if workload.RestoreCheckpoint == nil {
				return errors.New("existing Workspace generation is invalid and has no Ready Checkpoint")
			}
		} else if legacyData, legacyErr := legacyWorkspaceContainsData(layout.LegacyRoot); legacyErr != nil {
			return legacyErr
		} else if legacyData && workload.RestoreCheckpoint == nil {
			reconstructable, inspectErr := m.legacyGitWorkspaceMatchesCache(
				ctx, layout.LegacyRoot, cacheRepository, remote.URL, defaultBranch,
			)
			if inspectErr != nil || !reconstructable {
				return errors.New("legacy Workspace may contain unrecoverable local data and was preserved")
			}
		}
		staging, err := createWorkspaceStagingRoot(layout.Root)
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		if err := m.buildPrivateGitGeneration(
			ctx, staging, expected, cacheRepository, sessionBranch(workload.SessionID.String()), "",
		); err != nil {
			return err
		}
		return replaceWorkspaceGeneration(layout.Root, staging)
	}); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure(
			"workspace_invalid", "The Git Workspace could not be prepared from its validated cache.", true, true,
		)
	}
	if err := m.validatePrivateGitGeneration(ctx, layout, expected); err != nil {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The private Git Workspace failed validation.", true, false)
	}
	currentBranch, err := m.runGit(ctx, layout.Checkout, environment, "branch", "--show-current")
	if err != nil || currentBranch == "" {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout does not have a current branch.", true, false)
	}
	currentBranch, err = gitpolicy.NormalizeBranch(currentBranch, "")
	if err != nil {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout branch is invalid.", true, false)
	}
	headCommit, err := m.runGit(ctx, layout.Checkout, environment, "rev-parse", "HEAD")
	if err != nil || !validGitObjectID(headCommit) {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout HEAD is invalid.", true, false)
	}
	baseCommit, err := m.runGit(ctx, layout.Checkout, environment, "merge-base", "HEAD", "refs/remotes/origin/"+defaultBranch)
	if err != nil || !validGitObjectID(baseCommit) {
		return WorkspaceMaterialization{}, workspaceFailure("workspace_invalid", "The Git checkout base commit is invalid.", true, false)
	}
	baseMaterialization.CurrentBranch = &currentBranch
	baseMaterialization.BaseCommit = &baseCommit
	baseMaterialization.HeadCommit = &headCommit
	releaseOnFailure = false
	return baseMaterialization, nil
}

func (m *WorkspaceMaterializer) Inspect(
	ctx context.Context,
	materialized WorkspaceMaterialization,
) (WorkspaceInspection, error) {
	if !materialized.Managed {
		return WorkspaceInspection{}, nil
	}
	if materialized.LogicalRoot != "" && materialized.GitDir == "" {
		layout := workspaceLayout{
			Root: materialized.LogicalRoot, Checkout: materialized.Directory,
			GitDir: filepath.Join(materialized.LogicalRoot, "repo.git"), Manifest: filepath.Join(materialized.LogicalRoot, "manifest.json"),
		}
		if err := validateWorkspaceGenerationPath(m.root, layout.Root); err != nil {
			return WorkspaceInspection{}, errors.New("Workspace generation path became invalid")
		}
		if err := validateNonGitGeneration(layout, materialized.manifest); err != nil {
			return WorkspaceInspection{}, errors.New("Workspace generation became invalid")
		}
	}
	gitDirectory := filepath.Join(materialized.Directory, ".git")
	gitInfo, err := os.Lstat(gitDirectory)
	if errors.Is(err, os.ErrNotExist) {
		dirty, inspectErr := directoryContainsEntries(materialized.Directory)
		return WorkspaceInspection{Dirty: dirty}, inspectErr
	}
	if err != nil || gitInfo.Mode()&os.ModeSymlink != 0 || (!gitInfo.IsDir() && !gitInfo.Mode().IsRegular()) {
		return WorkspaceInspection{}, errors.New("Workspace Git metadata is unavailable")
	}
	environment := gitEnvironment(nil)
	if materialized.GitDir != "" {
		layout := workspaceLayout{
			Root: materialized.LogicalRoot, Checkout: materialized.Directory, GitDir: materialized.GitDir,
			Manifest: filepath.Join(materialized.LogicalRoot, "manifest.json"),
		}
		if err := m.validatePrivateGitGeneration(ctx, layout, materialized.manifest); err != nil {
			return WorkspaceInspection{}, errors.New("Workspace private Git generation became invalid")
		}
	}
	if err := m.rejectDangerousLocalGitConfig(ctx, materialized.Directory, environment); err != nil {
		return WorkspaceInspection{}, errors.New("Workspace Git configuration became unsafe")
	}
	if err := rejectUnsupportedPatchGitMetadata(ctx, materialized.Directory); err != nil {
		return WorkspaceInspection{}, errors.New("Workspace Git metadata became unsupported")
	}
	if err := rejectUnsupportedPatchIndexFlags(ctx, materialized.Directory); err != nil {
		return WorkspaceInspection{}, errors.New("Workspace Git index uses unsupported flags")
	}
	command := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "--untracked-files=all")
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
	dirty := stdout.buffer.Len() > 0
	if !dirty {
		ignored, ignoredErr := ignoredPatchFilePaths(ctx, materialized.Directory)
		if ignoredErr != nil {
			return WorkspaceInspection{}, errors.New("Workspace ignored files could not be inspected")
		}
		dirty = len(ignored) > 0
	}
	if materialized.CurrentBranch != nil && *materialized.CurrentBranch != currentBranch {
		dirty = true
	}
	if materialized.HeadCommit != nil && *materialized.HeadCommit != headCommit {
		dirty = true
	}
	return WorkspaceInspection{
		Dirty: dirty, CurrentBranch: &currentBranch, HeadCommit: &headCommit,
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
	cacheRoot, err := filepath.Abs(strings.TrimSpace(m.cacheRoot))
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
		if pathContainedBy(workspaceRoot, server.directory) || pathContainedBy(cacheRoot, server.directory) {
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
		key, value := localGitConfigEntry(entry)
		if dangerousLocalGitConfigEntry(key, value) {
			return errors.New("unsafe local Git configuration")
		}
	}
	return nil
}

func localGitConfigEntry(entry string) (string, string) {
	separator := strings.IndexAny(entry, "=\n")
	if separator < 0 {
		return strings.ToLower(strings.TrimSpace(entry)), ""
	}
	return strings.ToLower(strings.TrimSpace(entry[:separator])), strings.TrimSpace(entry[separator+1:])
}

func dangerousLocalGitConfigEntry(key, value string) bool {
	if dangerousLocalGitConfigKey(key) {
		return true
	}
	switch key {
	case "core.autocrlf":
		return !strings.EqualFold(strings.TrimSpace(value), "false")
	case "core.filemode", "core.symlinks":
		return !gitConfigTrue(value)
	case "core.ignorestat", "core.sparsecheckout", "core.sparsecheckoutcone", "index.sparse":
		return gitConfigTrue(value)
	case "core.safecrlf":
		return !strings.EqualFold(strings.TrimSpace(value), "false")
	default:
		return false
	}
}

func gitConfigTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "on", "true", "yes":
		return true
	default:
		return false
	}
}

func dangerousLocalGitConfigKey(key string) bool {
	return strings.HasPrefix(key, "credential.") || strings.HasPrefix(key, "color.") ||
		strings.HasPrefix(key, "diff.") || key == "core.askpass" || key == "core.hookspath" ||
		key == "core.attributesfile" || key == "core.bigfilethreshold" || key == "core.eol" ||
		key == "core.fsmonitor" || key == "core.quotepath" || key == "core.worktree" ||
		key == "extensions.partialclone" || key == "extensions.worktreeconfig" ||
		key == "core.sshcommand" || key == "http.proxy" || key == "http.extraheader" ||
		(strings.HasPrefix(key, "http.") && (strings.HasSuffix(key, ".proxy") || strings.HasSuffix(key, ".extraheader"))) ||
		(strings.HasPrefix(key, "remote.") && (strings.HasSuffix(key, ".partialclonefilter") ||
			strings.HasSuffix(key, ".promisor") || strings.HasSuffix(key, ".proxy") ||
			strings.HasSuffix(key, ".receivepack") || strings.HasSuffix(key, ".uploadpack"))) ||
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
		"GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_LFS_SKIP_SMUDGE=1", "GIT_NO_REPLACE_OBJECTS=1",
		"SSH_ASKPASS=/bin/false", "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityFile=/dev/null -o StrictHostKeyChecking=yes",
		"GIT_CONFIG_COUNT=5",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1=",
		"GIT_CONFIG_KEY_2=http.proxy", "GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_KEY_3=http.extraHeader", "GIT_CONFIG_VALUE_3=",
		"GIT_CONFIG_KEY_4=protocol.file.allow", "GIT_CONFIG_VALUE_4=never",
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
