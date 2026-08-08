package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/developerapi"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func TestDeveloperAPIServiceAccountCreatesSessionWithMachineAttribution(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	issued := issueDeveloperAPIKey(t, fixture, "member", []string{"api.access"})
	idempotencyKey := "machine-session-" + uuid.NewString()
	body := `{"title":"Machine-created Session","provider":"codex","executionTargetId":"` + fixture.targetID.String() + `"}`
	response := developerAPIRequest(
		t, fixture, http.MethodPost, "/v1/projects/"+fixture.projectID.String()+"/sessions",
		issued.Token, body, idempotencyKey,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("create Session status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		ID         uuid.UUID `json:"id"`
		Visibility string    `json:"visibility"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == uuid.Nil || created.Visibility != "project" {
		t.Fatalf("created Session = %#v", created)
	}

	var model persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, created.ID).Take(&model).Error; err != nil {
		t.Fatal(err)
	}
	if model.CreatedBy != fixture.ownerUserID {
		t.Fatalf("resource responsibility user = %s, want %s", model.CreatedBy, fixture.ownerUserID)
	}
	var event persistence.SessionEvent
	if err := fixture.db.Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.tenantID, created.ID, "session.created").Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.ActorType != "service_account" || event.ActorID == nil || *event.ActorID != issued.Account.ID {
		t.Fatalf("Session event actor = %q %#v", event.ActorType, event.ActorID)
	}
	var auditLog persistence.AuditLog
	if err := fixture.db.Where("tenant_id = ? AND action = ? AND resource_id = ?", fixture.tenantID, "session.created", created.ID).Take(&auditLog).Error; err != nil {
		t.Fatal(err)
	}
	if auditLog.ActorType != "service_account" || auditLog.ActorID == nil || *auditLog.ActorID != issued.Account.ID {
		t.Fatalf("Audit actor = %q %#v", auditLog.ActorType, auditLog.ActorID)
	}
	var idempotency persistence.APIIdempotencyKey
	if err := fixture.db.Where(
		"tenant_id = ? AND actor_id = ? AND idempotency_key = ?",
		fixture.tenantID, issued.Account.ID, idempotencyKey,
	).Take(&idempotency).Error; err != nil {
		t.Fatal(err)
	}
}

func TestDeveloperAPIServiceAccountEnforcesScopeRoleTenantAndPrivateBoundaries(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)

	t.Run("resource scope is required", func(t *testing.T) {
		issued := issueDeveloperAPIKey(t, fixture, "member", []string{"scim.read"})
		response := developerAPIRequest(t, fixture, http.MethodGet, "/v1/sessions/"+fixture.sessionID.String(), issued.Token, "", "")
		assertProblemResponse(t, response, http.StatusForbidden, "service_account_scope_forbidden")
	})

	t.Run("fixed role is enforced", func(t *testing.T) {
		issued := issueDeveloperAPIKey(t, fixture, "viewer", []string{"api.access"})
		body := `{"title":"Forbidden Session","provider":"codex","executionTargetId":"` + fixture.targetID.String() + `"}`
		response := developerAPIRequest(
			t, fixture, http.MethodPost, "/v1/projects/"+fixture.projectID.String()+"/sessions",
			issued.Token, body, "viewer-create-"+uuid.NewString(),
		)
		assertProblemResponse(t, response, http.StatusForbidden, "organization_forbidden")
	})

	t.Run("tenant path cannot substitute key scope", func(t *testing.T) {
		issued := issueDeveloperAPIKey(t, fixture, "member", []string{"api.access"})
		path := "/v1/tenants/" + uuid.NewString() + "/organizations/" + fixture.organizationID.String() + "/projects"
		response := developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", "")
		assertProblemResponse(t, response, http.StatusForbidden, "service_account_tenant_forbidden")
	})

	t.Run("organization scope cannot be substituted within its tenant", func(t *testing.T) {
		issued := issueDeveloperAPIKey(t, fixture, "member", []string{"api.access"})
		otherOrganizationID := uuid.New()
		if err := fixture.db.Create(&persistence.Organization{
			ID: otherOrganizationID, TenantID: fixture.tenantID, Slug: "other-" + uuid.NewString(),
			Name: "Other Organization", Kind: "business", Status: "active", Settings: map[string]any{},
			CreatedBy: fixture.ownerUserID,
		}).Error; err != nil {
			t.Fatal(err)
		}
		path := "/v1/tenants/" + fixture.tenantID.String() + "/organizations/" + otherOrganizationID.String() + "/projects"
		response := developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", "")
		assertProblemResponse(t, response, http.StatusNotFound, "organization_not_found")
	})

	t.Run("tenant scoped fixed role can authorize organization resources", func(t *testing.T) {
		issued := issueTenantDeveloperAPIKey(t, fixture, "admin")
		response := developerAPIRequest(t, fixture, http.MethodGet, "/v1/sessions/"+fixture.sessionID.String(), issued.Token, "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("tenant-scoped API Key status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("private Sessions are never visible to a machine", func(t *testing.T) {
		issued := issueDeveloperAPIKey(t, fixture, "member", []string{"api.access"})
		response := developerAPIRequest(t, fixture, http.MethodGet, "/v1/sessions/"+fixture.privateSessionID.String(), issued.Token, "", "")
		assertProblemResponse(t, response, http.StatusNotFound, "session_not_found")
	})

	t.Run("private Projects are never visible or creatable by a machine", func(t *testing.T) {
		issued := issueDeveloperAPIKey(t, fixture, "admin", []string{"api.access"})
		privateProjectID := uuid.New()
		if err := fixture.db.Create(&persistence.Project{
			ID: privateProjectID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
			Name: "Private Project", DefaultBranch: "main", Visibility: "private", CreatedBy: fixture.ownerUserID,
		}).Error; err != nil {
			t.Fatal(err)
		}
		response := developerAPIRequest(t, fixture, http.MethodGet, "/v1/projects/"+privateProjectID.String(), issued.Token, "", "")
		assertProblemResponse(t, response, http.StatusNotFound, "project_not_found")

		createPath := "/v1/tenants/" + fixture.tenantID.String() + "/organizations/" + fixture.organizationID.String() + "/projects"
		created := developerAPIRequest(
			t, fixture, http.MethodPost, createPath, issued.Token,
			`{"name":"Machine private Project","defaultBranch":"main","visibility":"private"}`,
			"private-project-"+uuid.NewString(),
		)
		assertProblemResponse(t, created, http.StatusBadRequest, "service_account_private_project_unsupported")
	})

	t.Run("internal routes do not accept developer API Keys", func(t *testing.T) {
		issued := issueDeveloperAPIKey(t, fixture, "member", []string{"api.access"})
		path := "/v1/tenants/" + fixture.tenantID.String() + "/worker-manifests"
		response := developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", "")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("internal route status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestDeveloperAPIServiceAccountRevocationIsImmediate(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	issued := issueDeveloperAPIKey(t, fixture, "member", []string{"api.access"})
	if err := fixture.server.serviceAccounts.Revoke(
		context.Background(), developerAPIKeyOwner(fixture), fixture.tenantID, issued.Account.ID,
		"revoke-"+uuid.NewString(), "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	response := developerAPIRequest(t, fixture, http.MethodGet, "/v1/sessions/"+fixture.sessionID.String(), issued.Token, "", "")
	assertProblemResponse(t, response, http.StatusUnauthorized, "invalid_service_account_token")
}

func TestDeveloperAPIProjectSessionsArePaginated(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	issued := issueDeveloperAPIKey(t, fixture, "member", []string{"api.access"})
	newerID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentSession{
		ID: newerID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, CreatedBy: fixture.ownerUserID, Title: "Newer public Session",
		Status: "active", Visibility: "project", Provider: "codex", ExecutionTargetID: fixture.targetID,
		CreatedAt: time.Now().UTC().Add(time.Hour), UpdatedAt: time.Now().UTC().Add(time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/" + fixture.projectID.String() + "/sessions?limit=1"
	first := developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first Session page status=%d body=%s", first.Code, first.Body.String())
	}
	var firstPage struct {
		Items []struct {
			ID uuid.UUID `json:"id"`
		} `json:"items"`
		NextCursor *string `json:"nextCursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 1 || firstPage.NextCursor == nil {
		t.Fatalf("first Session page = %#v", firstPage)
	}
	secondPath := "/v1/projects/" + fixture.projectID.String() + "/sessions?limit=1&cursor=" + url.QueryEscape(*firstPage.NextCursor)
	second := developerAPIRequest(t, fixture, http.MethodGet, secondPath, issued.Token, "", "")
	if second.Code != http.StatusOK || strings.Contains(second.Body.String(), fixture.privateSessionID.String()) {
		t.Fatalf("second Session page status=%d body=%s", second.Code, second.Body.String())
	}

	invalid := developerAPIRequest(
		t, fixture, http.MethodGet,
		"/v1/projects/"+fixture.projectID.String()+"/sessions?limit=201", issued.Token, "", "",
	)
	assertProblemResponse(t, invalid, http.StatusBadRequest, "invalid_session_limit")
}

func TestDeveloperAPISessionEventPageContract(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	issued := issueDeveloperAPIKey(t, fixture, "member", []string{"api.access"})
	basePath := "/v1/sessions/" + fixture.sessionID.String() + "/events"
	response := developerAPIRequest(t, fixture, http.MethodGet, basePath, issued.Token, "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("default Event page status=%d body=%s", response.Code, response.Body.String())
	}
	var page sessions.EventPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 || page.LastSequence < page.Items[len(page.Items)-1].Sequence {
		t.Fatalf("default Event page = %#v", page)
	}

	for _, query := range []string{"?limit=0", "?limit=201", "?afterSequence=-1"} {
		invalid := developerAPIRequest(t, fixture, http.MethodGet, basePath+query, issued.Token, "", "")
		wantCode := "invalid_event_limit"
		if query == "?afterSequence=-1" {
			wantCode = "invalid_event_sequence"
		}
		assertProblemResponse(t, invalid, http.StatusBadRequest, wantCode)
	}
}

func TestDeveloperAPIRateLimitAndUsageAttribution(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	limit := 1
	issued := issueDeveloperAPIKeyWithLimit(t, fixture, "member", []string{"api.access"}, &limit)
	path := "/v1/sessions/" + fixture.sessionID.String()

	first := developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", first.Code, first.Body.String())
	}
	if first.Header().Get("RateLimit-Limit") != "1" || first.Header().Get("RateLimit-Remaining") != "0" || first.Header().Get("RateLimit-Reset") == "" {
		t.Fatalf("first rate-limit headers = %#v", first.Header())
	}
	second := developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", "")
	assertProblemResponse(t, second, http.StatusTooManyRequests, "developer_api_rate_limited")
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("rate-limited response has no Retry-After header")
	}

	var aggregate persistence.ServiceAccountAPIUsageWindow
	if err := fixture.db.Where(
		"tenant_id = ? AND service_account_id = ? AND route_pattern = ?",
		fixture.tenantID, issued.Account.ID, "*",
	).Take(&aggregate).Error; err != nil {
		t.Fatal(err)
	}
	if aggregate.AdmittedCount != 1 || aggregate.RateLimitedCount != 1 || aggregate.RequestCount != 1 || aggregate.SuccessCount != 1 {
		t.Fatalf("aggregate usage = %#v", aggregate)
	}
	ownerToken := createProviderCapabilityLogin(t, fixture.db, fixture.ownerUserID, fixture.tenantID)
	usageResponse := fixture.request(
		t, ownerToken,
		"/v1/tenants/"+fixture.tenantID.String()+"/service-accounts/"+issued.Account.ID.String()+"/usage",
	)
	if usageResponse.Code != http.StatusOK {
		t.Fatalf("usage report status=%d body=%s", usageResponse.Code, usageResponse.Body.String())
	}
	var usageReport developerapi.UsageReport
	if err := json.Unmarshal(usageResponse.Body.Bytes(), &usageReport); err != nil {
		t.Fatal(err)
	}
	if usageReport.ServiceAccountID != issued.Account.ID || len(usageReport.Items) != 2 {
		t.Fatalf("usage report = %#v", usageReport)
	}
	invalidUsageRange := fixture.request(
		t, ownerToken,
		"/v1/tenants/"+fixture.tenantID.String()+"/service-accounts/"+issued.Account.ID.String()+
			"/usage?from=2026-08-03T12:00:00Z&to=2026-08-02T12:00:00Z",
	)
	assertProblemResponse(t, invalidUsageRange, http.StatusBadRequest, "invalid_developer_api_usage_range")

	secondKey := issueDeveloperAPIKeyWithLimit(t, fixture, "member", []string{"api.access"}, &limit)
	isolated := developerAPIRequest(t, fixture, http.MethodGet, path, secondKey.Token, "", "")
	if isolated.Code != http.StatusOK {
		t.Fatalf("isolated key request status=%d body=%s", isolated.Code, isolated.Body.String())
	}

	failureLimit := 5
	failureKey := issueDeveloperAPIKeyWithLimit(t, fixture, "member", []string{"api.access"}, &failureLimit)
	notFound := developerAPIRequest(t, fixture, http.MethodGet, "/v1/sessions/"+uuid.NewString(), failureKey.Token, "", "")
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("not-found request status=%d body=%s", notFound.Code, notFound.Body.String())
	}
	var routeUsage persistence.ServiceAccountAPIUsageWindow
	if err := fixture.db.Where(
		"tenant_id = ? AND service_account_id = ? AND route_pattern = ?",
		fixture.tenantID, failureKey.Account.ID, "GET /v1/sessions/{sessionID}",
	).Take(&routeUsage).Error; err != nil {
		t.Fatal(err)
	}
	if routeUsage.ClientErrorCount != 1 || routeUsage.RequestCount != 1 {
		t.Fatalf("failed request usage = %#v", routeUsage)
	}
}

func TestDeveloperAPIRateLimitRecoversInNextMinute(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	now := time.Date(2026, 8, 3, 12, 0, 30, 0, time.UTC)
	fixture.server.developerAPIUsage = developerapi.NewUsageService(fixture.db, developerapi.WithNow(func() time.Time { return now }))
	limit := 1
	issued := issueDeveloperAPIKeyWithLimit(t, fixture, "member", []string{"api.access"}, &limit)
	path := "/v1/sessions/" + fixture.sessionID.String()

	if response := developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", ""); response.Code != http.StatusOK {
		t.Fatalf("first window status=%d body=%s", response.Code, response.Body.String())
	}
	assertProblemResponse(
		t,
		developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", ""),
		http.StatusTooManyRequests,
		"developer_api_rate_limited",
	)
	now = now.Add(time.Minute)
	if response := developerAPIRequest(t, fixture, http.MethodGet, path, issued.Token, "", ""); response.Code != http.StatusOK {
		t.Fatalf("next window status=%d body=%s", response.Code, response.Body.String())
	}
}

func issueDeveloperAPIKey(
	t *testing.T,
	fixture providerCapabilityHTTPFixture,
	role string,
	scopes []string,
) serviceaccounts.IssuedAccount {
	return issueDeveloperAPIKeyWithLimit(t, fixture, role, scopes, nil)
}

func issueDeveloperAPIKeyWithLimit(
	t *testing.T,
	fixture providerCapabilityHTTPFixture,
	role string,
	scopes []string,
	rateLimitPerMinute *int,
) serviceaccounts.IssuedAccount {
	t.Helper()
	if fixture.server.serviceAccounts == nil {
		fixture.server.serviceAccounts = serviceaccounts.NewService(fixture.db)
	}
	organizationID := fixture.organizationID
	issued, err := fixture.server.serviceAccounts.Create(
		context.Background(), developerAPIKeyOwner(fixture), fixture.tenantID,
		serviceaccounts.CreateInput{
			OrganizationID: &organizationID, Name: "developer-key-" + uuid.NewString(),
			Role: role, Scopes: scopes, RateLimitPerMinute: rateLimitPerMinute,
		},
		"issue-"+uuid.NewString(), "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func issueTenantDeveloperAPIKey(
	t *testing.T,
	fixture providerCapabilityHTTPFixture,
	role string,
) serviceaccounts.IssuedAccount {
	t.Helper()
	if fixture.server.serviceAccounts == nil {
		fixture.server.serviceAccounts = serviceaccounts.NewService(fixture.db)
	}
	issued, err := fixture.server.serviceAccounts.Create(
		context.Background(), developerAPIKeyOwner(fixture), fixture.tenantID,
		serviceaccounts.CreateInput{
			Name: "tenant-developer-key-" + uuid.NewString(), Role: role, Scopes: []string{"api.access"},
		},
		"issue-"+uuid.NewString(), "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func developerAPIKeyOwner(fixture providerCapabilityHTTPFixture) identity.Principal {
	return identity.Principal{UserID: fixture.ownerUserID, ActiveTenantID: &fixture.tenantID}
}

func developerAPIRequest(
	t *testing.T,
	fixture providerCapabilityHTTPFixture,
	method, path, token, body, idempotencyKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	return response
}
