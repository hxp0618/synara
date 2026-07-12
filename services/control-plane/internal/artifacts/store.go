package artifacts

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrPresignUnsupported = errors.New("artifact store does not support presigned URLs")

type ObjectInfo struct {
	Size        int64
	ContentType string
	Version     string
}

type Store interface {
	Bucket() string
	IsLocal() bool
	Check(context.Context) error
	PresignUpload(context.Context, string, time.Duration) (string, error)
	PresignDownload(context.Context, string, time.Duration) (string, error)
	Put(context.Context, string, io.Reader, int64, string) (ObjectInfo, error)
	Stat(context.Context, string) (ObjectInfo, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}
