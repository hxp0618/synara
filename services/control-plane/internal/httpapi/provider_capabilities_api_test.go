package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	"github.com/synara-ai/synara/services/control-plane/internal/providercapabilities"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProviderCapabilityRoutesExposeOnlySanitizedStableProjection(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)

	projectResponse := fixture.request(
		t, fixture.memberToken,
		"/v1/projects/"+fixture.projectID.String()+"/provider-capabilities?executionTargetId="+fixture.targetID.String(),
	)
	if projectResponse.Code != http.StatusOK {
		t.Fatalf("project status = %d, body = %s", projectResponse.Code, projectResponse.Body.String())
	}
	project := decodeProviderCapabilityProjection(t, projectResponse)
	if project.ExecutionTargetID != fixture.targetID || project.TargetKind != "kubernetes" ||
		project.Basis != providercapabilities.BasisTarget || project.ExecutionID != nil {
		t.Fatalf("project projection identity = %#v", project)
	}
	if len(project.Items) != len(providercatalog.ProviderNames())*len(providercatalog.CapabilityIDs()) {
		t.Fatalf("project item count = %d", len(project.Items))
	}
	assertProjectedCapability(t, project, "codex", "send-turn", providercapabilities.StatusUnobserved, providercapabilities.ReasonWorkerManifestRequired)
	assertProjectedCapabilityMode(t, project, "codex", "rollback", providercapabilities.SupportModeEmulated)
	assertProjectedCapabilityMode(t, project, "codex", "fork", providercapabilities.SupportModeEmulated)
	assertProjectedCapability(t, project, "cursor", "send-turn", providercapabilities.StatusUnsupported, providercapabilities.ReasonCapabilityUnsupported)

	sessionResponse := fixture.request(
		t, fixture.operatorToken, "/v1/sessions/"+fixture.sessionID.String()+"/provider-capabilities",
	)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session status = %d, body = %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	sessionProjection := decodeProviderCapabilityProjection(t, sessionResponse)
	if sessionProjection.ExecutionTargetID != fixture.targetID || sessionProjection.TargetKind != "kubernetes" ||
		sessionProjection.Basis != providercapabilities.BasisExecution || sessionProjection.ExecutionID == nil ||
		*sessionProjection.ExecutionID != fixture.executionID {
		t.Fatalf("session projection identity = %#v", sessionProjection)
	}
	if len(sessionProjection.Items) != len(providercatalog.CapabilityIDs()) {
		t.Fatalf("session item count = %d", len(sessionProjection.Items))
	}
	assertProjectedCapability(t, sessionProjection, "codex", "steer-turn", providercapabilities.StatusUnobserved, providercapabilities.ReasonWorkerManifestRequired)
	assertProjectedCapability(t, sessionProjection, "codex", "compact", providercapabilities.StatusUnsupported, providercapabilities.ReasonProviderCursorRequired)
	assertProjectedCapabilityMode(t, sessionProjection, "codex", "rollback", providercapabilities.SupportModeEmulated)
	assertProjectedCapabilityMode(t, sessionProjection, "codex", "fork", providercapabilities.SupportModeEmulated)

	for _, response := range []*httptest.ResponseRecorder{projectResponse, sessionResponse} {
		var encoded map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &encoded); err != nil {
			t.Fatal(err)
		}
		if response == sessionResponse {
			assertManifestJSONKeys(t, encoded, "executionTargetId", "targetKind", "basis", "executionId", "items")
		} else {
			assertManifestJSONKeys(t, encoded, "executionTargetId", "targetKind", "basis", "items")
		}
		body := response.Body.String()
		for _, forbidden := range []string{
			"workerId", "workerManifestId", "manifestId", "lastHeartbeatAt", "workerStatusCounts",
			"clusterId", "namespace", "podName", "workerBuild", "runtimeVersion", "imageDigest",
		} {
			if containsJSONField(body, forbidden) {
				t.Fatalf("projection leaked operational field %q: %s", forbidden, body)
			}
		}
	}
}

func TestProviderCapabilityRoutesReuseProjectAndSessionReadAuthorization(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	projectPath := "/v1/projects/" + fixture.projectID.String() + "/provider-capabilities?executionTargetId=" + fixture.targetID.String()
	sessionPath := "/v1/sessions/" + fixture.sessionID.String() + "/provider-capabilities"

	for _, token := range []string{fixture.memberToken, fixture.operatorToken} {
		for _, path := range []string{projectPath, sessionPath} {
			if response := fixture.request(t, token, path); response.Code != http.StatusOK {
				t.Fatalf("authorized projection status = %d, body = %s", response.Code, response.Body.String())
			}
		}
	}
	for _, test := range []struct {
		name   string
		token  string
		path   string
		status int
		code   string
	}{
		{name: "unauthenticated", path: projectPath, status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "project outside organization", token: fixture.outsiderToken, path: projectPath, status: http.StatusNotFound, code: "organization_not_found"},
		{name: "session outside organization", token: fixture.outsiderToken, path: sessionPath, status: http.StatusNotFound, code: "organization_not_found"},
		{name: "private session hidden", token: fixture.memberToken, path: "/v1/sessions/" + fixture.privateSessionID.String() + "/provider-capabilities", status: http.StatusNotFound, code: "session_not_found"},
		{name: "invalid target query", token: fixture.memberToken, path: "/v1/projects/" + fixture.projectID.String() + "/provider-capabilities?executionTargetId=not-a-uuid", status: http.StatusBadRequest, code: "invalid_query_parameter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertProblemResponse(t, fixture.request(t, test.token, test.path), test.status, test.code)
		})
	}

	rawPath := "/v1/tenants/" + fixture.tenantID.String() + "/worker-manifests"
	assertProblemResponse(t, fixture.request(t, fixture.memberToken, rawPath), http.StatusForbidden, "tenant_forbidden")
}

func TestSessionCapabilityProjectionUsesExactExecutionManifest(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	boundManifestID := uuid.New()
	targetManifestID := uuid.New()
	seedProviderCapabilityManifest(t, fixture.db, boundManifestID, "unsupported")
	seedProviderCapabilityManifest(t, fixture.db, targetManifestID, "native")
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.executionID).
		Update("worker_manifest_id", boundManifestID).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := fixture.db.Create(&persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: fixture.targetID,
		TargetKind: "kubernetes", ClusterID: "sensitive-cluster", Namespace: "sensitive-namespace",
		PodName: "sensitive-pod", Version: "sensitive-worker-build", ProtocolVersion: 2,
		Capabilities: map[string]any{"secret": "must-not-leak"}, CurrentManifestID: &targetManifestID,
		CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("sensitive-worker-token"),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	projectResponse := fixture.request(
		t, fixture.memberToken,
		"/v1/projects/"+fixture.projectID.String()+"/provider-capabilities?executionTargetId="+fixture.targetID.String(),
	)
	project := decodeProviderCapabilityProjection(t, projectResponse)
	assertProjectedCapability(t, project, "codex", "steer-turn", providercapabilities.StatusSupported, providercapabilities.ReasonCapabilitySupported)

	sessionResponse := fixture.request(
		t, fixture.memberToken, "/v1/sessions/"+fixture.sessionID.String()+"/provider-capabilities",
	)
	sessionProjection := decodeProviderCapabilityProjection(t, sessionResponse)
	assertProjectedCapability(t, sessionProjection, "codex", "steer-turn", providercapabilities.StatusUnsupported, providercapabilities.ReasonCapabilityUnsupported)
	for _, secret := range []string{"sensitive-cluster", "sensitive-namespace", "sensitive-pod", "sensitive-worker-build", "must-not-leak", "sensitive-worker-token"} {
		if strings.Contains(projectResponse.Body.String(), secret) || strings.Contains(sessionResponse.Body.String(), secret) {
			t.Fatalf("capability projection leaked %q", secret)
		}
	}
}

type providerCapabilityHTTPFixture struct {
	db               *gorm.DB
	handler          http.Handler
	cookieName       string
	tenantID         uuid.UUID
	projectID        uuid.UUID
	sessionID        uuid.UUID
	privateSessionID uuid.UUID
	executionID      uuid.UUID
	targetID         uuid.UUID
	memberToken      string
	operatorToken    string
	outsiderToken    string
}

func newProviderCapabilityHTTPFixture(t *testing.T) providerCapabilityHTTPFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "provider-capability-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectID := uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Capability project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	if err := store.DB().Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "capability-kubernetes", Status: "active", ConfigurationEncrypted: []byte{},
		Capabilities: map[string]any{"providerPolicy": map[string]any{"experimentalProviders": []string{"codex", "claudeAgent"}}},
		CreatedAt:    now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	projectService := projects.NewService(store.DB())
	targetService := executiontargets.NewService(store.DB(), profile, nil)
	sessionService := sessions.NewService(store.DB(), projectService, targetService)
	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	created, err := sessionService.Create(ctx, owner, projectID, sessions.CreateSessionInput{
		Title: "Capability session", Visibility: "organization", Provider: "codex", ExecutionTargetID: &targetID,
	}, "provider-capability-session", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionService.CreateTurn(ctx, owner, created.ID, sessions.CreateTurnInput{
		InputText: "queue without a Worker", RuntimeMode: "full-access", InteractionMode: "default",
	}, "provider-capability-turn", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := store.DB().Where("tenant_id = ? AND session_id = ?", domain.TenantID, created.ID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	privateSession, err := sessionService.Create(ctx, owner, projectID, sessions.CreateSessionInput{
		Title: "Private capability session", Visibility: "private", Provider: "codex", ExecutionTargetID: &targetID,
	}, "provider-capability-private", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	memberID := createProviderCapabilityUser(t, store.DB(), domain.TenantID, domain.OrganizationID, "member")
	operatorID := createProviderCapabilityUser(t, store.DB(), domain.TenantID, domain.OrganizationID, "agent_operator")
	outsiderID := createProviderCapabilityUser(t, store.DB(), domain.TenantID, uuid.Nil, "")
	cfg := config.Config{
		Platform: profile, CookieName: "synara_provider_capability_session", CookiePath: "/",
		SessionTTL: time.Hour, SessionIdleTTL: time.Hour,
		SSEPollInterval: 20 * time.Millisecond, SSEHeartbeatInterval: time.Second,
		SSEWriteTimeout: time.Second, SSELeaseTTL: time.Minute,
		SSEMaxConnectionsPerUser: 1, SSEMaxConnectionsPerTenant: 10,
	}
	identityService := identity.NewService(store.DB(), cfg.SessionTTL, cfg.SessionIdleTTL)
	executionService := executions.NewService(
		store.DB(), sessionService, time.Minute, 90*time.Second, time.Hour, nil, targetService,
		executions.WithProjectService(projectService),
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(
		cfg, store.DB(), identityService, nil, projectService, sessionService, executionService, targetService,
		nil, nil, nil, nil, nil, observability.New(store.DB(), observability.Config{SessionIdleTTL: cfg.SessionIdleTTL}), nil, nil, nil, nil, nil, logger,
	)
	if err != nil {
		t.Fatal(err)
	}
	return providerCapabilityHTTPFixture{
		db: store.DB(), handler: server.Handler(), cookieName: cfg.CookieName, tenantID: domain.TenantID,
		projectID: projectID, sessionID: created.ID, privateSessionID: privateSession.ID,
		executionID: execution.ID, targetID: targetID,
		memberToken:   createProviderCapabilityLogin(t, store.DB(), memberID, domain.TenantID),
		operatorToken: createProviderCapabilityLogin(t, store.DB(), operatorID, domain.TenantID),
		outsiderToken: createProviderCapabilityLogin(t, store.DB(), outsiderID, domain.TenantID),
	}
}

func seedProviderCapabilityManifest(t *testing.T, db *gorm.DB, manifestID uuid.UUID, codexSteer string) {
	t.Helper()
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte("provider-capability-manifest:" + manifestID.String()))
	if err := db.Create(&persistence.WorkerManifest{
		ID: manifestID, ManifestHash: encodeProviderCapabilityDigest(digest),
		WorkerBuildVersion: "provider-capability-test", WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, OperatingSystem: "linux", Architecture: "amd64",
		FeatureFlags: map[string]any{}, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, entry := range providercatalog.Providers() {
		capabilities := make(map[string]any, len(entry.Capabilities))
		for capabilityID, support := range entry.Capabilities {
			capabilities[capabilityID] = support
		}
		if entry.Name == "codex" {
			capabilities["steer-turn"] = codexSteer
		}
		compatible := entry.SupportTier != "local-only"
		status := "compatible"
		var code, message *string
		if !compatible {
			status = "local-only"
			codeValue := providercapabilities.ReasonCapabilityUnsupported
			messageValue := "Provider is Local-only on remote Workers."
			code, message = &codeValue, &messageValue
		}
		version := entry.RuntimePolicy.CompatibleRange.MinimumInclusive
		descriptorDigest := sha256.Sum256([]byte(manifestID.String() + ":" + entry.Name))
		if err := db.Create(&persistence.WorkerProviderManifest{
			WorkerManifestID: manifestID, Provider: strings.ToLower(entry.Name), SupportTier: entry.SupportTier,
			CompatibilityStatus: status, ProviderHostMajor: 2, ProviderHostMinor: 1,
			HostBuildVersion: "host-test", AdapterVersion: entry.AdapterVersion,
			RuntimeKind: entry.RuntimePolicy.Kind, RuntimeName: entry.RuntimePolicy.Name, RuntimeVersion: &version,
			RuntimeAvailable: compatible, RuntimeVersionSource: entry.RuntimePolicy.VersionSource,
			RuntimeMinimumInclusive: entry.RuntimePolicy.CompatibleRange.MinimumInclusive,
			RuntimeCompatible:       compatible, ReleaseRequiresExplicitEnablement: entry.SupportTier == "experimental",
			ReleaseEnabled: true, MaximumCommandBytes: 1024, MaximumMessageBytes: 1024,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, CredentialDeliveryModes: []string{"anonymous-fd"},
			ResumeStrategies: []string{"authoritative-history"}, CapabilityDescriptorHash: encodeProviderCapabilityDigest(descriptorDigest),
			Capabilities: capabilities, IncompatibilityCode: code, IncompatibilityMessage: message, CheckedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func encodeProviderCapabilityDigest(digest [32]byte) string {
	const hexadecimal = "0123456789abcdef"
	encoded := make([]byte, len(digest)*2)
	for index, value := range digest {
		encoded[index*2] = hexadecimal[value>>4]
		encoded[index*2+1] = hexadecimal[value&0x0f]
	}
	return string(encoded)
}

func createProviderCapabilityUser(
	t *testing.T,
	db *gorm.DB,
	tenantID, organizationID uuid.UUID,
	organizationRole string,
) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	now := time.Now().UTC()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Capability reader",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "member", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if organizationID == uuid.Nil {
			return nil
		}
		return tx.Create(&persistence.OrganizationMembership{
			TenantID: tenantID, OrganizationID: organizationID, UserID: userID,
			Role: organizationRole, Status: "active", CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	return userID
}

func createProviderCapabilityLogin(t *testing.T, db *gorm.DB, userID, tenantID uuid.UUID) string {
	t.Helper()
	token := "provider-capability-" + uuid.NewString()
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

func (f providerCapabilityHTTPFixture) request(t *testing.T, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func decodeProviderCapabilityProjection(
	t *testing.T,
	response *httptest.ResponseRecorder,
) providercapabilities.Projection {
	t.Helper()
	var projection providercapabilities.Projection
	if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	return projection
}

func assertProjectedCapability(
	t *testing.T,
	projection providercapabilities.Projection,
	provider, capabilityID string,
	status providercapabilities.Status,
	reason string,
) {
	t.Helper()
	for _, item := range projection.Items {
		if item.Provider == provider && item.CapabilityID == capabilityID {
			if item.Status != status || item.ReasonCode != reason {
				t.Fatalf("item = %#v, want status=%q reason=%q", item, status, reason)
			}
			return
		}
	}
	t.Fatalf("missing capability %s/%s", provider, capabilityID)
}

func assertProjectedCapabilityMode(
	t *testing.T,
	projection providercapabilities.Projection,
	provider, capabilityID string,
	mode providercapabilities.SupportMode,
) {
	t.Helper()
	for _, item := range projection.Items {
		if item.Provider == provider && item.CapabilityID == capabilityID {
			if item.Status != providercapabilities.StatusSupported ||
				item.ReasonCode != providercapabilities.ReasonCapabilitySupported ||
				item.SupportMode == nil || *item.SupportMode != mode {
				t.Fatalf("item = %#v, want supported mode=%q", item, mode)
			}
			return
		}
	}
	t.Fatalf("missing capability %s/%s", provider, capabilityID)
}

func containsJSONField(body, field string) bool {
	return json.Valid([]byte(body)) && strings.Contains(body, `"`+field+`":`)
}
