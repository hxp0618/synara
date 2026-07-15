package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/workerreleases"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkerReleaseRoutesManageLifecycleReplayAndAbortProjection(t *testing.T) {
	fixture := newWorkerReleaseHTTPFixture(t)
	basePath := fixture.basePath()

	initial := fixture.request(t, http.MethodGet, basePath, fixture.ownerToken, "", "")
	if initial.Code != http.StatusOK {
		t.Fatalf("initial list status = %d, body = %s", initial.Code, initial.Body.String())
	}
	var initialOverview workerreleases.Overview
	decodeWorkerReleaseHTTPResponse(t, initial, &initialOverview)
	if initialOverview.Policy != nil || len(initialOverview.Revisions) != 0 || len(initialOverview.Transitions) != 0 {
		t.Fatalf("initial overview = %#v", initialOverview)
	}

	firstCreateBody := workerReleaseHTTPJSON(t, map[string]any{
		"workerManifestId": fixture.firstManifestID,
		"description":      "Initial release",
	})
	firstCreate := fixture.request(t, http.MethodPost, basePath, fixture.ownerToken, firstCreateBody, "release-create-first")
	assertWorkerReleaseHTTPStatus(t, firstCreate, http.StatusCreated)
	var firstRevision workerreleases.Revision
	decodeWorkerReleaseHTTPResponse(t, firstCreate, &firstRevision)
	assertWorkerReleaseHTTPReplay(
		t, firstCreate,
		fixture.request(t, http.MethodPost, basePath, fixture.ownerToken, firstCreateBody, "release-create-first"),
		http.StatusCreated,
	)

	secondCreateBody := workerReleaseHTTPJSON(t, map[string]any{
		"workerManifestId": fixture.secondManifestID,
		"description":      "Canary release",
	})
	secondCreate := fixture.request(t, http.MethodPost, basePath, fixture.ownerToken, secondCreateBody, "release-create-second")
	assertWorkerReleaseHTTPStatus(t, secondCreate, http.StatusCreated)
	var secondRevision workerreleases.Revision
	decodeWorkerReleaseHTTPResponse(t, secondCreate, &secondRevision)

	promotePath := basePath + "/" + firstRevision.ID.String() + "/promote"
	promoteBody := workerReleaseHTTPJSON(t, map[string]any{
		"expectedPolicyVersion": 0,
		"reason":                "Establish the baseline",
	})
	promoted := fixture.request(t, http.MethodPost, promotePath, fixture.ownerToken, promoteBody, "release-promote-first")
	assertWorkerReleaseHTTPStatus(t, promoted, http.StatusOK)
	var promotedPolicy workerreleases.Policy
	decodeWorkerReleaseHTTPResponse(t, promoted, &promotedPolicy)
	if promotedPolicy.PolicyVersion != 1 || promotedPolicy.PromotedRevisionID != firstRevision.ID {
		t.Fatalf("promoted policy = %#v", promotedPolicy)
	}
	assertWorkerReleaseHTTPReplay(
		t, promoted,
		fixture.request(t, http.MethodPost, promotePath, fixture.ownerToken, promoteBody, "release-promote-first"),
		http.StatusOK,
	)

	canaryPath := basePath + "/" + secondRevision.ID.String() + "/canary"
	canaryBody := workerReleaseHTTPJSON(t, map[string]any{
		"expectedPolicyVersion": 1,
		"canaryPercent":         25,
		"reason":                "Start a bounded canary",
	})
	canary := fixture.request(t, http.MethodPost, canaryPath, fixture.ownerToken, canaryBody, "release-canary-second")
	assertWorkerReleaseHTTPStatus(t, canary, http.StatusOK)
	var canaryPolicy workerreleases.Policy
	decodeWorkerReleaseHTTPResponse(t, canary, &canaryPolicy)
	if canaryPolicy.PolicyVersion != 2 || canaryPolicy.CanaryRevisionID == nil ||
		*canaryPolicy.CanaryRevisionID != secondRevision.ID || canaryPolicy.CanaryPercent != 25 {
		t.Fatalf("canary policy = %#v", canaryPolicy)
	}
	assertWorkerReleaseHTTPReplay(
		t, canary,
		fixture.request(t, http.MethodPost, canaryPath, fixture.ownerToken, canaryBody, "release-canary-second"),
		http.StatusOK,
	)

	// The public abort operation intentionally remains the rollback endpoint.
	rollbackPath := basePath + "/" + firstRevision.ID.String() + "/rollback"
	rollbackBody := workerReleaseHTTPJSON(t, map[string]any{
		"expectedPolicyVersion": 2,
		"reason":                "Abort the unhealthy canary",
	})
	aborted := fixture.request(t, http.MethodPost, rollbackPath, fixture.ownerToken, rollbackBody, "release-abort-canary")
	assertWorkerReleaseHTTPStatus(t, aborted, http.StatusOK)
	var abortedPolicy workerreleases.Policy
	decodeWorkerReleaseHTTPResponse(t, aborted, &abortedPolicy)
	if abortedPolicy.PolicyVersion != 3 || abortedPolicy.PromotedRevisionID != firstRevision.ID ||
		abortedPolicy.CanaryRevisionID != nil || abortedPolicy.CanaryPercent != 0 {
		t.Fatalf("aborted policy = %#v", abortedPolicy)
	}
	assertWorkerReleaseHTTPReplay(
		t, aborted,
		fixture.request(t, http.MethodPost, rollbackPath, fixture.ownerToken, rollbackBody, "release-abort-canary"),
		http.StatusOK,
	)

	listed := fixture.request(t, http.MethodGet, basePath, fixture.ownerToken, "", "")
	assertWorkerReleaseHTTPStatus(t, listed, http.StatusOK)
	var overview workerreleases.Overview
	decodeWorkerReleaseHTTPResponse(t, listed, &overview)
	if overview.Policy == nil || overview.Policy.PolicyVersion != 3 || len(overview.Revisions) != 2 ||
		len(overview.Transitions) != 3 {
		t.Fatalf("final overview = %#v", overview)
	}
	latest := overview.Transitions[0]
	if latest.Action != "abort-canary" || latest.ToPromotedRevisionID != firstRevision.ID ||
		latest.ToCanaryRevisionID != nil || latest.Reason != "Abort the unhealthy canary" {
		t.Fatalf("latest transition = %#v, want abort-canary projection", latest)
	}
}

func TestWorkerReleaseRoutesEnforceTenantPermissionsAndIdempotency(t *testing.T) {
	fixture := newWorkerReleaseHTTPFixture(t)
	basePath := fixture.basePath()

	for _, test := range []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{name: "unauthenticated", status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "active tenant mismatch", token: fixture.crossTenantToken, status: http.StatusNotFound, code: "tenant_not_found"},
		{name: "missing Worker read", token: fixture.memberToken, status: http.StatusForbidden, code: "tenant_forbidden"},
	} {
		t.Run("list "+test.name, func(t *testing.T) {
			assertProblemResponse(
				t,
				fixture.request(t, http.MethodGet, basePath, test.token, "", ""),
				test.status,
				test.code,
			)
		})
	}
	readable := fixture.request(t, http.MethodGet, basePath, fixture.readOnlyToken, "", "")
	assertWorkerReleaseHTTPStatus(t, readable, http.StatusOK)

	createBody := workerReleaseHTTPJSON(t, map[string]any{
		"workerManifestId": fixture.firstManifestID,
		"description":      "Permission probe",
	})
	for _, test := range []struct {
		name  string
		path  string
		body  string
		key   string
		token string
		code  string
	}{
		{name: "create read only", path: basePath, body: createBody, key: "release-read-only-create", token: fixture.readOnlyToken, code: "tenant_forbidden"},
		{name: "create wrong active tenant", path: basePath, body: createBody, key: "release-cross-tenant-create", token: fixture.crossTenantToken, code: "tenant_not_found"},
		{name: "promote read only", path: basePath + "/" + uuid.NewString() + "/promote", body: `{"expectedPolicyVersion":0,"reason":"probe"}`, key: "release-read-only-promote", token: fixture.readOnlyToken, code: "tenant_forbidden"},
		{name: "canary read only", path: basePath + "/" + uuid.NewString() + "/canary", body: `{"expectedPolicyVersion":0,"canaryPercent":10,"reason":"probe"}`, key: "release-read-only-canary", token: fixture.readOnlyToken, code: "tenant_forbidden"},
		{name: "rollback read only", path: basePath + "/" + uuid.NewString() + "/rollback", body: `{"expectedPolicyVersion":0,"reason":"probe"}`, key: "release-read-only-rollback", token: fixture.readOnlyToken, code: "tenant_forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := http.StatusForbidden
			if test.code == "tenant_not_found" {
				status = http.StatusNotFound
			}
			assertProblemResponse(
				t,
				fixture.request(t, http.MethodPost, test.path, test.token, test.body, test.key),
				status,
				test.code,
			)
		})
	}

	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{name: "create", path: basePath, body: createBody},
		{name: "promote", path: basePath + "/" + uuid.NewString() + "/promote", body: `{"expectedPolicyVersion":0,"reason":"probe"}`},
		{name: "canary", path: basePath + "/" + uuid.NewString() + "/canary", body: `{"expectedPolicyVersion":0,"canaryPercent":10,"reason":"probe"}`},
		{name: "rollback", path: basePath + "/" + uuid.NewString() + "/rollback", body: `{"expectedPolicyVersion":0,"reason":"probe"}`},
	} {
		t.Run(test.name+" requires Idempotency-Key", func(t *testing.T) {
			assertProblemResponse(
				t,
				fixture.request(t, http.MethodPost, test.path, fixture.ownerToken, test.body, ""),
				http.StatusBadRequest,
				"idempotency_key_required",
			)
		})
	}
}

func TestWorkerReleaseRoutesRejectInvalidJSONAndUUID(t *testing.T) {
	fixture := newWorkerReleaseHTTPFixture(t)
	basePath := fixture.basePath()

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "create", path: basePath},
		{name: "promote", path: basePath + "/" + uuid.NewString() + "/promote"},
		{name: "canary", path: basePath + "/" + uuid.NewString() + "/canary"},
		{name: "rollback", path: basePath + "/" + uuid.NewString() + "/rollback"},
	} {
		t.Run(test.name+" invalid JSON", func(t *testing.T) {
			assertProblemResponse(
				t,
				fixture.request(t, http.MethodPost, test.path, fixture.ownerToken, "{", "release-invalid-json-"+test.name),
				http.StatusBadRequest,
				"invalid_json",
			)
		})
	}

	for _, test := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "tenant", method: http.MethodGet, path: "/v1/tenants/not-a-uuid/execution-targets/" + fixture.targetID.String() + "/worker-releases"},
		{name: "target", method: http.MethodPost, path: "/v1/tenants/" + fixture.tenantID.String() + "/execution-targets/not-a-uuid/worker-releases", body: `{"workerManifestId":"` + fixture.firstManifestID.String() + `","description":"probe"}`},
		{name: "revision", method: http.MethodPost, path: basePath + "/not-a-uuid/promote", body: `{"expectedPolicyVersion":0,"reason":"probe"}`},
	} {
		t.Run("invalid "+test.name+" UUID", func(t *testing.T) {
			assertProblemResponse(
				t,
				fixture.request(t, test.method, test.path, fixture.ownerToken, test.body, "release-invalid-uuid-"+test.name),
				http.StatusBadRequest,
				"invalid_id",
			)
		})
	}
}

type workerReleaseHTTPFixture struct {
	handler          http.Handler
	cookieName       string
	tenantID         uuid.UUID
	targetID         uuid.UUID
	firstManifestID  uuid.UUID
	secondManifestID uuid.UUID
	ownerToken       string
	readOnlyToken    string
	memberToken      string
	crossTenantToken string
}

func newWorkerReleaseHTTPFixture(t *testing.T) workerReleaseHTTPFixture {
	t.Helper()
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "worker-release-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	firstManifestID, secondManifestID := seedWorkerReleaseHTTPPool(t, store.DB(), domain.ExecutionTargetID, now)

	ownerToken := createWorkerReleaseHTTPLogin(t, store.DB(), domain.UserID, domain.TenantID)
	readOnlyToken := createWorkerReleaseHTTPUser(t, store.DB(), domain.TenantID, "security_admin", now)
	memberToken := createWorkerReleaseHTTPUser(t, store.DB(), domain.TenantID, "member", now)
	otherTenantID := uuid.New()
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: otherTenantID, Slug: "worker-release-http-other-" + uuid.NewString(), Name: "Other tenant",
			Status: "active", PlanCode: "developer", Region: "local", Settings: map[string]any{},
			CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: otherTenantID, UserID: domain.UserID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	crossTenantToken := createWorkerReleaseHTTPLogin(t, store.DB(), domain.UserID, otherTenantID)

	cfg := config.Config{
		Platform: profile, CookieName: "synara_worker_release_session", CookiePath: "/",
		SessionTTL: time.Hour, SessionIdleTTL: time.Hour,
		SSEPollInterval: 20 * time.Millisecond, SSEHeartbeatInterval: time.Second,
		SSEWriteTimeout: time.Second, SSELeaseTTL: time.Minute,
		SSEMaxConnectionsPerUser: 1, SSEMaxConnectionsPerTenant: 10,
	}
	identityService := identity.NewService(store.DB(), cfg.SessionTTL, cfg.SessionIdleTTL)
	projectService := projects.NewService(store.DB())
	targetService := executiontargets.NewService(store.DB(), profile, nil)
	sessionService := sessions.NewService(store.DB(), projectService, targetService)
	executionService := executions.NewService(
		store.DB(), sessionService, time.Minute, 2*time.Minute, time.Hour, nil, targetService,
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(
		cfg, store.DB(), identityService, nil, projectService, sessionService, executionService, targetService,
		nil, nil, nil, nil, nil, observability.New(store.DB(), observability.Config{SessionIdleTTL: cfg.SessionIdleTTL}), nil, nil, nil, nil, nil, logger,
	)
	if err != nil {
		t.Fatal(err)
	}
	return workerReleaseHTTPFixture{
		handler: server.Handler(), cookieName: cfg.CookieName,
		tenantID: domain.TenantID, targetID: domain.ExecutionTargetID,
		firstManifestID: firstManifestID, secondManifestID: secondManifestID,
		ownerToken: ownerToken, readOnlyToken: readOnlyToken,
		memberToken: memberToken, crossTenantToken: crossTenantToken,
	}
}

func seedWorkerReleaseHTTPPool(
	t *testing.T,
	db *gorm.DB,
	targetID uuid.UUID,
	now time.Time,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	manifestIDs := [2]uuid.UUID{uuid.New(), uuid.New()}
	for index, manifestID := range manifestIDs {
		imageDigest := "sha256:" + workerReleaseHTTPDigest(fmt.Sprintf("image-%d", index))
		if err := db.Create(&persistence.WorkerManifest{
			ID: manifestID, ManifestHash: workerReleaseHTTPDigest(fmt.Sprintf("manifest-%d", index)),
			WorkerBuildVersion:    fmt.Sprintf("worker-v%d", index+1),
			WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
			OperatingSystem: "linux", Architecture: "amd64", ImageDigest: &imageDigest,
			FeatureFlags: map[string]any{}, CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.WorkerInstance{
			ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID,
			TargetKind: "local", ClusterID: "worker-release-http", Namespace: "default",
			PodName: fmt.Sprintf("worker-%d", index+1), Version: fmt.Sprintf("worker-v%d", index+1),
			ProtocolVersion: 2, Capabilities: map[string]any{}, CurrentManifestID: &manifestID,
			CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
			LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte(uuid.NewString()),
			Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return manifestIDs[0], manifestIDs[1]
}

func createWorkerReleaseHTTPUser(
	t *testing.T,
	db *gorm.DB,
	tenantID uuid.UUID,
	role string,
	now time.Time,
) string {
	t.Helper()
	userID := uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Worker release " + role,
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: role, Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	return createWorkerReleaseHTTPLogin(t, db, userID, tenantID)
}

func createWorkerReleaseHTTPLogin(t *testing.T, db *gorm.DB, userID, tenantID uuid.UUID) string {
	t.Helper()
	token := "worker-release-http-" + uuid.NewString()
	tokenHash := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	if err := db.Create(&persistence.LoginSession{
		ID: uuid.New(), UserID: userID, ActiveTenantID: &tenantID, RefreshTokenHash: tokenHash[:],
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return token
}

func (f workerReleaseHTTPFixture) basePath() string {
	return "/v1/tenants/" + f.tenantID.String() + "/execution-targets/" + f.targetID.String() + "/worker-releases"
}

func (f workerReleaseHTTPFixture) request(
	t *testing.T,
	method, path, token, body, idempotencyKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func workerReleaseHTTPJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func decodeWorkerReleaseHTTPResponse(t *testing.T, response *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
		t.Fatalf("decode response: %v, body = %s", err, response.Body.String())
	}
}

func assertWorkerReleaseHTTPStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, status, response.Body.String())
	}
}

func assertWorkerReleaseHTTPReplay(
	t *testing.T,
	first, replayed *httptest.ResponseRecorder,
	status int,
) {
	t.Helper()
	if first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("first request was marked replayed: headers=%v", first.Header())
	}
	if replayed.Code != status || replayed.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf(
			"replay status/header = %d/%q, body = %s",
			replayed.Code,
			replayed.Header().Get("Idempotent-Replayed"),
			replayed.Body.String(),
		)
	}
	if first.Body.String() != replayed.Body.String() {
		t.Fatalf("replay body changed:\nfirst=%s\nreplayed=%s", first.Body.String(), replayed.Body.String())
	}
}

func workerReleaseHTTPDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
