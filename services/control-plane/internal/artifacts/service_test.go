package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestLocalArtifactLifecycleAndTenantIsolation(t *testing.T) {
	fixture := newArtifactFixture(t)
	payload := []byte("artifact payload\n")
	grant, err := fixture.service.Create(context.Background(), fixture.principal, fixture.sessionID, CreateInput{
		Kind: "attachment", OriginalName: pointerString("../../report.txt"),
	}, "artifact-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if grant.Artifact.OriginalName == nil || *grant.Artifact.OriginalName != "report.txt" {
		t.Fatalf("original filename was not normalized: %#v", grant.Artifact.OriginalName)
	}
	token := uploadToken(t, grant.URL)
	if err := fixture.service.UploadLocal(context.Background(), grant.Artifact.ID, token, "text/plain; charset=utf-8", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}

	wrong := sha256.Sum256([]byte("wrong"))
	_, err = fixture.service.Complete(context.Background(), fixture.principal, grant.Artifact.ID, CompleteInput{
		SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(wrong[:]), ContentType: "text/plain; charset=utf-8",
	}, "artifact-complete-wrong", "127.0.0.1")
	assertProblemCode(t, err, "artifact_hash_mismatch")

	digest := sha256.Sum256(payload)
	completed, err := fixture.service.Complete(context.Background(), fixture.principal, grant.Artifact.ID, CompleteInput{
		SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]), ContentType: "text/plain; charset=utf-8",
	}, "artifact-complete", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "ready" || completed.SHA256 == nil || *completed.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("unexpected completed artifact: %#v", completed)
	}

	var stored persistence.Artifact
	if err := fixture.db.Where("id = ?", completed.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	expectedPrefix := "tenants/" + fixture.tenantID.String() + "/organizations/" + fixture.organizationID.String() +
		"/projects/" + fixture.projectID.String() + "/sessions/" + fixture.sessionID.String() + "/executions/_session/artifacts/"
	if stored.ObjectKey != expectedPrefix+completed.ID.String() {
		t.Fatalf("unexpected object key %q", stored.ObjectKey)
	}
	if _, err := os.Stat(filepath.Join(fixture.artifactRoot, filepath.FromSlash(stored.ObjectKey))); err != nil {
		t.Fatalf("artifact payload was not persisted: %v", err)
	}

	download, err := fixture.service.Download(context.Background(), fixture.principal, completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	downloadToken := uploadToken(t, download.URL)
	_, reader, err := fixture.service.OpenDownload(context.Background(), completed.ID, downloadToken)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(downloaded, payload) {
		t.Fatalf("download mismatch: %q, %v", downloaded, err)
	}

	otherTenant := uuid.New()
	_, err = fixture.service.Get(context.Background(), identity.Principal{UserID: fixture.principal.UserID, ActiveTenantID: &otherTenant}, completed.ID)
	assertProblemCode(t, err, "artifact_not_found")

	if err := fixture.service.Delete(context.Background(), fixture.principal, completed.ID, "artifact-delete", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.artifactRoot, filepath.FromSlash(stored.ObjectKey))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted artifact payload still exists: %v", err)
	}
}

func TestWorkerArtifactConfirmationRequiresCurrentLease(t *testing.T) {
	fixture := newArtifactFixture(t)
	workerID := uuid.New()
	executionID := uuid.New()
	turnID := uuid.New()
	leaseToken := "current-lease-token"
	now := time.Now().UTC()
	worker := persistence.WorkerInstance{
		ID: workerID, ExecutionTargetID: fixture.targetID, TargetKind: "local", ClusterID: "local",
		Namespace: "default", PodName: "worker-1", Version: "test", ProtocolVersion: 1, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: secret.HashToken("worker-token"),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}
	models := []any{
		&worker,
		&persistence.AgentTurn{ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID, CreatedBy: fixture.principal.UserID, Status: "running", InputText: "test"},
		&persistence.AgentExecution{
			ID: executionID, TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
			Attempt: 1, Status: "running", ExecutionTargetID: fixture.targetID, TargetKind: "local",
			WorkerID: &workerID, Generation: 2, RequestedBy: fixture.principal.UserID, QueuedAt: now, StartedAt: &now,
		},
		&persistence.WorkerLease{
			ExecutionID: executionID, TenantID: fixture.tenantID, WorkerID: workerID, Generation: 2,
			LeaseTokenHash: secret.HashToken(leaseToken), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour),
		},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatalf("seed worker artifact fixture %T: %v", model, err)
		}
	}

	grant, err := fixture.service.CreateForWorker(context.Background(), worker, executionID, WorkerCreateInput{
		TenantID: fixture.tenantID, Generation: 2, LeaseToken: leaseToken,
		Kind: "terminal_log", OriginalName: pointerString("worker.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("worker output")
	if err := fixture.service.UploadLocal(context.Background(), grant.Artifact.ID, uploadToken(t, grant.URL), "text/plain", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	complete := CompleteInput{SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]), ContentType: "text/plain"}
	_, err = fixture.service.Complete(context.Background(), fixture.principal, grant.Artifact.ID, complete, "user-bypass", "127.0.0.1")
	assertProblemCode(t, err, "worker_artifact_confirmation_required")

	_, err = fixture.service.CompleteForWorker(context.Background(), worker, executionID, grant.Artifact.ID, WorkerCompleteInput{
		TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken, CompleteInput: complete,
	})
	assertProblemCode(t, err, "generation_fenced")

	completed, err := fixture.service.CompleteForWorker(context.Background(), worker, executionID, grant.Artifact.ID, WorkerCompleteInput{
		TenantID: fixture.tenantID, Generation: 2, LeaseToken: leaseToken, CompleteInput: complete,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "ready" {
		t.Fatalf("worker artifact was not completed: %#v", completed)
	}
	var event persistence.SessionEvent
	if err := fixture.db.Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.tenantID, fixture.sessionID, "artifact.ready").Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.WorkerID == nil || *event.WorkerID != workerID || event.Generation == nil || *event.Generation != 2 {
		t.Fatalf("artifact event lost fencing envelope: %#v", event)
	}
}

func TestLocalStoreRejectsEscapingObjectKeys(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "../escape", strings.NewReader("x"), 1, "text/plain"); err == nil {
		t.Fatal("local artifact store accepted a path traversal object key")
	}
}

func TestObjectStoreUploadGrantCannotOverwriteReadyArtifact(t *testing.T) {
	fixture := newArtifactFixture(t)
	store := &testObjectStore{bucket: "artifact-bucket", objects: map[string][]byte{}, contentTypes: map[string]string{}}
	fixture.service.store = store
	payload := []byte("verified payload")
	grant, err := fixture.service.Create(context.Background(), fixture.principal, fixture.sessionID, CreateInput{
		Kind: "generated_file", OriginalName: pointerString("result.txt"),
	}, "object-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var pending persistence.Artifact
	if err := fixture.db.Where("id = ?", grant.Artifact.ID).Take(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.UploadObjectKey == nil || *pending.UploadObjectKey == pending.ObjectKey {
		t.Fatalf("object-store upload was not isolated from the final key: %#v", pending)
	}
	if _, err := store.Put(context.Background(), *pending.UploadObjectKey, bytes.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if _, err := fixture.service.Complete(context.Background(), fixture.principal, pending.ID, CompleteInput{
		SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]), ContentType: "text/plain",
	}, "object-complete", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), *pending.UploadObjectKey, strings.NewReader("tampered"), 8, "text/plain"); err != nil {
		t.Fatal(err)
	}
	readyPayload := store.objects[pending.ObjectKey]
	if !bytes.Equal(readyPayload, payload) {
		t.Fatalf("replayed upload grant overwrote ready payload: %q", readyPayload)
	}
	cleaned, err := fixture.service.CleanupExpiredUploads(context.Background(), pending.UploadExpiresAt.Add(time.Second), 10)
	if err != nil || cleaned != 1 {
		t.Fatalf("ready temporary-key cleanup = %d, %v", cleaned, err)
	}
	if _, ok := store.objects[*pending.UploadObjectKey]; ok {
		t.Fatal("replayed ready Artifact temporary object remains after URL expiry")
	}
	if !bytes.Equal(store.objects[pending.ObjectKey], payload) {
		t.Fatal("ready temporary-key cleanup removed or changed the verified final object")
	}
	var ready persistence.Artifact
	if err := fixture.db.Where("id = ?", pending.ID).Take(&ready).Error; err != nil {
		t.Fatal(err)
	}
	if ready.Status != "ready" || ready.UploadObjectKey != nil || ready.UploadExpiresAt != nil {
		t.Fatalf("ready Artifact upload metadata was not sealed: %#v", ready)
	}
}

func TestCleanupExpiredUploadRemovesTemporaryAndPromotedObjects(t *testing.T) {
	fixture := newArtifactFixture(t)
	store := &testObjectStore{bucket: "artifact-bucket", objects: map[string][]byte{}, contentTypes: map[string]string{}}
	fixture.service.store = store
	grant, err := fixture.service.Create(context.Background(), fixture.principal, fixture.sessionID, CreateInput{
		Kind: "generated_file", OriginalName: pointerString("expired.txt"),
	}, "expired-object-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var pending persistence.Artifact
	if err := fixture.db.Where("id = ?", grant.Artifact.ID).Take(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.UploadObjectKey == nil {
		t.Fatal("object-store upload did not use a temporary key")
	}
	for _, key := range []string{*pending.UploadObjectKey, pending.ObjectKey} {
		if _, err := store.Put(context.Background(), key, strings.NewReader("orphan"), 6, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	expiredAt := pending.UploadExpiresAt.Add(time.Second)
	cleaned, err := fixture.service.CleanupExpiredUploads(context.Background(), expiredAt, 10)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != 1 {
		t.Fatalf("cleaned uploads = %d, want 1", cleaned)
	}
	if _, ok := store.objects[*pending.UploadObjectKey]; ok {
		t.Fatal("expired temporary object remains")
	}
	if _, ok := store.objects[pending.ObjectKey]; ok {
		t.Fatal("orphaned promoted object remains")
	}
	var failed persistence.Artifact
	if err := fixture.db.Where("id = ?", pending.ID).Take(&failed).Error; err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.UploadExpiresAt != nil || failed.UploadObjectKey != nil || len(failed.UploadTokenHash) != 0 {
		t.Fatalf("expired Artifact metadata was not sealed: %#v", failed)
	}
	cleaned, err = fixture.service.CleanupExpiredUploads(context.Background(), expiredAt, 10)
	if err != nil || cleaned != 0 {
		t.Fatalf("idempotent cleanup = %d, %v", cleaned, err)
	}
}

func TestArtifactQuotaRejectsCompletionAndAllowsRetryAfterIncrease(t *testing.T) {
	fixture := newArtifactFixture(t)
	quotaBytes := int64(10)
	if err := fixture.db.Create(&persistence.TenantQuota{
		TenantID: fixture.tenantID, MaxArtifactBytes: &quotaBytes, UpdatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}

	completePayload := func(name string, payload []byte) (Artifact, error) {
		grant, err := fixture.service.Create(context.Background(), fixture.principal, fixture.sessionID, CreateInput{
			Kind: "generated_file", OriginalName: pointerString(name),
		}, "quota-create-"+name, "127.0.0.1")
		if err != nil {
			return Artifact{}, err
		}
		if err := fixture.service.UploadLocal(
			context.Background(), grant.Artifact.ID, uploadToken(t, grant.URL), "text/plain", int64(len(payload)), bytes.NewReader(payload),
		); err != nil {
			return Artifact{}, err
		}
		digest := sha256.Sum256(payload)
		return fixture.service.Complete(context.Background(), fixture.principal, grant.Artifact.ID, CompleteInput{
			SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]), ContentType: "text/plain",
		}, "quota-complete-"+name, "127.0.0.1")
	}

	if _, err := completePayload("first.txt", []byte("12345678")); err != nil {
		t.Fatal(err)
	}

	secondGrant, err := fixture.service.Create(context.Background(), fixture.principal, fixture.sessionID, CreateInput{
		Kind: "generated_file", OriginalName: pointerString("second.txt"),
	}, "quota-create-second", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	secondPayload := []byte("abcde")
	if err := fixture.service.UploadLocal(
		context.Background(), secondGrant.Artifact.ID, uploadToken(t, secondGrant.URL), "text/plain", int64(len(secondPayload)), bytes.NewReader(secondPayload),
	); err != nil {
		t.Fatal(err)
	}
	secondDigest := sha256.Sum256(secondPayload)
	completeInput := CompleteInput{
		SizeBytes: int64(len(secondPayload)), SHA256: hex.EncodeToString(secondDigest[:]), ContentType: "text/plain",
	}
	_, err = fixture.service.Complete(
		context.Background(), fixture.principal, secondGrant.Artifact.ID, completeInput, "quota-complete-second", "127.0.0.1",
	)
	assertProblemCode(t, err, "artifact_quota_exceeded")

	var pending persistence.Artifact
	if err := fixture.db.Where("id = ?", secondGrant.Artifact.ID).Take(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" {
		t.Fatalf("quota rejection changed the Artifact lifecycle state: %#v", pending)
	}

	quotaBytes = 13
	if err := fixture.db.Model(&persistence.TenantQuota{}).Where("tenant_id = ?", fixture.tenantID).
		Update("max_artifact_bytes", quotaBytes).Error; err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.service.Complete(
		context.Background(), fixture.principal, secondGrant.Artifact.ID, completeInput, "quota-complete-second-retry", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "ready" {
		t.Fatalf("Artifact did not become ready after quota increase: %#v", completed)
	}
}

type artifactFixture struct {
	db             *gorm.DB
	service        *Service
	principal      identity.Principal
	tenantID       uuid.UUID
	organizationID uuid.UUID
	projectID      uuid.UUID
	sessionID      uuid.UUID
	targetID       uuid.UUID
	artifactRoot   string
}

func newArtifactFixture(t *testing.T) artifactFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(root, "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	if err := metadata.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	bootstrapped, err := bootstrap.Ensure(ctx, metadata.DB(), platform.ProfilePersonal, "artifact-test-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectID := uuid.New()
	sessionID := uuid.New()
	if err := metadata.DB().Create(&persistence.Project{
		ID: projectID, TenantID: bootstrapped.TenantID, OrganizationID: bootstrapped.OrganizationID,
		Name: "Artifact Test", DefaultBranch: "main", Visibility: "organization", CreatedBy: bootstrapped.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := metadata.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: bootstrapped.TenantID, OrganizationID: bootstrapped.OrganizationID,
		ProjectID: projectID, CreatedBy: bootstrapped.UserID, Title: "Artifact Test Session",
		Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: bootstrapped.ExecutionTargetID,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	store, err := NewLocalStore(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Platform: platformConfig, ArtifactLocalPath: artifactRoot, ArtifactBucket: "local",
		ArtifactPresignTTL: time.Minute, ArtifactMaxUploadBytes: 1 << 20,
		WorkerLeaseTTL: time.Minute, WorkerHeartbeatTimeout: 2 * time.Minute, WorkerReceiptTTL: time.Hour,
	}
	targetService := executiontargets.NewService(metadata.DB(), platformConfig, nil)
	sessionService := sessions.NewService(metadata.DB(), projects.NewService(metadata.DB()), targetService)
	executionService := executions.NewService(
		metadata.DB(), sessionService, cfg.WorkerLeaseTTL, cfg.WorkerHeartbeatTimeout,
		cfg.WorkerReceiptTTL, nil, targetService,
	)
	return artifactFixture{
		db: metadata.DB(), service: NewService(metadata.DB(), store, cfg, executionService, sessionService),
		principal: identity.Principal{UserID: bootstrapped.UserID, ActiveTenantID: &bootstrapped.TenantID},
		tenantID:  bootstrapped.TenantID, organizationID: bootstrapped.OrganizationID,
		projectID: projectID, sessionID: sessionID, targetID: bootstrapped.ExecutionTargetID, artifactRoot: artifactRoot,
	}
}

func uploadToken(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	token := parsed.Query().Get("token")
	if token == "" {
		t.Fatalf("URL did not contain a token: %s", rawURL)
	}
	return token
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}

func pointerString(value string) *string { return &value }

type testObjectStore struct {
	bucket       string
	objects      map[string][]byte
	contentTypes map[string]string
}

func (s *testObjectStore) Bucket() string              { return s.bucket }
func (s *testObjectStore) IsLocal() bool               { return false }
func (s *testObjectStore) Check(context.Context) error { return nil }
func (s *testObjectStore) PresignUpload(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://objects.example/upload/" + url.PathEscape(key), nil
}
func (s *testObjectStore) PresignDownload(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://objects.example/download/" + url.PathEscape(key), nil
}
func (s *testObjectStore) Put(_ context.Context, key string, reader io.Reader, size int64, contentType string) (ObjectInfo, error) {
	payload, err := io.ReadAll(reader)
	if err != nil {
		return ObjectInfo{}, err
	}
	if int64(len(payload)) != size {
		return ObjectInfo{}, io.ErrUnexpectedEOF
	}
	s.objects[key] = payload
	s.contentTypes[key] = contentType
	return ObjectInfo{Size: size, ContentType: contentType, Version: "v1"}, nil
}
func (s *testObjectStore) Stat(_ context.Context, key string) (ObjectInfo, error) {
	payload, ok := s.objects[key]
	if !ok {
		return ObjectInfo{}, os.ErrNotExist
	}
	return ObjectInfo{Size: int64(len(payload)), ContentType: s.contentTypes[key], Version: "v1"}, nil
}
func (s *testObjectStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	payload, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(payload)), nil
}
func (s *testObjectStore) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	delete(s.contentTypes, key)
	return nil
}
