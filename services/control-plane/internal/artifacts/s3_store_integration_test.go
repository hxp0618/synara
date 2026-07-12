package artifacts

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestS3CompatiblePresignedLifecycle(t *testing.T) {
	bucket := strings.TrimSpace(os.Getenv("SYNARA_TEST_S3_BUCKET"))
	if bucket == "" {
		t.Skip("SYNARA_TEST_S3_BUCKET is not configured")
	}
	kind := platform.ArtifactS3
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SYNARA_TEST_S3_STORE_KIND")), "minio") {
		kind = platform.ArtifactMinIO
	}
	cfg := config.Config{
		Platform:       platform.Config{ArtifactStore: kind},
		ArtifactBucket: bucket, ArtifactRegion: envOr("SYNARA_TEST_S3_REGION", "us-east-1"),
		ArtifactEndpoint:        strings.TrimSpace(os.Getenv("SYNARA_TEST_S3_ENDPOINT")),
		ArtifactPublicEndpoint:  strings.TrimSpace(os.Getenv("SYNARA_TEST_S3_PUBLIC_ENDPOINT")),
		ArtifactAccessKeyID:     strings.TrimSpace(os.Getenv("SYNARA_TEST_S3_ACCESS_KEY_ID")),
		ArtifactSecretAccessKey: strings.TrimSpace(os.Getenv("SYNARA_TEST_S3_SECRET_ACCESS_KEY")),
		ArtifactSessionToken:    strings.TrimSpace(os.Getenv("SYNARA_TEST_S3_SESSION_TOKEN")),
		ArtifactUsePathStyle:    kind == platform.ArtifactMinIO || strings.EqualFold(os.Getenv("SYNARA_TEST_S3_USE_PATH_STYLE"), "true"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewS3Store(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	objectKey := "integration/s3-compatible/" + uuid.NewString()
	t.Cleanup(func() { _ = store.Delete(context.Background(), objectKey) })
	payload := []byte("synara s3-compatible artifact lifecycle\n")
	uploadURL, err := store.PresignUpload(ctx, objectKey, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "text/plain")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("presigned PUT returned %d", response.StatusCode)
	}
	info, err := store.Stat(ctx, objectKey)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(payload)) || info.ContentType != "text/plain" {
		t.Fatalf("stored object metadata = %#v", info)
	}
	reader, err := store.Open(ctx, objectKey)
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("streamed object mismatch: read=%v close=%v", readErr, closeErr)
	}
	downloadURL, err := store.PresignDownload(ctx, objectKey, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	downloadRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	downloadResponse, err := http.DefaultClient.Do(downloadRequest)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, readErr := io.ReadAll(downloadResponse.Body)
	_ = downloadResponse.Body.Close()
	if readErr != nil || downloadResponse.StatusCode != http.StatusOK || !bytes.Equal(downloaded, payload) {
		t.Fatalf("presigned GET mismatch: status=%d read=%v", downloadResponse.StatusCode, readErr)
	}
	if err := store.Delete(ctx, objectKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, objectKey); err == nil {
		t.Fatal("deleted S3-compatible object is still visible")
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
