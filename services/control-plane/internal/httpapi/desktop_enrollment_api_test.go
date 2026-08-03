package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestRequireAuthSeparatesWebCookiesAndDesktopBearerCredentials(t *testing.T) {
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "desktop-auth-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	publicKey := make([]byte, 32)
	publicKeyHash := sha256.Sum256(publicKey)
	device := persistence.DesktopDevice{
		ID: uuid.New(), ControlPlaneOrigin: "https://control.example.com",
		UserID: domain.UserID, DefaultTenantID: domain.TenantID,
		PublicKey: publicKey, PublicKeySHA256: publicKeyHash[:], Platform: "darwin",
		AppVersion: "0.6.3", DeviceLabel: "HTTP test device", Status: "active", Version: 1,
		LastSeenAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&device).Error; err != nil {
		t.Fatal(err)
	}
	webToken, webHash, err := secret.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	desktopToken, desktopHash, err := secret.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	familyID := uuid.New()
	for _, session := range []persistence.LoginSession{
		{
			ID: uuid.New(), UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
			AuthMethod: "local", Audience: "web", RefreshTokenHash: webHash,
			ExpiresAt: now.Add(time.Hour), LastSeenAt: now, CreatedAt: now,
		},
		{
			ID: uuid.New(), UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
			AuthMethod: "desktop", Audience: "desktop", DesktopDeviceID: &device.ID,
			CredentialFamilyID: &familyID, RefreshTokenHash: desktopHash,
			ExpiresAt: now.Add(time.Hour), LastSeenAt: now, CreatedAt: now,
		},
	} {
		if err := store.DB().Create(&session).Error; err != nil {
			t.Fatal(err)
		}
	}
	identityService := identity.NewService(store.DB(), time.Hour, time.Hour)
	server := &Server{
		config: config.Config{CookieName: "synara_test", PlatformOperatorTenantID: domain.TenantID},
		db:     store.DB(), identity: identityService,
		supportAccess: supportaccess.NewService(store.DB(), domain.TenantID), logger: slog.Default(),
	}
	echoPrincipal := server.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mustPrincipal(r))
	}))

	webRequest := httptest.NewRequest(http.MethodGet, "http://control.test/v1/auth/session", nil)
	webRequest.AddCookie(&http.Cookie{Name: "synara_test", Value: webToken})
	webResponse := httptest.NewRecorder()
	echoPrincipal.ServeHTTP(webResponse, webRequest)
	if webResponse.Code != http.StatusOK {
		t.Fatalf("Web cookie status = %d, body = %s", webResponse.Code, webResponse.Body.String())
	}
	var webPrincipal identity.Principal
	if err := json.Unmarshal(webResponse.Body.Bytes(), &webPrincipal); err != nil {
		t.Fatal(err)
	}
	if webPrincipal.Audience != "web" || webPrincipal.DesktopDeviceID != nil {
		t.Fatalf("Web principal = %#v", webPrincipal)
	}

	desktopRequest := httptest.NewRequest(http.MethodGet, "http://control.test/v1/auth/session", nil)
	desktopRequest.Header.Set("Authorization", "Bearer "+desktopToken)
	desktopResponse := httptest.NewRecorder()
	echoPrincipal.ServeHTTP(desktopResponse, desktopRequest)
	if desktopResponse.Code != http.StatusOK {
		t.Fatalf("Desktop Bearer status = %d, body = %s", desktopResponse.Code, desktopResponse.Body.String())
	}
	var desktopPrincipal identity.Principal
	if err := json.Unmarshal(desktopResponse.Body.Bytes(), &desktopPrincipal); err != nil {
		t.Fatal(err)
	}
	if desktopPrincipal.Audience != "desktop" || desktopPrincipal.DesktopDeviceID == nil ||
		*desktopPrincipal.DesktopDeviceID != device.ID {
		t.Fatalf("Desktop principal = %#v", desktopPrincipal)
	}

	ambiguous := httptest.NewRequest(http.MethodGet, "http://control.test/v1/auth/session", nil)
	ambiguous.AddCookie(&http.Cookie{Name: "synara_test", Value: webToken})
	ambiguous.Header.Set("Authorization", "Bearer "+desktopToken)
	ambiguousResponse := httptest.NewRecorder()
	echoPrincipal.ServeHTTP(ambiguousResponse, ambiguous)
	if ambiguousResponse.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous auth status = %d, body = %s", ambiguousResponse.Code, ambiguousResponse.Body.String())
	}
	assertHTTPProblemCode(t, ambiguousResponse, "ambiguous_authentication")

	webAsBearer := httptest.NewRequest(http.MethodGet, "http://control.test/v1/auth/session", nil)
	webAsBearer.Header.Set("Authorization", "Bearer "+webToken)
	webAsBearerResponse := httptest.NewRecorder()
	echoPrincipal.ServeHTTP(webAsBearerResponse, webAsBearer)
	if webAsBearerResponse.Code != http.StatusUnauthorized {
		t.Fatalf("Web token as Bearer status = %d, body = %s", webAsBearerResponse.Code, webAsBearerResponse.Body.String())
	}
	assertHTTPProblemCode(t, webAsBearerResponse, "invalid_desktop_session")

	platformHandler := server.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := server.supportAccess.RequirePlatformAdmin(r.Context(), mustPrincipal(r)); err != nil {
			server.writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	platformRequest := httptest.NewRequest(http.MethodPost, "http://control.test/v1/platform/tenants", nil)
	platformRequest.Header.Set("Authorization", "Bearer "+desktopToken)
	platformResponse := httptest.NewRecorder()
	platformHandler.ServeHTTP(platformResponse, platformRequest)
	if platformResponse.Code != http.StatusForbidden {
		t.Fatalf("Desktop Platform status = %d, body = %s", platformResponse.Code, platformResponse.Body.String())
	}
	assertHTTPProblemCode(t, platformResponse, "platform_web_session_required")
}

func TestDesktopRedemptionRateLimiterUsesBoundedWindows(t *testing.T) {
	limiter := newDesktopRedemptionRateLimiter(2, time.Minute)
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	if !limiter.Allow("127.0.0.1", now) || !limiter.Allow("127.0.0.1", now.Add(time.Second)) {
		t.Fatal("first two redemption attempts should be allowed")
	}
	if limiter.Allow("127.0.0.1", now.Add(2*time.Second)) {
		t.Fatal("third redemption attempt should be rate limited")
	}
	if !limiter.Allow("127.0.0.1", now.Add(time.Minute)) {
		t.Fatal("redemption window should reset at the configured boundary")
	}
}
