package artifacts

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

type S3Store struct {
	client *minio.Client
	signer *minio.Client
	bucket string
}

func NewS3Store(ctx context.Context, cfg config.Config) (*S3Store, error) {
	endpoint, secure, err := normalizeS3Endpoint(cfg.ArtifactEndpoint)
	if err != nil {
		return nil, err
	}
	credentialProvider := credentials.NewIAM("")
	if cfg.ArtifactAccessKeyID != "" || cfg.ArtifactSecretAccessKey != "" {
		credentialProvider = credentials.NewStaticV4(cfg.ArtifactAccessKeyID, cfg.ArtifactSecretAccessKey, cfg.ArtifactSessionToken)
	}
	bucketLookup := minio.BucketLookupAuto
	if cfg.ArtifactUsePathStyle {
		bucketLookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentialProvider, Secure: secure, Region: cfg.ArtifactRegion, BucketLookup: bucketLookup,
	})
	if err != nil {
		return nil, fmt.Errorf("create S3-compatible artifact client: %w", err)
	}
	signer := client
	if cfg.ArtifactPublicEndpoint != "" {
		publicEndpoint, publicSecure, err := normalizeS3Endpoint(cfg.ArtifactPublicEndpoint)
		if err != nil {
			return nil, fmt.Errorf("SYNARA_ARTIFACT_PUBLIC_ENDPOINT: %w", err)
		}
		signer, err = minio.New(publicEndpoint, &minio.Options{
			Creds: credentialProvider, Secure: publicSecure, Region: cfg.ArtifactRegion, BucketLookup: bucketLookup,
		})
		if err != nil {
			return nil, fmt.Errorf("create public artifact URL signer: %w", err)
		}
	}
	store := &S3Store{client: client, signer: signer, bucket: cfg.ArtifactBucket}
	exists, err := client.BucketExists(ctx, store.bucket)
	if err != nil {
		return nil, fmt.Errorf("check artifact bucket %q: %w", store.bucket, err)
	}
	if !exists {
		if cfg.Platform.ArtifactStore != platform.ArtifactMinIO {
			return nil, fmt.Errorf("artifact bucket %q does not exist", store.bucket)
		}
		if err := client.MakeBucket(ctx, store.bucket, minio.MakeBucketOptions{Region: cfg.ArtifactRegion}); err != nil {
			return nil, fmt.Errorf("create MinIO artifact bucket %q: %w", store.bucket, err)
		}
	}
	return store, nil
}

func normalizeS3Endpoint(raw string) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "s3.amazonaws.com", true, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false, fmt.Errorf("SYNARA_ARTIFACT_ENDPOINT must be an HTTP(S) origin without a path")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false, fmt.Errorf("SYNARA_ARTIFACT_ENDPOINT must use http or https")
	}
	return parsed.Host, parsed.Scheme == "https", nil
}

func (s *S3Store) Bucket() string { return s.bucket }
func (s *S3Store) IsLocal() bool  { return false }

func (s *S3Store) Check(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("artifact bucket %q does not exist", s.bucket)
	}
	return nil
}

func (s *S3Store) PresignUpload(ctx context.Context, objectKey string, ttl time.Duration) (string, error) {
	signed, err := s.signer.PresignedPutObject(ctx, s.bucket, objectKey, ttl)
	if err != nil {
		return "", err
	}
	return signed.String(), nil
}

func (s *S3Store) PresignDownload(ctx context.Context, objectKey string, ttl time.Duration) (string, error) {
	signed, err := s.signer.PresignedGetObject(ctx, s.bucket, objectKey, ttl, nil)
	if err != nil {
		return "", err
	}
	return signed.String(), nil
}

func (s *S3Store) Put(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) (ObjectInfo, error) {
	uploaded, err := s.client.PutObject(ctx, s.bucket, objectKey, reader, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: uploaded.Size, ContentType: contentType, Version: uploaded.VersionID}, nil
}

func (s *S3Store) Stat(ctx context.Context, objectKey string) (ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, objectKey, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: info.Size, ContentType: info.ContentType, Version: info.VersionID}, nil
}

func (s *S3Store) Open(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	object, err := s.client.GetObject(ctx, s.bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return object, nil
}

func (s *S3Store) Delete(ctx context.Context, objectKey string) error {
	return s.client.RemoveObject(ctx, s.bucket, objectKey, minio.RemoveObjectOptions{})
}

func NewStore(ctx context.Context, cfg config.Config) (Store, error) {
	if cfg.Platform.ArtifactStore == platform.ArtifactLocal {
		return NewLocalStore(cfg.ArtifactLocalPath)
	}
	return NewS3Store(ctx, cfg)
}
