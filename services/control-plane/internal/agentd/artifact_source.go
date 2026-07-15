package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

type artifactUploadSource struct {
	file *os.File
	info os.FileInfo
	path string
}

type workspaceArtifactRoot struct {
	root  *os.Root
	info  os.FileInfo
	path  string
	label string
}

type runtimeOutputArtifactRoot struct {
	directory string
	bound     *workspaceArtifactRoot
}

func openWorkspaceArtifactRoot(workspaceDirectory string) (*workspaceArtifactRoot, error) {
	return openBoundArtifactRoot(workspaceDirectory, "execution workspace")
}

func openRuntimeOutputArtifactRoot(runtimeOutputDirectory string) (*workspaceArtifactRoot, error) {
	return openBoundArtifactRoot(runtimeOutputDirectory, "runtime output")
}

func newRuntimeOutputArtifactRoot() (*runtimeOutputArtifactRoot, error) {
	directory, err := os.MkdirTemp("", "synara-runtime-output-")
	if err != nil {
		return nil, fmt.Errorf("create Runtime Output root: %w", err)
	}
	bound, err := openRuntimeOutputArtifactRoot(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	return &runtimeOutputArtifactRoot{directory: directory, bound: bound}, nil
}

func (r *runtimeOutputArtifactRoot) Close() error {
	if r == nil {
		return nil
	}
	var closeErr error
	if r.bound != nil {
		closeErr = r.bound.Close()
		r.bound = nil
	}
	var removeErr error
	if r.directory != "" {
		removeErr = os.RemoveAll(r.directory)
	}
	r.directory = ""
	return errors.Join(closeErr, removeErr)
}

func (r *runtimeOutputArtifactRoot) open(candidate string) (*artifactUploadSource, string, error) {
	if r == nil || r.bound == nil {
		return nil, "", errors.New("runtime output Artifact root is unavailable")
	}
	return r.bound.open(candidate)
}

func openBoundArtifactRoot(directory, label string) (*workspaceArtifactRoot, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve %s root: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	before, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("stat %s root: %w", label, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("%s root is not a regular directory", label)
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open %s root: %w", label, err)
	}
	opened, openedErr := root.Stat(".")
	after, afterErr := os.Lstat(absolute)
	if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("%s root changed while it was opened", label)
	}
	return &workspaceArtifactRoot{root: root, info: opened, path: absolute, label: label}, nil
}

func (r *workspaceArtifactRoot) Close() error {
	if r == nil || r.root == nil {
		return nil
	}
	return r.root.Close()
}

func (s *artifactUploadSource) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	return s.file.Close()
}

func (s *artifactUploadSource) rewind() (os.FileInfo, error) {
	if s == nil || s.file == nil || s.info == nil {
		return nil, errors.New("runner artifact source is unavailable")
	}
	info, err := s.file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(s.info, info) {
		return nil, errors.New("runner artifact source changed after it was opened")
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return info, nil
}

// guardedArtifactUploadSource copies an opened Runner Artifact into a private,
// immutable staging file. Text is redacted while it is copied; binary content
// is released unchanged only after the detect-only stream proves it safe.
func guardedArtifactUploadSource(
	ctx context.Context,
	guard *executionSecretGuard,
	contentType string,
	source *artifactUploadSource,
) (*artifactUploadSource, func(), error) {
	if guard == nil {
		return source, func() {}, nil
	}
	if _, err := source.rewind(); err != nil {
		return nil, nil, fmt.Errorf("rewind guarded Artifact source: %w", err)
	}
	mode := secretguard.StreamBinaryDetectOnly
	if artifactContentIsText(contentType) {
		mode = secretguard.StreamText
	}
	stream, err := guard.NewStream(mode)
	if err != nil {
		return nil, nil, err
	}
	defer stream.Close()
	staging, err := os.CreateTemp("", "synara-guarded-artifact-*.payload")
	if err != nil {
		return nil, nil, fmt.Errorf("create guarded Artifact staging: %w", err)
	}
	stagingPath := staging.Name()
	cleanupStaging := func() {
		if staging != nil {
			_ = staging.Close()
		}
		_ = os.Remove(stagingPath)
	}
	failed := true
	defer func() {
		if failed {
			cleanupStaging()
		}
	}()
	buffer := make([]byte, 64<<10)
	defer zeroBytes(buffer)
	reader := contextReader{ctx: ctx, reader: source.file}
	for {
		read, readErr := reader.Read(buffer)
		if read > 0 {
			output, transformErr := stream.Transform(buffer[:read])
			if transformErr != nil {
				return nil, nil, transformErr
			}
			if err := writeGuardedArtifactBytes(staging, output); err != nil {
				zeroBytes(output)
				return nil, nil, err
			}
			zeroBytes(output)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, nil, fmt.Errorf("read guarded Artifact source: %w", readErr)
		}
	}
	final, err := stream.Finish()
	if err != nil {
		return nil, nil, err
	}
	if err := writeGuardedArtifactBytes(staging, final); err != nil {
		zeroBytes(final)
		return nil, nil, err
	}
	zeroBytes(final)
	if err := staging.Sync(); err != nil {
		return nil, nil, fmt.Errorf("sync guarded Artifact staging: %w", err)
	}
	if err := staging.Close(); err != nil {
		return nil, nil, fmt.Errorf("close guarded Artifact staging: %w", err)
	}
	staging = nil
	safeSource, err := openRegularArtifactSource(stagingPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open guarded Artifact staging: %w", err)
	}
	failed = false
	cleanup := func() {
		_ = safeSource.Close()
		_ = os.Remove(stagingPath)
	}
	return safeSource, cleanup, nil
}

func writeGuardedArtifactBytes(destination io.Writer, value []byte) error {
	if len(value) == 0 {
		return nil
	}
	written, err := destination.Write(value)
	if err != nil {
		return fmt.Errorf("write guarded Artifact staging: %w", err)
	}
	if written != len(value) {
		return errors.New("write guarded Artifact staging: short write")
	}
	return nil
}

func artifactContentIsText(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" ||
		mediaType == "application/problem+json" || strings.HasSuffix(mediaType, "+json") ||
		mediaType == "application/xml" || strings.HasSuffix(mediaType, "+xml") ||
		mediaType == "application/javascript" || mediaType == "application/x-ndjson"
}

func openRegularArtifactSource(path string) (*artifactUploadSource, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, errors.New("runner artifact source is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fileInfo, statErr := file.Stat()
	currentPathInfo, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || currentPathInfo.Mode()&os.ModeSymlink != 0 ||
		!currentPathInfo.Mode().IsRegular() || !fileInfo.Mode().IsRegular() ||
		!os.SameFile(pathInfo, currentPathInfo) || !os.SameFile(currentPathInfo, fileInfo) {
		_ = file.Close()
		return nil, errors.New("runner artifact source changed while it was opened")
	}
	return &artifactUploadSource{file: file, info: fileInfo, path: path}, nil
}

func openWorkspaceArtifactSource(
	workspaceDirectory,
	candidate string,
) (*artifactUploadSource, string, error) {
	root, err := openWorkspaceArtifactRoot(workspaceDirectory)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	return root.open(candidate)
}

func (r *workspaceArtifactRoot) open(candidate string) (*artifactUploadSource, string, error) {
	if r == nil || r.root == nil || r.info == nil {
		return nil, "", fmt.Errorf("%s Artifact root is unavailable", r.label)
	}
	openedRoot, err := r.root.Stat(".")
	if err != nil || !openedRoot.IsDir() || !os.SameFile(r.info, openedRoot) {
		return nil, "", fmt.Errorf("%s Artifact root changed after it was bound", r.label)
	}
	relative, err := artifactRelativePath(r.path, candidate, r.label)
	if err != nil {
		return nil, "", err
	}

	roots := []*os.Root{r.root}
	defer func() {
		for index := len(roots) - 1; index >= 1; index-- {
			_ = roots[index].Close()
		}
	}()

	segments := strings.Split(relative, string(filepath.Separator))
	current := r.root
	for _, segment := range segments[:len(segments)-1] {
		before, err := current.Lstat(segment)
		if err != nil {
			return nil, "", fmt.Errorf("stat runner artifact parent: %w", err)
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			return nil, "", errors.New("runner artifact parent is not a regular directory")
		}
		next, err := current.OpenRoot(segment)
		if err != nil {
			return nil, "", fmt.Errorf("open runner artifact parent: %w", err)
		}
		opened, openedErr := next.Stat(".")
		after, afterErr := current.Lstat(segment)
		if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
			!os.SameFile(before, after) || !os.SameFile(after, opened) {
			_ = next.Close()
			return nil, "", errors.New("runner artifact parent changed while it was opened")
		}
		roots = append(roots, next)
		current = next
	}

	name := segments[len(segments)-1]
	before, err := current.Lstat(name)
	if err != nil {
		return nil, "", fmt.Errorf("stat runner artifact: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, "", errors.New("runner artifact must be a regular file")
	}
	file, err := current.Open(name)
	if err != nil {
		return nil, "", fmt.Errorf("open runner artifact: %w", err)
	}
	opened, openedErr := file.Stat()
	after, afterErr := current.Lstat(name)
	if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 ||
		!after.Mode().IsRegular() || !opened.Mode().IsRegular() ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = file.Close()
		return nil, "", errors.New("runner artifact changed while it was opened")
	}
	return &artifactUploadSource{
		file: file,
		info: opened,
		path: filepath.Join(r.path, relative),
	}, relative, nil
}

func workspaceArtifactPath(workspaceDirectory, candidate string) (string, string, error) {
	workspaceAbsolute, err := filepath.Abs(workspaceDirectory)
	if err != nil {
		return "", "", fmt.Errorf("resolve execution workspace: %w", err)
	}
	workspaceAbsolute = filepath.Clean(workspaceAbsolute)
	relative, err := workspaceArtifactRelativePath(workspaceAbsolute, candidate)
	return workspaceAbsolute, relative, err
}

func workspaceArtifactRelativePath(workspaceAbsolute, candidate string) (string, error) {
	return artifactRelativePath(workspaceAbsolute, candidate, "execution workspace")
}

func artifactRelativePath(rootAbsolute, candidate, label string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", errors.New("runner artifact path is empty")
	}
	relative := candidate
	if filepath.IsAbs(candidate) {
		requested := filepath.Clean(candidate)
		candidateRelative, relativeErr := filepath.Rel(rootAbsolute, requested)
		if relativeErr == nil && pathContainedRelative(candidateRelative) {
			relative = candidateRelative
		}
	}
	relative = filepath.Clean(relative)
	if !pathContainedRelative(relative) {
		return "", fmt.Errorf("runner artifact path escapes the %s root", label)
	}
	return relative, nil
}

func pathContainedRelative(relative string) bool {
	return relative != "" && relative != "." && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	read, err := r.reader.Read(buffer)
	if err == nil {
		if contextErr := r.ctx.Err(); contextErr != nil {
			return read, contextErr
		}
	}
	return read, err
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	written, err := io.CopyBuffer(destination, contextReader{ctx: ctx, reader: source}, make([]byte, 64<<10))
	if err == nil {
		err = ctx.Err()
	}
	return written, err
}
