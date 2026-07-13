package agentd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

type workspaceCacheLayout struct {
	Root     string
	RepoGit  string
	LockPath string
}

func (m *WorkspaceMaterializer) resolveCacheLayout(
	layout workspaceLayout,
	tenantID, projectID uuid.UUID,
	repositoryFingerprint string,
) (workspaceCacheLayout, error) {
	if len(repositoryFingerprint) != 64 || !validGitObjectID(repositoryFingerprint) {
		return workspaceCacheLayout{}, errors.New("Repository fingerprint is invalid")
	}
	segments := []string{layout.TargetID.String(), tenantID.String(), projectID.String(), repositoryFingerprint}
	root := filepath.Join(append([]string{m.cacheRoot, "v1"}, segments...)...)
	if !pathContainedBy(m.cacheRoot, root) || root == m.cacheRoot {
		return workspaceCacheLayout{}, errors.New("Git cache path escapes the configured root")
	}
	if err := ensureContainedDirectory(m.cacheRoot, root); err != nil {
		return workspaceCacheLayout{}, err
	}
	lockPath, err := lockPathFor(m.cacheRoot, "git-cache-v1", segments...)
	if err != nil {
		return workspaceCacheLayout{}, err
	}
	return workspaceCacheLayout{Root: root, RepoGit: filepath.Join(root, "repo.git"), LockPath: lockPath}, nil
}

func (m *WorkspaceMaterializer) withPreparedCache(
	ctx context.Context,
	cache workspaceCacheLayout,
	remote gitpolicy.Remote,
	defaultBranch string,
	credential *GitHTTPSCredential,
	use func(string) error,
) error {
	lock, err := acquireWorkspaceFileLock(ctx, m.cacheRoot, cache.LockPath)
	if err != nil {
		return fmt.Errorf("acquire Git cache lock: %w", err)
	}
	defer lock.Release()
	if err := m.ensureCacheRepository(ctx, cache, remote, defaultBranch, credential); err != nil {
		return err
	}
	if use != nil {
		return use(cache.RepoGit)
	}
	return nil
}

func (m *WorkspaceMaterializer) withCacheReadLock(
	ctx context.Context,
	cache workspaceCacheLayout,
	repositoryURL string,
	use func(string) error,
) error {
	lock, err := acquireWorkspaceFileLock(ctx, m.cacheRoot, cache.LockPath)
	if err != nil {
		return fmt.Errorf("acquire Git cache lock: %w", err)
	}
	defer lock.Release()
	if err := m.validateBareRepository(ctx, cache.RepoGit, repositoryURL); err != nil {
		return errors.New("Git cache is unavailable for Workspace recovery")
	}
	return use(cache.RepoGit)
}

func (m *WorkspaceMaterializer) ensureCacheRepository(
	ctx context.Context,
	cache workspaceCacheLayout,
	remote gitpolicy.Remote,
	defaultBranch string,
	credential *GitHTTPSCredential,
) error {
	if m.validateBareRepository(ctx, cache.RepoGit, remote.URL) == nil {
		if err := m.fetchCacheRepository(ctx, cache.RepoGit, remote, defaultBranch, credential); err == nil {
			return nil
		}
	}
	staging := filepath.Join(cache.Root, ".repo.git.staging-"+uuid.NewString())
	defer os.RemoveAll(staging)
	if err := m.initializeBareRepository(ctx, cache.Root, staging, defaultBranch, remote.URL); err != nil {
		return errors.New("Git cache repository could not be initialized")
	}
	if err := m.fetchCacheRepository(ctx, staging, remote, defaultBranch, credential); err != nil {
		return errors.New("Git repository could not be fetched into the cache")
	}
	if err := m.validateBareRepository(ctx, staging, remote.URL); err != nil {
		return errors.New("Git cache repository failed validation")
	}
	if err := replaceWorkspaceGeneration(cache.RepoGit, staging); err != nil {
		return errors.New("Git cache repository could not be installed")
	}
	return nil
}

func (m *WorkspaceMaterializer) initializeBareRepository(
	ctx context.Context,
	parent, repository, defaultBranch, repositoryURL string,
) error {
	if _, err := m.runGit(
		ctx, parent, gitEnvironment(nil),
		"init", "--bare", "--template=", "--initial-branch="+defaultBranch, "--", repository,
	); err != nil {
		return err
	}
	settings := [][]string{
		{"config", "core.filemode", "true"},
		{"config", "core.symlinks", "true"},
		{"config", "core.autocrlf", "false"},
		{"config", "core.safecrlf", "false"},
		{"config", "gc.auto", "0"},
		{"config", "maintenance.auto", "false"},
		{"remote", "add", "origin", repositoryURL},
	}
	for _, arguments := range settings {
		if _, err := m.runGit(ctx, repository, gitEnvironment(nil), arguments...); err != nil {
			return err
		}
	}
	return nil
}

func (m *WorkspaceMaterializer) fetchCacheRepository(
	ctx context.Context,
	repository string,
	remote gitpolicy.Remote,
	defaultBranch string,
	credential *GitHTTPSCredential,
) error {
	refspec := "+refs/heads/" + defaultBranch + ":refs/remotes/origin/" + defaultBranch
	if _, err := m.runNetworkGit(
		ctx, repository, remote, credential, "fetch", "--prune", "--no-tags", "origin", refspec,
	); err != nil {
		return err
	}
	commit, err := m.runGit(
		ctx, repository, gitEnvironment(nil), "rev-parse", "refs/remotes/origin/"+defaultBranch+"^{commit}",
	)
	if err != nil || !validGitObjectID(commit) {
		return errors.New("Git cache default branch is unavailable")
	}
	return nil
}

func (m *WorkspaceMaterializer) validateBareRepository(
	ctx context.Context,
	repository, expectedRepositoryURL string,
) error {
	if err := validateExistingRealDirectory(repository); err != nil {
		return err
	}
	bare, err := m.runGit(ctx, repository, gitEnvironment(nil), "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return errors.New("repository is not bare")
	}
	if err := m.rejectDangerousLocalGitConfig(ctx, repository, gitEnvironment(nil)); err != nil {
		return err
	}
	origin, err := m.runGit(ctx, repository, gitEnvironment(nil), "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(origin) != expectedRepositoryURL {
		return errors.New("repository origin does not match its cache identity")
	}
	return rejectGitObjectAlternates(repository)
}

func rejectGitObjectAlternates(repository string) error {
	for _, relative := range []string{
		filepath.Join("objects", "info", "alternates"), filepath.Join("objects", "info", "http-alternates"),
	} {
		if _, err := os.Lstat(filepath.Join(repository, relative)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("Git repository uses object alternates")
		}
	}
	return nil
}

func (m *WorkspaceMaterializer) buildPrivateGitGeneration(
	ctx context.Context,
	stagingRoot string,
	expected workspaceGenerationManifest,
	cacheRepository string,
	branch string,
	startCommit string,
) error {
	if err := validateExistingRealDirectory(stagingRoot); err != nil {
		return err
	}
	repository := filepath.Join(stagingRoot, "repo.git")
	checkout := filepath.Join(stagingRoot, "checkout")
	if err := m.initializeBareRepository(
		ctx, stagingRoot, repository, expected.DefaultBranch, expected.RepositoryURL,
	); err != nil {
		return err
	}
	if err := m.fetchPrivateRepositoryFromCache(ctx, stagingRoot, repository, cacheRepository, expected.DefaultBranch); err != nil {
		return err
	}
	if strings.TrimSpace(startCommit) == "" {
		startCommit = "refs/remotes/origin/" + expected.DefaultBranch
	}
	resolvedCommit, err := m.runGit(ctx, repository, gitEnvironment(nil), "rev-parse", startCommit+"^{commit}")
	if err != nil || !validGitObjectID(resolvedCommit) {
		return errors.New("Workspace start Commit is unavailable in the private repository")
	}
	if _, err := m.runGit(
		ctx, stagingRoot, gitEnvironment(nil), "--git-dir="+repository,
		"worktree", "add", "--relative-paths", "-b", branch, "--", checkout, resolvedCommit,
	); err != nil {
		return err
	}
	if err := writeWorkspaceManifest(stagingRoot, expected); err != nil {
		return err
	}
	layout := workspaceLayout{
		Root: stagingRoot, Checkout: checkout, GitDir: repository, Manifest: filepath.Join(stagingRoot, "manifest.json"),
	}
	return m.validatePrivateGitGeneration(ctx, layout, expected)
}

func (m *WorkspaceMaterializer) fetchPrivateRepositoryFromCache(
	ctx context.Context,
	directory, repository, cacheRepository, defaultBranch string,
) error {
	cacheURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(cacheRepository)}).String()
	refspec := "+refs/remotes/origin/" + defaultBranch + ":refs/remotes/origin/" + defaultBranch
	_, err := m.runGit(
		ctx, directory, gitEnvironment(nil), "-c", "protocol.file.allow=always", "--git-dir="+repository,
		"fetch", "--no-tags", "--no-write-fetch-head", "--", cacheURL, refspec,
	)
	return err
}

func (m *WorkspaceMaterializer) validatePrivateGitGeneration(
	ctx context.Context,
	layout workspaceLayout,
	expected workspaceGenerationManifest,
) error {
	if err := validateWorkspaceGenerationPath(m.root, layout.Root); err != nil {
		return err
	}
	if err := validatePrivateWorktreeFilesystem(layout, expected); err != nil {
		return err
	}
	if err := m.rejectDangerousLocalGitConfig(ctx, layout.Checkout, gitEnvironment(nil)); err != nil {
		return err
	}
	topLevel, err := m.runGit(ctx, layout.Checkout, gitEnvironment(nil), "rev-parse", "--show-toplevel")
	if err != nil || !sameExistingPath(topLevel, layout.Checkout) {
		return errors.New("Workspace checkout top level is invalid")
	}
	commonDir, err := m.runGit(ctx, layout.Checkout, gitEnvironment(nil), "rev-parse", "--git-common-dir")
	if err != nil || !sameExistingPath(commonDir, layout.GitDir) {
		return errors.New("Workspace checkout does not use its private common Git directory")
	}
	origin, err := m.runGit(ctx, layout.Checkout, gitEnvironment(nil), "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(origin) != expected.RepositoryURL {
		return errors.New("Workspace origin does not match its manifest")
	}
	return rejectGitObjectAlternates(layout.GitDir)
}

func createWorkspaceStagingRoot(activeRoot string) (string, error) {
	parent := filepath.Dir(activeRoot)
	if err := validateExistingRealDirectory(parent); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(activeRoot)+".staging-*")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	return staging, nil
}

func (m *WorkspaceMaterializer) legacyGitWorkspaceMatchesCache(
	ctx context.Context,
	legacyRoot, cacheRepository, repositoryURL, defaultBranch string,
) (bool, error) {
	if err := validateExistingRealDirectory(legacyRoot); err != nil || !workspaceHasGitMetadata(legacyRoot) {
		return false, err
	}
	environment := gitEnvironment(nil)
	topLevel, err := m.runGit(ctx, legacyRoot, environment, "rev-parse", "--show-toplevel")
	if err != nil || !sameExistingPath(topLevel, legacyRoot) {
		return false, nil
	}
	if err := m.rejectDangerousLocalGitConfig(ctx, legacyRoot, environment); err != nil {
		return false, nil
	}
	origin, err := m.runGit(ctx, legacyRoot, environment, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(origin) != repositoryURL {
		return false, nil
	}
	status, err := m.runGit(ctx, legacyRoot, environment, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || strings.TrimSpace(status) != "" {
		return false, nil
	}
	ignored, err := ignoredPatchFilePaths(ctx, legacyRoot)
	if err != nil || len(ignored) != 0 {
		return false, nil
	}
	if err := rejectUnsupportedPatchGitMetadata(ctx, legacyRoot); err != nil {
		return false, nil
	}
	if err := rejectUnsupportedPatchIndexFlags(ctx, legacyRoot); err != nil {
		return false, nil
	}
	legacyHead, err := m.runGit(ctx, legacyRoot, environment, "rev-parse", "HEAD^{commit}")
	if err != nil || !validGitObjectID(legacyHead) {
		return false, nil
	}
	cacheHead, err := m.runGit(
		ctx, cacheRepository, environment, "rev-parse", "refs/remotes/origin/"+defaultBranch+"^{commit}",
	)
	if err != nil || !validGitObjectID(cacheHead) {
		return false, err
	}
	return legacyHead == cacheHead, nil
}
