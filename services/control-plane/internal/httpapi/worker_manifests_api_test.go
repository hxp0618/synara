package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
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
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestListWorkerManifestsRouteProjectsOnlySafeManifestFields(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	recorder := fixture.request(t, fixture.ownerToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertManifestJSONKeys(t, envelope, "items")
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["items"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	item := items[0]
	assertManifestJSONKeys(t, item,
		"executionTargetId", "manifestId", "workerStatusCounts", "lastHeartbeatAt",
		"workerBuild", "workerProtocol", "runtimeEvent", "providers",
	)
	var targetID, manifestID uuid.UUID
	if err := json.Unmarshal(item["executionTargetId"], &targetID); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(item["manifestId"], &manifestID); err != nil {
		t.Fatal(err)
	}
	if targetID != fixture.targetID || manifestID != fixture.manifestID {
		t.Fatalf("unexpected Target/Manifest IDs: %s %s", targetID, manifestID)
	}

	var counts, build, workerProtocol, runtimeEvent map[string]json.RawMessage
	for encoded, destination := range map[string]*map[string]json.RawMessage{
		"workerStatusCounts": &counts,
		"workerBuild":        &build,
		"workerProtocol":     &workerProtocol,
		"runtimeEvent":       &runtimeEvent,
	} {
		if err := json.Unmarshal(item[encoded], destination); err != nil {
			t.Fatal(err)
		}
	}
	assertManifestJSONKeys(t, counts, "online", "draining", "offline")
	assertManifestJSONKeys(t, build, "version", "gitSha", "imageDigest", "operatingSystem", "architecture")
	assertManifestJSONKeys(t, workerProtocol, "minimum", "maximum")
	assertManifestJSONKeys(t, runtimeEvent, "minimum", "maximum")

	var providers []map[string]json.RawMessage
	if err := json.Unmarshal(item["providers"], &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers) != len(workerManifestHTTPProviderNames) {
		t.Fatalf("provider count = %d", len(providers))
	}
	for index, provider := range providers {
		assertManifestJSONKeys(t, provider,
			"provider", "supportTier", "compatibilityStatus", "runtime", "releasePolicy", "capabilities",
		)
		var providerName string
		if err := json.Unmarshal(provider["provider"], &providerName); err != nil {
			t.Fatal(err)
		}
		if providerName != workerManifestHTTPProviderNames[index] {
			t.Fatalf("provider %d = %q", index, providerName)
		}
		var runtime, releasePolicy, capabilities map[string]json.RawMessage
		if err := json.Unmarshal(provider["runtime"], &runtime); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(provider["releasePolicy"], &releasePolicy); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(provider["capabilities"], &capabilities); err != nil {
			t.Fatal(err)
		}
		expectedRuntimeKeys := []string{"kind", "name", "available", "versionSource", "compatibleRange", "compatible"}
		if providerName == "codex" || providerName == "claudeAgent" {
			expectedRuntimeKeys = append(expectedRuntimeKeys, "version")
		}
		assertManifestJSONKeys(t, runtime, expectedRuntimeKeys...)
		assertManifestJSONKeys(t, releasePolicy, "requiresExplicitEnablement", "enabled")
		assertManifestJSONKeys(t, capabilities, workerManifestHTTPCapabilityIDs...)
	}

	body := recorder.Body.String()
	for _, forbidden := range fixture.sensitiveValues {
		if forbidden != "" && strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
}

func TestListWorkerManifestsRouteEnforcesAuthenticationActiveTenantAndWorkerRead(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	tests := []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{name: "unauthenticated", status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "active tenant mismatch", token: fixture.crossTenantToken, status: http.StatusNotFound, code: "tenant_not_found"},
		{name: "missing worker read", token: fixture.memberToken, status: http.StatusForbidden, code: "tenant_forbidden"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertProblemResponse(t, fixture.request(t, test.token), test.status, test.code)
		})
	}
}

type workerManifestHTTPFixture struct {
	handler          http.Handler
	cookieName       string
	tenantID         uuid.UUID
	targetID         uuid.UUID
	workerID         uuid.UUID
	manifestID       uuid.UUID
	ownerToken       string
	readOnlyToken    string
	memberToken      string
	crossTenantToken string
	sensitiveValues  []string
}

func newWorkerManifestHTTPFixture(t *testing.T) workerManifestHTTPFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "manifest-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	manifestID := uuid.New()
	workerID := uuid.New()
	sensitive := []string{
		workerID.String(), "cluster-sensitive", "namespace-sensitive", "pod-sensitive",
		"RAW-WORKER-CAPABILITY-SECRET", "WORKER-TOKEN-HASH-SECRET", "TARGET-CONFIG-SECRET",
		"MANIFEST-FEATURE-SECRET", "RAW-WORKER-VERSION",
	}
	seedWorkerManifestHTTPModels(t, store.DB(), domain.ExecutionTargetID, workerID, manifestID, now)
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", domain.ExecutionTargetID).
		Update("configuration_encrypted", []byte("TARGET-CONFIG-SECRET")).Error; err != nil {
		t.Fatal(err)
	}

	ownerToken := createWorkerManifestHTTPLogin(t, store.DB(), domain.UserID, domain.TenantID)
	memberID := uuid.New()
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Manifest reader",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	memberToken := createWorkerManifestHTTPLogin(t, store.DB(), memberID, domain.TenantID)
	securityAdminID := uuid.New()
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: securityAdminID, Email: uuid.NewString() + "@example.com", DisplayName: "Worker reader",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: securityAdminID, Role: "security_admin", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	readOnlyToken := createWorkerManifestHTTPLogin(t, store.DB(), securityAdminID, domain.TenantID)

	otherTenantID := uuid.New()
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: otherTenantID, Slug: "manifest-http-other-" + uuid.NewString(), Name: "Other tenant",
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
	crossTenantToken := createWorkerManifestHTTPLogin(t, store.DB(), domain.UserID, otherTenantID)

	cfg := config.Config{
		Platform: profile, CookieName: "synara_manifest_session", CookiePath: "/",
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
	return workerManifestHTTPFixture{
		handler: server.Handler(), cookieName: cfg.CookieName, tenantID: domain.TenantID,
		targetID: domain.ExecutionTargetID, workerID: workerID, manifestID: manifestID,
		ownerToken: ownerToken, readOnlyToken: readOnlyToken, memberToken: memberToken, crossTenantToken: crossTenantToken,
		sensitiveValues: sensitive,
	}
}

func (f workerManifestHTTPFixture) request(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/tenants/"+f.tenantID.String()+"/worker-manifests", nil)
	if token != "" {
		request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func seedWorkerManifestHTTPModels(
	t *testing.T,
	db *gorm.DB,
	targetID, workerID, manifestID uuid.UUID,
	now time.Time,
) {
	t.Helper()
	gitSHA := "abcdef1234567890"
	imageDigest := "sha256:" + digestWorkerManifestHTTP("image:"+manifestID.String())
	if err := db.Create(&persistence.WorkerManifest{
		ID: manifestID, ManifestHash: digestWorkerManifestHTTP("manifest:" + manifestID.String()),
		WorkerBuildVersion: "manifest-version", WorkerBuildGitSHA: &gitSHA,
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", ImageDigest: &imageDigest,
		FeatureFlags: map[string]any{"secret": "MANIFEST-FEATURE-SECRET"}, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	capabilities := make(map[string]any, len(workerManifestHTTPCapabilityIDs))
	for _, capabilityID := range workerManifestHTTPCapabilityIDs {
		capabilities[capabilityID] = "native"
	}
	for _, provider := range workerManifestHTTPProviderNames {
		experimental := provider == "codex" || provider == "claudeAgent"
		supportTier := "local-only"
		compatibilityStatus := "local-only"
		runtimeAvailable := false
		runtimeCompatible := false
		var runtimeVersion *string
		if experimental {
			supportTier = "experimental"
			compatibilityStatus = "compatible"
			runtimeAvailable = true
			runtimeCompatible = true
			version := "1.0.0"
			runtimeVersion = &version
		}
		runtimeKind := "cli"
		runtimeSource := "probe"
		if provider == "claudeAgent" {
			runtimeKind = "sdk"
			runtimeSource = "package"
		}
		if err := db.Create(&persistence.WorkerProviderManifest{
			WorkerManifestID: manifestID, Provider: provider, SupportTier: supportTier,
			CompatibilityStatus: compatibilityStatus, ProviderHostMajor: 2, ProviderHostMinor: 1,
			HostBuildVersion: "host-test", AdapterVersion: "adapter-test",
			RuntimeKind: runtimeKind, RuntimeName: provider + "-runtime", RuntimeVersion: runtimeVersion,
			RuntimeAvailable: runtimeAvailable, RuntimeVersionSource: runtimeSource,
			RuntimeMinimumInclusive: "0.0.0", RuntimeCompatible: runtimeCompatible,
			ReleaseRequiresExplicitEnablement: experimental, ReleaseEnabled: true,
			MaximumCommandBytes: 1024, MaximumMessageBytes: 1024,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
			CredentialDeliveryModes: []string{"anonymous-fd"}, ResumeStrategies: []string{"authoritative-history"},
			CapabilityDescriptorHash: digestWorkerManifestHTTP(manifestID.String() + ":" + provider),
			Capabilities:             capabilities, CheckedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID, TargetKind: "local",
		ClusterID: "cluster-sensitive", Namespace: "namespace-sensitive", PodName: "pod-sensitive",
		Version: "RAW-WORKER-VERSION", ProtocolVersion: 2,
		Capabilities: map[string]any{"raw": "RAW-WORKER-CAPABILITY-SECRET"}, CurrentManifestID: &manifestID,
		CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("WORKER-TOKEN-HASH-SECRET"),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func createWorkerManifestHTTPLogin(t *testing.T, db *gorm.DB, userID, tenantID uuid.UUID) string {
	t.Helper()
	token := "manifest-http-" + uuid.NewString()
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

func digestWorkerManifestHTTP(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func assertManifestJSONKeys(t *testing.T, value map[string]json.RawMessage, expected ...string) {
	t.Helper()
	actual := make([]string, 0, len(value))
	for key := range value {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if len(actual) != len(expected) {
		t.Fatalf("JSON keys = %v, want %v", actual, expected)
	}
	for index := range actual {
		if actual[index] != expected[index] {
			t.Fatalf("JSON keys = %v, want %v", actual, expected)
		}
	}
}

var workerManifestHTTPProviderNames = []string{
	"codex", "claudeAgent", "cursor", "gemini", "grok", "kilo", "opencode", "pi",
}

var workerManifestHTTPCapabilityIDs = []string{
	"discovery", "start-session", "resume-session", "send-turn", "steer-turn", "interrupt-turn",
	"approval", "structured-user-input", "plan-mode", "review", "compact", "rollback", "fork",
	"read-history", "model-list", "model-switch", "skill-discovery", "skill-mentions",
	"plugin-discovery", "plugin-mentions", "native-commands", "tool-events", "diff-events", "usage-events",
	"checkpoint", "credential-injection", "authoritative-history-reconstruction", "worker-migration",
}
