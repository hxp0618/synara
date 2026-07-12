package metadatamigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPersonalExportImportPreservesIDsAndEncryptedCursor(t *testing.T) {
	ctx := context.Background()
	personal, _ := platform.Defaults(platform.ProfilePersonal)
	source, err := database.OpenMetadataStore(ctx, personal, "", filepath.Join(t.TempDir(), "source.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	if err := source.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, source.DB(), platform.ProfilePersonal, "migration-test")
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.New()
	sessionID := uuid.New()
	cursor := []byte{1, 2, 3, 4, 5, 6}
	if err := source.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Migration project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := source.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Migration session", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
		ProviderResumeCursorEncrypted: cursor,
	}).Error; err != nil {
		t.Fatal(err)
	}
	manifest, err := Export(ctx, source.DB(), platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}

	targetStoreConfig := personal
	targetStore, err := database.OpenMetadataStore(ctx, targetStoreConfig, "", filepath.Join(t.TempDir(), "target.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = targetStore.Close() })
	if err := targetStore.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	targetProfile, _ := platform.Defaults(platform.ProfileSingleNode)
	report, err := Import(ctx, targetStore.DB(), targetProfile, decoded, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArtifactPayloadMigration != "not_required" || report.Replayed {
		t.Fatalf("unexpected import report: %#v", report)
	}
	var imported persistence.AgentSession
	if err := targetStore.DB().Where("id = ?", sessionID).Take(&imported).Error; err != nil {
		t.Fatal(err)
	}
	if imported.ID != sessionID || imported.ExecutionTargetID != domain.ExecutionTargetID ||
		!bytes.Equal(imported.ProviderResumeCursorEncrypted, cursor) {
		t.Fatalf("import did not preserve session identity/cursor: %#v", imported)
	}
	replayed, err := Import(ctx, targetStore.DB(), targetProfile, decoded, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed {
		t.Fatal("re-import of the same manifest was not idempotent")
	}
}

func TestArtifactPayloadMigrationIsVerifiedAndReentrant(t *testing.T) {
	ctx := context.Background()
	personal, _ := platform.Defaults(platform.ProfilePersonal)
	root := t.TempDir()
	source, err := database.OpenMetadataStore(ctx, personal, "", filepath.Join(root, "source.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	if err := source.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, source.DB(), platform.ProfilePersonal, "artifact-migration-test")
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.New()
	sessionID := uuid.New()
	artifactID := uuid.New()
	if err := source.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Artifact migration", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := source.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Artifact migration", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	objectKey := "tenants/" + domain.TenantID.String() + "/organizations/" + domain.OrganizationID.String() +
		"/projects/" + projectID.String() + "/sessions/" + sessionID.String() + "/executions/_session/artifacts/" + artifactID.String()
	payload := []byte("migrate me")
	digest := sha256.Sum256(payload)
	digestText := hex.EncodeToString(digest[:])
	contentType := "text/plain"
	size := int64(len(payload))
	readyAt := time.Now().UTC()
	if err := source.DB().Create(&persistence.Artifact{
		ID: artifactID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, SessionID: sessionID, Kind: "attachment", Status: "ready",
		Bucket: "local", ObjectKey: objectKey, ContentType: &contentType, SizeBytes: &size, SHA256: &digestText,
		CreatedByType: "user", CreatedByID: domain.UserID, ReadyAt: &readyAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	sourceObjects, err := artifacts.NewLocalStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceObjects.Put(ctx, objectKey, bytes.NewReader(payload), size, contentType); err != nil {
		t.Fatal(err)
	}
	manifest, err := Export(ctx, source.DB(), platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Artifacts.Entries) != 1 || len(manifest.Data.ArtifactRecords) != 1 {
		t.Fatalf("artifact export was incomplete: %#v", manifest.Artifacts)
	}
	encoded, err := Encode(manifest)
	if err != nil {
		t.Fatal(err)
	}

	target, err := database.OpenMetadataStore(ctx, personal, "", filepath.Join(root, "target.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	if err := target.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	singleNode, _ := platform.Defaults(platform.ProfileSingleNode)
	if _, err := Import(ctx, target.DB(), singleNode, manifest, encoded); err != nil {
		t.Fatal(err)
	}
	destination := newMemoryObjectStore("synara-artifacts")
	report, err := MigrateArtifactPayloads(ctx, target.DB(), manifest, sourceObjects, destination)
	if err != nil {
		t.Fatal(err)
	}
	if report.Migrated != 1 || report.Replayed != 0 || !bytes.Equal(destination.objects[objectKey], payload) {
		t.Fatalf("unexpected migration report or payload: %#v", report)
	}
	var imported persistence.Artifact
	if err := target.DB().Where("id = ?", artifactID).Take(&imported).Error; err != nil {
		t.Fatal(err)
	}
	if imported.Bucket != destination.Bucket() {
		t.Fatalf("artifact bucket was not switched to destination: %#v", imported)
	}
	replayed, err := MigrateArtifactPayloads(ctx, target.DB(), manifest, sourceObjects, destination)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Migrated != 0 || replayed.Replayed != 1 {
		t.Fatalf("artifact migration was not reentrant: %#v", replayed)
	}
}

type memoryObjectStore struct {
	bucket       string
	objects      map[string][]byte
	contentTypes map[string]string
}

func newMemoryObjectStore(bucket string) *memoryObjectStore {
	return &memoryObjectStore{bucket: bucket, objects: map[string][]byte{}, contentTypes: map[string]string{}}
}

func (s *memoryObjectStore) Bucket() string              { return s.bucket }
func (s *memoryObjectStore) IsLocal() bool               { return false }
func (s *memoryObjectStore) Check(context.Context) error { return nil }
func (s *memoryObjectStore) PresignUpload(context.Context, string, time.Duration) (string, error) {
	return "", artifacts.ErrPresignUnsupported
}
func (s *memoryObjectStore) PresignDownload(context.Context, string, time.Duration) (string, error) {
	return "", artifacts.ErrPresignUnsupported
}
func (s *memoryObjectStore) Put(_ context.Context, key string, reader io.Reader, size int64, contentType string) (artifacts.ObjectInfo, error) {
	payload, err := io.ReadAll(reader)
	if err != nil {
		return artifacts.ObjectInfo{}, err
	}
	if int64(len(payload)) != size {
		return artifacts.ObjectInfo{}, io.ErrUnexpectedEOF
	}
	s.objects[key] = payload
	s.contentTypes[key] = contentType
	return artifacts.ObjectInfo{Size: size, ContentType: contentType, Version: "v1"}, nil
}
func (s *memoryObjectStore) Stat(_ context.Context, key string) (artifacts.ObjectInfo, error) {
	payload, ok := s.objects[key]
	if !ok {
		return artifacts.ObjectInfo{}, errors.New("not found")
	}
	return artifacts.ObjectInfo{Size: int64(len(payload)), ContentType: s.contentTypes[key], Version: "v1"}, nil
}
func (s *memoryObjectStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	payload, ok := s.objects[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(payload)), nil
}
func (s *memoryObjectStore) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	delete(s.contentTypes, key)
	return nil
}
