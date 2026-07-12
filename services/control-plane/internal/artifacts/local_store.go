package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type LocalStore struct {
	root string
}

func NewLocalStore(root string) (*LocalStore, error) {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return nil, fmt.Errorf("resolve local artifact root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create local artifact root: %w", err)
	}
	return &LocalStore{root: root}, nil
}

func (s *LocalStore) Bucket() string { return "local" }
func (s *LocalStore) IsLocal() bool  { return true }

func (s *LocalStore) Check(context.Context) error {
	info, err := os.Stat(s.root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("local artifact root is not a directory")
	}
	return nil
}

func (s *LocalStore) PresignUpload(context.Context, string, time.Duration) (string, error) {
	return "", ErrPresignUnsupported
}

func (s *LocalStore) PresignDownload(context.Context, string, time.Duration) (string, error) {
	return "", ErrPresignUnsupported
}

func (s *LocalStore) Put(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) (ObjectInfo, error) {
	target, err := s.resolve(objectKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return ObjectInfo{}, fmt.Errorf("create artifact directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".artifact-*")
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("create temporary artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	written, err := copyContext(ctx, temporary, reader)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("write local artifact: %w", err)
	}
	if size >= 0 && written != size {
		return ObjectInfo{}, fmt.Errorf("artifact size changed during upload: expected %d bytes, wrote %d", size, written)
	}
	if err := temporary.Sync(); err != nil {
		return ObjectInfo{}, fmt.Errorf("sync local artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return ObjectInfo{}, fmt.Errorf("close local artifact: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return ObjectInfo{}, fmt.Errorf("protect local artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return ObjectInfo{}, fmt.Errorf("commit local artifact: %w", err)
	}
	committed = true
	return ObjectInfo{Size: written, ContentType: contentType}, nil
}

func (s *LocalStore) Stat(_ context.Context, objectKey string) (ObjectInfo, error) {
	target, err := s.resolve(objectKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return ObjectInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return ObjectInfo{}, errors.New("artifact object is not a regular file")
	}
	return ObjectInfo{Size: info.Size()}, nil
}

func (s *LocalStore) Open(_ context.Context, objectKey string) (io.ReadCloser, error) {
	target, err := s.resolve(objectKey)
	if err != nil {
		return nil, err
	}
	return os.Open(target)
}

func (s *LocalStore) Delete(_ context.Context, objectKey string) error {
	target, err := s.resolve(objectKey)
	if err != nil {
		return err
	}
	err = os.Remove(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *LocalStore) resolve(objectKey string) (string, error) {
	normalized := filepath.FromSlash(strings.TrimSpace(objectKey))
	if normalized == "" || filepath.IsAbs(normalized) || filepath.Clean(normalized) != normalized || normalized == ".." || strings.HasPrefix(normalized, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid artifact object key")
	}
	target := filepath.Join(s.root, normalized)
	if !strings.HasPrefix(target, s.root+string(filepath.Separator)) {
		return "", errors.New("artifact object key escapes the local root")
	}
	return target, nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
