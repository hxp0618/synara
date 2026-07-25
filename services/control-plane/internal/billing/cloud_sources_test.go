package billing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestS3ReadOnlyBlobSourceRequiresVersionAndConfinesPrefix(t *testing.T) {
	source := newS3ReadOnlyBlobSource(&fakeS3ReadAPI{}, "billing-bucket", "exports/tenant-a")
	if _, _, err := source.Open(context.Background(), BlobObjectRef{Key: "cur/2026-07.csv"}); err == nil {
		t.Fatal("expected missing object version to fail closed")
	}
	if _, _, err := source.Open(context.Background(), BlobObjectRef{Key: "../escape.csv", Version: "v1"}); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestS3ReadOnlyBlobSourceUsesPrefixedVersionedObjectAndMapsNotFound(t *testing.T) {
	contentLength := int64(12)
	client := &fakeS3ReadAPI{
		headOutput: &awss3.HeadObjectOutput{ContentLength: &contentLength},
		getOutput:  &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewBufferString("hello world!"))},
	}
	source := newS3ReadOnlyBlobSource(client, "billing-bucket", "exports/tenant-a")

	reader, metadata, err := source.Open(context.Background(), BlobObjectRef{
		Key: "cur/2026-07.csv", Version: "ver-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if metadata.SizeBytes != 12 {
		t.Fatalf("metadata size = %d, want 12", metadata.SizeBytes)
	}
	if client.headInput == nil || aws.ToString(client.headInput.Key) != "exports/tenant-a/cur/2026-07.csv" ||
		aws.ToString(client.headInput.VersionId) != "ver-1" {
		t.Fatalf("unexpected HeadObject input: %#v", client.headInput)
	}
	if client.getInput == nil || aws.ToString(client.getInput.Key) != "exports/tenant-a/cur/2026-07.csv" ||
		aws.ToString(client.getInput.VersionId) != "ver-1" {
		t.Fatalf("unexpected GetObject input: %#v", client.getInput)
	}

	notFoundSource := newS3ReadOnlyBlobSource(&fakeS3ReadAPI{headErr: fakeSmithyAPIError{code: "NoSuchKey"}}, "billing-bucket", "")
	if _, _, err := notFoundSource.Open(context.Background(), BlobObjectRef{Key: "cur.csv", Version: "ver-2"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}

func TestS3ReadOnlyBlobSourceFailsClosedWhenSizeMetadataIsMissing(t *testing.T) {
	source := newS3ReadOnlyBlobSource(&fakeS3ReadAPI{
		headOutput: &awss3.HeadObjectOutput{ContentLength: nil},
	}, "billing-bucket", "")
	if _, _, err := source.Open(context.Background(), BlobObjectRef{Key: "cur.csv", Version: "ver-3"}); err == nil {
		t.Fatal("expected missing size metadata to fail closed")
	}
}

func TestGCSReadOnlyBlobSourceUsesPrefixedGenerationAndClose(t *testing.T) {
	reader := io.NopCloser(bytes.NewBufferString("gcs"))
	object := &fakeGCSObjectClient{
		attrs:  gcsObjectAttrs{Size: 3},
		reader: reader,
	}
	closer := &fakeCloser{}
	source := newGCSReadOnlyBlobSource(&fakeGCSBucketClient{object: object}, "exports/tenant-b", closer)

	opened, metadata, err := source.Open(context.Background(), BlobObjectRef{Key: "invoice.json", Version: "42"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	if metadata.SizeBytes != 3 {
		t.Fatalf("metadata size = %d, want 3", metadata.SizeBytes)
	}
	if object.key != "exports/tenant-b/invoice.json" || object.generation != 42 {
		t.Fatalf("unexpected GCS object routing: key=%q generation=%d", object.key, object.generation)
	}
	if err := source.Close(); err != nil || !closer.closed {
		t.Fatalf("close err=%v closed=%v", err, closer.closed)
	}
}

func TestGCSReadOnlyBlobSourceMapsNotFoundAndMissingSize(t *testing.T) {
	notFound := newGCSReadOnlyBlobSource(&fakeGCSBucketClient{object: &fakeGCSObjectClient{attrsErr: storage.ErrObjectNotExist}}, "", nil)
	if _, _, err := notFound.Open(context.Background(), BlobObjectRef{Key: "invoice.json", Version: "99"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
	missingSize := newGCSReadOnlyBlobSource(&fakeGCSBucketClient{object: &fakeGCSObjectClient{attrs: gcsObjectAttrs{Size: -1}}}, "", nil)
	if _, _, err := missingSize.Open(context.Background(), BlobObjectRef{Key: "invoice.json", Version: "99"}); err == nil {
		t.Fatal("expected missing GCS size metadata to fail closed")
	}
}

func TestAzureReadOnlyBlobSourceUsesPrefixedVersionAndMapsNotFound(t *testing.T) {
	contentLength := int64(5)
	blob := &fakeAzureBlobClient{
		properties: azureBlobProperties{ContentLength: &contentLength},
		reader:     io.NopCloser(bytes.NewBufferString("azure")),
	}
	source := newAzureReadOnlyBlobSource(&fakeAzureContainerClient{blob: blob}, "exports/tenant-c")

	reader, metadata, err := source.Open(context.Background(), BlobObjectRef{Key: "invoice.csv", Version: "ver-9"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if metadata.SizeBytes != 5 {
		t.Fatalf("metadata size = %d, want 5", metadata.SizeBytes)
	}
	if blob.key != "exports/tenant-c/invoice.csv" || blob.version != "ver-9" {
		t.Fatalf("unexpected Azure blob routing: key=%q version=%q", blob.key, blob.version)
	}

	notFound := newAzureReadOnlyBlobSource(&fakeAzureContainerClient{
		blob: &fakeAzureBlobClient{propertiesErr: &azcore.ResponseError{StatusCode: 404}},
	}, "")
	if _, _, err := notFound.Open(context.Background(), BlobObjectRef{Key: "invoice.csv", Version: "ver-10"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}

func TestAzureReadOnlyBlobSourceFailsClosedWhenSizeMetadataIsMissing(t *testing.T) {
	source := newAzureReadOnlyBlobSource(&fakeAzureContainerClient{
		blob: &fakeAzureBlobClient{properties: azureBlobProperties{}},
	}, "")
	if _, _, err := source.Open(context.Background(), BlobObjectRef{Key: "invoice.csv", Version: "ver-11"}); err == nil {
		t.Fatal("expected missing Azure size metadata to fail closed")
	}
}

type fakeS3ReadAPI struct {
	headInput  *awss3.HeadObjectInput
	headOutput *awss3.HeadObjectOutput
	headErr    error
	getInput   *awss3.GetObjectInput
	getOutput  *awss3.GetObjectOutput
	getErr     error
}

func (f *fakeS3ReadAPI) HeadObject(_ context.Context, input *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	f.headInput = input
	if f.headErr != nil {
		return nil, f.headErr
	}
	return f.headOutput, nil
}

func (f *fakeS3ReadAPI) GetObject(_ context.Context, input *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	f.getInput = input
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getOutput, nil
}

type fakeSmithyAPIError struct {
	code string
}

func (e fakeSmithyAPIError) Error() string              { return e.code }
func (e fakeSmithyAPIError) ErrorCode() string          { return e.code }
func (e fakeSmithyAPIError) ErrorMessage() string       { return e.code }
func (e fakeSmithyAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }

type fakeGCSBucketClient struct {
	object *fakeGCSObjectClient
}

func (b *fakeGCSBucketClient) Object(key string) gcsObjectClient {
	b.object.key = key
	return b.object
}

type fakeGCSObjectClient struct {
	key        string
	generation int64
	attrs      gcsObjectAttrs
	attrsErr   error
	reader     io.ReadCloser
	readerErr  error
}

func (o *fakeGCSObjectClient) Generation(generation int64) gcsObjectClient {
	o.generation = generation
	return o
}

func (o *fakeGCSObjectClient) Attrs(context.Context) (gcsObjectAttrs, error) {
	if o.attrsErr != nil {
		return gcsObjectAttrs{}, o.attrsErr
	}
	return o.attrs, nil
}

func (o *fakeGCSObjectClient) NewReader(context.Context) (io.ReadCloser, error) {
	if o.readerErr != nil {
		return nil, o.readerErr
	}
	return o.reader, nil
}

type fakeAzureContainerClient struct {
	blob *fakeAzureBlobClient
}

func (c *fakeAzureContainerClient) Blob(key string) azureBlobClient {
	c.blob.key = key
	return c.blob
}

type fakeAzureBlobClient struct {
	key           string
	version       string
	versionErr    error
	properties    azureBlobProperties
	propertiesErr error
	reader        io.ReadCloser
	readerErr     error
}

func (b *fakeAzureBlobClient) WithVersionID(version string) (azureBlobClient, error) {
	b.version = version
	if b.versionErr != nil {
		return nil, b.versionErr
	}
	return b, nil
}

func (b *fakeAzureBlobClient) GetProperties(context.Context) (azureBlobProperties, error) {
	if b.propertiesErr != nil {
		return azureBlobProperties{}, b.propertiesErr
	}
	return b.properties, nil
}

func (b *fakeAzureBlobClient) DownloadStream(context.Context) (io.ReadCloser, error) {
	if b.readerErr != nil {
		return nil, b.readerErr
	}
	return b.reader, nil
}

type fakeCloser struct {
	closed bool
}

func (c *fakeCloser) Close() error {
	c.closed = true
	return nil
}
