package httpapi

import (
	"bytes"
	"context"
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
	"github.com/synara-ai/synara/services/control-plane/internal/entitlements"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestTenantCreationSeparatesSelfServiceAndPlatformAuthority(t *testing.T) {
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(
		ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(
		ctx, store.DB(), platform.ProfilePersonal, "platform-tenant-http-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	customerOwner := persistence.User{
		ID: uuid.New(), Email: "http-customer-owner@example.com", DisplayName: "HTTP Customer Owner",
		Status: "active", EmailVerifiedAt: &now,
	}
	outsider := persistence.User{
		ID: uuid.New(), Email: "http-outsider@example.com", DisplayName: "HTTP Outsider",
		Status: "active", EmailVerifiedAt: &now,
	}
	for _, user := range []persistence.User{customerOwner, outsider} {
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{
		config: config.Config{PlatformOperatorTenantID: domain.TenantID},
		db:     store.DB(), tenancy: tenancy.NewService(store.DB()), entitlements: entitlements.NewService(store.DB()),
		supportAccess: supportaccess.NewService(store.DB(), domain.TenantID), logger: slog.Default(),
	}

	selfServiceResponse := callPlatformTenantHandler(t, server.createTenant, identity.Principal{
		UserID: outsider.ID,
	}, map[string]any{
		"slug": "self-enterprise", "name": "Self Enterprise", "entitlementProfileCode": "enterprise", "status": "active",
	})
	if selfServiceResponse.Code != http.StatusForbidden {
		t.Fatalf("self-service enterprise status = %d, body = %s", selfServiceResponse.Code, selfServiceResponse.Body.String())
	}
	assertHTTPProblemCode(t, selfServiceResponse, "self_service_entitlement_profile_forbidden")

	unauthorizedResponse := callPlatformTenantHandler(t, server.provisionPlatformTenant, identity.Principal{
		UserID: outsider.ID,
	}, map[string]any{
		"ownerEmail": customerOwner.Email, "slug": "unauthorized-enterprise",
		"name": "Unauthorized Enterprise", "region": "default", "entitlementProfileCode": "enterprise", "status": "active",
	})
	if unauthorizedResponse.Code != http.StatusForbidden {
		t.Fatalf("unauthorized platform provision status = %d, body = %s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}
	assertHTTPProblemCode(t, unauthorizedResponse, "platform_operator_forbidden")

	authorizedResponse := callPlatformTenantHandler(t, server.provisionPlatformTenant, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
	}, map[string]any{
		"ownerEmail": customerOwner.Email, "slug": "authorized-enterprise",
		"name": "Authorized Enterprise", "region": "eu-west", "entitlementProfileCode": "enterprise", "status": "active",
	})
	if authorizedResponse.Code != http.StatusCreated {
		t.Fatalf("authorized platform provision status = %d, body = %s", authorizedResponse.Code, authorizedResponse.Body.String())
	}
	var created tenancy.ProvisionedTenant
	if err := json.Unmarshal(authorizedResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.PlanCode != "enterprise" || created.OwnerUserID != customerOwner.ID {
		t.Fatalf("authorized platform Tenant = %#v", created)
	}

	periodStart := now
	periodEnd := now.AddDate(0, 1, 0)
	assignmentResponse := callPlatformTenantPathHandler(
		t, server.assignPlatformTenantEntitlementProfile, identity.Principal{
			UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
		}, created.ID, map[string]any{
			"entitlementProfileCode": "standard", "status": "active", "expectedVersion": 1,
			"reportingPeriodStart": periodStart, "reportingPeriodEnd": periodEnd,
			"reason": "Approved internal entitlement profile change.",
		},
	)
	if assignmentResponse.Code != http.StatusOK {
		t.Fatalf("Platform Plan assignment status = %d, body = %s", assignmentResponse.Code, assignmentResponse.Body.String())
	}
	var snapshot entitlementProfileResponse
	if err := json.Unmarshal(assignmentResponse.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Profile.Code != "standard" || snapshot.Assignment.Version != 2 ||
		snapshot.Assignment.AssignmentSource != "platform_admin" {
		t.Fatalf("Platform entitlement profile assignment = %#v", snapshot)
	}
	var publicContract map[string]json.RawMessage
	if err := json.Unmarshal(assignmentResponse.Body.Bytes(), &publicContract); err != nil {
		t.Fatal(err)
	}
	for _, legacyField := range []string{"plan", "planAssignment", "planCode", "trialEndsAt"} {
		if _, exists := publicContract[legacyField]; exists {
			t.Fatalf("Platform entitlement response exposed legacy field %q: %s", legacyField, assignmentResponse.Body.String())
		}
	}
	for _, legacyToken := range [][]byte{[]byte(`"planCode"`), []byte(`"trialEndsAt"`), []byte(`"currentPeriodStart"`), []byte(`"trialing"`)} {
		if bytes.Contains(assignmentResponse.Body.Bytes(), legacyToken) {
			t.Fatalf("Platform entitlement response exposed legacy token %s: %s", legacyToken, assignmentResponse.Body.String())
		}
	}

	operatorTargetResponse := callPlatformTenantPathHandler(
		t, server.assignPlatformTenantEntitlementProfile, identity.Principal{
			UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
		}, domain.TenantID, map[string]any{
			"entitlementProfileCode": "standard", "status": "active", "expectedVersion": 1,
			"reportingPeriodStart": periodStart, "reportingPeriodEnd": periodEnd,
			"reason": "Forbidden Operator Tenant change.",
		},
	)
	if operatorTargetResponse.Code != http.StatusForbidden {
		t.Fatalf("Operator Tenant Plan assignment status = %d, body = %s", operatorTargetResponse.Code, operatorTargetResponse.Body.String())
	}
	assertHTTPProblemCode(t, operatorTargetResponse, "platform_operator_entitlement_profile_forbidden")
}

func callPlatformTenantHandler(
	t *testing.T,
	handler http.HandlerFunc,
	principal identity.Principal,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://control-plane.test/v1/tenants", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func callPlatformTenantPathHandler(
	t *testing.T,
	handler http.HandlerFunc,
	principal identity.Principal,
	tenantID uuid.UUID,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPut,
		"http://control-plane.test/v1/platform/tenants/"+tenantID.String()+"/entitlement-profile",
		bytes.NewReader(encoded),
	)
	request.Header.Set("Content-Type", "application/json")
	request.SetPathValue("tenantID", tenantID.String())
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertHTTPProblemCode(t *testing.T, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != want {
		t.Fatalf("problem code = %q, want %q; body = %s", envelope.Error.Code, want, response.Body.String())
	}
}
