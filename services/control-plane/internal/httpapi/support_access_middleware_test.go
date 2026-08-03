package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSupportAccessMiddlewareAuditsEveryRequestAndBlocksWrites(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "support-middleware-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	supportUser := persistence.User{ID: uuid.New(), Email: "middleware-support@example.com", DisplayName: "Middleware Support", Status: "active", EmailVerifiedAt: &now}
	platformUser := persistence.User{ID: uuid.New(), Email: "middleware-platform@example.com", DisplayName: "Middleware Platform", Status: "active", EmailVerifiedAt: &now}
	customerUser := persistence.User{ID: uuid.New(), Email: "middleware-customer@example.com", DisplayName: "Middleware Customer", Status: "active", EmailVerifiedAt: &now}
	for _, user := range []persistence.User{supportUser, platformUser, customerUser} {
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, membership := range []persistence.TenantMembership{
		{TenantID: domain.TenantID, UserID: supportUser.ID, Role: "security_admin", Status: "active", JoinedAt: &now},
		{TenantID: domain.TenantID, UserID: platformUser.ID, Role: "admin", Status: "active", JoinedAt: &now},
	} {
		if err := store.DB().Create(&membership).Error; err != nil {
			t.Fatal(err)
		}
	}
	customerPrincipal := identity.Principal{UserID: customerUser.ID}
	customerTenant, err := tenancy.NewService(store.DB()).CreateTenant(ctx, customerPrincipal, tenancy.CreateTenantInput{
		Slug: "middleware-customer-" + uuid.NewString()[:8], Name: "Middleware Customer", PlanCode: "free", Status: "active",
	}, "middleware-customer-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantSupportPolicy{
		TenantID: customerTenant.ID, SupportAccessEnabled: true, Version: 1,
		Reason: "Enable middleware support access verification.", UpdatedBy: customerUser.ID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	grantID := uuid.New()
	decisionReason := "Approved for middleware read-only verification."
	decidedBy := domain.UserID
	expiresAt := now.Add(time.Hour)
	if err := store.DB().Create(&persistence.SupportAccessGrant{
		ID: grantID, TenantID: customerTenant.ID, OperatorTenantID: &domain.TenantID,
		RequesterUserID: supportUser.ID,
		Status:          "pending", Version: 1, Reason: "Inspect middleware authorization without changing customer state.",
		RequestedDurationSeconds: 3600, RequestedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.SupportAccessGrant{}).Where("id = ?", grantID).
		Updates(map[string]any{
			"status": "active", "version": 2, "decided_by": decidedBy,
			"decision_reason": decisionReason, "decided_at": now, "expires_at": expiresAt,
		}).Error; err != nil {
		t.Fatal(err)
	}
	token := "middleware-support-token"
	tokenHash := sha256.Sum256([]byte(token))
	sessionID := uuid.New()
	if err := store.DB().Create(&persistence.LoginSession{
		ID: sessionID, UserID: supportUser.ID, ActiveTenantID: &customerTenant.ID,
		AuthMethod: "local", RefreshTokenHash: tokenHash[:], ExpiresAt: expiresAt, LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	platformToken := "middleware-platform-token"
	platformTokenHash := sha256.Sum256([]byte(platformToken))
	platformSessionID := uuid.New()
	if err := store.DB().Create(&persistence.LoginSession{
		ID: platformSessionID, UserID: platformUser.ID, ActiveTenantID: &customerTenant.ID,
		AuthMethod: "local", RefreshTokenHash: platformTokenHash[:], ExpiresAt: expiresAt, LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	server := &Server{
		config: config.Config{CookieName: "synara_support_test"}, db: store.DB(),
		identity:      identity.NewService(store.DB(), time.Hour, time.Hour),
		supportAccess: supportaccess.NewService(store.DB(), domain.TenantID),
		tenancy:       tenancy.NewService(store.DB()),
	}
	handlerCalls := 0
	handler := server.withRequestContext(server.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handlerCalls++
		w.WriteHeader(http.StatusNoContent)
	})))

	request := func(method, pattern string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://control-plane.test/v1/test", nil)
		req.Pattern = pattern
		req.AddCookie(&http.Cookie{Name: "synara_support_test", Value: token})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := request(http.MethodGet, "GET /v1/test"); response.Code != http.StatusNoContent {
		t.Fatalf("support GET status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := request(http.MethodPost, "POST /v1/test"); response.Code != http.StatusForbidden {
		t.Fatalf("support POST status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := request(http.MethodPut, "PUT /v1/auth/active-tenant"); response.Code != http.StatusNoContent {
		t.Fatalf("support exit control status = %d, body = %s", response.Code, response.Body.String())
	}
	if handlerCalls != 2 {
		t.Fatalf("support middleware invoked protected handler %d times, want GET plus exit control", handlerCalls)
	}
	var auditCount, readCount, controlCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action LIKE ? AND resource_id = ?", customerTenant.ID, "support.%", grantID).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 3 {
		t.Fatalf("Support Access request audits = %d, want all 3 requests", auditCount)
	}
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND resource_id = ?", customerTenant.ID, "support.read_accessed", grantID).
		Count(&readCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND resource_id = ?", customerTenant.ID, "support.control_attempted", grantID).
		Count(&controlCount).Error; err != nil {
		t.Fatal(err)
	}
	if readCount != 1 || controlCount != 2 {
		t.Fatalf("Support Access read/control audits = %d/%d, want 1/2", readCount, controlCount)
	}

	t.Run("all explicit customer routes preserve Platform and Support boundaries", func(t *testing.T) {
		routes := explicitTenantRouteInventory(t)
		invoke := func(token string, route explicitTenantRoute, next http.HandlerFunc) *httptest.ResponseRecorder {
			t.Helper()
			path := strings.ReplaceAll(route.template, "{tenantID}", customerTenant.ID.String())
			path = routeParameterPattern.ReplaceAllString(path, uuid.NewString())
			req := httptest.NewRequest(route.method, "http://control-plane.test"+path, nil)
			req.Pattern = route.method + " " + route.template
			req.SetPathValue("tenantID", customerTenant.ID.String())
			req.AddCookie(&http.Cookie{Name: "synara_support_test", Value: token})
			response := httptest.NewRecorder()
			server.withRequestContext(server.requireAuth(next)).ServeHTTP(response, req)
			return response
		}

		for _, route := range routes {
			route := route
			t.Run("support/"+route.method+" "+route.template, func(t *testing.T) {
				reached := false
				response := invoke(token, route, func(w http.ResponseWriter, _ *http.Request) {
					reached = true
					w.WriteHeader(http.StatusNoContent)
				})
				if route.method == http.MethodGet || route.method == http.MethodHead {
					if !reached || response.Code != http.StatusNoContent {
						t.Fatalf("Support read did not reach handler: status=%d body=%s", response.Code, response.Body.String())
					}
					return
				}
				if reached {
					t.Fatal("Support write reached a customer handler")
				}
				assertProblemResponse(t, response, http.StatusForbidden, "support_access_read_only")
			})

			if route.method == http.MethodPost && route.template == "/v1/tenants/{tenantID}/restore" {
				continue
			}
			t.Run("platform-without-grant/"+route.method+" "+route.template, func(t *testing.T) {
				reached := false
				response := invoke(platformToken, route, func(w http.ResponseWriter, _ *http.Request) {
					reached = true
					w.WriteHeader(http.StatusNoContent)
				})
				if reached {
					t.Fatal("Platform operator without Support Grant reached a customer handler")
				}
				assertProblemResponse(t, response, http.StatusNotFound, "tenant_not_found")
			})
		}
		var allSupportAuditCount int64
		if err := store.DB().Model(&persistence.AuditLog{}).
			Where("tenant_id = ? AND action LIKE ? AND resource_id = ?", customerTenant.ID, "support.%", grantID).
			Count(&allSupportAuditCount).Error; err != nil {
			t.Fatal(err)
		}
		if expected := int64(3 + len(routes)); allSupportAuditCount != expected {
			t.Fatalf("Support Access audit count = %d, want %d for generic plus every explicit Tenant route", allSupportAuditCount, expected)
		}

		t.Run("deletion recovery real handler rejects Platform operator without Grant", func(t *testing.T) {
			body := bytes.NewBufferString(`{"expectedVersion":1,"reason":"Verify the owner-only customer recovery boundary."}`)
			req := httptest.NewRequest(http.MethodPost, "http://control-plane.test/v1/tenants/"+customerTenant.ID.String()+"/restore", body)
			req.Pattern = "POST /v1/tenants/{tenantID}/restore"
			req.SetPathValue("tenantID", customerTenant.ID.String())
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: "synara_support_test", Value: platformToken})
			response := httptest.NewRecorder()
			server.withRequestContext(server.requireAuth(http.HandlerFunc(server.restoreTenant))).ServeHTTP(response, req)
			assertProblemResponse(t, response, http.StatusNotFound, "tenant_not_found")
		})
	})
}
