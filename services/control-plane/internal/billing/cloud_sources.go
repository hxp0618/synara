package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	azblob "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	azcontainer "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type s3ReadAPI interface {
	HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
}

type S3ReadOnlyBlobSource struct {
	client s3ReadAPI
	bucket string
	prefix string
}

func NewS3ReadOnlyBlobSource(ctx context.Context, config SourceConfig) (*S3ReadOnlyBlobSource, error) {
	normalized, err := config.Normalize()
	if err != nil {
		return nil, err
	}
	awsRuntimeConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(normalized.S3Region))
	if err != nil {
		return nil, err
	}
	client := awss3.NewFromConfig(awsRuntimeConfig, func(options *awss3.Options) {
		options.UsePathStyle = normalized.S3UsePathStyle
		if normalized.S3Endpoint != "" {
			options.BaseEndpoint = aws.String(normalized.S3Endpoint)
		}
	})
	return newS3ReadOnlyBlobSource(client, normalized.S3Bucket, normalized.Prefix), nil
}

func newS3ReadOnlyBlobSource(client s3ReadAPI, bucket string, prefix string) *S3ReadOnlyBlobSource {
	return &S3ReadOnlyBlobSource{client: client, bucket: bucket, prefix: prefix}
}

func (s *S3ReadOnlyBlobSource) Open(
	ctx context.Context,
	object BlobObjectRef,
) (io.ReadCloser, BlobObjectMetadata, error) {
	key, version, err := resolveVersionedCloudObject(s.prefix, object, "billing s3 object version")
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	head, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), VersionId: aws.String(version),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, BlobObjectMetadata{}, os.ErrNotExist
		}
		return nil, BlobObjectMetadata{}, err
	}
	if head.ContentLength == nil || *head.ContentLength < 0 {
		return nil, BlobObjectMetadata{}, errors.New("billing s3 object size metadata is unavailable")
	}
	response, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), VersionId: aws.String(version),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, BlobObjectMetadata{}, os.ErrNotExist
		}
		return nil, BlobObjectMetadata{}, err
	}
	resolvedVersion := strings.TrimSpace(aws.ToString(head.VersionId))
	if resolvedVersion != "" && resolvedVersion != version {
		_ = response.Body.Close()
		return nil, BlobObjectMetadata{}, fmt.Errorf("billing s3 object version changed: got %q, want %q", resolvedVersion, version)
	}
	return response.Body, BlobObjectMetadata{
		SizeBytes: *head.ContentLength, Version: version, ETag: strings.Trim(aws.ToString(head.ETag), `"`),
		LastModified: aws.ToTime(head.LastModified).UTC(),
	}, nil
}

func (s *S3ReadOnlyBlobSource) ResolveManifestObject(
	ctx context.Context,
	rawKey string,
) (ResolvedBlobObject, error) {
	if s == nil || s.client == nil {
		return ResolvedBlobObject{}, errors.New("billing s3 blob source is nil")
	}
	key, err := s.manifestRelativeObjectKey(rawKey)
	if err != nil {
		return ResolvedBlobObject{}, err
	}
	resolvedKey, err := resolveBlobObjectKey(s.prefix, key)
	if err != nil {
		return ResolvedBlobObject{}, err
	}
	head, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(resolvedKey),
	})
	if err != nil {
		if isS3NotFound(err) {
			return ResolvedBlobObject{}, os.ErrNotExist
		}
		return ResolvedBlobObject{}, err
	}
	version := strings.TrimSpace(aws.ToString(head.VersionId))
	if version == "" || strings.EqualFold(version, "null") {
		return ResolvedBlobObject{}, fmt.Errorf("billing s3 manifest object %q has no immutable version id", rawKey)
	}
	if head.ContentLength == nil || *head.ContentLength < 0 || head.LastModified == nil || strings.TrimSpace(aws.ToString(head.ETag)) == "" {
		return ResolvedBlobObject{}, fmt.Errorf("billing s3 manifest object %q immutable metadata is incomplete", rawKey)
	}
	return ResolvedBlobObject{
		Ref: BlobObjectRef{Key: key, Version: version},
		Metadata: BlobObjectMetadata{
			SizeBytes: *head.ContentLength, Version: version, ETag: strings.Trim(aws.ToString(head.ETag), `"`),
			LastModified: aws.ToTime(head.LastModified).UTC(),
		},
	}, nil
}

func (s *S3ReadOnlyBlobSource) manifestRelativeObjectKey(rawKey string) (string, error) {
	trimmed := strings.TrimSpace(rawKey)
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" {
		if parsed.Scheme != "s3" || parsed.Host != s.bucket || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("billing s3 manifest object %q must belong to bucket %q", rawKey, s.bucket)
		}
		trimmed = strings.TrimPrefix(parsed.EscapedPath(), "/")
		decoded, decodeErr := url.PathUnescape(trimmed)
		if decodeErr != nil {
			return "", fmt.Errorf("billing s3 manifest object %q has an invalid path", rawKey)
		}
		trimmed = decoded
	}
	key, err := normalizeBlobRelativeKey(trimmed)
	if err != nil {
		return "", err
	}
	prefix, err := normalizeBlobPrefix(s.prefix)
	if err != nil {
		return "", err
	}
	if prefix != "" && strings.HasPrefix(key, prefix+"/") {
		key = strings.TrimPrefix(key, prefix+"/")
	}
	if key == "" || key == "." || key == ".." || strings.HasPrefix(key, "../") {
		return "", errors.New("billing s3 manifest object must stay within the configured prefix")
	}
	return path.Clean(key), nil
}

func isS3NotFound(err error) bool {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchVersion":
			return true
		}
	}
	return false
}

type gcsBucketClient interface {
	Object(string) gcsObjectClient
}

type gcsObjectClient interface {
	Generation(int64) gcsObjectClient
	Attrs(context.Context) (gcsObjectAttrs, error)
	NewReader(context.Context) (io.ReadCloser, error)
}

type gcsObjectAttrs struct {
	Size int64
}

type GCSReadOnlyBlobSource struct {
	bucket gcsBucketClient
	prefix string
	closer io.Closer
}

func NewGCSReadOnlyBlobSource(ctx context.Context, config SourceConfig) (*GCSReadOnlyBlobSource, error) {
	normalized, err := config.Normalize()
	if err != nil {
		return nil, err
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return newGCSReadOnlyBlobSource(&realGCSBucketClient{bucket: client.Bucket(normalized.GCSBucket)}, normalized.Prefix, client), nil
}

func newGCSReadOnlyBlobSource(bucket gcsBucketClient, prefix string, closer io.Closer) *GCSReadOnlyBlobSource {
	return &GCSReadOnlyBlobSource{bucket: bucket, prefix: prefix, closer: closer}
}

func (s *GCSReadOnlyBlobSource) Open(
	ctx context.Context,
	object BlobObjectRef,
) (io.ReadCloser, BlobObjectMetadata, error) {
	key, version, err := resolveVersionedCloudObject(s.prefix, object, "billing gcs object generation")
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	generation, parseErr := strconv.ParseInt(version, 10, 64)
	if parseErr != nil || generation <= 0 {
		return nil, BlobObjectMetadata{}, fmt.Errorf("billing gcs object generation %q must be a positive integer", version)
	}
	handle := s.bucket.Object(key).Generation(generation)
	attrs, err := handle.Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, BlobObjectMetadata{}, os.ErrNotExist
		}
		return nil, BlobObjectMetadata{}, err
	}
	if attrs.Size < 0 {
		return nil, BlobObjectMetadata{}, errors.New("billing gcs object size metadata is unavailable")
	}
	reader, err := handle.NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, BlobObjectMetadata{}, os.ErrNotExist
		}
		return nil, BlobObjectMetadata{}, err
	}
	return reader, BlobObjectMetadata{SizeBytes: attrs.Size, Version: version}, nil
}

func (s *GCSReadOnlyBlobSource) Close() error {
	if s == nil || s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

type realGCSBucketClient struct {
	bucket *storage.BucketHandle
}

func (b *realGCSBucketClient) Object(name string) gcsObjectClient {
	return &realGCSObjectClient{object: b.bucket.Object(name)}
}

type realGCSObjectClient struct {
	object *storage.ObjectHandle
}

func (o *realGCSObjectClient) Generation(generation int64) gcsObjectClient {
	return &realGCSObjectClient{object: o.object.Generation(generation)}
}

func (o *realGCSObjectClient) Attrs(ctx context.Context) (gcsObjectAttrs, error) {
	attrs, err := o.object.Attrs(ctx)
	if err != nil {
		return gcsObjectAttrs{}, err
	}
	return gcsObjectAttrs{Size: attrs.Size}, nil
}

func (o *realGCSObjectClient) NewReader(ctx context.Context) (io.ReadCloser, error) {
	return o.object.NewReader(ctx)
}

type azureContainerClient interface {
	Blob(string) azureBlobClient
}

type azureBlobClient interface {
	WithVersionID(string) (azureBlobClient, error)
	GetProperties(context.Context) (azureBlobProperties, error)
	DownloadStream(context.Context) (io.ReadCloser, error)
}

type azureBlobProperties struct {
	ContentLength *int64
}

type AzureReadOnlyBlobSource struct {
	container azureContainerClient
	prefix    string
}

func NewAzureReadOnlyBlobSource(ctx context.Context, config SourceConfig) (*AzureReadOnlyBlobSource, error) {
	normalized, err := config.Normalize()
	if err != nil {
		return nil, err
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	containerClient, err := azcontainer.NewClient(normalized.AzureContainerURL, credential, nil)
	if err != nil {
		return nil, err
	}
	return newAzureReadOnlyBlobSource(&realAzureContainerClient{container: containerClient}, normalized.Prefix), nil
}

func newAzureReadOnlyBlobSource(container azureContainerClient, prefix string) *AzureReadOnlyBlobSource {
	return &AzureReadOnlyBlobSource{container: container, prefix: prefix}
}

func (s *AzureReadOnlyBlobSource) Open(
	ctx context.Context,
	object BlobObjectRef,
) (io.ReadCloser, BlobObjectMetadata, error) {
	key, version, err := resolveVersionedCloudObject(s.prefix, object, "billing azure object version")
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	client, err := s.container.Blob(key).WithVersionID(version)
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	properties, err := client.GetProperties(ctx)
	if err != nil {
		if isAzureNotFound(err) {
			return nil, BlobObjectMetadata{}, os.ErrNotExist
		}
		return nil, BlobObjectMetadata{}, err
	}
	if properties.ContentLength == nil || *properties.ContentLength < 0 {
		return nil, BlobObjectMetadata{}, errors.New("billing azure object size metadata is unavailable")
	}
	reader, err := client.DownloadStream(ctx)
	if err != nil {
		if isAzureNotFound(err) {
			return nil, BlobObjectMetadata{}, os.ErrNotExist
		}
		return nil, BlobObjectMetadata{}, err
	}
	return reader, BlobObjectMetadata{SizeBytes: *properties.ContentLength, Version: version}, nil
}

type realAzureContainerClient struct {
	container *azcontainer.Client
}

func (c *realAzureContainerClient) Blob(name string) azureBlobClient {
	return &realAzureBlobClient{blob: c.container.NewBlobClient(name)}
}

type realAzureBlobClient struct {
	blob *azblob.Client
}

func (b *realAzureBlobClient) WithVersionID(version string) (azureBlobClient, error) {
	client, err := b.blob.WithVersionID(version)
	if err != nil {
		return nil, err
	}
	return &realAzureBlobClient{blob: client}, nil
}

func (b *realAzureBlobClient) GetProperties(ctx context.Context) (azureBlobProperties, error) {
	response, err := b.blob.GetProperties(ctx, nil)
	if err != nil {
		return azureBlobProperties{}, err
	}
	return azureBlobProperties{ContentLength: response.ContentLength}, nil
}

func (b *realAzureBlobClient) DownloadStream(ctx context.Context) (io.ReadCloser, error) {
	response, err := b.blob.DownloadStream(ctx, nil)
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}

func isAzureNotFound(err error) bool {
	var responseError *azcore.ResponseError
	if errors.As(err, &responseError) {
		return responseError.StatusCode == http.StatusNotFound
	}
	return false
}

func resolveVersionedCloudObject(prefix string, object BlobObjectRef, name string) (string, string, error) {
	key, err := resolveBlobObjectKey(prefix, object.Key)
	if err != nil {
		return "", "", err
	}
	version := strings.TrimSpace(object.Version)
	if version == "" {
		return "", "", fmt.Errorf("%s is required", name)
	}
	return key, version, nil
}
