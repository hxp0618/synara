package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/incidentgovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformIncidentGovernanceEnforcesAssignedPublicRoleThroughHTTP(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "incident-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	communications := persistence.User{
		ID: uuid.New(), Email: "incident-http-comms@example.test", DisplayName: "Incident HTTP Comms",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&communications).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: communications.ID, Role: "admin", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	server := &Server{
		supportAccess: supportaccess.NewService(store.DB(), domain.TenantID),
		incidentGovernance: incidentgovernance.NewService(
			store.DB(), domain.TenantID, "https://status.example.test/history",
		),
	}

	createdResponse := callIncidentHandler(t, server.createPlatformIncident, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
	}, "", map[string]any{
		"incidentKey": "INC-HTTP-117", "severity": "SEV-1",
		"title":                 "Public execution delay",
		"internalImpactSummary": "Customers may observe delayed execution starts while recovery is in progress.",
		"broadInternalImpact":   true, "securityPrivacyImpact": false,
		"affectedComponents": []string{"execution-scheduling"}, "affectedRegions": []string{"global"},
		"communicationsLeadUserId": communications.ID, "startedAt": now,
		"impactConfirmedAt": now.Add(time.Minute),
	})
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("incident create status = %d, body = %s", createdResponse.Code, createdResponse.Body.String())
	}
	var incident incidentgovernance.Incident
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &incident); err != nil {
		t.Fatal(err)
	}
	boundResponse := callIncidentHandler(t, server.bindPlatformIncidentStatusBoard, identity.Principal{
		UserID: communications.ID, ActiveTenantID: &domain.TenantID,
	}, incident.ID.String(), map[string]any{
		"expectedVersion": incident.Version, "internalStatusBoardIncidentReference": "provider-http-117",
		"reason": "Bind the external public incident before recording updates.",
	})
	if boundResponse.Code != http.StatusOK {
		t.Fatalf("incident bind status = %d, body = %s", boundResponse.Code, boundResponse.Body.String())
	}
	if err := json.Unmarshal(boundResponse.Body.Bytes(), &incident); err != nil {
		t.Fatal(err)
	}
	wrongRole := callIncidentHandler(t, server.addPlatformIncidentInternalUpdate, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
	}, incident.ID.String(), map[string]any{
		"expectedVersion": incident.Version, "kind": "initial",
		"summary":           "The commander must not publish as the assigned communications lead.",
		"publishedAt":       now.Add(2 * time.Minute),
		"evidenceReference": "https://status.example.test/incidents/provider-http-117/updates/initial",
	})
	if wrongRole.Code != http.StatusConflict {
		t.Fatalf("wrong-role update status = %d, body = %s", wrongRole.Code, wrongRole.Body.String())
	}
	assertHTTPProblemCode(t, wrongRole, "incident_internal_update_conflict")

	updatedResponse := callIncidentHandler(t, server.addPlatformIncidentInternalUpdate, identity.Principal{
		UserID: communications.ID, ActiveTenantID: &domain.TenantID,
	}, incident.ID.String(), map[string]any{
		"expectedVersion": incident.Version, "kind": "initial",
		"summary":           "We are investigating delayed execution starts and will provide another update.",
		"publishedAt":       now.Add(2 * time.Minute),
		"evidenceReference": "https://status.example.test/incidents/provider-http-117/updates/initial",
	})
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("incident update status = %d, body = %s", updatedResponse.Code, updatedResponse.Body.String())
	}
	if err := json.Unmarshal(updatedResponse.Body.Bytes(), &incident); err != nil {
		t.Fatal(err)
	}
	if len(incident.InternalUpdates) != 1 || incident.InternalUpdates[0].CreatedBy.UserID != communications.ID {
		t.Fatalf("HTTP incident update = %#v", incident)
	}
}

func callIncidentHandler(
	t *testing.T,
	handler http.HandlerFunc,
	principal identity.Principal,
	incidentID string,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://control-plane.test/v1/platform/incidents", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", uuid.NewString())
	if incidentID != "" {
		request.SetPathValue("incidentID", incidentID)
	}
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
