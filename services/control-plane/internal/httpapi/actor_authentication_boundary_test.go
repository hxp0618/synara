package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
)

func TestActorCredentialsAreNonInterchangeableAcrossHTTPAuthenticationBoundaries(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	var tenant persistence.Tenant
	if err := fixture.db.Where("id = ?", fixture.tenantID).Take(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	serviceAccountService := serviceaccounts.NewService(fixture.db)
	fixture.server.serviceAccounts = serviceAccountService
	serviceAccount, err := serviceAccountService.Create(
		context.Background(),
		identity.Principal{UserID: tenant.CreatedBy, ActiveTenantID: &fixture.tenantID},
		fixture.tenantID,
		serviceaccounts.CreateInput{Name: "Actor boundary SCIM", Scopes: []string{"scim.read"}},
		"actor-boundary-service-account", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	workerToken := "actor-boundary-worker-" + uuid.NewString()
	if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", fixture.workerID).
		Update("auth_token_hash", secret.HashToken(workerToken)).Error; err != nil {
		t.Fatal(err)
	}

	type boundary struct {
		name    string
		wrap    func(http.Handler) http.Handler
		failure string
	}
	boundaries := []boundary{
		{name: "login-session", wrap: fixture.server.requireAuth, failure: "invalid_session"},
		{name: "service-account", wrap: fixture.server.requireServiceAccount, failure: "invalid_service_account_token"},
		{name: "worker", wrap: fixture.server.requireWorker, failure: "invalid_worker_token"},
	}
	credentials := []struct {
		name  string
		token string
	}{
		{name: "login-session", token: fixture.ownerToken},
		{name: "service-account", token: serviceAccount.Token},
		{name: "worker", token: workerToken},
	}

	for _, target := range boundaries {
		for _, credential := range credentials {
			t.Run(credential.name+"_to_"+target.name, func(t *testing.T) {
				reached := false
				handler := target.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					reached = true
					w.WriteHeader(http.StatusNoContent)
				}))
				request := httptest.NewRequest(http.MethodGet, "http://control-plane.test/v1/actor-boundary", nil)
				if target.name == "login-session" {
					request.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: credential.token})
				} else {
					request.Header.Set("Authorization", "Bearer "+credential.token)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if credential.name == target.name {
					if !reached || response.Code != http.StatusNoContent {
						t.Fatalf("matching credential was rejected: status=%d body=%s", response.Code, response.Body.String())
					}
					return
				}
				if reached {
					t.Fatal("credential crossed into a different authentication boundary")
				}
				assertProblemResponse(t, response, http.StatusUnauthorized, target.failure)
			})
		}
	}
}
