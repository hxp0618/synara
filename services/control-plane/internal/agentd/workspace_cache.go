package agentd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

type workspaceCacheLayout struct {
	Root        string
	RepoGit     string
	FetchMarker string
	LockPath    string
}

const (
	workspaceCacheFetchMarkerName       = "last-successful-fetch"
	workspaceCacheFetchMarkerMaxSize    = int64(128)
	workspaceCacheFetchOutcomeFetched   = "fetched"
	workspaceCacheFetchOutcomeFreshSkip = "cache-fresh-skip"
	workspaceCacheFetchOutcomeFallback  = "fallback-after-skip-doubt"
)

type workspaceCacheRefresh func(context.Context, *WorkspaceGitCredential) error

type workspaceCachePreparation struct {
	outcome           string
	backgroundRefresh workspaceCacheRefresh
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
	return workspaceCacheLayout{
		Root: root, RepoGit: filepath.Join(root, "repo.git"),
		FetchMarker: filepath.Join(root, workspaceCacheFetchMarkerName), LockPath: lockPath,
	}, nil
}

func (m *WorkspaceMaterializer) withPreparedCache(
	ctx context.Context,
	cache workspaceCacheLayout,
	remote gitpolicy.Remote,
	defaultBranch string,
	requiredCommits []string,
	credential *WorkspaceGitCredential,
	use func(string) error,
) (workspaceCachePreparation, error) {
	lock, err := acquireWorkspaceFileLock(ctx, m.cacheRoot, cache.LockPath)
	if err != nil {
		return workspaceCachePreparation{}, fmt.Errorf("acquire Git cache lock: %w", err)
	}
	defer lock.Release()
	cacheReconciled := false
	if m.fetchFreshnessWindow > 0 {
		if err := m.reconcileCacheRepository(ctx, cache, remote.URL); err != nil {
			return workspaceCachePreparation{}, errors.New("Git cache repository could not be reconciled")
		}
		cacheReconciled = true
		if m.cacheFetchIsFresh(ctx, cache, remote.URL, defaultBranch, requiredCommits) {
			preparation := workspaceCachePreparation{
				outcome: workspaceCacheFetchOutcomeFreshSkip,
				backgroundRefresh: func(refreshContext context.Context, refreshCredential *WorkspaceGitCredential) error {
					return m.refreshCacheRepository(
						refreshContext, cache, remote, defaultBranch, refreshCredential,
					)
				},
			}
			if use != nil {
				if err := use(cache.RepoGit); err != nil {
					return workspaceCachePreparation{}, err
				}
			}
			return preparation, nil
		}
	}
	if err := m.ensureCacheRepository(ctx, cache, remote, defaultBranch, credential, cacheReconciled, false); err != nil {
		return workspaceCachePreparation{}, err
	}
	if use != nil {
		if err := use(cache.RepoGit); err != nil {
			return workspaceCachePreparation{}, err
		}
	}
	outcome := workspaceCacheFetchOutcomeFetched
	if m.fetchFreshnessWindow > 0 {
		outcome = workspaceCacheFetchOutcomeFallback
	}
	return workspaceCachePreparation{outcome: outcome}, nil
}

func (m *WorkspaceMaterializer) cacheFetchIsFresh(
	ctx context.Context,
	cache workspaceCacheLayout,
	repositoryURL, defaultBranch string,
	requiredCommits []string,
) bool {
	if m.fetchFreshnessWindow <= 0 || m.validateBareRepository(ctx, cache.RepoGit, repositoryURL) != nil {
		return false
	}
	marker, err := readSmallRegularFile(cache.FetchMarker, workspaceCacheFetchMarkerMaxSize)
	if err != nil {
		return false
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(marker))
	if err != nil {
		return false
	}
	now := time.Now().UTC()
	if m.now != nil {
		now = m.now().UTC()
	}
	age := now.Sub(fetchedAt)
	if age < 0 || age >= m.fetchFreshnessWindow {
		return false
	}
	references := []string{"refs/remotes/origin/" + defaultBranch}
	for _, requiredCommit := range requiredCommits {
		requiredCommit = strings.TrimSpace(requiredCommit)
		if !validGitObjectID(requiredCommit) {
			return false
		}
		references = append(references, requiredCommit)
	}
	for _, reference := range references {
		commit, err := m.runGit(
			ctx, cache.RepoGit, gitEnvironment(nil), "rev-parse", "--verify", strings.TrimSpace(reference)+"^{commit}",
		)
		if err != nil || !validGitObjectID(commit) {
			return false
		}
	}
	return true
}

func (m *WorkspaceMaterializer) refreshCacheRepository(
	ctx context.Context,
	cache workspaceCacheLayout,
	remote gitpolicy.Remote,
	defaultBranch string,
	credential *WorkspaceGitCredential,
) error {
	lock, err := acquireWorkspaceFileLock(ctx, m.cacheRoot, cache.LockPath)
	if err != nil {
		return fmt.Errorf("acquire Git cache lock: %w", err)
	}
	defer lock.Release()
	return m.ensureCacheRepository(ctx, cache, remote, defaultBranch, credential, false, true)
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
	credential *WorkspaceGitCredential,
	reconciled bool,
	requireFetchMarker bool,
) error {
	if !reconciled {
		if err := m.reconcileCacheRepository(ctx, cache, remote.URL); err != nil {
			return errors.New("Git cache repository could not be reconciled")
		}
	}
	if m.validateBareRepository(ctx, cache.RepoGit, remote.URL) == nil {
		if err := m.fetchCacheRepository(ctx, cache.RepoGit, remote, defaultBranch, credential); err == nil {
			markerErr := m.writeCacheFetchMarker(cache)
			if requireFetchMarker {
				return markerErr
			}
			// A foreground Fetch remains authoritative even when optional
			// optimization metadata cannot be recorded. The next Claim then
			// treats the missing/old marker as doubt and Fetches again.
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
	markerErr := m.writeCacheFetchMarker(cache)
	if requireFetchMarker {
		return markerErr
	}
	// Preserve the default foreground path's success semantics; a missing
	// marker only disables cache-first reuse on the next Claim.
	return nil
}

func (m *WorkspaceMaterializer) writeCacheFetchMarker(cache workspaceCacheLayout) error {
	if !pathContainedBy(cache.Root, cache.FetchMarker) || filepath.Dir(cache.FetchMarker) != cache.Root {
		return errors.New("Git cache Fetch marker path is invalid")
	}
	fetchedAt := time.Now().UTC()
	if m.now != nil {
		fetchedAt = m.now().UTC()
	}
	temporary, err := os.CreateTemp(cache.Root, ".fetch-marker-*")
	if err != nil {
		return errors.New("Git cache Fetch marker could not be staged")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return errors.New("Git cache Fetch marker permissions could not be set")
	}
	if _, err := temporary.WriteString(fetchedAt.Format(time.RFC3339Nano) + "\n"); err != nil {
		_ = temporary.Close()
		return errors.New("Git cache Fetch marker could not be written")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("Git cache Fetch marker could not be synced")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("Git cache Fetch marker could not be closed")
	}
	if err := os.Rename(temporaryPath, cache.FetchMarker); err != nil {
		return errors.New("Git cache Fetch marker could not be installed")
	}
	return nil
}

func (m *WorkspaceMaterializer) reconcileCacheRepository(
	ctx context.Context,
	cache workspaceCacheLayout,
	expectedRepositoryURL string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	staging, backups, err := workspaceGenerationResidue(cache.RepoGit)
	if err != nil {
		return err
	}
	activeInfo, activeErr := os.Lstat(cache.RepoGit)
	switch {
	case activeErr == nil:
		if activeInfo.Mode()&os.ModeSymlink != 0 || !activeInfo.IsDir() {
			return errors.New("Git cache repository is unsafe")
		}
		// A valid active cache is the sole commit point. Any sibling generation
		// is residue from an interrupted install and can be rebuilt later.
		if m.validateBareRepository(ctx, cache.RepoGit, expectedRepositoryURL) == nil {
			return m.removeWorkspaceGenerationResidueStrict(ctx, append(staging, backups...))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// The cache is rebuildable, but keep the invalid active directory in
		// place until replaceWorkspaceGeneration atomically installs its
		// replacement. Only stale siblings are discarded here.
		return m.removeWorkspaceGenerationResidueStrict(ctx, append(staging, backups...))
	case !errors.Is(activeErr, os.ErrNotExist):
		return activeErr
	}

	// An orphan staging directory is never authoritative: Git cache population
	// can fail after creating a structurally valid tree.
	// A single backup, however, was previously the committed active cache and
	// may be restored before the normal Fetch refreshes it.
	if len(backups) == 1 && m.validateBareRepository(ctx, backups[0], expectedRepositoryURL) == nil {
		backup := backups[0]
		if err := os.Rename(backup, cache.RepoGit); err != nil {
			return err
		}
		if err := m.validateBareRepository(ctx, cache.RepoGit, expectedRepositoryURL); err != nil {
			rollbackErr := os.Rename(cache.RepoGit, backup)
			if rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("preserve invalid restored Git cache: %w", rollbackErr))
			}
			return err
		}
		return m.removeWorkspaceGenerationResidueStrict(ctx, staging)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Multiple or invalid backups are deliberately not ranked by UUID or
	// mtime. Unlike a Workspace, this cache contains no authoritative user
	// state, so fail-safe recovery is to discard every residue and rebuild.
	return m.removeWorkspaceGenerationResidueStrict(ctx, append(staging, backups...))
}

func (m *WorkspaceMaterializer) removeWorkspaceGenerationResidueStrict(
	ctx context.Context,
	paths []string,
) error {
	if len(paths) == 0 {
		return nil
	}
	parentPath := filepath.Dir(filepath.Clean(paths[0]))
	parent, err := openVerifiedWorkspaceRoot(parentPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		path = filepath.Clean(path)
		if filepath.Dir(path) != parentPath || !pathContainedBy(parentPath, path) {
			return errors.New("Workspace generation residue escaped its parent")
		}
		name := filepath.Base(path)
		if err := removeRootRelativeTree(ctx, parent, name, m.persistWorkspaceCleanupDirectory); err != nil {
			return err
		}
		if _, err := parent.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("Workspace generation residue still exists")
			}
			return err
		}
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
	credential *WorkspaceGitCredential,
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
		"worktree", "add", "-b", branch, "--", checkout, resolvedCommit,
	); err != nil {
		return err
	}
	if err := relativizeWorktreeMetadata(repository, checkout); err != nil {
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

// relativizeWorktreeMetadata rewrites the two absolute pointers `git worktree
// add` leaves behind, so the generation still resolves after its staging root
// is renamed into place. `git worktree add --relative-paths` does exactly this,
// but that option needs git 2.48 while Ubuntu 24.04 still ships 2.43, and an
// absolute pointer does not merely degrade — validatePrivateWorktreeFilesystem
// rejects the generation outright. Computing the pointers here gives one
// behaviour on every supported git; the byte output matches --relative-paths.
// The third pointer, `commondir`, is already relative on every version.
//
// The written paths are derived from the caller's own layout rather than from
// git's output, because git resolves symlinks in the staging prefix and we do
// not: relating the two directly would emit a pointer that walks out of the
// generation and back in through the resolved prefix.
func relativizeWorktreeMetadata(repository, checkout string) error {
	gitFile := filepath.Join(checkout, ".git")
	value, err := readSmallRegularFile(gitFile, gitMetadataPointerMaxSize)
	if err != nil {
		return errors.New("Workspace Git file is unavailable")
	}
	const prefix = "gitdir: "
	if !strings.HasPrefix(value, prefix) {
		return errors.New("Workspace Git file is invalid")
	}
	pointer := filepath.Clean(strings.TrimSpace(strings.TrimPrefix(value, prefix)))
	if !filepath.IsAbs(pointer) {
		return nil
	}
	worktreeGitDir := filepath.Join(repository, "worktrees", filepath.Base(pointer))
	if !sameExistingPath(worktreeGitDir, pointer) {
		return errors.New("Workspace Git file escapes the private repository")
	}
	relativeGitDir, err := filepath.Rel(checkout, worktreeGitDir)
	if err != nil {
		return err
	}
	relativeGitFile, err := filepath.Rel(worktreeGitDir, gitFile)
	if err != nil {
		return err
	}
	if err := replaceGitMetadataPointer(gitFile, prefix+filepath.ToSlash(relativeGitDir)); err != nil {
		return err
	}
	return replaceGitMetadataPointer(filepath.Join(worktreeGitDir, "gitdir"), filepath.ToSlash(relativeGitFile))
}

// replaceGitMetadataPointer writes value through a temporary file in the same
// directory, so the destination is never opened through a symlink and never
// observed half-written.
func replaceGitMetadataPointer(path, value string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".git-pointer-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(value + "\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
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
